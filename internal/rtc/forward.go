package rtc

import (
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
)

// Each layer is packetized once (layerPacketizer); every peer has its own output
// track fed by a forwarder that picks one layer and rewrites sequence numbers and
// timestamps so the peer sees one continuous stream across layer switches.
// Switches happen only at the target layer's next keyframe.

// Layer IDs match media's: HIGH, LOW, then the renditions at
// fixed IDs per height.
const (
	mtu        = 1200
	clockRate  = 90000
	layerHigh  = 0
	layerLow   = 1
	layerR1080 = 2
	layerR720  = 3
	numLayers  = 4 // encoded layers
	layerOff   = 4 // forwarder state: no video to this peer (audio continues)
	frameTicks = clockRate / 30
)

// LayerOff is what Mesh.Layers reports for a peer we send no video.
const LayerOff = layerOff

// The wire MaxLayer values (signalMsg.MaxLayer) predate the renditions and
// stay 0 high, 1 low, 2 none for older clients.
const (
	wireHigh = 0
	wireLow  = 1
	wireOff  = 2
)

// wireLevel is the level cap a wire MaxLayer value means (ok false if invalid).
func wireLevel(w int) (int, bool) {
	switch w {
	case wireHigh:
		return levelHigh, true
	case wireLow:
		return levelLow, true
	case wireOff:
		return levelOff, true
	}
	return 0, false
}

// rendHeights are the rendition heights, largest first, and their layers.
var rendHeights = []int{1080, 720}

func rendLayer(h int) int {
	switch h {
	case 1080:
		return layerR1080
	case 720:
		return layerR720
	}
	return -1
}

// layerHeight is a layer's height given the top's (0 = off or unknown).
func layerHeight(layer, top int) int {
	switch layer {
	case layerHigh:
		return top
	case layerLow:
		return 360
	case layerR1080:
		return 1080
	case layerR720:
		return 720
	}
	return 0
}

// rendFor is the best rendition no taller than maxHeight that a top of
// height top offers (0 = none).
func rendFor(top, maxHeight int) int {
	for _, h := range rendHeights {
		if h < top && h <= maxHeight {
			return h
		}
	}
	return 0
}

type layerPacketizer struct {
	mu sync.Mutex
	p  rtp.Packetizer
}

func newLayerPacketizer() *layerPacketizer {
	// SSRC and payload type are rewritten per binding by Pion's TrackLocalStaticRTP.
	return &layerPacketizer{p: rtp.NewPacketizer(mtu, 0, 0, &codecs.H264Payloader{}, rtp.NewRandomSequencer(), clockRate)}
}

func (lp *layerPacketizer) packetize(au []byte, d time.Duration) []*rtp.Packet {
	samples := uint32(d.Seconds() * clockRate)
	lp.mu.Lock()
	defer lp.mu.Unlock()
	return lp.p.Packetize(au, samples)
}

// isKeyframeStart reports whether an access unit's first packet starts with SPS
// or an IDR slice (h264parse config-interval=-1 puts SPS/PPS before every IDR).
func isKeyframeStart(pkts []*rtp.Packet) bool {
	if len(pkts) == 0 || len(pkts[0].Payload) == 0 {
		return false
	}
	pl := pkts[0].Payload
	switch t := pl[0] & 0x1f; t {
	case 5, 7:
		return true
	case 24: // STAP-A: first aggregated NAL after the 2-byte size
		return len(pl) > 3 && (pl[3]&0x1f == 7 || pl[3]&0x1f == 5)
	case 28: // FU-A start fragment
		return len(pl) > 1 && pl[1]&0x80 != 0 && (pl[1]&0x1f == 5 || pl[1]&0x1f == 7)
	case 9: // access unit delimiter alone; keyframe-ness is in the next packet
		if len(pkts) > 1 {
			return isKeyframeStart(pkts[1:])
		}
	}
	return false
}

// forwarder feeds one peer's track from one layer at a time.
type forwarder struct {
	track *webrtc.TrackLocalStaticRTP

	mu        sync.Mutex
	cur       int // layer being sent; -1 until the first keyframe
	want      int // layer we want; switch happens at its next keyframe
	seq       uint16
	tsOffset  uint32
	lastOutTS uint32
	started   bool
	needSync  bool
}

