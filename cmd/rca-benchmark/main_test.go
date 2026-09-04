package main

import "testing"

func TestBenchmarkScoringHelpers(t *testing.T) {
	if !containsAll([]string{"error_logs", "service_metrics"}, []string{"error_logs"}) {
		t.Fatal("required tool was not recognized")
	}
	if containsAll([]string{"error_logs"}, []string{"kubernetes_events"}) {
		t.Fatal("missing required tool was accepted")
	}
	concepts := [][]string{{"dns", "name resolution"}, {"failed", "nxdomain"}}
	if !matchesConcepts("Cluster DNS returned NXDOMAIN", concepts) {
		t.Fatal("alternative RCA concepts were not matched")
	}
	if matchesConcepts("The upstream timed out", concepts) {
		t.Fatal("unrelated RCA was accepted")
	}
}
