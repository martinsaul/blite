package media

import (
	"sync/atomic"
	"testing"
	"time"

	"blite/internal/gst"
)

// The preview branch costs nothing until SetPreview opens it, and a
// self-preview tile in the grid shows it.
func TestPreviewValve(t *testing.T) {
	Init()
	ch := NewPreviewChannel()
	var high atomic.Int64
	src, _, err := StartVideoSource(SourceTest, "", "", nil, "", ch, func(layer int, _ AccessUnit) {
		if layer == High {
			high.Add(1)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Stop()
	time.Sleep(time.Second)
	if n := src.PreviewFrames(); n != 0 {
		t.Fatalf("closed preview passed %d frames", n)
	}
	pl, err := StartPlayer(ViewGrid)
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Stop()
	g, err := StartGrid(GridConfig{
		Tiles: []Tile{
			{Channel: pl.Channel, Label: "alice", ID: "a"},
			{Channel: ch, Label: "You", ID: "self", Self: true},
		},
		View:      ViewCount,
		OnControl: func(string, Control, float64) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Stop()

	src.SetPreview(true)
	time.Sleep(time.Second) // opening passes a short burst
	n, h := src.PreviewFrames(), high.Load()
	time.Sleep(3 * time.Second)
	// The preview runs at PreviewFPS, half the 30 fps capture.
	d, dh := src.PreviewFrames()-n, int(high.Load()-h)
	if dh < 30 || d < dh*3/10 || d > dh*7/10 {
		t.Fatalf("open preview passed %d frames while HIGH encoded %d, want about half", d, dh)
	}
	if g.Frames() <= 0 {
		t.Fatalf("grid showed %d frames", g.Frames())
	}
	src.SetPreview(false)
	time.Sleep(300 * time.Millisecond)
	n = src.PreviewFrames()
	time.Sleep(time.Second)
	if d := src.PreviewFrames() - n; d != 0 {
		t.Fatalf("closed again, the preview passed %d frames", d)
	}
}

// A stream whose frames stop (a still window: Windows window capture only
// sends changes) keeps its last picture instead of turning black.
func TestInterHoldsLastFrame(t *testing.T) {
	Init()
	ch := NewPreviewChannel()
	src, err := launch("videotestsrc num-buffers=10 pattern=white ! video/x-raw,format=I420,width=64,height=36,framerate=30/1 ! intervideosink channel=" + ch)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Stop()
	view, err := launch("intervideosrc channel=" + ch + " " + interHold + " ! video/x-raw,format=I420,width=64,height=36 ! appsink name=out sync=false max-buffers=1 drop=true")
	if err != nil {
		t.Fatal(err)
	}
	defer view.Stop()
	out, err := view.element("out")
	if err != nil {
		t.Fatal(err)
	}
	if err := src.start(); err != nil {
		t.Fatal(err)
	}
	if err := view.start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond) // the 10 frames are long gone
	data, _, err := out.Pull(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if y := data[0]; y < 200 {
		t.Fatalf("2.5 s after the last frame the picture is %d (black), want the last white frame", y)
	}
}

// The big preview (our preview as a tile or in its own window) comes at
// PreviewBigW x PreviewBigH on PreviewBigChannel instead of the corner
// picture's PreviewW x PreviewH, and switching while it runs keeps going.
func TestPreviewBig(t *testing.T) {
	Init()
	ch := NewPreviewChannel()
	src, _, err := StartVideoSource(SourceTest, Res1080, "", nil, "", ch, func(int, AccessUnit) {})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Stop()
	view := func(ch string) *gst.Element {
		t.Helper()
		p, err := launch("intervideosrc channel=" + ch + " " + interHold + " ! videoconvert ! video/x-raw,format=I420 ! appsink name=out sync=false max-buffers=1 drop=true")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Stop)
		out, err := p.element("out")
		if err != nil {
			t.Fatal(err)
		}
		if err := p.start(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	small, big := view(ch), view(PreviewBigChannel(ch))
	size := func(out *gst.Element, w, h int) {
		t.Helper()
		want := w * h * 3 / 2
		var got int
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
			if data, _, err := out.Pull(time.Second); err == nil {
				if got = len(data); got == want {
					return
				}
			}
		}
		t.Fatalf("preview frames of %d bytes, want %dx%d (%d)", got, w, h, want)
	}
	src.SetPreview(true)
	size(small, PreviewW, PreviewH)
	for range 2 {
		src.SetPreviewBig(true)
		time.Sleep(200 * time.Millisecond)
		n := src.PreviewFrames()
		size(big, PreviewBigW, PreviewBigH)
		time.Sleep(time.Second)
		if d := src.PreviewFrames() - n; d < PreviewFPS/2 || d > PreviewFPS*2 {
			t.Fatalf("%d big preview frames in about a second, want about %d (one valve open)", d, PreviewFPS)
		}
		src.SetPreviewBig(false)
		size(small, PreviewW, PreviewH)
	}
	select {
	case err := <-src.Err:
		t.Fatalf("capture: %v", err)
	default:
	}
}
