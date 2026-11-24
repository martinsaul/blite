package session

import (
	"log/slog"
	"sync"
	"time"
)

// keyStats logs, per encoder layer, the keyframes viewers asked for, the ones
// we forced and the IDRs the encoder actually produced: at most one line per
// layer every keyStatsEvery while viewers ask, and a warning when a forced
// keyframe brings no IDR within keyLate.
type keyStats struct {
	mu     sync.Mutex
	layers map[int]*layerKeys
}

type layerKeys struct {
	asked, forced, idrs int
	pending             time.Time // forced, no IDR since; zero: none
	logAt, warnAt       time.Time
}

const (
	keyStatsEvery = 10 * time.Second
	keyLate       = 2 * time.Second
)

func (k *keyStats) layer(l int) *layerKeys {
	if k.layers == nil {
		k.layers = map[int]*layerKeys{}
	}
	lk := k.layers[l]
	if lk == nil {
		lk = &layerKeys{}
		k.layers[l] = lk
	}
	return lk
}

// asked counts a viewer's request (PLI/FIR, or a layer switch); forced says
// whether it went to the encoder or the throttle dropped it.
func (k *keyStats) asked(l int, forced bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	lk := k.layer(l)
	lk.asked++
	if forced {
		lk.forced++
		if lk.pending.IsZero() {
			lk.pending = time.Now()
		}
	}
}

// sent notes one encoded access unit of layer l. It runs for every frame
// and asked takes the same lock from the RTCP path, so it logs after
// unlocking.
func (k *keyStats) sent(log *slog.Logger, l int, idr bool) {
	var late time.Duration
	var asked, forced, idrs int
	k.mu.Lock()
	lk := k.layer(l)
	now := time.Now()
	if idr {
		lk.idrs++
		lk.pending = time.Time{}
	} else if !lk.pending.IsZero() && now.Sub(lk.pending) >= keyLate && now.Sub(lk.warnAt) >= keyStatsEvery {
		lk.warnAt = now
		late = now.Sub(lk.pending)
	}
	if now.Sub(lk.logAt) >= keyStatsEvery {
		asked, forced, idrs = lk.asked, lk.forced, lk.idrs
		lk.logAt, lk.asked, lk.forced, lk.idrs = now, 0, 0, 0
	}
	k.mu.Unlock()
	if late > 0 {
		log.Warn("encoder made no IDR after a forced keyframe", "layer", l, "waitedMs", late.Milliseconds())
	}
	if asked > 0 {
		log.Info("keyframes", "layer", l, "asked", asked, "forced", forced, "idrs", idrs)
	}
}
