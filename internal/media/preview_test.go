package media

import (
	"sync/atomic"
	"testing"
	"time"
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
