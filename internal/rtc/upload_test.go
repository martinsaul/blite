package rtc

import (
	"io"
	"log/slog"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func newTestMeshForUpload(t *testing.T) *Mesh {
	t.Helper()
	m := &Mesh{
		opt:      Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		peers:    map[string]*remote{},
		joinedAt: map[string]time.Time{},
		done:     make(chan struct{}),
	}
	for i := range m.pkz {
		m.pkz[i] = newLayerPacketizer()
	}
	m.rend.init()
	m.lowKbps.Store(500)
	t.Cleanup(func() { close(m.done) })
	return m
}

func addTestRemote(m *Mesh, id string, joined time.Time) *remote {
	tr, _ := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: clockRate}, "s-"+id, "b")
	lowTr, _ := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: clockRate}, "l-"+id, "b")
	r := &remote{id: id, fwd: newForwarder(tr, layerHigh)}
	r.lowFwd = newForwarder(lowTr, layerOff)
	r.mid, r.uploadMid = -1, -1
	m.mu.Lock()
	m.peers[id] = r
	m.joinedAt[id] = joined
	m.mu.Unlock()
	return r
}

func TestAllocateUploadBasicAndRenditions(t *testing.T) {
	t0 := time.Unix(1000, 0)
	peers := []PeerDemand{
		{ID: "bob", Owner: true, RTT: 20 * time.Millisecond, JoinedAt: t0},
		{ID: "carol", RTT: 40 * time.Millisecond, JoinedAt: t0.Add(time.Second)},
		{ID: "dave", RTT: 90 * time.Millisecond, JoinedAt: t0.Add(2 * time.Second)},
	}
	baseCfg := UploadConfig{
		TopHeight:   1080,
		HighKbps:    5000,
		LowHeight:   360,
		LowKbps:     500,
		SmallerCopy: true,
		CanEncode:   true,
	}

	// 1. Plenty of upload budget (16 Mbit/s >= 3 * 5 Mbit/s): nobody capped.
	cfg := baseCfg
	cfg.BudgetKbps = 16000
	al, rends := AllocateUpload(cfg, peers)
	for _, p := range peers {
		if al[p.ID].CapLevel != levelHigh {
			t.Fatalf("16 Mbit/s: %s got %+v, want levelHigh", p.ID, al[p.ID])
		}
	}
	if len(rends) != 0 {
		t.Fatalf("16 Mbit/s: rends = %v, want none", rends)
	}

	// 2. 13 Mbit/s: Bob & Carol stay HIGH (5000+5000), lowest-priority Dave steps to 720p (2500) -> 12500 <= 13000.
	cfg.BudgetKbps = 13000
	al, rends = AllocateUpload(cfg, peers)
	if al["bob"].CapLevel != levelHigh || al["carol"].CapLevel != levelHigh {
		t.Fatalf("13 Mbit/s: bob=%+v carol=%+v, want both HIGH", al["bob"], al["carol"])
	}
	if al["dave"].CapLevel != levelMid || al["dave"].RendHeight != 720 || !slices.Equal(rends, []int{720}) {
		t.Fatalf("13 Mbit/s: dave=%+v rends=%v, want 720p", al["dave"], rends)
	}

	// 3. 11.5 Mbit/s: Bob & Carol stay HIGH (10000), Dave steps to 540p (1200) -> 11200 <= 11500.
	cfg.BudgetKbps = 11500
	al, rends = AllocateUpload(cfg, peers)
	if al["bob"].CapLevel != levelHigh || al["carol"].CapLevel != levelHigh {
		t.Fatalf("11.5 Mbit/s: bob=%+v carol=%+v, want both HIGH", al["bob"], al["carol"])
	}
	if al["dave"].CapLevel != levelMid || al["dave"].RendHeight != 540 || !slices.Equal(rends, []int{540}) {
		t.Fatalf("11.5 Mbit/s: dave=%+v rends=%v, want 540p", al["dave"], rends)
	}

	// 4. 10 Mbit/s: Bob stays HIGH (5000), Carol & Dave share 720p (2500+2500) -> 10000 <= 10000 with 1 rendition encoder.
	cfg.BudgetKbps = 10000
	al, rends = AllocateUpload(cfg, peers)
	if al["bob"].CapLevel != levelHigh {
		t.Fatalf("10 Mbit/s: bob=%+v, want HIGH", al["bob"])
	}
	if al["carol"].RendHeight != 720 || al["dave"].RendHeight != 720 || !slices.Equal(rends, []int{720}) {
		t.Fatalf("10 Mbit/s: carol=%+v dave=%+v rends=%v, want both 720p", al["carol"], al["dave"], rends)
	}

	// 5. 8.5 Mbit/s: Bob stays HIGH (5000), Carol gets 720p (2500), Dave gets LOW (500) -> 8000 <= 8500.
	cfg.BudgetKbps = 8500
	al, rends = AllocateUpload(cfg, peers)
	if al["bob"].CapLevel != levelHigh || al["carol"].RendHeight != 720 || al["dave"].CapLevel != levelLow {
		t.Fatalf("8.5 Mbit/s: bob=%+v carol=%+v dave=%+v", al["bob"], al["carol"], al["dave"])
	}

	// 6. 2 Mbit/s: everyone drops to LOW (3 * 500 = 1500 <= 2000).
	cfg.BudgetKbps = 2000
	al, rends = AllocateUpload(cfg, peers)
	for _, p := range peers {
		if al[p.ID].CapLevel != levelLow {
			t.Fatalf("2 Mbit/s: %s got %+v, want LOW", p.ID, al[p.ID])
		}
	}
}

