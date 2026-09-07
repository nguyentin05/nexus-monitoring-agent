package agent

import (
	"slices"
	"testing"
)

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

func TestMergeContractKeepsRequiredQueryForSameScope(t *testing.T) {
	contract := contractForIncident(Incident{Description: "database connection refused"})
	merged := mergeContract(CollectionPlan{Steps: []CollectionStep{{Tool: ToolErrorLogs}}}, contract)
	if len(merged.Steps) != 1 || !slices.Contains(merged.Steps[0].LogTerms, "refused") {
		t.Fatalf("required query was replaced: %+v", merged.Steps)
	}
}

func TestMergeContractRejectsEvidenceUnrelatedToSignalFamily(t *testing.T) {
	contract := contractForIncident(Incident{Description: "CPU usage exceeded threshold"})
	merged := mergeContract(CollectionPlan{Steps: []CollectionStep{{Tool: ToolKubernetesEvents}}}, contract)
	if planHasTool(merged, ToolKubernetesEvents) {
		t.Fatalf("compute plan accepted unrelated events: %+v", merged.Steps)
	}
}
