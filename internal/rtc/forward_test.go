package rtc

import (
	"slices"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func pkt(ts uint32, nal byte) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Timestamp: ts}, Payload: []byte{nal, 0xaa}}
}

const (
	nalSPS   = 7
	nalSlice = 1
)

// capture replaces the forwarder's output with a slice (no Pion track needed).
type captured struct {
	seq []uint16
	ts  []uint32
}

func runForward(f *forwarder, c *captured, layer int, pkts ...*rtp.Packet) bool {
	need := f.forwardTo(layer, pkts, func(p *rtp.Packet) {
		c.seq = append(c.seq, p.SequenceNumber)
		c.ts = append(c.ts, p.Timestamp)
	})
	return need
}

func TestForwarderWaitsForKeyframeAndStaysContinuous(t *testing.T) {
	f := newForwarder(nil, layerHigh)
	c := &captured{}

	// HIGH delta frames before any keyframe: nothing sent, keyframe requested.
	if !runForward(f, c, layerHigh, pkt(1000, nalSlice)) || len(c.seq) != 0 {
		t.Fatal("sent a delta frame before the first keyframe")
	}
	// LOW packets are ignored entirely while we want HIGH.
	if runForward(f, c, layerLow, pkt(50000, nalSPS)) || len(c.seq) != 0 {
		t.Fatal("forwarded the unwanted layer")
	}
	runForward(f, c, layerHigh, pkt(4000, nalSPS), pkt(4000, nalSlice))
	runForward(f, c, layerHigh, pkt(7000, nalSlice))
	if len(c.seq) != 3 || c.ts[0] != 4000 || c.ts[2] != 7000 {
		t.Fatalf("first keyframe: %+v", c)
	}

	// Switch to LOW: HIGH keeps flowing until LOW's keyframe arrives.
	f.setWant(layerLow)
	runForward(f, c, layerLow, pkt(90000, nalSlice))  // LOW delta: wait
	runForward(f, c, layerHigh, pkt(10000, nalSlice)) // still HIGH
	runForward(f, c, layerLow, pkt(96000, nalSPS))    // LOW keyframe: switch
	runForward(f, c, layerHigh, pkt(13000, nalSlice)) // HIGH now dropped
	runForward(f, c, layerLow, pkt(102000, nalSlice))

	for i := 1; i < len(c.seq); i++ {
		if c.seq[i] != c.seq[i-1]+1 {
			t.Fatalf("sequence gap at %d: %v", i, c.seq)
		}
		if int32(c.ts[i]-c.ts[i-1]) < 0 {
			t.Fatalf("timestamp went backwards at %d: %v", i, c.ts)
		}
	}
	// After the switch the timeline continues one frame on, then advances by
	// LOW's own deltas (6000 ticks at 15 fps).
	n := len(c.ts)
	if c.ts[n-2] != 10000+frameTicks || c.ts[n-1] != 10000+frameTicks+6000 {
		t.Fatalf("timeline after switch: %v", c.ts)
	}
	if cur, _ := f.layers(); cur != layerLow {
		t.Fatalf("cur = %d", cur)
	}
}

func TestLayerPolicyLoss(t *testing.T) {
	var lp layerPolicy
	now := time.Unix(0, 0)
	bad, good := rr{lost: 0.10}, rr{}
	if lp.observe(levelHigh, bad, now) != levelHigh {
		t.Fatal("one bad report should not switch")
	}
	if lp.observe(levelHigh, bad, now) != levelLow {
		t.Fatal("two bad reports should switch to LOW")
	}
	if lp.observe(levelLow, bad, now.Add(time.Second)) != levelLow ||
		lp.observe(levelLow, bad, now.Add(2*time.Second)) != levelLow {
		t.Fatal("judged LOW before it settled")
	}
	if lp.observe(levelLow, good, now.Add(5*time.Second)) != levelLow {
		t.Fatal("probed HIGH before backoff")
	}
	lp.ewma = 0
	if lp.observe(levelLow, good, now.Add(11*time.Second)) != levelHigh {
		t.Fatal("did not probe HIGH after backoff")
	}
	// Failing again doubles the backoff.
	lp.observe(levelHigh, rr{lost: 0.2}, now.Add(15*time.Second))
	lp.observe(levelHigh, rr{lost: 0.2}, now.Add(16*time.Second))
	if lp.backoff != 2*minBackoff {
		t.Fatalf("backoff = %v", lp.backoff)
	}
	// Receiver cap wins.
	lp2 := layerPolicy{maxLevel: levelLow}
	if lp2.observe(levelHigh, good, now) != levelLow {
		t.Fatal("ignored receiver cap")
	}
}

