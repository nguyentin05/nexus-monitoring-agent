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

type fakeCollector struct{ calls int }

func (f *fakeCollector) Collect(_ context.Context, incident Incident, _ CollectionPlan) Evidence {
	f.calls++
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
		{name: "exact pattern still uses planner and analysis", incident: Incident{Description: "ImagePullBackOff returned 404"}, wantPath: "llm_planner", wantPlans: 1, wantAnalyses: 1, wantCollects: 1},
		{name: "known alert uses planner and analysis", incident: Incident{Kind: "error_rate_high"}, wantPath: "llm_planner", wantPlans: 1, wantAnalyses: 1, wantCollects: 1},
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

func TestRepeatedIncidentRunsFullAgenticFlow(t *testing.T) {
	llm := &fakeLLM{}
	collector := &fakeCollector{}
	processor := NewProcessor(Config{Mode: "detect", QueueSize: 1, Cooldown: time.Minute, RCACacheTTL: time.Hour}, collector, llm, fakeNotifier{})
	incident := Incident{Namespace: "apps", Service: "auth-service", Kind: "cpu_high"}

	first := processor.Process(context.Background(), incident)
	second := processor.Process(context.Background(), incident)
	if first.Path != "llm_planner" || second.Path != "llm_planner" {
		t.Fatalf("first=%s second=%s", first.Path, second.Path)
	}
	if llm.plans != 2 || llm.analyses != 2 || collector.calls != 2 {
		t.Fatalf("plans=%d analyses=%d collects=%d", llm.plans, llm.analyses, collector.calls)
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