func TestAllocateUploadPriorityAndViewerSettings(t *testing.T) {
	t0 := time.Unix(1000, 0)
	peers := []PeerDemand{
		{ID: "unwatched", WireCap: levelOff, RTT: 10 * time.Millisecond, JoinedAt: t0},
		{ID: "lossy", Loss: 0.08, RTT: 25 * time.Millisecond, JoinedAt: t0},
		{ID: "clean1", RTT: 30 * time.Millisecond, JoinedAt: t0.Add(time.Second)},
		{ID: "clean2", RTT: 45 * time.Millisecond, JoinedAt: t0.Add(2 * time.Second)},
	}
	al, _ := AllocateUpload(UploadConfig{
		BudgetKbps:  10600,
		TopHeight:   1080,
		HighKbps:    5000,
		LowHeight:   360,
		LowKbps:     500,
		SmallerCopy: true,
		CanEncode:   false,
	}, peers)

	if al["clean1"].CapLevel != levelHigh || al["clean2"].CapLevel != levelHigh {
		t.Fatalf("clean viewers should keep HIGH: clean1=%+v clean2=%+v", al["clean1"], al["clean2"])
	}
	if al["lossy"].CapLevel != levelLow {
		t.Fatalf("lossy viewer should be demoted to LOW first: %+v", al["lossy"])
	}
	if al["unwatched"].CapLevel != levelHigh {
		t.Fatalf("unwatched viewer should not get uploadCap: %+v", al["unwatched"])
	}
}

func TestAllocateUploadFloorAndSmallerCopyOff(t *testing.T) {
	peers := []PeerDemand{
		{ID: "a", RTT: 20 * time.Millisecond},
		{ID: "b", RTT: 50 * time.Millisecond},
	}
	al, rends := AllocateUpload(UploadConfig{
		BudgetKbps:  4000,
		TopHeight:   1080,
		HighKbps:    5000,
		LowHeight:   0,
		SmallerCopy: false,
		CanEncode:   true,
	}, peers)
	if al["a"].CapLevel != levelHigh || al["b"].CapLevel != levelHigh || len(rends) != 0 {
		t.Fatalf("SmallerCopy=false: a=%+v b=%+v rends=%v", al["a"], al["b"], rends)
	}

	al, rends = AllocateUpload(UploadConfig{
		BudgetKbps:  6500,
		TopHeight:   1080,
		HighKbps:    5000,
		LowHeight:   540,
		LowKbps:     1000,
		SmallerCopy: true,
		CanEncode:   true,
	}, peers)
	if al["a"].CapLevel != levelHigh || al["b"].CapLevel != levelLow || len(rends) != 0 {
		t.Fatalf("540p floor at 6.5 Mbit/s: a=%+v b=%+v rends=%v (5000+1000=6000)", al["a"], al["b"], rends)
	}
}

