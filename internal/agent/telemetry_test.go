package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
	events, err := client.Events(context.Background(), "apps", "service", time.Date(2026, 9, 5, 10, 1, 30, 0, time.UTC), 2*time.Minute, 10)
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

func TestNetworkPoliciesSummarizesPoliciesSelectingService(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/networking.k8s.io/v1/namespaces/apps/networkpolicies" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"items":[
			{"metadata":{"name":"deny-service-egress"},"spec":{"podSelector":{"matchLabels":{"app.kubernetes.io/name":"service"}},"policyTypes":["Egress"],"egress":[{"ports":[{"protocol":"UDP","port":53},{"protocol":"TCP","port":53}]}]}},
			{"metadata":{"name":"other"},"spec":{"podSelector":{"matchLabels":{"app.kubernetes.io/name":"other"}},"policyTypes":["Egress"]}}]}`))
	}))
	defer server.Close()

	client := &KubeClient{baseURL: server.URL, httpClient: server.Client()}
	policies, err := client.NetworkPolicies(context.Background(), "apps", "service")
	if err != nil {
		t.Fatal(err)
	}
	if len(policies) != 1 || policies[0].Name != "deny-service-egress" || len(policies[0].AllowedPorts) != 2 || policies[0].AllowedPorts[0] != "UDP/53" {
		t.Fatalf("unexpected policies: %+v", policies)
	}
}

func TestTraceAndDeploymentContextAreBounded(t *testing.T) {
	anchor := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/search":
			_, _ = w.Write([]byte(`{"traces":[{"traceID":"abc","rootServiceName":"service","rootTraceName":"GET /users","startTimeUnixNano":"1788602400000000000","durationMs":750}]}`))
		case "/apis/apps/v1/namespaces/apps/replicasets":
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"service-new","creationTimestamp":"2026-09-05T09:55:00Z","annotations":{"deployment.kubernetes.io/revision":"2"}},"spec":{"replicas":1,"template":{"spec":{"containers":[{"image":"repo/service:v2"}]}}},"status":{"readyReplicas":1}},{"metadata":{"name":"service-future","creationTimestamp":"2026-09-05T10:10:00Z"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	telemetry := &Telemetry{cfg: Config{TempoURL: server.URL}, httpClient: server.Client()}
	traces, err := telemetry.TraceContext(context.Background(), "service", anchor, CollectionStep{LookbackMinutes: 10, Limit: 1, TraceStatus: TraceStatusSlow, MinDurationMS: 500})
	if err != nil || len(traces) != 1 || traces[0].TraceID != "abc" {
		t.Fatalf("traces=%+v err=%v", traces, err)
	}
	client := &KubeClient{baseURL: server.URL, httpClient: server.Client()}
	changes, err := client.RecentChanges(context.Background(), "apps", "service", anchor, 10*time.Minute, 5)
	if err != nil || len(changes) != 1 || changes[0].Revision != "2" {
		t.Fatalf("changes=%+v err=%v", changes, err)
	}
}

func TestLogTargetOnlyAllowsKnownRelatedOperator(t *testing.T) {
	incident := Incident{Kind: "unknown_signal", Namespace: "apps", Service: "auth-service", Description: "Vault secret reconciliation failed"}
	namespace, container, err := logTarget(incident, TargetRelatedOperator)
	if err != nil || namespace != "external-secrets" || container != "external-secrets" {
		t.Fatalf("namespace=%q container=%q err=%v", namespace, container, err)
	}
	if _, _, err := logTarget(Incident{Description: "network timeout"}, TargetRelatedOperator); err == nil {
		t.Fatal("unexpected related operator mapping")
	}
}

func TestRelatedNodePrefersUnhealthyPod(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"service-ready"},"spec":{"nodeName":"node-a"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}},{"metadata":{"name":"service-failed"},"spec":{"nodeName":"node-b"},"status":{"phase":"Failed","conditions":[{"type":"Ready","status":"False"}]}}]}`))
	}))
	defer server.Close()

	client := &KubeClient{baseURL: server.URL, httpClient: server.Client()}
	node, err := client.RelatedNode(context.Background(), "apps", "service")
	if err != nil || node != "node-b" {
		t.Fatalf("node=%q err=%v", node, err)
	}
}

func TestFilteredErrorLogsUsesIncidentCorrelationAndAnchor(t *testing.T) {
	anchor := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if got := query.Get("query"); !strings.Contains(got, `|= "run-123"`) || strings.Contains(got, "|~") {
			t.Errorf("unexpected LogQL: %s", got)
		}
		wantEnd := anchor.Add(2 * time.Minute).UnixNano()
		if got, _ := strconv.ParseInt(query.Get("end"), 10, 64); got != wantEnd {
			t.Errorf("end=%d want=%d", got, wantEnd)
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	defer server.Close()

	telemetry := &Telemetry{cfg: Config{LokiURL: server.URL}, httpClient: server.Client()}
	if _, err := telemetry.filteredErrorLogs(context.Background(), "apps", "service", anchor, "run-123", 5*time.Minute, nil, 10); err != nil {
		t.Fatal(err)
	}
}
