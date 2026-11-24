package media

import (
	"testing"
	"time"
)

// A capture that stops for 2 s (a still screen) and goes on must not make
// videorate back-fill the gap in one burst: HIGH's leaky queue would drop
// most of it, and the encode check took that for a slow encoder.
func TestCaptureGapNoBurst(t *testing.T) {
	Init()
	const fps = 30
	src := "videotestsrc is-live=true pattern=ball ! video/x-raw,width=320,height=180,framerate=30/1 ! valve name=gap drop=false"
	p, err := launch(captureBranch(src, "I420", Layer{Width: 320, Height: 180, FPS: fps}, ChainCPU) + " ! fakesink sync=false")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	gap, err := p.element("gap")
	if err != nil {
		t.Fatal(err)
	}
	offered, err := p.element("offered")
	if err != nil {
		t.Fatal(err)
	}
	n, ok := offered.CountBuffers()
	if !ok {
		t.Fatal("no counter")
	}
	if err := p.start(); err != nil {
		t.Fatal(err)
	}
	// Readings every 100 ms; any 1 s span of them is a window.
	const step = 100 * time.Millisecond
	var reads []int
	tick := time.NewTicker(step)
	defer tick.Stop()
	for i := 0; i < 60; i++ {
		switch i {
		case 15:
			gap.SetBool("drop", true)
		case 35:
			gap.SetBool("drop", false)
		}
		<-tick.C
		reads = append(reads, n.Load())
	}
	most := 0
	for i := 10; i < len(reads); i++ {
		most = max(most, reads[i]-reads[i-10])
	}
	t.Logf("most frames offered in 1 s: %d (target %d), all %v", most, fps, reads)
	if most > 2*fps {
		t.Errorf("offered %d frames in 1 s after a 2 s gap, want at most %d", most, 2*fps)
	}
	if after := reads[len(reads)-1] - reads[len(reads)-11]; after < fps/2 {
		t.Errorf("offered %d frames in the last second, want the capture's rate again", after)
	}
}
