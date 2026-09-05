package agent

import (
	"testing"
	"time"
)

func TestAdaptivePlansPersistAcrossRestart(t *testing.T) {
	cfg := Config{
		StateDir:                    t.TempDir(),
		AdaptivePlanMinObservations: 2,
		AdaptivePlanMinServices:     2,
		AdaptivePlanShadowMatches:   1,
		MaxPatterns:                 10,
	}
	auth := Incident{AlertName: "DependencyFailure", Kind: "dependency_failure", Service: "auth-service", Description: "auth-service dependency timed out"}
	profile := Incident{AlertName: auth.AlertName, Kind: auth.Kind, Service: "profile-service", Description: "profile-service dependency timed out"}
	plan := CollectionPlan{Tools: []string{ToolErrorLogs}}

	registry := newAdaptivePlanRegistry(cfg)
	registry.Observe(auth, plan)
	registry.Observe(profile, plan)
	if !registry.Observe(auth, plan) {
		t.Fatal("adaptive plan was not promoted")
	}

	reloaded := newAdaptivePlanRegistry(cfg)
	if _, ok := reloaded.Lookup(profile); !ok {
		t.Fatal("active adaptive plan was not restored")
	}
}

func TestCallBudgetPersistsAcrossRestart(t *testing.T) {
	path := Config{StateDir: t.TempDir()}.StateFile("call-budget.json")
	now := time.Now()
	if !newCallBudget(1, path).Allow(now) {
		t.Fatal("first call was unexpectedly rejected")
	}
	if newCallBudget(1, path).Allow(now.Add(time.Minute)) {
		t.Fatal("reloaded budget forgot the previous call")
	}
}
