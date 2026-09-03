package agent

import (
	"sync"
	"time"
)

type callBudget struct {
	mu    sync.Mutex
	limit int
	calls []time.Time
}

func newCallBudget(limit int) *callBudget {
	return &callBudget{limit: limit}
}

func (b *callBudget) Allow(now time.Time) bool {
	if b.limit == 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
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
