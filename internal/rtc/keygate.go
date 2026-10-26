package rtc

import "time"

// keyGate keeps damaged video away from the decoders. After lost packets
// (or at the start) the frames that follow reference pictures we don't
// have, and decoding them smears blocks over the picture until the next
// keyframe, up to a GOP (2 s) later. So we ask the
// provider for a keyframe right away (PLI, at most every keyAsk) and
// hold the last good picture until it arrives. An encoder that never sends
// an IDR must not freeze us: after keyWait we pass frames again.
type keyGate struct {
	pli     func()
	waiting bool
	since   time.Time // when waiting started
	asked   time.Time
	started bool
}

const (
	keyWait = 3 * time.Second
	// keyAsk spaces our requests: each one costs the sender a keyframe, which
	// every viewer of that encoder pays for in bits.
	keyAsk = time.Second
)

// pass reports whether the access unit au may go to the decoder; dropped is
// how many packets were lost just before it.
func (g *keyGate) pass(au []byte, dropped uint16) bool {
	now := time.Now()
	if !g.started || dropped > 0 {
		g.started = true
		if !g.waiting {
			g.waiting, g.since = true, now
		}
	}
	if !g.waiting {
		return true
	}
	if hasIDR(au) || now.Sub(g.since) > keyWait {
		g.waiting = false
		return true
	}
	if now.Sub(g.asked) >= keyAsk {
		g.asked = now
		g.pli()
	}
	return false
}

// hasIDR reports whether an Annex-B access unit contains an IDR slice.
func hasIDR(au []byte) bool {
	for i := 0; i+3 < len(au); i++ {
		if au[i] == 0 && au[i+1] == 0 && au[i+2] == 1 {
			if au[i+3]&0x1f == 5 {
				return true
			}
			i += 2
		}
	}
	return false
}
