package media

import (
	"errors"
	"fmt"
	"sync"

	"blite/internal/gst"
	"testing"
	"time"
)

// A capture that stops for 2 s (a still screen) and goes on must not
// back-fill the gap in one burst: HIGH's leaky queue would drop
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
	if _, err := paceCapture(p, fps); err != nil {
		t.Fatal(err)
	}
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

// changeFeed is a live source that only delivers the frames a test pushes,
// like Windows capture on a mostly still screen.
const changeFeed = "appsrc name=feed is-live=true format=time do-timestamp=true " +
	"caps=video/x-raw,format=I420,width=320,height=180,framerate=30/1"

// pushChange pushes a 320x180 I420 frame whose luma is all v.
func pushChange(t *testing.T, feed interface {
	PushDur([]byte, time.Duration) bool
}, v byte) time.Time {
	t.Helper()
	f := make([]byte, 320*180*3/2)
	for i := range f {
		f[i] = v
	}
	at := time.Now()
	if !feed.PushDur(f, time.Second/30) {
		t.Fatal("push failed")
	}
	return at
}

// A change on a still screen must reach the encoders at once, not wait for
// the next change (videorate held the newest frame until then), and on a
// still screen go out only a few times. LOW (and a rendition) scales and
// paces it again, with a videorate: the pacer's repeats bring it out.
// HIGH thinned by SetHighFPS lets it through at once too.
func TestCaptureChangeOnTime(t *testing.T) {
	low := fmt.Sprintf(" ! videoscale ! %s ! video/x-raw,width=160,height=90,framerate=15/1", videorate(15))
	for _, tc := range []struct {
		name      string
		fps, thin int
		tail      string        // after the tee
		within    time.Duration // the latency allowed
		every     time.Duration // between changes
		n         int
	}{
		{"HIGH, one change, then still", 30, 0, "", 50 * time.Millisecond, 2 * time.Second, 2},
		{"HIGH, slides at 2 fps", 30, 0, "", 50 * time.Millisecond, 500 * time.Millisecond, 6},
		{"HIGH at 60 thinned to 10, slides at 2 fps", 60, 10, "", 50 * time.Millisecond, 500 * time.Millisecond, 6},
		{"LOW, one change, then still", 30, 0, low, 100 * time.Millisecond, 2 * time.Second, 2},
		{"LOW, slides at 2 fps", 30, 0, low, 100 * time.Millisecond, 500 * time.Millisecond, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lat, repeats := changeLatency(t, tc.fps, tc.thin, tc.tail, tc.every, tc.n)
			worst := time.Duration(0)
			for _, d := range lat {
				worst = max(worst, d)
			}
			t.Logf("latency per change %v, frames out per change %v", lat, repeats)
			if worst > tc.within {
				t.Errorf("a change took %v to come out, want at most %v", worst, tc.within)
			}
			for i, n := range repeats {
				if n > 6 {
					t.Errorf("change %d went out %d times on a still screen, want a few", i, n)
				}
			}
		})
	}
}

// changeLatency pushes n changes, every apart, into a capture branch at
// fps (HIGH's thinning to thin if > 0) and tail, and returns how long each
// took to come out and how many frames of it did.
func changeLatency(t *testing.T, fps, thin int, tail string, every time.Duration, n int) ([]time.Duration, []int) {
	Init()
	p, err := launch(captureBranch(changeFeed, "I420", Layer{Width: 320, Height: 180, FPS: fps}, ChainCPU) +
		" ! identity name=thin" + tail + " ! appsink name=got sync=false max-buffers=1000 drop=false")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	if _, err := paceCapture(p, fps); err != nil {
		t.Fatal(err)
	}
	if thin > 0 {
		e, err := p.element("thin")
		if err != nil {
			t.Fatal(err)
		}
		th, ok := e.Thin(fps)
		if !ok {
			t.Fatal("no thinner")
		}
		th.SetFPS(thin)
		p.atStop(th.Free)
	}
	feed, err := p.element("feed")
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.element("got")
	if err != nil {
		t.Fatal(err)
	}
	type out struct {
		at time.Time
		v  byte
	}
	outs := make(chan out, 1000)
	p.goLoop(func() bool {
		data, _, err := got.Pull(20 * time.Millisecond)
		if err == nil && len(data) > 0 {
			outs <- out{time.Now(), data[0]}
		}
		return !errors.Is(err, gst.ErrEOS)
	})
	if err := p.start(); err != nil {
		t.Fatal(err)
	}
	pushed := make([]time.Time, n+1)
	for i := 0; i <= n; i++ { // change 0 starts the stream
		pushed[i] = pushChange(t, feed, byte(16+20*i))
		time.Sleep(every)
	}
	p.Stop()
	close(outs)
	lat, count := make([]time.Duration, n), make([]int, n)
	for i := range lat {
		lat[i] = -1
	}
	for o := range outs {
		i := int(o.v-16)/20 - 1
		if i < 0 || i >= n {
			continue
		}
		if count[i]++; count[i] == 1 {
			lat[i] = o.at.Sub(pushed[i+1])
		}
	}
	for i, d := range lat {
		if d < 0 {
			t.Errorf("change %d never came out", i+1)
		}
	}
	return lat, count
}

