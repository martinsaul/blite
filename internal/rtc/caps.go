package rtc

import (
	"net"
	"time"

	"github.com/pion/webrtc/v4"

	"blite/internal/proto"
)

// Caps reporting: what we could do as a relay, for the server's planner
// (proto.Caps). Checked every capsEvery, sent when it changed enough, at
// most every capsMinGap.
const (
	capsEvery  = 5 * time.Second
	capsMinGap = 15 * time.Second
)

// relayCaps is what we would report now.
func (m *Mesh) relayCaps() proto.Caps {
	c := proto.Caps{CPUFree: -1, Reach: m.reach(), Uptime: int(time.Since(m.started).Seconds())}
	if m.opt.UploadKbps > 0 {
		c.UpKbps, c.UpSource = m.opt.UploadKbps, "declared"
	} else if peak := int(m.rend.peakKbps.Load()); peak > 0 {
		c.UpKbps, c.UpSource = peak, "passive"
	}
	if m.opt.CPUFree != nil {
		c.CPUFree = m.opt.CPUFree()
	}
	return c
}

// reach is how peers reach us, from our connected candidate pairs: public
// if one uses a public address of ours directly, nat if one connects
// without TURN, turn if all go through it; "" before any connects.
func (m *Mesh) reach() string {
	best := ""
	for _, r := range m.peerList() {
		if r.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
			continue
		}
		m.statsMu.RLock()
		st := r.pc.GetStats()
		m.statsMu.RUnlock()
		for _, v := range st {
			cp, ok := v.(webrtc.ICECandidatePairStats)
			if !ok || !cp.Nominated {
				continue
			}
			l, _ := st[cp.LocalCandidateID].(webrtc.ICECandidateStats)
			switch ip := net.ParseIP(l.IP); {
			case l.CandidateType == webrtc.ICECandidateTypeRelay:
				if best == "" {
					best = "turn"
				}
			case l.CandidateType == webrtc.ICECandidateTypeHost && ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate():
				return "public"
			default:
				best = "nat"
			}
		}
	}
	return best
}

// capsChanged reports whether c differs enough from what we sent.
func capsChanged(old, c proto.Caps) bool {
	far := func(a, b, pct int) bool {
		d := a - b
		if d < 0 {
			d = -d
		}
		return d*100 > max(a, b)*pct
	}
	return old.Reach != c.Reach || old.UpSource != c.UpSource || far(old.UpKbps, c.UpKbps, 20) ||
		(old.CPUFree < 0) != (c.CPUFree < 0) || far(old.CPUFree, c.CPUFree, 25)
}

func (m *Mesh) capsLoop() {
	t := time.NewTicker(capsEvery)
	defer t.Stop()
	var sent proto.Caps
	var sentAt time.Time
	for {
		select {
		case <-m.done:
			return
		case <-t.C:
		}
		c := m.relayCaps()
		if (sentAt.IsZero() || capsChanged(sent, c)) && time.Since(sentAt) >= capsMinGap {
			if m.sig.Caps(c) == nil {
				sent, sentAt = c, time.Now()
			}
		}
	}
}
