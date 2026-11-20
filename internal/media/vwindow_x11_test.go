//go:build linux

package media

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"blite/internal/gst"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// Real clicks (XTEST, through xdotool) on the controls of a grid drawn into
// our own X11 window reach OnControl, with every sink we draw with. They
// didn't with xvimagesink: it doesn't ask for button events on a window it
// was given (only on windows it makes), so the clicks went nowhere while
// moving the pointer still showed the controls. Opt-in, on a test X server:
//
//	Xvfb :77 -screen 0 5120x2880x24 &
//	DISPLAY=:77 BLITE_X11_TEST=1 go test -run TestVideoWindowClicks ./internal/media
func TestVideoWindowClicks(t *testing.T) {
	if os.Getenv("BLITE_X11_TEST") == "" {
		t.Skip("set BLITE_X11_TEST=1 with DISPLAY pointing at a test X server")
	}
	if _, err := exec.LookPath("xdotool"); err != nil {
		t.Skip("needs xdotool")
	}
	Init()
	c, err := xgb.NewConn()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	scr := xproto.Setup(c).DefaultScreen(c)
	sw, sh := int(scr.WidthInPixels), int(scr.HeightInPixels)
	// ximagesink stands in for xvimagesink where there is no Xv port (Xvfb):
	// both come from the same X11 code and handle a given window alike.
	for _, sink := range append(VideoWindowSinks(), "ximagesink") {
		sizes := [][2]int{{1280, 720}, {sw * 9 / 10, sh * 9 / 10}}
		if sink == "ximagesink" {
			sizes = [][2]int{{gridW, gridH}} // it doesn't scale
		}
		for _, size := range sizes {
			t.Run(fmt.Sprintf("%s/%dx%d", sink, size[0], size[1]), func(t *testing.T) {
				if !gst.HasFactory(sink) {
					t.Skip("no " + sink)
				}
				videoWindowClicks(t, c, sink, size[0], size[1])
			})
		}
	}
}

func videoWindowClicks(t *testing.T, c *xgb.Conn, sink string, ww, wh int) {
	old := videoWindowSinks
	videoWindowSinks = []string{sink}
	defer func() { videoWindowSinks = old }()
	pl, err := StartPlayer(ViewGrid)
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Stop()
	win, err := OpenVideoWindow("clicks", ww, wh)
	if err != nil {
		t.Fatal(err)
	}
	defer win.Destroy()
	got := make(chan Control, 16)
	g, err := StartGrid(GridConfig{
		Tiles:     []Tile{{Channel: pl.Channel, Label: "alice", ID: "a", Volume: 1}},
		View:      ViewWindow,
		Window:    win,
		OnControl: func(_ string, ctl Control, _ float64) { got <- ctl },
	})
	if err != nil {
		t.Skipf("%s can't draw here: %v", sink, err)
	}
	defer g.Stop()
	if !g.InWindow() || g.Sink != sink {
		t.Skipf("%s can't draw here (%s)", sink, g.Sink)
	}
	// The window's size and place now (a window manager may have changed them).
	geo, err := xproto.GetGeometry(c, xproto.Drawable(win.Handle())).Reply()
	if err != nil {
		t.Fatal(err)
	}
	root := xproto.Setup(c).DefaultScreen(c).Root
	tr, err := xproto.TranslateCoordinates(c, xproto.Window(win.Handle()), root, 0, 0).Reply()
	if err != nil {
		t.Fatal(err)
	}
	// The canvas (gridW x gridH) fits the window, letterboxed.
	scale := min(float64(geo.Width)/gridW, float64(geo.Height)/gridH)
	offX := (float64(geo.Width) - gridW*scale) / 2
	offY := (float64(geo.Height) - gridH*scale) / 2
	g.ctl.Lock()
	o := g.ctl.ov[0]
	g.ctl.Unlock()
	at := func(u float64) (int, int) {
		x := float64(o.barX) + u*float64(o.barH)
		y := float64(o.barY) + float64(o.barH)/2
		return int(tr.DstX) + int(offX+x*scale), int(tr.DstY) + int(offY+y*scale)
	}
	xdo := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("xdotool", args...).CombinedOutput(); err != nil {
			t.Fatalf("xdotool %v: %v %s", args, err, out)
		}
	}
	for _, tc := range []struct {
		u    float64
		want Control
	}{{barPinX, ControlPin}, {barMuteX, ControlMute}, {barEyeX, ControlEye}} {
		x, y := at(tc.u)
		xdo("mousemove", strconv.Itoa(x-20), strconv.Itoa(y)) // show the controls
		time.Sleep(4 * controlTick)
		xdo("mousemove", strconv.Itoa(x), strconv.Itoa(y))
		time.Sleep(4 * controlTick)
		g.ctl.Lock()
		shown := g.ctl.shown
		g.ctl.Unlock()
		if shown != 0 {
			t.Fatalf("pointer moves on the window didn't show the controls")
		}
		xdo("click", "1")
		select {
		case ctl := <-got:
			if ctl != tc.want {
				t.Fatalf("clicked %s at %d,%d, got %s", tc.want, x, y, ctl)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("clicked %s at %d,%d (window %dx%d at %d,%d): the controls showed, the click did nothing", tc.want, x, y, geo.Width, geo.Height, tr.DstX, tr.DstY)
		}
		select {
		case ctl := <-got:
			t.Fatalf("one click on %s gave a second control: %s", tc.want, ctl)
		case <-time.After(300 * time.Millisecond):
		}
	}
}
