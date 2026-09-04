package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorkloadStatusIncludesLastTerminationReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis/apps/v1/namespaces/apps/deployments/service":
			_, _ = w.Write([]byte(`{"spec":{"replicas":1},"status":{"availableReplicas":0}}`))
		case "/api/v1/namespaces/apps/pods":
			_, _ = w.Write([]byte(`{"items":[{"status":{"containerStatuses":[{"restartCount":2,"lastState":{"terminated":{"reason":"OOMKilled"}}}]}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &KubeClient{baseURL: server.URL, httpClient: server.Client()}
	status, err := client.WorkloadStatus(context.Background(), "apps", "service")
	if err != nil {
		t.Fatal(err)
	}
	if status.Restarts != 2 || len(status.TerminationReasons) != 1 || status.TerminationReasons[0] != "OOMKilled" {
		t.Fatalf("unexpected workload status: %+v", status)
	}
}