func newForwarder(track *webrtc.TrackLocalStaticRTP, startLayer int) *forwarder {
	return &forwarder{track: track, cur: -1, want: startLayer}
}

// setWant requests a layer; it reports whether a keyframe on that layer is needed.
func (f *forwarder) setWant(layer int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.want == layer && f.cur == layer {
		return false
	}
	f.want = layer
	return f.cur != layer
}

func (f *forwarder) layers() (cur, want int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cur, f.want
}

// forward sends one access unit's packets if they belong to the layer this peer
// should see now. It returns true if the peer is waiting for a keyframe on
// `layer` (caller may request one).
func (f *forwarder) forward(layer int, pkts []*rtp.Packet) (needKey bool) {
	var werr error
	return f.forwardTo(layer, pkts, func(p *rtp.Packet) {
		if werr == nil {
			werr = f.track.WriteRTP(p)
		}
	})
}

func (f *forwarder) forwardTo(layer int, pkts []*rtp.Packet, write func(*rtp.Packet)) (needKey bool) {
	f.mu.Lock()
	if f.want == layerOff {
		f.cur = layerOff
		f.mu.Unlock()
		return false
	}
	if layer != f.cur {
		if layer != f.want {
			f.mu.Unlock()
			return false
		}
		if !isKeyframeStart(pkts) {
			f.mu.Unlock()
			return true
		}
		f.cur = layer // switch at this keyframe
		f.needSync = true
	}
	if f.needSync {
		// Continue the timeline one frame after the last packet we sent.
		next := f.lastOutTS + frameTicks
		if !f.started {
			next = pkts[0].Timestamp
		}
		f.tsOffset = next - pkts[0].Timestamp
		f.needSync = false
		f.started = true
	}
	out := make([]*rtp.Packet, len(pkts))
	for i, p := range pkts {
		h := p.Header.Clone()
		h.SequenceNumber = f.seq
		f.seq++
		h.Timestamp = p.Timestamp + f.tsOffset
		f.lastOutTS = h.Timestamp
		out[i] = &rtp.Packet{Header: h, Payload: p.Payload}
	}
	f.mu.Unlock()
	for _, p := range out {
		write(p)
	}
	return false
}

// Send levels, best first. A peer's level picks its forwarder's layer; levels
// below LOW keep sending LOW but ask for a lower LOW bitrate, which the shared
// LOW encoder runs at for the worst-off LOW peer. In a mesh the uplink carries
// one copy per viewer, so stepping LOW's bitrate down is what lets a 1 Mbit
// uplink feed two viewers instead of turning both off.
//
// levelMid is the peer's granted rendition; peers without one
// skip it.
const (
	levelHigh = iota // HIGH layer
	levelMid         // the peer's rendition, if it was granted one
	levelLow         // LOW at lowKbps[levelLow]
	levelLow2
	levelLow3
	levelOff // no video (audio continues)
)

var lowKbps = [...]int{levelLow: 500, levelLow2: 300, levelLow3: 150}

// levelLayer is the forwarder layer for a level; mid is the peer's
// rendition layer (-1 none, then levelMid means LOW).
func levelLayer(l, mid int) int {
	switch {
	case l == levelHigh:
		return layerHigh
	case l == levelMid && mid >= 0:
		return mid
	case l >= levelOff:
		return layerOff
	}
	return layerLow
}

// layerPolicy turns receiver reports into level decisions: one step down when
// the path to a peer is congested, back up one step at a time after an
// exponential backoff. Congestion is loss, or queueing delay (RTT above the
// path's baseline), because shaped links and bloated buffers often delay
// packets for seconds before they drop any.
type layerPolicy struct {
	ewma      float64
	badCount  int
	goodCount int
	backoff   time.Duration
	nextProbe time.Time
	settle    time.Time // ignore reports until the last switch has had time to show
	maxLevel  int       // receiver's cap (their viewer settings), levelHigh = no cap
	hasMid    bool      // levelMid exists for this peer (a granted rendition)

	// Baseline RTT: the minimum over the current and previous window, so it
	// follows route changes within rttWindow..2*rttWindow.
	rttMin, rttPrevMin time.Duration
	rttWindowStart     time.Time
}