func TestAutoUploadBudgetAndLimitReason(t *testing.T) {
	m := newTestMeshForUpload(t)
	m.SetShareConfig(1080, 30, 5000, 360, true, false)
	m.rend.peakKbps.Store(8500)

	t0 := time.Unix(2000, 0)
	r1 := addTestRemote(m, "a", t0)
	r2 := addTestRemote(m, "b", t0.Add(time.Second))

	now := time.Unix(2010, 0)
	// Before any congestion, Automatic (uploadKbps=0) lets both peers stay on HIGH.
	m.rebalanceUpload(now)
	if r1.limit() != "" || r2.limit() != "" {
		t.Fatalf("uncongested auto: limits = %q, %q", r1.limit(), r2.limit())
	}

	// Manual upload limit of 6 Mbit/s immediately caps the lower-priority peer ("b") and reports "upload".
	m.SetUploadLimit(6000)
	if r1.limit() != "" || r2.limit() != "upload" {
		t.Fatalf("manual 6 Mbit/s: a.limit=%q b.limit=%q, want \"\" and \"upload\"", r1.limit(), r2.limit())
	}

	// Back to Automatic: single viewer loss (1 of 2 = 50%, not > 50%) does NOT trigger upload congestion.
	m.SetUploadLimit(0)
	if r2.limit() != "" {
		t.Fatalf("reset to auto without congestion should lift upload cap, got %q", r2.limit())
	}
	oneBad := []PeerDemand{
		{ID: "a", RTT: 20 * time.Millisecond},
		{ID: "b", Loss: 0.08, RTT: 80 * time.Millisecond},
	}
	if got := m.autoUploadBudget(now, oneBad); got != 0 {
		t.Fatalf("autoUploadBudget with 1 of 2 bad = %d, want 0", got)
	}

	// Majority congestion (2 of 2 showing loss or rising RTT) activates peakKbps (8500).
	bothBad := []PeerDemand{
		{ID: "a", QueueDelay: 300 * time.Millisecond, RTT: 320 * time.Millisecond},
		{ID: "b", Loss: 0.08, RTT: 80 * time.Millisecond},
	}
	if got := m.autoUploadBudget(now, bothBad); got != 8500 {
		t.Fatalf("autoUploadBudget on majority congestion = %d, want 8500", got)
	}
	if _, auto, peak, _ := m.UploadStats(); auto != 8500 || peak != 8500 {
		t.Fatalf("UploadStats auto=%d peak=%d, want 8500", auto, peak)
	}

	// Regression: lifting wireOff -> wireHigh resumes on levelLow with backoff=0;
	// rebalanceUpload on Automatic must NOT mistake that resume state for link congestion.
	m.rend.resetAutoCap()
	m.rend.peakKbps.Store(800)
	r2.polMu.Lock()
	r2.wireCap = levelOff
	m.applyCap(r2)
	r2.wireCap = levelHigh
	m.applyCap(r2)
	r2.polMu.Unlock()
	m.rebalanceUpload(now)
	if r1.uploadCap != levelHigh || r2.uploadCap != levelHigh || m.rend.autoCapActive.Load() {
		t.Fatalf("lifting wireOff falsely triggered auto upload cap: r1=%d r2=%d active=%v",
			r1.uploadCap, r2.uploadCap, m.rend.autoCapActive.Load())
	}
}

// M10.1: 3 viewers, 1 lossy, measured rates well under nominal -> nobody else is demoted.
func TestAutoUploadThreeViewersOneLossyMeasuredRates(t *testing.T) {
	m := newTestMeshForUpload(t)
	m.SetShareConfig(1080, 30, 5000, 360, true, false)

	t0 := time.Unix(3000, 0)
	r1 := addTestRemote(m, "alice", t0)
	r2 := addTestRemote(m, "bob", t0.Add(time.Second))
	r3 := addTestRemote(m, "carol", t0.Add(2*time.Second))

	// Simulate a mostly still 1080p desktop: ~400 kbps HIGH, ~80 kbps LOW, passive peak 1500 kbps.
	m.sampleLayerRates(t0)
	for sec := 1; sec <= 5; sec++ {
		// 50,000 bytes/s = 400 kbps on HIGH; 10,000 bytes/s = 80 kbps on LOW.
		m.rend.layerBytes[layerHigh].Add(50000)
		m.rend.layerBytes[layerLow].Add(10000)
		m.sampleLayerRates(t0.Add(time.Duration(sec) * time.Second))
	}
	if hi := m.rend.layerKbps[layerHigh].Load(); hi < 350 || hi > 450 {
		t.Fatalf("measured HIGH kbps = %d, want ~400", hi)
	}
	m.recordTxSample(t0.Add(5*time.Second), 1500)

	// Carol has a Wi-Fi blip (12% loss), while Alice and Bob are clean.
	r3.loss.Store(math.Float64bits(0.12))
	now := t0.Add(6 * time.Second)
	m.rebalanceUpload(now)

	if m.rend.autoCapActive.Load() {
		t.Fatal("1 lossy viewer out of 3 must not trigger Automatic upload congestion")
	}
	for _, r := range []*remote{r1, r2, r3} {
		if r.uploadCap != levelHigh {
			t.Fatalf("viewer %s demoted to uploadCap=%d when only 1 of 3 had loss", r.id, r.uploadCap)
		}
	}

	// Even when 2 of 3 viewers show congestion (activating the 1500 kbps budget),
	// measured HIGH is ~400 kbps (3 * 400 = 1200 <= 1500), so nobody is demoted
	// despite nominal 3 * 5000 = 15000 kbps exceeding 1500 kbps.
	r2.polMu.Lock()
	r2.policy.lastQD = 300 * time.Millisecond
	r2.polMu.Unlock()
	m.rebalanceUpload(now.Add(time.Second))
	if !m.rend.autoCapActive.Load() {
		t.Fatal("2 of 3 viewers congested should activate Automatic upload budget")
	}
	for _, r := range []*remote{r1, r2, r3} {
		if r.uploadCap != levelHigh {
			t.Fatalf("viewer %s demoted despite measured 3*400 <= 1500 budget: uploadCap=%d", r.id, r.uploadCap)
		}
	}
}

