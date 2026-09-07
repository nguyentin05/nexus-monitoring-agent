package agent

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

type Discovery struct {
	cfg       Config
	telemetry *Telemetry
	processor *Processor
	catalog   *PatternCatalog
}

func NewDiscovery(cfg Config, telemetry *Telemetry, processor *Processor, catalog *PatternCatalog) *Discovery {
	return &Discovery{cfg: cfg, telemetry: telemetry, processor: processor, catalog: catalog}
}

func (d *Discovery) Run(ctx context.Context) {
	d.RunOnce(ctx)
	ticker := time.NewTicker(d.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.RunOnce(ctx)
		}
	}
}

func (d *Discovery) RunOnce(ctx context.Context) int {
	d.processor.Stats.Polls.Add(1)
	found := 0
	for _, service := range d.cfg.DiscoveryServices {
		logs, logErr := d.telemetry.ErrorLogs(ctx, service, 5*time.Minute)
		if logErr != nil {
			slog.Warn("discovery logs", "service", service, "error", logErr)
		}
		evidence := Evidence{Logs: logs}
		incidents := make([]Incident, 0, 1)
		if d.catalog != nil {
			patterns, err := d.catalog.Observe(service, logs, d.cfg.Mode == "shadow")
			if err != nil {
				slog.Warn("persist pattern catalog", "service", service, "error", err)
			}
			for _, pattern := range patterns {
				d.processor.Stats.NewPatterns.Add(1)
				correlationKey := ""
				if isOTelCollectorExportFailure(pattern.Template) {
					correlationKey = "dependency:opentelemetry-collector:export-failure"
				}
				incidents = append(incidents, Incident{
					Source:         "discovery",
					AlertName:      "DiscoveryNovelLogPattern",
					Kind:           "novel_log_pattern",
					Service:        service,
					Namespace:      d.cfg.Namespace,
					Severity:       "warning",
					Description:    "New error log pattern: " + pattern.Template,
					Fingerprint:    pattern.ID,
					CorrelationKey: correlationKey,
					StartedAt:      pattern.LastSeen,
					Evidence:       evidence,
				})
			}
		}
		for _, incident := range incidents {
			if d.processor.Submit(incident) {
				found++
			}
		}
	}
	return found
}

func isOTelCollectorExportFailure(template string) bool {
	text := strings.ToLower(template)
	return strings.Contains(text, "opentelemetry.exporter.otlp") &&
		strings.Contains(text, "opentelemetry-collector.monitoring.svc.cluster.local")
}
