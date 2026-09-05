package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
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

func TestEventsDropsStaleEvidenceAndSortsNewestFirst(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[
			{"metadata":{"creationTimestamp":"2026-09-05T09:59:00Z"},"reason":"Old","message":"stale","involvedObject":{"name":"service-old"}},
			{"metadata":{"creationTimestamp":"2026-09-05T10:01:00Z"},"reason":"New","message":"current","involvedObject":{"name":"service-new"}},
			{"metadata":{"creationTimestamp":"2026-09-05T10:00:00Z"},"reason":"Current","message":"current","involvedObject":{"name":"service-current"}},
			{"metadata":{"creationTimestamp":"2026-09-05T10:02:00Z"},"reason":"Other","message":"unrelated","involvedObject":{"name":"other"}}]}`))
	}))
	defer server.Close()

	client := &KubeClient{baseURL: server.URL, httpClient: server.Client()}
	events, err := client.Events(context.Background(), "apps", "service", time.Date(2026, 9, 5, 10, 1, 30, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Reason != "New" || events[1].Reason != "Current" {
		t.Fatalf("unexpected events: %+v", events)
	}
}

func TestIncidentLookbackIsBounded(t *testing.T) {
	now := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	if got := incidentLookback(now.Add(-time.Minute), now); got != 3*time.Minute {
		t.Fatalf("lookback = %s", got)
	}
	if got := incidentLookback(now.Add(-time.Hour), now); got != 5*time.Minute {
		t.Fatalf("bounded lookback = %s", got)
	}
}

func TestMessageTextJoinsAllTextBlocks(t *testing.T) {
	got, err := messageText([]types.ContentBlock{
		&types.ContentBlockMemberText{Value: "prefix"},
		&types.ContentBlockMemberText{Value: `{"root_cause":"certificate expired"}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "prefix\n{\"root_cause\":\"certificate expired\"}" {
		t.Fatalf("unexpected text: %q", got)
	}
}
