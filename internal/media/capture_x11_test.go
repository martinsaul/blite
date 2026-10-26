//go:build linux

package media

import (
	"fmt"
	"os"
	"testing"
	"time"

	"blite/internal/capture"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// Captures every listed monitor and window on a real X server. Opt-in:
//
//	Xvfb :77 & xterm -display :77 -T probe &
//	DISPLAY=:77 BLITE_X11_TEST=1 go test -run TestCaptureTargets ./internal/media
func TestCaptureTargets(t *testing.T) {
	if os.Getenv("BLITE_X11_TEST") == "" {
		t.Skip("set BLITE_X11_TEST=1 with DISPLAY pointing at a test X server")
	}
	Init()
	ts, err := capture.List()
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, tg := range ts {
		kinds[tg.Kind] = true
		got := make(chan struct{}, 1)
		src, _, err := StartVideoSource(SourceKind(tg.Source), "", nil, "", func(layer int, _ AccessUnit) {
			select {
			case got <- struct{}{}:
			default:
			}
		})
		if err != nil {
			t.Fatalf("%s %q: %v", tg.Kind, tg.Name, err)
		}
		select {
		case <-got:
			t.Logf("captured %s %q (%s)", tg.Kind, tg.Name, tg.Source)
		case err := <-src.Err:
			t.Errorf("%s %q: %v", tg.Kind, tg.Name, err)
		case <-time.After(5 * time.Second):
			t.Errorf("%s %q: no frames", tg.Kind, tg.Name)
		}
		src.Stop()
	}
	if !kinds["monitor"] || !kinds["window"] {
		t.Fatalf("want monitors and windows, got %+v", ts)
	}
}

// A shared window that closes ends the capture with an error; it used to
// kill the process from Xlib's default error handler (BadWindow). Opt-in
// like TestCaptureTargets (needs only Xvfb).
func TestCapturedWindowCloses(t *testing.T) {
	if os.Getenv("BLITE_X11_TEST") == "" {
		t.Skip("set BLITE_X11_TEST=1 with DISPLAY pointing at a test X server")
	}
	Init()
	c, err := xgb.NewConn()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	scr := xproto.Setup(c).DefaultScreen(c)
	w, _ := xproto.NewWindowId(c)
	xproto.CreateWindow(c, scr.RootDepth, w, scr.Root, 0, 0, 320, 240, 0, xproto.WindowClassInputOutput, scr.RootVisual,
		xproto.CwBackPixel, []uint32{0x336699})
	xproto.MapWindow(c, w)
	c.Sync()

	frames := make(chan struct{}, 1)
	src, _, err := StartVideoSource(SourceKind(fmt.Sprintf("window:%#x", uint32(w))), "", nil, "", func(int, AccessUnit) {
		select {
		case frames <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Stop()
	select {
	case <-frames:
	case <-time.After(5 * time.Second):
		t.Fatal("no frames from the window")
	}
	xproto.DestroyWindow(c, w)
	c.Sync()
	select {
	case err := <-src.Err:
		t.Logf("capture ended: %v", err)
	case <-time.After(5 * time.Second):
		t.Log("capture still running after the window closed (no error posted)")
	}
}
