package agent

import (
	"context"
	"fmt"
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
		metrics, metricErr := d.telemetry.ServiceMetrics(ctx, service)
		logs, logErr := d.telemetry.ErrorLogs(ctx, service, 5*time.Minute)
		if metricErr != nil {
			slog.Warn("discovery metrics", "service", service, "error", metricErr)
		}
		if logErr != nil {
			slog.Warn("discovery logs", "service", service, "error", logErr)
		}
		evidence := Evidence{Metrics: &metrics, Logs: logs}
		incidents := d.detectMetrics(service, evidence)
		if d.catalog != nil {
			patterns, err := d.catalog.Observe(service, logs, d.cfg.Mode == "training")
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

func (d *Discovery) detectMetrics(service string, evidence Evidence) []Incident {
	now := time.Now().UTC()
	base := Incident{Source: "discovery", Service: service, Namespace: d.cfg.Namespace, Severity: "warning", StartedAt: now, Evidence: evidence}
	incidents := make([]Incident, 0, 3)
	if value := evidence.Metrics.CPUPercent; value != nil && *value > d.cfg.CPUThreshold {
		incident := base
		incident.AlertName, incident.Kind = "DiscoveryCPUHigh", "cpu_high"
		incident.Description = fmt.Sprintf("CPU usage %.2f%% exceeded %.2f%%", *value, d.cfg.CPUThreshold)
		if *value >= 95 {
			incident.Severity = "critical"
		}
		incidents = append(incidents, incident)
	}
	if value := evidence.Metrics.ErrorRatePercent; value != nil && *value > d.cfg.ErrorRateThreshold {
		incident := base
		incident.AlertName, incident.Kind, incident.Severity = "DiscoveryHigh5xxRate", "error_rate_high", "critical"
		incident.Description = fmt.Sprintf("5xx rate %.2f%% exceeded %.2f%%", *value, d.cfg.ErrorRateThreshold)
		incidents = append(incidents, incident)
	}
	if value := evidence.Metrics.P99LatencyMS; value != nil && *value > d.cfg.P99ThresholdMS {
		incident := base
		incident.AlertName, incident.Kind = "DiscoveryHighP99Latency", "latency_high"
		incident.Description = fmt.Sprintf("p99 latency %.2fms exceeded %.2fms", *value, d.cfg.P99ThresholdMS)
		incidents = append(incidents, incident)
	}
	return incidents
}
