package session

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// Windows go fast at once and slow only after slowAfter below fastBelow.
func TestViewRatesSticky(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var v viewRates
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	n, fast := 0, false
	// step runs d at fps, a window at a time, and checks fast after it and
	// whether it changed on the last window only.
	step := func(fps int, d time.Duration, want bool) {
		t.Helper()
		changed := false
		for end := now.Add(d); now.Before(end); {
			now = now.Add(viewRateWindow)
			n += fps * int(viewRateWindow/time.Second)
			var ch bool
			fast, ch = v.update(log, now, map[string]int{"a": n}, map[string]int{"": 0}, fast)
			if changed && ch {
				t.Fatalf("%d fps: switched twice", fps)
			}
			changed = changed || ch
		}
		if fast != want {
			t.Fatalf("%d fps for %v: fast=%v, want %v", fps, d, fast, want)
		}
	}
	v.update(log, now, map[string]int{"a": 0}, nil, false)
	step(60, viewRateWindow, true)          // fast at once
	step(40, time.Minute, true)             // in the gap: stays fast
	step(30, slowAfter-4*time.Second, true) // below: not yet
	step(60, viewRateWindow, true)          // back above: the wait starts over
	step(30, slowAfter-4*time.Second, true)
	step(40, viewRateWindow, true) // in the gap also starts it over
	step(30, slowAfter-4*time.Second, true)
	step(0, 6*time.Second, false) // a still share counts as below: slow now
	step(40, time.Minute, false)  // in the gap: stays slow
	step(50, viewRateWindow, true)
	// Within a window nothing is measured.
	if _, ch := v.update(log, now.Add(time.Second), map[string]int{"a": n + 120}, nil, true); ch || !v.at.Equal(now) {
		t.Fatal("updated within the window")
	}
}
