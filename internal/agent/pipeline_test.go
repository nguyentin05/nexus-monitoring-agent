package agent

import (
	"context"
	"testing"
	"time"
)

type fakeLLM struct {
	plans    int
	analyses int
}

func (f *fakeLLM) Plan(context.Context, Incident) (CollectionPlan, TokenUsage, error) {
	f.plans++
	return CollectionPlan{Tools: []string{ToolErrorLogs}}, TokenUsage{Input: 10}, nil
}

func (f *fakeLLM) Analyze(context.Context, Incident) (RCAResult, TokenUsage, error) {
	f.analyses++
	return RCAResult{RootCause: "test", Confidence: "high", SuggestedActions: []string{"inspect"}}, TokenUsage{Input: 20, Output: 5}, nil
}

type fakeCollector struct {
	calls int
	plan  CollectionPlan
}

func (f *fakeCollector) Collect(_ context.Context, incident Incident, plan CollectionPlan) Evidence {
	f.calls++
	f.plan = plan
	return incident.Evidence
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

	if len(collector.plan.Tools) != 2 || collector.plan.Tools[1] != ToolNetworkPolicies {
		t.Fatalf("unexpected plan: %+v", collector.plan.Tools)
	}
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
	incident := Incident{Namespace: "apps", Service: "auth-service", Kind: "cpu_high"}

	first := processor.Process(context.Background(), incident)
	second := processor.Process(context.Background(), incident)
	if first.Path != "known_plan" || second.Path != "known_plan" {
		t.Fatalf("first=%s second=%s", first.Path, second.Path)
	}
	if llm.plans != 0 || llm.analyses != 1 || collector.calls != 1 {
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
