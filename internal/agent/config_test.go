package agent

import (
	"os"
	"testing"
	"time"
)

func TestConfigDefaultsOnlyWhenEnvironmentVariableIsUnset(t *testing.T) {
	const name = "NEXUS_TEST_CONFIG"
	if value, exists := os.LookupEnv(name); exists {
		t.Cleanup(func() { _ = os.Setenv(name, value) })
	} else {
		t.Cleanup(func() { _ = os.Unsetenv(name) })
	}
	_ = os.Unsetenv(name)

	if got := env(name, "default"); got != "default" {
		t.Fatalf("unset variable = %q, want default", got)
	}
	t.Setenv(name, "")
	if got := env(name, "default"); got != "" {
		t.Fatalf("empty variable = %q, want empty value", got)
	}
	if _, err := durationEnv(name, time.Minute); err == nil {
		t.Fatal("empty duration variable should fail validation")
	}
	t.Setenv("WATCHED_SERVICES", "auth-service,aiops-benchmark-service")
	t.Setenv("DISCOVERY_SERVICES", "auth-service")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.WatchedServices) != 2 || len(cfg.DiscoveryServices) != 1 || cfg.DiscoveryServices[0] != "auth-service" {
		t.Fatalf("watched=%v discovery=%v", cfg.WatchedServices, cfg.DiscoveryServices)
	}
}
