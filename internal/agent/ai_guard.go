package agent

import (
	"log/slog"
	"sync"
	"time"
)

type callBudget struct {
	mu    sync.Mutex
	limit int
	calls []time.Time
	path  string
}

func newCallBudget(limit int, path string) *callBudget {
	budget := &callBudget{limit: limit, path: path}
	if err := loadState(path, &budget.calls); err != nil {
		slog.Warn("load LLM call budget state", "error", err)
	}
	return budget
}

func (b *callBudget) Allow(now time.Time) bool {
	if b.limit == 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	defer func() {
		if err := saveState(b.path, b.calls); err != nil {
			slog.Warn("persist LLM call budget state", "error", err)
		}
	}()
	cutoff := now.Add(-time.Hour)
	first := 0
	for first < len(b.calls) && b.calls[first].Before(cutoff) {
		first++
	}
	b.calls = append([]time.Time(nil), b.calls[first:]...)
	if len(b.calls) >= b.limit {
		return false
	}
	b.calls = append(b.calls, now)
	return true
}

type rcaCacheEntry struct {
	Result    RCAResult
	Path      string
	ExpiresAt time.Time
}
