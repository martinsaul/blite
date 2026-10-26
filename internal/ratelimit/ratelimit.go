// Package ratelimit counts failures per key in a sliding window.
package ratelimit

import (
	"sync"
	"time"
)

// Window allows at most Max events per key within Period.
type Window struct {
	Max    int
	Period time.Duration

	mu     sync.Mutex
	events map[string][]time.Time
	now    func() time.Time
}

// New returns a Window allowing max events per key per period.
func New(max int, period time.Duration) *Window {
	return &Window{Max: max, Period: period, events: map[string][]time.Time{}, now: time.Now}
}

// Blocked reports whether key has used up its allowance.
func (w *Window) Blocked(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.pruneLocked(key)) >= w.Max
}

// Add records one event for key.
func (w *Window) Add(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events[key] = append(w.pruneLocked(key), w.now())
}

func (w *Window) pruneLocked(key string) []time.Time {
	cutoff := w.now().Add(-w.Period)
	ev := w.events[key]
	i := 0
	for i < len(ev) && !ev[i].After(cutoff) {
		i++
	}
	ev = ev[i:]
	if len(ev) == 0 {
		delete(w.events, key)
		return nil
	}
	w.events[key] = ev
	return ev
}

// Sweep drops keys with no events inside the window, bounding memory.
func (w *Window) Sweep() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k := range w.events {
		w.pruneLocked(k)
	}
}