// M10.1: peakKbps decays to the maximum over the recent 2-minute window.
func TestPeakKbpsDecaysOverWindow(t *testing.T) {
	m := newTestMeshForUpload(t)
	t0 := time.Unix(4000, 0)

	m.recordTxSample(t0, 12000)
	m.recordTxSample(t0.Add(30*time.Second), 2800)
	m.recordTxSample(t0.Add(90*time.Second), 3100)
	if got := m.rend.peakKbps.Load(); got != 12000 {
		t.Fatalf("within 2m window: peakKbps = %d, want 12000", got)
	}

	// Once t0 is older than peakWindow (2m), peakKbps drops to the recent max (3100).
	m.recordTxSample(t0.Add(peakWindow+5*time.Second), 2900)
	if got := m.rend.peakKbps.Load(); got != 3100 {
		t.Fatalf("after 2m window: peakKbps = %d, want 3100", got)
	}
}

// M10.2: jittering loss and RTT do not flip viewer assignments within 10 s,
// and budget changes never call OnRenditions to restart screen capture.
func TestUploadRankingHysteresisAndNoCaptureRestart(t *testing.T) {
	m := newTestMeshForUpload(t)
	var rendCalls atomic.Int32
	m.opt.CanEncode = func() bool { return true }
	m.opt.OnRenditions = func([]int) error {
		rendCalls.Add(1)
		return nil
	}
	m.SetShareConfig(1080, 30, 5000, 360, true, false)

	t0 := time.Unix(5000, 0)
	rA := addTestRemote(m, "a", t0)
	rB := addTestRemote(m, "b", t0.Add(time.Second))
	rA.polMu.Lock()
	rA.policy.lastRTT = 24 * time.Millisecond
	rA.polMu.Unlock()
	rB.polMu.Lock()
	rB.policy.lastRTT = 26 * time.Millisecond
	rB.polMu.Unlock()

	// Budget 6000 fits one HIGH (5000) + one LOW (500): initial order protects "a" on HIGH, demotes "b" to LOW.
	m.SetUploadLimit(6000)
	if rA.uploadCap != levelHigh || rB.uploadCap != levelLow {
		t.Fatalf("initial: a=%d b=%d, want HIGH and LOW", rA.uploadCap, rB.uploadCap)
	}

	// Jitter loss and RTT across bucket boundaries for 20 seconds (never holding a flip >= 10 s).
	for sec := 1; sec <= 20; sec++ {
		// Flip for 5 consecutive seconds at a time, ending on the original order at +20s.
		aWorse := ((sec-1)/5)%2 == 0
		rA.polMu.Lock()
		rB.polMu.Lock()
		if aWorse {
			rA.loss.Store(math.Float64bits(0.06))
			rA.policy.lastRTT = 85 * time.Millisecond
			rB.loss.Store(0)
			rB.policy.lastRTT = 20 * time.Millisecond
		} else {
			rA.loss.Store(0)
			rA.policy.lastRTT = 20 * time.Millisecond
			rB.loss.Store(math.Float64bits(0.06))
			rB.policy.lastRTT = 85 * time.Millisecond
		}
		rB.polMu.Unlock()
		rA.polMu.Unlock()

		m.rebalanceUpload(t0.Add(time.Duration(sec) * time.Second))
		if rA.uploadCap != levelHigh || rB.uploadCap != levelLow {
			t.Fatalf("jitter at +%ds flipped assignment: a=%d b=%d", sec, rA.uploadCap, rB.uploadCap)
		}
	}

	// Now hold "a worse than b" steady starting at +30s: must NOT swap at +39s (9 s < 10 s),
	// and MUST swap at +40s (10 s held).
	rA.polMu.Lock()
	rA.loss.Store(math.Float64bits(0.09))
	rA.policy.lastRTT = 120 * time.Millisecond
	rA.polMu.Unlock()
	rB.polMu.Lock()
	rB.loss.Store(0)
	rB.policy.lastRTT = 18 * time.Millisecond
	rB.polMu.Unlock()

	for sec := 30; sec <= 39; sec++ {
		m.rebalanceUpload(t0.Add(time.Duration(sec) * time.Second))
		if rA.uploadCap != levelHigh || rB.uploadCap != levelLow {
			t.Fatalf("swapped too early at +%ds (held %ds < 10s): a=%d b=%d", sec, sec-30, rA.uploadCap, rB.uploadCap)
		}
	}
	m.rebalanceUpload(t0.Add(40 * time.Second))
	if rA.uploadCap != levelLow || rB.uploadCap != levelHigh {
		t.Fatalf("after 10s steady inversion (+40s): a=%d b=%d, want LOW and HIGH", rA.uploadCap, rB.uploadCap)
	}

	// Flipping back immediately at +41s..+49s (< 10s since +40s change) must not swap back.
	rA.polMu.Lock()
	rA.loss.Store(0)
	rA.policy.lastRTT = 18 * time.Millisecond
	rA.polMu.Unlock()
	rB.polMu.Lock()
	rB.loss.Store(math.Float64bits(0.09))
	rB.policy.lastRTT = 120 * time.Millisecond
	rB.polMu.Unlock()
	for sec := 41; sec <= 49; sec++ {
		m.rebalanceUpload(t0.Add(time.Duration(sec) * time.Second))
		if rA.uploadCap != levelLow || rB.uploadCap != levelHigh {
			t.Fatalf("swapped back within 10s cooldown at +%ds: a=%d b=%d", sec, rA.uploadCap, rB.uploadCap)
		}
	}

	// Varying budget across rendition thresholds (13 Mbit/s, 11.5 Mbit/s, 8.5 Mbit/s)
	// must never call OnRenditions (no capture restarts caused by budget changes).
	for _, lim := range []int{13000, 11500, 8500, 0} {
		m.SetUploadLimit(lim)
	}
	if got := rendCalls.Load(); got != 0 {
		t.Fatalf("OnRenditions called %d times on budget changes, want 0", got)
	}
}

