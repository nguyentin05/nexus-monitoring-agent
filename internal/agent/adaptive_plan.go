package agent

import (
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	adaptiveCandidate = "candidate"
	adaptiveShadow    = "shadow"
	adaptiveActive    = "active"
)

type adaptivePlanEntry struct {
	Tools          []string
	State          string
	Observations   int
	ShadowMatches  int
	Services       map[string]struct{}
	LastObservedAt time.Time
}

type adaptivePlanRegistry struct {
	mu              sync.Mutex
	minObservations int
	minServices     int
	shadowMatches   int
	maxEntries      int
	entries         map[string]*adaptivePlanEntry
	path            string
}

func newAdaptivePlanRegistry(cfg Config) *adaptivePlanRegistry {
	registry := &adaptivePlanRegistry{
		minObservations: cfg.AdaptivePlanMinObservations,
		minServices:     cfg.AdaptivePlanMinServices,
		shadowMatches:   cfg.AdaptivePlanShadowMatches,
		maxEntries:      cfg.MaxPatterns,
		entries:         make(map[string]*adaptivePlanEntry),
		path:            cfg.StateFile("adaptive-plans.json"),
	}
	state := struct {
		Entries map[string]*adaptivePlanEntry `json:"entries"`
	}{}
	if err := loadState(registry.path, &state); err != nil {
		slog.Warn("load adaptive plan state", "error", err)
	} else if state.Entries != nil {
		registry.entries = state.Entries
		for _, entry := range registry.entries {
			if entry.Services == nil {
				entry.Services = make(map[string]struct{})
			}
		}
	}
	return registry
}

func (r *adaptivePlanRegistry) Lookup(incident Incident) (CollectionPlan, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.entries[adaptivePlanKey(incident)]
	if entry == nil || entry.State != adaptiveActive {
		return CollectionPlan{}, false
	}
	return CollectionPlan{Tools: slices.Clone(entry.Tools), Reason: "adaptive plan"}, true
}

func (r *adaptivePlanRegistry) Observe(incident Incident, plan CollectionPlan) bool {
	tools := slices.Clone(plan.Tools)
	sort.Strings(tools)
	key := adaptivePlanKey(incident)
	now := time.Now().UTC()

	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.persistLocked()
	entry := r.entries[key]
	if entry == nil || !slices.Equal(entry.Tools, tools) {
		r.entries[key] = &adaptivePlanEntry{Tools: tools, State: adaptiveCandidate, Observations: 1, Services: map[string]struct{}{incident.Service: {}}, LastObservedAt: now}
		r.evictLocked()
		return false
	}
	entry.Observations++
	entry.Services[incident.Service] = struct{}{}
	entry.LastObservedAt = now
	if entry.State == adaptiveCandidate && entry.Observations >= r.minObservations && len(entry.Services) >= r.minServices {
		entry.State = adaptiveShadow
		entry.ShadowMatches = 0
		return false
	}
	if entry.State == adaptiveShadow {
		entry.ShadowMatches++
		if entry.ShadowMatches >= r.shadowMatches {
			entry.State = adaptiveActive
			return true
		}
	}
	return false
}

func (r *adaptivePlanRegistry) Demote(incident Incident) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.entries[adaptivePlanKey(incident)]
	if entry == nil || entry.State != adaptiveActive {
		return false
	}
	entry.State = adaptiveShadow
	entry.ShadowMatches = 0
	r.persistLocked()
	return true
}

func (r *adaptivePlanRegistry) persistLocked() {
	state := struct {
		Entries map[string]*adaptivePlanEntry `json:"entries"`
	}{Entries: r.entries}
	if err := saveState(r.path, state); err != nil {
		slog.Warn("persist adaptive plan state", "error", err)
	}
}

func (r *adaptivePlanRegistry) Counts() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	counts := map[string]int{adaptiveCandidate: 0, adaptiveShadow: 0, adaptiveActive: 0}
	for _, entry := range r.entries {
		counts[entry.State]++
	}
	return counts
}

func (r *adaptivePlanRegistry) evictLocked() {
	for len(r.entries) > r.maxEntries {
		var oldestKey string
		var oldest time.Time
		for key, entry := range r.entries {
			if oldestKey == "" || entry.LastObservedAt.Before(oldest) {
				oldestKey, oldest = key, entry.LastObservedAt
			}
		}
		delete(r.entries, oldestKey)
	}
}

func adaptivePlanKey(incident Incident) string {
	description := strings.ReplaceAll(strings.ToLower(incident.Description), strings.ToLower(incident.Service), "<service>")
	return patternID(incident.Kind+"\x00"+incident.AlertName, normalizeLogPattern(description))
}
