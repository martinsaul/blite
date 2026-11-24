package session

import (
	"log/slog"
	"time"

	"blite/internal/media"
)

// viewRates measures, from counters sampled every viewRateWindow, the
// frames/s we decode per watched stream and draw per video window. Video
// windows run at media.GridFastFPS as soon as a stream decodes at fastAbove
// or more, and go back to media.GridFPS only once every stream stayed below
// fastBelow for slowAfter (so a stream near the line, or a still window
// share that sends no frames for a while, doesn't switch them back and
// forth).
type viewRates struct {
	at       time.Time
	slow     time.Time      // since when every stream is below fastBelow (zero: one isn't)
	decoded  map[string]int // counters at `at`
	shown    map[string]int
	decFPS   map[string]int // the last window's rates
	shownFPS map[string]int
	logAt    time.Time
}

const (
	viewRateWindow = 2 * time.Second
	viewLogEvery   = 30 * time.Second
	fastAbove      = 45
	fastBelow      = 38
	slowAfter      = 30 * time.Second
)

// update takes the counters at now; it returns whether windows should run
// fast and whether that differs from fast.
func (v *viewRates) update(log *slog.Logger, now time.Time, decoded, shown map[string]int, fast bool) (bool, bool) {
	if v.at.IsZero() {
		v.at, v.decoded, v.shown = now, decoded, shown
		return fast, false
	}
	dt := now.Sub(v.at)
	if dt < viewRateWindow {
		return fast, false
	}
	rate := func(now, then map[string]int) map[string]int {
		out := map[string]int{}
		for k, n := range now {
			if p, ok := then[k]; ok && n >= p && p >= 0 {
				out[k] = int(float64(n-p)/dt.Seconds() + 0.5)
			}
		}
		return out
	}
	v.decFPS, v.shownFPS = rate(decoded, v.decoded), rate(shown, v.shown)
	v.at, v.decoded, v.shown = now, decoded, shown
	top := 0
	for _, f := range v.decFPS {
		top = max(top, f)
	}
	if len(v.decFPS) > 0 && now.Sub(v.logAt) >= viewLogEvery {
		v.logAt = now
		log.Info("view stats", "decodedFPS", v.decFPS, "windowFPS", v.shownFPS, "windowRate", windowRate(fast))
	}
	if top >= fastBelow {
		v.slow = time.Time{}
	} else if v.slow.IsZero() {
		v.slow = now
	}
	want := fast
	switch {
	case top >= fastAbove:
		want = true
	case !v.slow.IsZero() && now.Sub(v.slow) >= slowAfter:
		want = false
	}
	return want, want != fast
}

// windowRate is the frame rate video windows run at.
func windowRate(fast bool) int {
	if fast {
		return media.GridFastFPS
	}
	return media.GridFPS
}
