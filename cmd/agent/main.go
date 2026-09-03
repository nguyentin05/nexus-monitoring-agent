package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/nguyentin05/nexus-monitoring-agent/internal/agent"
)

func main() {
	if err := run(); err != nil {
		slog.Error("monitoring agent stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := agent.LoadConfig()
	if err != nil {
		return err
	}
	catalog, err := agent.NewPatternCatalog(cfg.PatternStatePath, cfg.PatternAutoPromote, cfg.MaxPatterns)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	httpClient := &http.Client{Timeout: cfg.HTTPTimeout}
	telemetry := agent.NewTelemetry(cfg, httpClient)
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.AWSRegion))
	if err != nil {
		return err
	}
	bedrock := agent.NewBedrock(bedrockruntime.NewFromConfig(awsCfg), cfg.BedrockModelID, cfg.BedrockTimeout)
	notifier := agent.NewDiscord(cfg.DiscordWebhookURL, httpClient)
	processor := agent.NewProcessor(cfg, telemetry, bedrock, notifier)
	discovery := agent.NewDiscovery(cfg, telemetry, processor, catalog)
	server := &http.Server{Addr: cfg.Address, Handler: agent.NewHTTPServer(cfg, processor, discovery).Handler(), ReadHeaderTimeout: 5 * time.Second}

	go processor.Run(ctx)
	go discovery.Run(ctx)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	slog.Info("Nexus monitoring agent started", "address", cfg.Address, "services", cfg.WatchedServices, "model", cfg.BedrockModelID)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
