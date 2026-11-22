package rtc

import (
	"log/slog"
	"time"
)

// keyGate keeps damaged video away from the decoders. After lost packets,
// undecryptable frames (or at the start) the frames that follow reference
// pictures we don't have, and decoding them smears blocks over the picture
// until the next keyframe. So we ask the provider for a keyframe right away
// (PLI, again every keyAsk while we wait) and hold the last good picture
// until an IDR arrives. The gate never opens without one: a held picture
// heals with the next IDR, a smeared one may not (the encoders all make an
// IDR per GOP and on request).
type keyGate struct {
	pli  func()
	log  *slog.Logger // nil: no logging
	peer string

	waiting bool
	started bool
	since   time.Time // when waiting started
	asked   time.Time
	asks    int // PLIs sent while waiting

	resumes int       // waits ended by an IDR since logAt
	logAt   time.Time // last "keyframe arrived" line
	warnAt  time.Time // last "still waiting" line
}

const (
	// keyAsk spaces our requests: each one costs the sender a keyframe, which
	// every viewer of that encoder pays for in bits.
	keyAsk = time.Second
	// keyWarn: waiting longer than this is worth a warning (at most every
	// keyLogEvery), as the IDRs every GOP should have ended it.
	keyWarn     = 5 * time.Second
	keyLogEvery = 10 * time.Second
)

// pass reports whether the access unit au may go to the decoder; dropped is
// how many packets were lost just before it.
func (g *keyGate) pass(au []byte, dropped uint16) bool {
	now := time.Now()
	if !g.started || dropped > 0 {
		g.wait(now)
	}
	if !g.waiting {
		return true
	}
	if HasIDR(au) {
		g.waiting = false
		g.resumes++
		if g.log != nil && now.Sub(g.logAt) >= keyLogEvery {
			g.log.Info("video: keyframe arrived", "peer", g.peer, "waitedMs", now.Sub(g.since).Milliseconds(), "plis", g.asks, "waits", g.resumes)
			g.logAt, g.resumes = now, 0
		}
		return true
	}
	if now.Sub(g.asked) >= keyAsk {
		g.asked = now
		g.asks++
		g.pli()
	}
	if g.log != nil && now.Sub(g.since) >= keyWarn && now.Sub(g.warnAt) >= keyLogEvery {
		g.warnAt = now
		g.log.Warn("video: still waiting for a keyframe", "peer", g.peer, "waitedMs", now.Sub(g.since).Milliseconds(), "plis", g.asks)
	}
	return false
}

// lost makes the gate wait for a keyframe, as after lost packets.
func (g *keyGate) lost() { g.wait(time.Now()) }

func (g *keyGate) wait(now time.Time) {
	g.started = true
	if !g.waiting {
		g.waiting, g.since, g.asks = true, now, 0
	}
}

// HasIDR reports whether an Annex-B access unit is an IDR picture. All its
// slices are, or none, so it looks no further than the first slice NAL.
func HasIDR(au []byte) bool {
	for i := 0; i+3 < len(au); i++ {
		if au[i] == 0 && au[i+1] == 0 && au[i+2] == 1 {
			switch au[i+3] & 0x1f {
			case 5:
				return true
			case 1:
				return false
			}
			i += 2
		}
	}
	return false
}
