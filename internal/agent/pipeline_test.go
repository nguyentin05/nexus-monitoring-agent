package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeLLM struct {
	plans      int
	analyses   int
	result     *RCAResult
	analyzeErr error
}

func (f *fakeLLM) Plan(context.Context, Incident) (CollectionPlan, TokenUsage, error) {
	f.plans++
	return planFor("test", ToolErrorLogs), TokenUsage{Input: 10}, nil
}

func (f *fakeLLM) Analyze(context.Context, Incident) (RCAResult, TokenUsage, error) {
	f.analyses++
	if f.analyzeErr != nil {
		return RCAResult{}, TokenUsage{}, f.analyzeErr
	}
	if f.result != nil {
		return *f.result, TokenUsage{Input: 20, Output: 5}, nil
	}
	return RCAResult{RootCause: "CPU test evidence", Confidence: "high", Evidence: []string{"test evidence"}, SuggestedActions: []string{"inspect"}}, TokenUsage{Input: 20, Output: 5}, nil
}

type fakeCollector struct {
	calls int
	plan  CollectionPlan
}

func (f *fakeCollector) Collect(_ context.Context, incident Incident, plan CollectionPlan) Evidence {
	f.calls++
	f.plan = plan
	evidence := incident.Evidence
	for _, step := range plan.Steps {
		switch step.Tool {
		case ToolServiceMetrics:
			value := 1.0
			evidence.Metrics = &MetricSnapshot{CPUPercent: &value, MemoryMB: &value, ErrorRatePercent: &value, P99LatencyMS: &value, RestartCount: &value}
		case ToolErrorLogs:
			namespace, container := incident.Namespace, incident.Service
			if step.Target == TargetRelatedOperator {
				namespace, container = "external-secrets", "external-secrets"
			}
			if !hasLogScope(evidence.Logs, namespace, container) {
				evidence.Logs = append(evidence.Logs, LogSample{Namespace: namespace, Container: container, Message: "test evidence dependency unavailable"})
			}
		case ToolWorkloadStatus:
			evidence.Workload = &WorkloadStatus{DesiredReplicas: 1, AvailableReplicas: 1, ReadyPods: 1, TotalPods: 1}
		case ToolKubernetesEvents:
			evidence.Events = []KubernetesEvent{{Reason: "Test", Message: "test evidence"}}
		case ToolNetworkPolicies:
			evidence.NetworkPolicies = []NetworkPolicyEvidence{{Name: "test-policy"}}
		case ToolTraceContext:
			evidence.Traces = []TraceEvidence{{TraceID: "test"}}
		case ToolRecentChanges:
			evidence.RecentChanges = []DeploymentChange{{ReplicaSet: "test"}}
		case ToolNodeStatus:
			evidence.NodeStatus = &NodeStatusEvidence{Name: "test-node"}
		}
	}
	return evidence
}

type fakeNotifier struct{}

func (fakeNotifier) Send(context.Context, Outcome) error { return nil }

func TestProcessingPathsLimitLLMCalls(t *testing.T) {
	tests := []struct {
		name         string
		incident     Incident
		wantPath     string
		wantPlans    int
		wantAnalyses int
		wantCollects int
	}{
		{name: "exact pattern skips LLM", incident: Incident{Description: "ImagePullBackOff returned 404"}, wantPath: "exact_pattern", wantPlans: 0, wantAnalyses: 0, wantCollects: 0},
		{name: "known alert skips planner", incident: Incident{Kind: "error_rate_high"}, wantPath: "known_plan", wantPlans: 0, wantAnalyses: 1, wantCollects: 1},
		{name: "unknown alert uses planner and analysis", incident: Incident{Kind: "unknown_signal"}, wantPath: "llm_planner", wantPlans: 1, wantAnalyses: 1, wantCollects: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			llm := &fakeLLM{}
			collector := &fakeCollector{}
			processor := NewProcessor(Config{QueueSize: 1, Cooldown: time.Minute}, collector, llm, fakeNotifier{})

			outcome := processor.Process(context.Background(), test.incident)
			if outcome.Path != test.wantPath || llm.plans != test.wantPlans || llm.analyses != test.wantAnalyses || collector.calls != test.wantCollects {
				t.Fatalf("path=%s plans=%d analyses=%d collects=%d", outcome.Path, llm.plans, llm.analyses, collector.calls)
			}
		})
	}
}