// M10.5: sharer's own WriteVideo is never slowed by the pacer, and manual upload
// limits adjust encoder bitrates instead.
func TestSharerVideoUnpacedAndManualBitrateAdjusted(t *testing.T) {
	m := newTestMeshForUpload(t)
	var mu sync.Mutex
	gotBitrates := map[int]int{}
	m.opt.OnBitrate = func(layer, kbps int) {
		mu.Lock()
		gotBitrates[layer] = kbps
		mu.Unlock()
	}
	m.SetShareConfig(1080, 30, 5000, 360, true, false)

	t0 := time.Unix(6000, 0)
	r1 := addTestRemote(m, "bob", t0)

	// Single viewer with a 2 Mbit/s manual limit on a 5 Mbit/s 1080p share:
	// viewer stays on HIGH, and HIGH encoder bitrate is lowered to 2000 kbps.
	m.SetUploadLimit(2000)
	if r1.uploadCap != levelHigh {
		t.Fatalf("single viewer with 2 Mbit/s manual limit: uploadCap=%d, want levelHigh", r1.uploadCap)
	}
	if got := m.LayerBitrate(layerHigh); got != 2000 {
		t.Fatalf("LayerBitrate(HIGH) = %d, want 2000", got)
	}
	mu.Lock()
	cbHigh := gotBitrates[layerHigh]
	mu.Unlock()
	if cbHigh != 2000 {
		t.Fatalf("OnBitrate(HIGH) = %d, want 2000", cbHigh)
	}

	// WriteVideo of a large burst at 500 kbps manual limit must complete immediately
	// without pacer queueing (pacing is for relay forwarding only).
	m.SetUploadLimit(500)
	au := append([]byte{0x00, 0x00, 0x00, 0x01, 0x67, 0x42, 0x00, 0x1f, 0x00, 0x00, 0x00, 0x01, 0x65}, make([]byte, 24000)...)
	start := time.Now()
	for range 5 {
		m.WriteVideo(layerHigh, au, 33*time.Millisecond)
	}
	// 120 KB at 500 kbps would sleep ~1.9 s if paced; unpaced finishes in a few ms.
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("sharer WriteVideo took %v (was slowed by pacer)", elapsed)
	}

	// Switching back to Automatic restores nominal 5000 kbps HIGH bitrate.
	m.SetUploadLimit(0)
	if got := m.LayerBitrate(layerHigh); got != 5000 {
		t.Fatalf("LayerBitrate(HIGH) after Automatic = %d, want 5000", got)
	}
}

// M10.6: frame-rate stepping on the sole remaining HIGH viewer must not lower
// the multi-viewer HIGH estimate and re-promote a demoted viewer in a loop.
func TestUploadNoFeedbackLoopWithFPSStepping(t *testing.T) {
	m := newTestMeshForUpload(t)
	m.SetShareConfig(1080, 30, 5000, 360, true, true) // fpsSteps = true (30 fps desktop)

	t0 := time.Unix(7000, 0)
	rA := addTestRemote(m, "alice", t0)
	rB := addTestRemote(m, "bob", t0.Add(time.Second))

	// Budget 6800 kbps: 1 HIGH (5000) + 1 LOW (500) = 5500 <= 6800, whereas 2 HIGH = 10000 > 6800.
	m.SetUploadLimit(6800)
	if rA.uploadCap != levelHigh || rB.uploadCap != levelLow {
		t.Fatalf("step 1: alice=%d bob=%d, want HIGH and LOW", rA.uploadCap, rB.uploadCap)
	}

	// Step 2: Alice (now alone on HIGH) steps down to 10 fps, dropping measured HIGH rate to 2000 kbps.
	// Without the fix, both the old nominal formula (3333 * 2 = 6666 <= 6800) and measured 2000 * 2 = 4000 <= 6800
	// would promote Bob back to HIGH, removing the 10 fps cap and immediately demoting Bob again.
	rA.fpsCap.Store(10)
	m.updateHighFPS()
	if m.HighFPSCap() != 10 {
		t.Fatalf("HighFPSCap = %d, want 10", m.HighFPSCap())
	}
	m.rend.layerKbps[layerHigh].Store(2000)
	m.rend.layerKbps[layerLow].Store(400)

	for sec := 1; sec <= 25; sec++ {
		m.rebalanceUpload(t0.Add(time.Duration(sec) * time.Second))
		if rB.uploadCap != levelLow {
			t.Fatalf("feedback loop at +%ds: bob promoted to uploadCap=%d while Alice was FPS-capped", sec, rB.uploadCap)
		}
	}
}

