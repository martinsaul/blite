package ratelimit

import (
	"testing"
	"time"
)

func TestWindowAndSweep(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	w := New(3, time.Minute)
	w.now = func() time.Time { return now }

	for i := range 3 {
		if w.Blocked("1.2.3.4") {
			t.Fatalf("blocked on attempt %d", i)
		}
		w.Add("1.2.3.4")
	}
	if !w.Blocked("1.2.3.4") {
		t.Fatal("4th attempt not blocked")
	}
	if w.Blocked("5.6.7.8") {
		t.Fatal("other key blocked")
	}
	w.Add("5.6.7.8")

	now = now.Add(30 * time.Second)
	w.Add("5.6.7.8")
	now = now.Add(31 * time.Second) // 61s: 1.2.3.4 expired, 5.6.7.8 still has one recent event
	w.Sweep()

	w.mu.Lock()
	_, deadPresent := w.events["1.2.3.4"]
	aliveCount := len(w.events["5.6.7.8"])
	w.mu.Unlock()
	if deadPresent || aliveCount != 1 {
		t.Fatalf("after sweep: dead=%v alive=%d", deadPresent, aliveCount)
	}
	if w.Blocked("1.2.3.4") {
		t.Fatal("still blocked after window elapsed")
	}
}
