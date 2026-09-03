package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type LogPattern struct {
	ID        string    `json:"id"`
	Service   string    `json:"service"`
	Template  string    `json:"template"`
	Count     int       `json:"count"`
	Known     bool      `json:"known"`
	Alerted   bool      `json:"alerted"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

type PatternCatalog struct {
	mu          sync.Mutex
	path        string
	autoPromote int
	maxPatterns int
	Patterns    map[string]*LogPattern `json:"patterns"`
	Cursors     map[string]time.Time   `json:"cursors"`
}

func NewPatternCatalog(path string, autoPromote, maxPatterns int) (*PatternCatalog, error) {
	catalog := &PatternCatalog{
		path:        path,
		autoPromote: autoPromote,
		maxPatterns: maxPatterns,
		Patterns:    make(map[string]*LogPattern),
		Cursors:     make(map[string]time.Time),
	}
	if path == "" {
		return catalog, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return catalog, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, catalog); err != nil {
		return nil, err
	}
	catalog.path = path
	catalog.autoPromote = autoPromote
	catalog.maxPatterns = maxPatterns
	if catalog.Patterns == nil {
		catalog.Patterns = make(map[string]*LogPattern)
	}
	if catalog.Cursors == nil {
		catalog.Cursors = make(map[string]time.Time)
	}
	return catalog, nil
}

// Observe updates the bounded catalog and returns patterns that are new in
// shadow/detect mode. The per-service cursor prevents overlapping Loki windows
// from relearning the same log records.
func (c *PatternCatalog) Observe(service string, logs []LogSample, training bool) ([]LogPattern, error) {
	ordered := append([]LogSample(nil), logs...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Timestamp.Before(ordered[j].Timestamp) })

	c.mu.Lock()
	defer c.mu.Unlock()

	cursor := c.Cursors[service]
	var novel []LogPattern
	for _, sample := range ordered {
		if !sample.Timestamp.After(cursor) {
			continue
		}
		if sample.Timestamp.After(c.Cursors[service]) {
			c.Cursors[service] = sample.Timestamp
		}
		template := normalizeLogPattern(sample.Message)
		if template == "" {
			continue
		}
		id := patternID(service, template)
		pattern := c.Patterns[id]
		if pattern == nil {
			pattern = &LogPattern{ID: id, Service: service, Template: template, FirstSeen: sample.Timestamp}
			c.Patterns[id] = pattern
		}
		pattern.Count++
		pattern.LastSeen = sample.Timestamp
		if training && pattern.Count >= c.autoPromote {
			pattern.Known = true
		}
		if !training && !pattern.Known && !pattern.Alerted {
			pattern.Alerted = true
			novel = append(novel, *pattern)
		}
	}
	c.evict()
	return novel, c.persistLocked()
}

func (c *PatternCatalog) evict() {
	for len(c.Patterns) > c.maxPatterns {
		var oldestID string
		var oldest time.Time
		for id, pattern := range c.Patterns {
			if oldestID == "" || pattern.LastSeen.Before(oldest) {
				oldestID, oldest = id, pattern.LastSeen
			}
		}
		delete(c.Patterns, oldestID)
	}
}

func (c *PatternCatalog) persistLocked() error {
	if c.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

var patternVariables = []*regexp.Regexp{
	regexp.MustCompile(`(?i)eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`),
	regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`),
	regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}(?::\d+)?\b`),
	regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:Z|[+-]\d{2}:?\d{2})?\b`),
	regexp.MustCompile(`\b(?:0x)?[0-9a-fA-F]{16,}\b`),
	regexp.MustCompile(`\b\d+(?:\.\d+)?\b`),
}

func normalizeLogPattern(message string) string {
	message = redact(message, 2000)
	for _, expression := range patternVariables {
		message = expression.ReplaceAllString(message, "<*>")
	}
	return strings.Join(strings.Fields(message), " ")
}

func patternID(service, template string) string {
	sum := sha256.Sum256([]byte(service + "\x00" + template))
	return hex.EncodeToString(sum[:6])
}