func TestPolicyChangeAlwaysCollectsNetworkPolicies(t *testing.T) {
	collector := &fakeCollector{}
	processor := NewProcessor(Config{QueueSize: 1}, collector, &fakeLLM{}, fakeNotifier{})
	processor.Process(context.Background(), Incident{Kind: "unknown_signal", Description: "dependency unreachable after a policy change"})

	if !planHasTool(collector.plan, ToolNetworkPolicies) {
		t.Fatalf("unexpected plan: %+v", collector.plan.Steps)
	}
}

func TestEvidenceContractCannotBeOmittedByPlanner(t *testing.T) {
	collector := &fakeCollector{}
	processor := NewProcessor(Config{QueueSize: 1}, collector, &fakeLLM{}, fakeNotifier{})
	outcome := processor.Process(context.Background(), Incident{Kind: "unknown_signal", Service: "auth-service", Description: "container restarted after rapid memory growth"})

	if !planHasTarget(collector.plan, ToolWorkloadStatus, TargetIncidentPods) || !planHasTarget(collector.plan, ToolKubernetesEvents, TargetIncidentPods) {
		t.Fatalf("mandatory pod evidence missing: %+v", collector.plan.Steps)
	}
	if !outcome.EvidenceComplete || !outcome.Grounded {
		t.Fatalf("unexpected evidence result: %+v", outcome)
	}
}

func TestUngroundedRCAIsNotCached(t *testing.T) {
	llm := &fakeLLM{result: &RCAResult{RootCause: "no evidence available", Confidence: "high", Evidence: []string{"unrelated claim"}}}
	processor := NewProcessor(Config{QueueSize: 1, RCACacheTTL: time.Hour}, &fakeCollector{}, llm, fakeNotifier{})
	incident := Incident{Kind: "unknown_signal", Service: "auth-service", StartedAt: time.Now()}

	first := processor.Process(context.Background(), incident)
	second := processor.Process(context.Background(), incident)
	if first.Grounded || first.RCA.Confidence != "low" || second.Grounded || llm.analyses != 2 {
		t.Fatalf("first=%+v second=%+v analyses=%d", first, second, llm.analyses)
	}
}

func planHasTarget(plan CollectionPlan, tool, target string) bool {
	for _, step := range plan.Steps {
		if step.Tool == tool && step.Target == target {
			return true
		}
	}
	return false
}

func TestAdaptivePlanPromotesAfterCrossServiceShadowValidation(t *testing.T) {
	llm := &fakeLLM{}
	processor := NewProcessor(Config{QueueSize: 1, AdaptivePlanMinObservations: 2, AdaptivePlanMinServices: 2, AdaptivePlanShadowMatches: 1}, &fakeCollector{}, llm, fakeNotifier{})
	auth := Incident{AlertName: "DependencyFailure", Kind: "dependency_failure", Service: "auth-service", Description: "auth-service dependency timed out"}
	profile := Incident{AlertName: auth.AlertName, Kind: auth.Kind, Service: "profile-service", Description: "profile-service dependency timed out"}

	processor.Process(context.Background(), auth)
	processor.Process(context.Background(), profile)
	processor.Process(context.Background(), auth)
	outcome := processor.Process(context.Background(), profile)

	if outcome.Path != "adaptive_plan" || llm.plans != 3 || llm.analyses != 4 {
		t.Fatalf("path=%s plans=%d analyses=%d", outcome.Path, llm.plans, llm.analyses)
	}
	if processor.Stats.AdaptivePromoted.Load() != 1 || processor.Stats.PlannerSaved.Load() != 1 {
		t.Fatalf("promoted=%d saved=%d", processor.Stats.AdaptivePromoted.Load(), processor.Stats.PlannerSaved.Load())
	}
}

