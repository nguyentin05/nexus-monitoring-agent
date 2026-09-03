package agent

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPatternCatalogLearnsNormalizesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "patterns.json")
	catalog, err := NewPatternCatalog(path, 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC()
	training := []LogSample{
		{Timestamp: start, Message: "database timeout request=101"},
		{Timestamp: start.Add(time.Second), Message: "database timeout request=202"},
		{Timestamp: start.Add(2 * time.Second), Message: "database timeout request=303"},
	}
	novel, err := catalog.Observe("auth-service", training, true)
	if err != nil || len(novel) != 0 {
		t.Fatalf("novel=%d err=%v", len(novel), err)
	}
	if len(catalog.Patterns) != 1 {
		t.Fatalf("patterns=%d, want 1", len(catalog.Patterns))
	}
	for _, pattern := range catalog.Patterns {
		if !pattern.Known || pattern.Count != 3 || pattern.Template != "database timeout request=<*>" {
			t.Fatalf("unexpected learned pattern: %+v", pattern)
		}
	}

	reloaded, err := NewPatternCatalog(path, 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	novel, err = reloaded.Observe("auth-service", []LogSample{{Timestamp: start.Add(3 * time.Second), Message: "database timeout request=404"}}, false)
	if err != nil || len(novel) != 0 {
		t.Fatalf("known persisted pattern became novel: novel=%d err=%v", len(novel), err)
	}
}

func TestRedactRemovesCommonSecrets(t *testing.T) {
	input := "password=hunter2 Authorization: Bearer abc.def token email=user@example.com key=AKIA1234567890ABCDEF"
	output := redact(input, 1000)
	for _, secret := range []string{"hunter2", "abc.def", "user@example.com", "AKIA1234567890ABCDEF"} {
		if strings.Contains(output, secret) {
			t.Fatalf("redaction leaked %q in %q", secret, output)
		}
	}
}
