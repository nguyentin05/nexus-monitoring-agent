package agent

import "testing"

func TestNormalizeCollectionPlanIsStableAndRejectsUnsafeInput(t *testing.T) {
	left, err := normalizeCollectionPlan(CollectionPlan{Steps: []CollectionStep{{Tool: ToolErrorLogs, LogTerms: []string{"timeout", "error"}}, {Tool: ToolServiceMetrics, Metrics: []string{MetricCPU}}}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := normalizeCollectionPlan(CollectionPlan{Steps: []CollectionStep{{Tool: ToolServiceMetrics, Metrics: []string{MetricCPU}}, {Tool: ToolErrorLogs, LogTerms: []string{"error", "timeout"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !collectionPlansEqual(left, right) {
		t.Fatalf("equivalent plans differ: %#v %#v", left, right)
	}
	if _, err := normalizeCollectionPlan(CollectionPlan{Steps: []CollectionStep{{Tool: ToolErrorLogs, LookbackMinutes: 31}}}); err == nil {
		t.Fatal("unsafe lookback accepted")
	}
	if _, err := normalizeCollectionPlan(CollectionPlan{Steps: []CollectionStep{{Tool: ToolTraceContext, Limit: 6}}}); err == nil {
		t.Fatal("unsafe trace limit accepted")
	}
}