func activateAdaptivePlan(processor *Processor, incident Incident) {
	processor.adaptive.entries[adaptivePlanKey(incident)] = &adaptivePlanEntry{
		Plan:            planFor("test", ToolErrorLogs),
		PlanFingerprint: collectionPlanFingerprint(planFor("test", ToolErrorLogs)),
		State:           adaptiveActive,
		Services:        map[string]struct{}{incident.Service: {}},
	}
}

func TestAdaptivePlanIgnoresInconclusiveFailures(t *testing.T) {
	low := RCAResult{RootCause: "insufficient evidence", Confidence: "low"}
	tests := []struct {
		name          string
		llm           *fakeLLM
		evidence      Evidence
		exhaustBudget bool
	}{
		{name: "LLM error", llm: &fakeLLM{analyzeErr: errors.New("bedrock unavailable")}},
		{name: "collector error", llm: &fakeLLM{result: &low}, evidence: Evidence{CollectionErrs: []string{"loki unavailable"}}},
		{name: "budget exhausted", llm: &fakeLLM{}, exhaustBudget: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			maxCalls := 0
			if test.exhaustBudget {
				maxCalls = 1
			}
			processor := NewProcessor(Config{QueueSize: 1, MaxBedrockCalls: maxCalls}, &fakeCollector{}, test.llm, fakeNotifier{})
			incident := Incident{AlertName: "DependencyFailure", Kind: "dependency_failure", Service: "auth-service", Description: "dependency timed out", Evidence: test.evidence}
			activateAdaptivePlan(processor, incident)
			if test.exhaustBudget {
				processor.budget.Allow(time.Now())
			}

			processor.Process(context.Background(), incident)

			entry := processor.adaptive.entries[adaptivePlanKey(incident)]
			if entry.State != adaptiveActive || entry.ConsecutiveFailures != 0 || processor.Stats.AdaptiveDemoted.Load() != 0 {
				t.Fatalf("state=%s failures=%d demoted=%d", entry.State, entry.ConsecutiveFailures, processor.Stats.AdaptiveDemoted.Load())
			}
		})
	}
}

func TestAdaptivePlanDemotesOnlyAfterConsecutiveConclusiveFailures(t *testing.T) {
	low := RCAResult{RootCause: "insufficient evidence", Confidence: "low"}
	high := RCAResult{RootCause: "dependency unavailable", Confidence: "high", Evidence: []string{"dependency unavailable"}}
	llm := &fakeLLM{result: &low}
	processor := NewProcessor(Config{QueueSize: 1}, &fakeCollector{}, llm, fakeNotifier{})
	incident := Incident{AlertName: "DependencyFailure", Kind: "dependency_failure", Service: "auth-service", Description: "dependency timed out"}
	activateAdaptivePlan(processor, incident)

	processor.Process(context.Background(), incident)
	llm.result = &high
	processor.Process(context.Background(), incident)
	llm.result = &low
	processor.Process(context.Background(), incident)

	entry := processor.adaptive.entries[adaptivePlanKey(incident)]
	if entry.State != adaptiveActive || entry.ConsecutiveFailures != 1 {
		t.Fatalf("successful RCA did not reset failures: state=%s failures=%d", entry.State, entry.ConsecutiveFailures)
	}
	processor.Process(context.Background(), incident)
	if entry.State != adaptiveShadow || entry.ConsecutiveFailures != 0 || processor.Stats.AdaptiveDemoted.Load() != 1 {
		t.Fatalf("state=%s failures=%d demoted=%d", entry.State, entry.ConsecutiveFailures, processor.Stats.AdaptiveDemoted.Load())
	}
}

