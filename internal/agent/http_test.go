package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAlertsRequireAuthenticatedAlertmanager(t *testing.T) {
	processor := NewProcessor(Config{QueueSize: 1, Cooldown: time.Minute}, &fakeCollector{}, &fakeLLM{}, fakeNotifier{})
	server := NewHTTPServer(Config{WatchedServices: []string{"auth-service"}}, processor)
	server.authenticate = func(_ context.Context, token string) (bool, error) {
		return token == "valid-token", nil
	}
	handler := server.Handler()
	payload := `{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"NexusServiceHigh5xxRate","service":"auth-service"}}]}`

	request := httptest.NewRequest(http.MethodPost, "/alerts", strings.NewReader(payload))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/alerts", strings.NewReader(payload))
	request.Header.Set("Authorization", "Bearer valid-token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || processor.Stats.Received.Load() != 1 {
		t.Fatalf("authenticated status=%d received=%d", response.Code, processor.Stats.Received.Load())
	}
}

func TestNodeAlertBypassesServiceWatchlist(t *testing.T) {
	processor := NewProcessor(Config{QueueSize: 1, Cooldown: time.Minute}, &fakeCollector{}, &fakeLLM{}, fakeNotifier{})
	server := NewHTTPServer(Config{WatchedServices: []string{"auth-service"}}, processor)
	server.authenticate = func(context.Context, string) (bool, error) { return true, nil }
	payload := `{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"NodeHighMemoryUsage","node":"ip-10-0-1-2"},"annotations":{"summary":"Node memory is high"}}]}`
	request := httptest.NewRequest(http.MethodPost, "/alerts", strings.NewReader(payload))
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d", response.Code)
	}
	select {
	case incident := <-processor.queue:
		if incident.Service != "ip-10-0-1-2" || incident.Kind != "node_memory_high" {
			t.Fatalf("unexpected incident: %+v", incident)
		}
	default:
		t.Fatal("node alert was not submitted")
	}
}

func TestTriggerEndpointIsNotExposed(t *testing.T) {
	server := NewHTTPServer(Config{}, NewProcessor(Config{QueueSize: 1}, &fakeCollector{}, &fakeLLM{}, fakeNotifier{}))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/trigger", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d", response.Code)
	}
}