// M10.7: concurrent rebalances, SetShareConfig atomicity, and single +20% step in autoUploadBudget.
func TestConcurrentRebalanceAndShareConfigAtomic(t *testing.T) {
	m := newTestMeshForUpload(t)
	m.SetShareConfig(1080, 30, 5000, 360, true, true)
	t0 := time.Unix(8000, 0)
	addTestRemote(m, "a", t0)
	addTestRemote(m, "b", t0.Add(time.Second))
	addTestRemote(m, "c", t0.Add(2*time.Second))

	// 1. Concurrent autoUploadBudget at exact probe boundary must apply +20% once, never twice.
	m.rend.peakKbps.Store(6000)
	badDemands := []PeerDemand{
		{ID: "a", Loss: 0.08},
		{ID: "b", Loss: 0.08},
		{ID: "c", Loss: 0.0},
	}
	m.autoUploadBudget(t0, badDemands)
	cleanDemands := []PeerDemand{
		{ID: "a", Loss: 0},
		{ID: "b", Loss: 0},
		{ID: "c", Loss: 0},
	}
	m.autoUploadBudget(t0.Add(time.Second), cleanDemands) // sets autoCleanAt = t0 + 1s

	probeAt := t0.Add(time.Second + autoProbeWindow)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.autoUploadBudget(probeAt, cleanDemands)
		}()
	}
	wg.Wait()
	if got := m.rend.autoHeadroom.Load(); got != 20 {
		t.Fatalf("concurrent autoUploadBudget headroom = %d%%, want 20%%", got)
	}

	// 2. Concurrent SetShareConfig + rebalanceUpload + loadCfg reads never observe a half-applied config.
	stop := make(chan struct{})
	var rwg sync.WaitGroup
	for i := range 4 {
		rwg.Add(1)
		go func(i int) {
			defer rwg.Done()
			for n := range 60 {
				if (i+n)%2 == 0 {
					m.SetShareConfig(2160, 60, 14000, 720, true, false)
				} else {
					m.SetShareConfig(720, 30, 2500, 360, false, true)
				}
			}
		}(i)
	}
	for range 4 {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			for n := range 80 {
				m.triggerRebalance()
				m.rebalanceUpload(t0.Add(time.Duration(n) * time.Second))
			}
		}()
	}
	doneCheck := make(chan string, 1)
	go func() {
		for {
			select {
			case <-stop:
				doneCheck <- ""
				return
			default:
				sc := m.rend.loadCfg()
				validA := sc.top == 2160 && sc.fps == 60 && sc.highKbps == 14000 && sc.minHeight == 720 && sc.smallerCopy && !sc.fpsSteps
				validB := sc.top == 720 && sc.fps == 30 && sc.highKbps == 2500 && sc.minHeight == 360 && !sc.smallerCopy && sc.fpsSteps
				validInit := sc.top == 1080 && sc.fps == 30 && sc.highKbps == 5000 && sc.minHeight == 360 && sc.smallerCopy && sc.fpsSteps
				if !validA && !validB && !validInit {
					doneCheck <- "half-applied shareCfg observed"
					return
				}
			}
		}
	}()
	rwg.Wait()
	close(stop)
	if bad := <-doneCheck; bad != "" {
		t.Fatalf("%s: %+v", bad, m.rend.loadCfg())
	}
}

// M10.8: ResetRenditions keeps current upload allocations instead of briefly jumping viewers to HIGH.
func TestResetRenditionsKeepsUploadAllocation(t *testing.T) {
	m := newTestMeshForUpload(t)
	m.SetShareConfig(1080, 30, 5000, 360, true, false)

	t0 := time.Unix(9000, 0)
	rA := addTestRemote(m, "a", t0)
	rB := addTestRemote(m, "b", t0.Add(time.Second))
	rC := addTestRemote(m, "c", t0.Add(2*time.Second))

	// Put an active 720p rendition in m.rend.own (as if granted earlier via handleResize)
	// and set manual limit 8500 kbps -> A=HIGH (5000), B=720p (2500), C=LOW (500).
	m.rend.mu.Lock()
	m.rend.own[layerR720] = t0
	m.rend.mu.Unlock()

	m.SetUploadLimit(8500)
	if rA.uploadCap != levelHigh || rB.uploadCap != levelMid || rC.uploadCap != levelLow {
		t.Fatalf("before reset: a=%d b=%d c=%d", rA.uploadCap, rB.uploadCap, rC.uploadCap)
	}

	m.ResetRenditions()

	if rA.uploadCap != levelHigh {
		t.Fatalf("after ResetRenditions: a.uploadCap = %d, want levelHigh", rA.uploadCap)
	}
	if rB.uploadCap != levelLow || int(rB.level.Load()) == levelHigh {
		t.Fatalf("after ResetRenditions: b jumped to uploadCap=%d level=%d, want levelLow", rB.uploadCap, rB.level.Load())
	}
	if rC.uploadCap != levelLow || int(rC.level.Load()) == levelHigh {
		t.Fatalf("after ResetRenditions: c jumped to uploadCap=%d level=%d, want levelLow", rC.uploadCap, rC.level.Load())
	}
}

