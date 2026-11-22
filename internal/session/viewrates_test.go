package session

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestViewRatesHysteresis(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var v viewRates
	n := 0
	step := func(fps int, fast bool) (bool, bool) {
		v.at = v.at.Add(-viewRateWindow) // as if a window passed
		n += fps * 2
		return v.update(log, map[string]int{"a": n}, map[string]int{"": 0}, fast)
	}
	v.update(log, map[string]int{"a": 0}, nil, false)
	if f, ch := step(60, false); !f || !ch {
		t.Fatalf("60 fps stream: fast=%v changed=%v", f, ch)
	}
	if f, ch := step(40, true); !f || ch {
		t.Fatalf("40 fps in the gap: fast=%v changed=%v, want to stay fast", f, ch)
	}
	if f, ch := step(30, true); f || !ch {
		t.Fatalf("30 fps stream: fast=%v changed=%v", f, ch)
	}
	if f, ch := step(40, false); f || ch {
		t.Fatalf("40 fps in the gap: fast=%v changed=%v, want to stay slow", f, ch)
	}
	if _, ch := v.update(log, map[string]int{"a": n + 120}, nil, false); ch || time.Since(v.at) > time.Second {
		t.Fatal("updated within the window")
	}
}
