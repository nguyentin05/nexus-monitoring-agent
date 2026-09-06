package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDiscoveryOnlyQueriesLogs(t *testing.T) {
	var prometheusCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/query" {
			prometheusCalls.Add(1)
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		if r.URL.Path != "/loki/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"result":[{"stream":{"pod":"auth-service-x"},"values":[["%d","ERROR dependency timeout"]]}]}}`, time.Now().UnixNano())
	}))
	defer server.Close()
	cfg := Config{Mode: "shadow", Namespace: "apps", DiscoveryServices: []string{"auth-service"}, PrometheusURL: server.URL, LokiURL: server.URL, MaxLogSamples: 10, QueueSize: 2, Cooldown: time.Minute, MaxPatterns: 10, PatternAutoPromote: 3}
	processor := NewProcessor(cfg, &fakeCollector{}, &fakeLLM{}, fakeNotifier{})
	catalog, err := NewPatternCatalog("", 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	discovery := NewDiscovery(cfg, NewTelemetry(cfg, server.Client()), processor, catalog)
	if found := discovery.RunOnce(t.Context()); found != 1 {
		t.Fatalf("found=%d", found)
	}
	if prometheusCalls.Load() != 0 {
		t.Fatalf("metric discovery made %d Prometheus calls", prometheusCalls.Load())
	}
}