// M10 review fix 1: 3 viewers, a tight budget -> one viewer gets a mid-size copy
// after the 10 s hysteresis window, the copy is kept for at least rendIdle, and
// after a source change (ResetRenditions + SetShareConfig) the viewer ends up on mid again.
func TestBudgetMidSizeCopyRetentionAndSourceChange(t *testing.T) {
	m := newTestMeshForUpload(t)
	var mu sync.Mutex
	var rendHistory [][]int
	m.opt.CanEncode = func() bool { return true }
	m.opt.OnRenditions = func(heights []int) error {
		cp := slices.Clone(heights)
		slices.Sort(cp)
		mu.Lock()
		rendHistory = append(rendHistory, cp)
		mu.Unlock()
		return nil
	}
	m.SetShareConfig(1080, 30, 5000, 360, true, false)

	t0 := time.Unix(10000, 0)
	rBob := addTestRemote(m, "bob", t0)
	rCarol := addTestRemote(m, "carol", t0.Add(time.Second))
	rDave := addTestRemote(m, "dave", t0.Add(2*time.Second))

	// Tight budget 13 Mbit/s: 2 HIGH (10000) + 1 720p (2500) = 12500 <= 13000.
	m.SetUploadLimit(13000)

	// Before the 10 s hysteresis window holds, Dave is held at LOW and no mid-size copy starts yet.
	for sec := 0; sec <= 9; sec++ {
		m.rebalanceUpload(t0.Add(time.Duration(sec) * time.Second))
		if rBob.uploadCap != levelHigh || rCarol.uploadCap != levelHigh || rDave.uploadCap != levelLow {
			t.Fatalf("at +%ds (< 10s): bob=%d carol=%d dave=%d, want HIGH/HIGH/LOW",
				sec, rBob.uploadCap, rCarol.uploadCap, rDave.uploadCap)
		}
	}
	mu.Lock()
	if len(rendHistory) != 0 {
		t.Fatalf("mid-size copy created before 10s hysteresis: %v", rendHistory)
	}
	mu.Unlock()

	// At +10s, the ranking has held for 10 s: budget creates 720p and moves Dave to mid.
	m.rebalanceUpload(t0.Add(10 * time.Second))
	if rDave.uploadCap != levelMid || rDave.uploadMid != layerR720 {
		t.Fatalf("at +10s: dave uploadCap=%d uploadMid=%d, want levelMid/layerR720", rDave.uploadCap, rDave.uploadMid)
	}
	mu.Lock()
	if len(rendHistory) != 1 || !slices.Equal(rendHistory[0], []int{720}) {
		t.Fatalf("OnRenditions at +10s = %v, want [[720]]", rendHistory)
	}
	mu.Unlock()

	// Source change (Session.SetSource calls ResetRenditions, starts new source, then SetShareConfig):
	// Dave never jumps to HIGH during the reset and goes right back to 720p mid afterward.
	m.ResetRenditions()
	if rDave.uploadCap == levelHigh || int(rDave.level.Load()) == levelHigh {
		t.Fatalf("during source reset: dave jumped to HIGH (uploadCap=%d level=%d)", rDave.uploadCap, rDave.level.Load())
	}
	m.SetShareConfig(1080, 30, 5000, 360, true, false)
	if rBob.uploadCap != levelHigh || rCarol.uploadCap != levelHigh {
		t.Fatalf("after source change: bob=%d carol=%d, want HIGH", rBob.uploadCap, rCarol.uploadCap)
	}
	if rDave.uploadCap != levelMid || rDave.uploadMid != layerR720 {
		t.Fatalf("after source change: dave uploadCap=%d uploadMid=%d, want levelMid/layerR720", rDave.uploadCap, rDave.uploadMid)
	}
	mu.Lock()
	if len(rendHistory) != 2 || !slices.Equal(rendHistory[1], []int{720}) {
		t.Fatalf("OnRenditions after source change = %v, want [[720] [720]]", rendHistory)
	}
	mu.Unlock()

	// Retention: if budget widens so nobody needs 720p, the created copy is kept for at least rendIdle.
	tIdle := t0.Add(30 * time.Second)
	m.rebalanceUpload(tIdle) // aligns simulated clock with the copy restored above
	m.SetUploadLimit(20000)
	if rDave.uploadCap != levelHigh {
		t.Fatalf("at 20 Mbit/s: dave uploadCap=%d, want HIGH", rDave.uploadCap)
	}
	m.rend.mu.Lock()
	m.dropIdleLocked(tIdle.Add(rendIdle - 5*time.Second))
	_, stillThere := m.rend.own[layerR720]
	m.rend.mu.Unlock()
	if !stillThere {
		t.Fatal("budget-created 720p copy dropped before rendIdle elapsed")
	}

	// Tightening back within rendIdle reuses the kept 720p copy with no new OnRenditions call.
	m.SetUploadLimit(13000)
	if rDave.uploadCap != levelMid || rDave.uploadMid != layerR720 {
		t.Fatalf("reuse within rendIdle: dave uploadCap=%d uploadMid=%d", rDave.uploadCap, rDave.uploadMid)
	}
	mu.Lock()
	if len(rendHistory) != 2 {
		t.Fatalf("expected 0 extra OnRenditions calls when reusing within rendIdle, got %v", rendHistory)
	}
	mu.Unlock()

	// If a reset happens when the budget no longer allows 720p (2 Mbit/s), Dave stays on LOW.
	m.SetUploadLimit(2000)
	m.ResetRenditions()
	m.SetShareConfig(1080, 30, 5000, 360, true, false)
	if rDave.uploadCap != levelLow {
		t.Fatalf("reset under 2 Mbit/s: dave uploadCap=%d, want levelLow", rDave.uploadCap)
	}
}

