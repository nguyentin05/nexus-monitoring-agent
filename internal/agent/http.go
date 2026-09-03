package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type HTTPServer struct {
	cfg       Config
	processor *Processor
	discovery *Discovery
	watched   map[string]struct{}
}

func NewHTTPServer(cfg Config, processor *Processor, discovery *Discovery) *HTTPServer {
	watched := make(map[string]struct{}, len(cfg.WatchedServices))
	for _, service := range cfg.WatchedServices {
		watched[service] = struct{}{}
	}
	return &HTTPServer{cfg: cfg, processor: processor, discovery: discovery, watched: watched}
}

func (s *HTTPServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("GET /watched-services", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"services": s.cfg.WatchedServices})
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, s.processor.Metrics())
	})
	mux.HandleFunc("POST /trigger", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusAccepted, map[string]int{"incidents_submitted": s.discovery.RunOnce(r.Context())})
	})
	mux.HandleFunc("POST /alerts", s.alerts)
	return mux
}

type alertmanagerPayload struct {
	Status string `json:"status"`
	Alerts []struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		StartsAt    time.Time         `json:"startsAt"`
		Fingerprint string            `json:"fingerprint"`
	} `json:"alerts"`
}

func (s *HTTPServer) alerts(w http.ResponseWriter, r *http.Request) {
	var payload alertmanagerPayload
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid Alertmanager payload"})
		return
	}
	accepted := 0
	for _, alert := range payload.Alerts {
		if payload.Status == "resolved" || alert.Status == "resolved" {
			continue
		}
		service := first(alert.Labels, "service", "job", "app", "container", "container_name")
		if _, ok := s.watched[service]; !ok {
			continue
		}
		namespace := first(alert.Labels, "namespace", "k8s_ns_name")
		if namespace == "" {
			namespace = s.cfg.Namespace
		}
		alertName := alert.Labels["alertname"]
		startedAt := alert.StartsAt
		if startedAt.IsZero() {
			startedAt = time.Now().UTC()
		}
		incident := Incident{
			Source:      "alertmanager",
			AlertName:   alertName,
			Kind:        alertKind(alertName),
			Service:     service,
			Namespace:   namespace,
			Severity:    first(alert.Labels, "severity", "priority"),
			Description: redact(strings.TrimSpace(alert.Annotations["summary"]+". "+alert.Annotations["description"]), 2000),
			Fingerprint: alert.Fingerprint,
			StartedAt:   startedAt,
		}
		if incident.Severity == "" {
			incident.Severity = "warning"
		}
		if s.processor.Submit(incident) {
			accepted++
		} else {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "incident queue is full"})
			return
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"accepted": accepted})
}

func alertKind(alertName string) string {
	switch alertName {
	case "NexusServiceHigh5xxRate":
		return "error_rate_high"
	case "NexusPodFrequentRestarts":
		return "frequent_restarts"
	case "NodeHighCpuUsage":
		return "cpu_high"
	default:
		return strings.ToLower(alertName)
	}
}

func first(values map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(values[key]); value != "" {
			return value
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		_, _ = fmt.Fprintln(w, `{"error":"encode response"}`)
	}
}
