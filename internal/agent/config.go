package agent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Address            string
	Mode               string
	PrometheusURL      string
	LokiURL            string
	AWSRegion          string
	BedrockModelID     string
	DiscordWebhookURL  string
	Namespace          string
	WatchedServices    []string
	DiscoveryServices  []string
	PollInterval       time.Duration
	Cooldown           time.Duration
	HTTPTimeout        time.Duration
	BedrockTimeout     time.Duration
	RCACacheTTL        time.Duration
	CPUThreshold       float64
	ErrorRateThreshold float64
	P99ThresholdMS     float64
	MaxLogSamples      int
	QueueSize          int
	MaxBedrockCalls    int
	PatternStatePath   string
	PatternAutoPromote int
	MaxPatterns        int
}

func LoadConfig() (Config, error) {
	watchedServices := env("WATCHED_SERVICES", "auth-service,profile-service")
	cfg := Config{
		Address:            env("ADDRESS", ":8080"),
		Mode:               env("AGENT_MODE", "shadow"),
		PrometheusURL:      env("PROMETHEUS_URL", "http://monitoring-prometheus.monitoring.svc.cluster.local:9090"),
		LokiURL:            env("LOKI_URL", "http://loki-gateway.monitoring.svc.cluster.local"),
		AWSRegion:          env("AWS_REGION", "ap-southeast-1"),
		BedrockModelID:     env("BEDROCK_MODEL_ID", "global.amazon.nova-2-lite-v1:0"),
		DiscordWebhookURL:  os.Getenv("DISCORD_WEBHOOK_URL"),
		Namespace:          env("TARGET_NAMESPACE", "apps"),
		WatchedServices:    strings.Split(watchedServices, ","),
		DiscoveryServices:  strings.Split(env("DISCOVERY_SERVICES", watchedServices), ","),
		CPUThreshold:       80,
		ErrorRateThreshold: 5,
		P99ThresholdMS:     500,
		MaxLogSamples:      10,
		QueueSize:          64,
		MaxBedrockCalls:    20,
		PatternStatePath:   os.Getenv("PATTERN_STATE_PATH"),
		PatternAutoPromote: 3,
		MaxPatterns:        1000,
	}

	var err error
	if cfg.PollInterval, err = durationEnv("POLL_INTERVAL", time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.Cooldown, err = durationEnv("INCIDENT_COOLDOWN", 10*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.HTTPTimeout, err = durationEnv("HTTP_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.BedrockTimeout, err = durationEnv("BEDROCK_TIMEOUT", 30*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.RCACacheTTL, err = durationEnv("RCA_CACHE_TTL", time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.CPUThreshold, err = floatEnv("CPU_THRESHOLD_PERCENT", cfg.CPUThreshold); err != nil {
		return Config{}, err
	}
	if cfg.ErrorRateThreshold, err = floatEnv("ERROR_RATE_THRESHOLD_PERCENT", cfg.ErrorRateThreshold); err != nil {
		return Config{}, err
	}
	if cfg.P99ThresholdMS, err = floatEnv("P99_LATENCY_THRESHOLD_MS", cfg.P99ThresholdMS); err != nil {
		return Config{}, err
	}
	if cfg.MaxLogSamples, err = intEnv("MAX_LOG_SAMPLES", cfg.MaxLogSamples); err != nil {
		return Config{}, err
	}
	if cfg.QueueSize, err = intEnv("INCIDENT_QUEUE_SIZE", cfg.QueueSize); err != nil {
		return Config{}, err
	}
	if cfg.MaxBedrockCalls, err = nonNegativeIntEnv("MAX_BEDROCK_CALLS_PER_HOUR", cfg.MaxBedrockCalls); err != nil {
		return Config{}, err
	}
	if cfg.PatternAutoPromote, err = intEnv("PATTERN_AUTO_PROMOTE_AFTER", cfg.PatternAutoPromote); err != nil {
		return Config{}, err
	}
	if cfg.MaxPatterns, err = intEnv("MAX_PATTERNS", cfg.MaxPatterns); err != nil {
		return Config{}, err
	}

	if cfg.Mode != "training" && cfg.Mode != "shadow" && cfg.Mode != "detect" {
		return Config{}, fmt.Errorf("AGENT_MODE must be training, shadow or detect")
	}
	if cfg.Address == "" || cfg.PrometheusURL == "" || cfg.LokiURL == "" || cfg.AWSRegion == "" || cfg.BedrockModelID == "" || cfg.Namespace == "" {
		return Config{}, fmt.Errorf("ADDRESS, PROMETHEUS_URL, LOKI_URL, AWS_REGION, BEDROCK_MODEL_ID and TARGET_NAMESPACE must not be empty")
	}
	for _, service := range cfg.WatchedServices {
		if service == "" || service != strings.TrimSpace(service) {
			return Config{}, fmt.Errorf("WATCHED_SERVICES must contain comma-separated service names without spaces")
		}
	}
	for _, service := range cfg.DiscoveryServices {
		if service == "" || service != strings.TrimSpace(service) {
			return Config{}, fmt.Errorf("DISCOVERY_SERVICES must contain comma-separated service names without spaces")
		}
	}
	return cfg, nil
}

func env(name, fallback string) string {
	value, exists := os.LookupEnv(name)
	if !exists {
		return fallback
	}
	return value
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value, exists := os.LookupEnv(name)
	if !exists {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return parsed, nil
}

func floatEnv(name string, fallback float64) (float64, error) {
	value, exists := os.LookupEnv(name)
	if !exists {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative number", name)
	}
	return parsed, nil
}

func intEnv(name string, fallback int) (int, error) {
	value, exists := os.LookupEnv(name)
	if !exists {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

func nonNegativeIntEnv(name string, fallback int) (int, error) {
	value, exists := os.LookupEnv(name)
	if !exists {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return parsed, nil
}