// M10 review fix 2: a ranking swap applies demotion and promotion together as one
// unit, never leaving both viewers on LOW or both on HIGH over budget.
func TestUploadSwapAppliesAsOneUnit(t *testing.T) {
	m := newTestMeshForUpload(t)
	m.SetShareConfig(1080, 30, 5000, 360, true, false)

	t0 := time.Unix(11000, 0)
	rA := addTestRemote(m, "a", t0)
	rB := addTestRemote(m, "b", t0.Add(time.Second))
	rA.polMu.Lock()
	rA.policy.lastRTT = 20 * time.Millisecond
	rA.polMu.Unlock()
	rB.polMu.Lock()
	rB.policy.lastRTT = 40 * time.Millisecond
	rB.polMu.Unlock()

	// Start in Automatic mode, uncongested at t0: both on HIGH.
	m.rend.peakKbps.Store(6000)
	m.rebalanceUpload(t0)
	if rA.uploadCap != levelHigh || rB.uploadCap != levelHigh {
		t.Fatalf("t0: a=%d b=%d, want both HIGH", rA.uploadCap, rB.uploadCap)
	}

	// At +5s, majority congestion hits (6000 kbps budget fits 1 HIGH + 1 LOW):
	// B is demoted to LOW while A has never changed (asymmetric per-viewer history).
	rA.polMu.Lock()
	rA.policy.lastQD = 300 * time.Millisecond
	rA.polMu.Unlock()
	rB.loss.Store(math.Float64bits(0.08))
	m.rebalanceUpload(t0.Add(5 * time.Second))
	if rA.uploadCap != levelHigh || rB.uploadCap != levelLow {
		t.Fatalf("at +5s: a=%d b=%d, want HIGH and LOW", rA.uploadCap, rB.uploadCap)
	}

	// At +7s (only 2 s after B was demoted!), A becomes lossier than B while congestion continues.
	rA.polMu.Lock()
	rA.policy.lastQD = 0
	rA.policy.lastRTT = 130 * time.Millisecond
	rA.polMu.Unlock()
	rA.loss.Store(math.Float64bits(0.12))
	rB.polMu.Lock()
	rB.policy.lastQD = 300 * time.Millisecond
	rB.policy.lastRTT = 20 * time.Millisecond
	rB.polMu.Unlock()
	rB.loss.Store(0)

	swappedAt := -1
	for sec := 7; sec <= 22; sec++ {
		m.rebalanceUpload(t0.Add(time.Duration(sec) * time.Second))
		if rA.uploadCap == levelLow && rB.uploadCap == levelLow {
			t.Fatalf("at +%ds: both viewers ended up on LOW!", sec)
		}
		if rA.uploadCap == levelHigh && rB.uploadCap == levelHigh {
			t.Fatalf("at +%ds: both viewers ended up on HIGH over budget!", sec)
		}
		if rA.uploadCap == levelLow && rB.uploadCap == levelHigh && swappedAt < 0 {
			swappedAt = sec
		}
	}
	if swappedAt != 17 {
		t.Fatalf("unit swap occurred at +%ds, want +17s (10s after +7s inversion)", swappedAt)
	}
}