// rr is what one receiver report tells us.
type rr struct {
	lost float64       // fraction lost since the previous report
	rtt  time.Duration // 0 if the report had no sender report to refer to
}

const (
	lossDown        = 0.05                   // > 5% loss on two reports -> step down
	lossUp          = 0.02                   // < 2% smoothed loss and probe time reached -> step up
	delayDown       = 250 * time.Millisecond // queueing delay above baseline counting as bad
	delayUp         = 80 * time.Millisecond  // queueing delay low enough to step up
	settleTime      = 3 * time.Second
	minBackoff      = 10 * time.Second
	maxBackoff      = 2 * time.Minute
	rttWindow       = 5 * time.Minute
	ewmaWeight      = 0.5
	reportsToGoDown = 2
)

// queueDelay folds one RTT sample into the baseline and returns how far above
// it the sample is.
func (lp *layerPolicy) queueDelay(rtt time.Duration, now time.Time) time.Duration {
	if rtt <= 0 {
		return 0
	}
	if lp.rttWindowStart.IsZero() || now.Sub(lp.rttWindowStart) > rttWindow {
		lp.rttPrevMin, lp.rttMin, lp.rttWindowStart = lp.rttMin, rtt, now
	}
	lp.rttMin = min(lp.rttMin, rtt)
	base := lp.rttMin
	if lp.rttPrevMin > 0 {
		base = min(base, lp.rttPrevMin)
	}
	return rtt - base
}

// observe records one receiver report at level cur and returns the level to use.
func (lp *layerPolicy) observe(cur int, r rr, now time.Time) int {
	target := cur
	if cur == levelOff {
		// Nothing flows, so reports say nothing new: probe on time alone,
		// with the cheapest video level.
		if now.After(lp.nextProbe) {
			target = levelOff - 1
		}
		return lp.switchTo(cur, target, now)
	}
	lp.ewma = ewmaWeight*r.lost + (1-ewmaWeight)*lp.ewma
	qd := lp.queueDelay(r.rtt, now)
	if now.Before(lp.settle) {
		return lp.switchTo(cur, cur, now)
	}
	// Down on consecutive raw bad reports (fast); up only on the smoothed
	// value (cautious).
	if r.lost > lossDown || qd > delayDown {
		lp.badCount++
		lp.goodCount = 0
	} else {
		lp.badCount = 0
		lp.goodCount++
	}
	if cur == levelHigh && lp.goodCount >= 60 { // a clean minute on HIGH: forget past backoff
		lp.backoff = 0
	}
	switch {
	case lp.badCount >= reportsToGoDown:
		target = cur + 1
		lp.badCount = 0
		if lp.backoff < minBackoff {
			lp.backoff = minBackoff
		} else if lp.backoff < maxBackoff {
			lp.backoff *= 2
		}
		lp.nextProbe = now.Add(lp.backoff)
	case cur > levelHigh && lp.ewma < lossUp && qd < delayUp && now.After(lp.nextProbe):
		target = cur - 1
	}
	return lp.switchTo(cur, target, now)
}

func (lp *layerPolicy) switchTo(cur, target int, now time.Time) int {
	if target == levelMid && !lp.hasMid {
		if target > cur {
			target = levelLow
		} else {
			target = levelHigh
		}
	}
	if target < lp.maxLevel {
		target = lp.maxLevel
	}
	if target == levelMid && !lp.hasMid {
		target = levelLow // capped at a rendition that is gone
	}
	if target != cur {
		lp.settle = now.Add(settleTime)
		lp.badCount, lp.goodCount = 0, 0
		if target < cur {
			// Each step up earns its own wait before the next one (OFF -> LOW
			// must not climb on a deadline that ran out while OFF).
			lp.nextProbe = now.Add(max(lp.backoff, minBackoff))
		}
	}
	return target
}