func TestLayerPolicyDelayLadderAndOff(t *testing.T) {
	var lp layerPolicy
	now := time.Unix(0, 0)
	at := func(s int) time.Time { return now.Add(time.Duration(s) * time.Second) }
	// 40 ms baseline, then a queue builds with no loss at all.
	for i := range 5 {
		if lp.observe(levelHigh, rr{rtt: 40 * time.Millisecond}, at(i)) != levelHigh {
			t.Fatal("stepped down on a clean path")
		}
	}
	bloat := rr{rtt: 600 * time.Millisecond}
	cur, sec := levelHigh, 5
	for _, want := range []int{levelLow, levelLow2, levelLow3, levelOff} {
		lp.observe(cur, bloat, at(sec))
		if got := lp.observe(cur, bloat, at(sec+1)); got != want {
			t.Fatalf("level %d under queueing delay -> %d, want %d", cur, got, want)
		}
		cur, sec = want, sec+4 // past the settle time
	}
	if lp.observe(levelOff, rr{}, at(sec)) != levelOff {
		t.Fatal("left OFF before backoff")
	}
	sec += int(lp.backoff/time.Second) + 1
	if lp.observe(levelOff, rr{}, at(sec)) != levelLow3 {
		t.Fatal("did not probe the cheapest video level after backoff")
	}
	lp.ewma = 0
	if lp.observe(levelLow3, rr{rtt: 40 * time.Millisecond}, at(sec+5)) != levelLow3 {
		t.Fatal("climbed again right after coming back from OFF")
	}
	if levelLayer(levelLow2, -1) != layerLow || levelLayer(levelOff, -1) != layerOff || levelLayer(levelHigh, -1) != layerHigh ||
		levelLayer(levelMid, layerR720) != layerR720 || levelLayer(levelMid, -1) != layerLow {
		t.Fatal("levelLayer")
	}
}

// levelMid exists only for a peer granted a rendition; others skip it both
// ways, and a cap at levelMid holds the peer there.
func TestLayerPolicyMid(t *testing.T) {
	now := time.Unix(0, 0)
	bad := rr{lost: 0.10}
	var lp layerPolicy
	lp.observe(levelHigh, bad, now)
	if got := lp.observe(levelHigh, bad, now); got != levelLow {
		t.Fatalf("no rendition: HIGH -> %d, want LOW", got)
	}
	lp.ewma = 0
	if got := lp.observe(levelLow, rr{}, now.Add(11*time.Second)); got != levelHigh {
		t.Fatalf("no rendition: LOW -> %d, want HIGH", got)
	}

	mid := layerPolicy{hasMid: true}
	mid.observe(levelHigh, bad, now)
	if got := mid.observe(levelHigh, bad, now); got != levelMid {
		t.Fatalf("with rendition: HIGH -> %d, want MID", got)
	}
	mid.ewma = 0
	if got := mid.observe(levelMid, rr{}, now.Add(11*time.Second)); got != levelHigh {
		t.Fatalf("with rendition: MID -> %d, want HIGH", got)
	}

	held := layerPolicy{hasMid: true, maxLevel: levelMid}
	if got := held.observe(levelHigh, rr{}, now); got != levelMid {
		t.Fatalf("capped at MID: -> %d", got)
	}
	gone := layerPolicy{maxLevel: levelMid}
	if got := gone.observe(levelMid, rr{}, now); got != levelLow {
		t.Fatalf("capped at a rendition that is gone: -> %d, want LOW", got)
	}
}