func TestSubmitDeduplicatesDuringCooldown(t *testing.T) {
	processor := NewProcessor(Config{QueueSize: 1, Cooldown: time.Minute}, &fakeCollector{}, &fakeLLM{}, fakeNotifier{})
	incident := Incident{Namespace: "apps", Service: "auth-service", Kind: "cpu_high"}

	if !processor.Submit(incident) || !processor.Submit(incident) {
		t.Fatal("submit unexpectedly failed")
	}
	if processor.Stats.Received.Load() != 1 || processor.Stats.Deduplicated.Load() != 1 {
		t.Fatalf("received=%d deduplicated=%d", processor.Stats.Received.Load(), processor.Stats.Deduplicated.Load())
	}
}

func TestRepeatedIncidentUsesRCACache(t *testing.T) {
	llm := &fakeLLM{}
	collector := &fakeCollector{}
	processor := NewProcessor(Config{Mode: "detect", QueueSize: 1, Cooldown: time.Minute, RCACacheTTL: time.Hour}, collector, llm, fakeNotifier{})
	incident := Incident{Namespace: "apps", Service: "auth-service", Kind: "cpu_high", StartedAt: time.Now()}

	first := processor.Process(context.Background(), incident)
	second := processor.Process(context.Background(), incident)
	incident.StartedAt = incident.StartedAt.Add(time.Minute)
	third := processor.Process(context.Background(), incident)
	if first.Path != "known_plan" || second.Path != "known_plan" || third.Path != "known_plan" {
		t.Fatalf("first=%s second=%s third=%s", first.Path, second.Path, third.Path)
	}
	if llm.plans != 0 || llm.analyses != 2 || collector.calls != 2 {
		t.Fatalf("plans=%d analyses=%d collects=%d", llm.plans, llm.analyses, collector.calls)
	}
}

func TestCorrelationKeyDeduplicatesSharedDependencyFailure(t *testing.T) {
	processor := NewProcessor(Config{QueueSize: 2, Cooldown: time.Minute}, &fakeCollector{}, &fakeLLM{}, fakeNotifier{})
	auth := Incident{Namespace: "apps", Service: "auth-service", Kind: "novel_log_pattern", CorrelationKey: "dependency:opentelemetry-collector:export-failure"}
	profile := Incident{Namespace: "apps", Service: "profile-service", Kind: "novel_log_pattern", CorrelationKey: auth.CorrelationKey}

	if !processor.Submit(auth) || !processor.Submit(profile) {
		t.Fatal("submit unexpectedly failed")
	}
	if processor.Stats.Received.Load() != 1 || processor.Stats.Deduplicated.Load() != 1 {
		t.Fatalf("received=%d deduplicated=%d", processor.Stats.Received.Load(), processor.Stats.Deduplicated.Load())
	}
}

func TestBedrockBudgetFallsBackDeterministically(t *testing.T) {
	llm := &fakeLLM{}
	processor := NewProcessor(Config{Mode: "detect", QueueSize: 1, Cooldown: time.Minute, MaxBedrockCalls: 1}, &fakeCollector{}, llm, fakeNotifier{})

	outcome := processor.Process(context.Background(), Incident{Kind: "unknown_signal", Description: "unexpected signal"})
	if !outcome.Fallback || outcome.RCA == nil || outcome.RCA.Confidence != "low" {
		t.Fatalf("unexpected fallback: %+v", outcome)
	}
	if llm.plans != 1 || llm.analyses != 0 {
		t.Fatalf("plans=%d analyses=%d", llm.plans, llm.analyses)
	}
}

func TestComputeRCARejectsUnrelatedPodEvent(t *testing.T) {
	if rootCauseMatchesSignal(familyCompute, "readiness probe failed on port 8000") {
		t.Fatal("unrelated pod event accepted as compute root cause")
	}
}