// Through the whole share, a change after a still second comes out of
// HIGH's encoder at once, with a repeat or two, and a still screen encodes
// nothing more. The frames aren't told apart here (LOW's first after the
// change is the picture before, which its videorate held; videorate on
// HIGH did the same): TestCaptureChangeOnTime checks which frames come out.
// SetHighFPS's thinning lets the change through too: in a game share
// thinned to 10 of 60 fps it dropped 5 of every 6 frames, the change and
// its repeats alike.
func TestShareChangeOnTime(t *testing.T) {
	Init()
	enc, err := PickEncoder("x264enc")
	if err != nil {
		t.Skip(err)
	}
	t.Run("30 fps", func(t *testing.T) { shareChange(t, enc, 30, 0) })
	t.Run("60 fps thinned to 10", func(t *testing.T) { shareChange(t, enc, 60, 10) })
}

func shareChange(t *testing.T, enc encoder, fps, thin int) {
	layers := make([]Layer, NumLayers)
	layers[High] = Layer{Width: 640, Height: 360, FPS: fps, BitrateKbs: 800}
	layers[Low] = Layer{Width: 320, Height: 180, FPS: 15, BitrateKbs: 300}
	var mu sync.Mutex
	aus := make([][]time.Time, NumLayers)
	vs, err := startVideoChain(changeFeed, ChainCPU, layers, enc, "", func(layer int, au AccessUnit) {
		mu.Lock()
		aus[layer] = append(aus[layer], time.Now())
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer vs.Stop()
	if thin > 0 {
		vs.SetHighFPS(thin)
	}
	feed, err := vs.element("feed")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ { // a second of motion
		pushChange(t, feed, byte(16+i))
		time.Sleep(time.Second / 30)
	}
	time.Sleep(1500 * time.Millisecond)
	count := func(l int, from time.Time) (n int, first time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		first = -1
		for _, at := range aus[l] {
			if at.After(from) {
				if n++; n == 1 {
					first = at.Sub(from)
				}
			}
		}
		return n, first
	}
	for l := range []int{High, Low} {
		if n, _ := count(l, time.Now().Add(-time.Second)); n > 0 {
			t.Errorf("layer %d encoded %d frames in a still second", l, n)
		}
	}
	at := pushChange(t, feed, 200)
	time.Sleep(2 * time.Second)
	for _, c := range []struct {
		layer int
		in    time.Duration
	}{{High, 100 * time.Millisecond}, {Low, 150 * time.Millisecond}} {
		n, first := count(c.layer, at)
		t.Logf("layer %d: first frame %v after the change, %d frames in the 2 s after it", c.layer, first, n)
		if first < 0 || first > c.in {
			t.Errorf("layer %d: first frame %v after the change, want within %v", c.layer, first, c.in)
		}
		if n > 6 {
			t.Errorf("layer %d encoded %d frames after one change, want a few", c.layer, n)
		}
		// Thinned, the repeats go like any frame.
		if want := 2 - min(thin, 1); c.layer == High && n < want {
			t.Errorf("HIGH encoded %d frames after a change, want at least %d (it and a repeat unless thinned)", n, want)
		}
	}
}

// A keyframe asked for on a still screen (a viewer joined, or switched
// layer) comes out at once: an encoder makes it from its next frame, and
// a still screen gave none until it changed. A phone waited 12 s.
func TestStillScreenKeyframe(t *testing.T) {
	Init()
	enc, err := PickEncoder("x264enc")
	if err != nil {
		t.Skip(err)
	}
	layers := make([]Layer, NumLayers)
	layers[High] = Layer{Width: 640, Height: 360, FPS: 30, BitrateKbs: 800}
	layers[Low] = Layer{Width: 320, Height: 180, FPS: 15, BitrateKbs: 300}
	var mu sync.Mutex
	keys := make([][]time.Time, NumLayers)
	vs, err := startVideoChain(changeFeed, ChainCPU, layers, enc, "", func(layer int, au AccessUnit) {
		if annexBKey(au.Data) {
			mu.Lock()
			keys[layer] = append(keys[layer], time.Now())
			mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer vs.Stop()
	feed, err := vs.element("feed")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 15; i++ {
		pushChange(t, feed, byte(16+i))
		time.Sleep(time.Second / 30)
	}
	time.Sleep(1500 * time.Millisecond)
	for _, l := range []int{High, Low} {
		at := time.Now()
		vs.ForceKeyframe(l)
		time.Sleep(time.Second)
		mu.Lock()
		first := time.Duration(-1)
		for _, k := range keys[l] {
			if k.After(at) {
				first = k.Sub(at)
				break
			}
		}
		mu.Unlock()
		t.Logf("layer %d: keyframe %v after asking", l, first)
		if first < 0 || first > 300*time.Millisecond {
			t.Errorf("layer %d: keyframe %v after asking on a still screen, want within 300ms", l, first)
		}
	}
}