func TestRendFor(t *testing.T) {
	for _, c := range []struct{ top, max, want int }{
		{2160, 2159, 1080}, {2160, 1080, 1080}, {2160, 1079, 720}, {2160, 700, 0},
		{1080, 1079, 720}, {1080, 2000, 720}, {720, 719, 0},
	} {
		if got := rendFor(c.top, c.max); got != c.want {
			t.Errorf("rendFor(%d, %d) = %d, want %d", c.top, c.max, got, c.want)
		}
	}
	if w, ok := wireLevel(wireOff); !ok || w != levelOff {
		t.Error("wire off")
	}
	if _, ok := wireLevel(3); ok {
		t.Error("wire 3 accepted")
	}
}

func TestRRRTT(t *testing.T) {
	now := time.Unix(1_800_000_000, 250_000_000)
	sent := now.Add(-300 * time.Millisecond)
	rep := rtcp.ReceptionReport{LastSenderReport: ntpMiddle(sent), Delay: 100 << 16 / 1000} // held 100 ms
	if got := rrRTT(rep, now); got < 199*time.Millisecond || got > 201*time.Millisecond {
		t.Fatalf("rtt = %v, want 200ms", got)
	}
	if rrRTT(rtcp.ReceptionReport{}, now) != 0 {
		t.Fatal("rtt without a sender report")
	}
}

func TestForwarderOff(t *testing.T) {
	f := newForwarder(nil, layerHigh)
	var n int
	write := func(*rtp.Packet) { n++ }
	f.forwardTo(layerHigh, []*rtp.Packet{pkt(0, nalSPS)}, write)
	f.setWant(layerOff)
	f.forwardTo(layerHigh, []*rtp.Packet{pkt(0, nalSPS)}, write)
	if cur, _ := f.layers(); cur != layerOff || n != 1 {
		t.Fatalf("cur=%d sent=%d, want off after 1", cur, n)
	}
	if !f.setWant(layerLow) || !f.forwardTo(layerLow, []*rtp.Packet{pkt(0, nalSlice)}, write) {
		t.Fatal("resuming from OFF should wait for a LOW keyframe")
	}
	f.forwardTo(layerLow, []*rtp.Packet{pkt(0, nalSPS)}, write)
	if cur, _ := f.layers(); cur != layerLow || n != 2 {
		t.Fatalf("cur=%d sent=%d after resume", cur, n)
	}
}

func TestRelayFwdKeepsSeqGaps(t *testing.T) {
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "slot0", "s")
	if err != nil {
		t.Fatal(err)
	}
	f := newRelayFwd(track)
	key := func(seq uint16, ts uint32) *rtp.Packet {
		return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: ts}, Payload: []byte{0x65, 0}}
	}
	delta := func(seq uint16, ts uint32) *rtp.Packet {
		return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: ts}, Payload: []byte{0x41, 0}}
	}
	var out []uint16
	f.setWant(srcScreen)
	if !f.forward(srcScreen, delta(100, 0)) {
		t.Fatal("delta before the first keyframe should ask for one")
	}
	for _, p := range []*rtp.Packet{key(101, 3000), delta(102, 3000), delta(104, 6000), delta(103, 6000)} {
		f.forward(srcScreen, p)
		out = append(out, f.nextSeq)
	}
	// 101 -> first out seq 0; a gap (103 missing) and its late arrival keep
	// their places: nextSeq tracks the highest.
	if want := []uint16{1, 2, 4, 4}; !slices.Equal(out, want) {
		t.Fatalf("nextSeq after each packet %v, want %v", out, want)
	}
	// Switch to LOW at a keyframe: output continues after the highest seq,
	// and a straggler from before the switch point is dropped.
	f.setWant(srcLow)
	f.forward(srcLow, delta(5000, 90000))
	f.forward(srcLow, key(5001, 90000))
	if f.current() != srcLow || f.nextSeq != 5 {
		t.Fatalf("after switch: cur %d nextSeq %d", f.current(), f.nextSeq)
	}
	f.forward(srcLow, delta(4999, 87000))
	if f.nextSeq != 5 {
		t.Fatalf("pre-switch straggler was forwarded (nextSeq %d)", f.nextSeq)
	}
	if f.tsOff+90000 != 6000+frameTicks {
		t.Fatalf("timeline not continued: %d", f.tsOff+90000)
	}
}
