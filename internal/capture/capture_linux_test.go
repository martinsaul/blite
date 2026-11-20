package capture

import (
	"encoding/binary"
	"os"
	"testing"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

func TestDecodeNetWMIcon(t *testing.T) {
	var v []byte
	put := func(n uint32) { v = binary.LittleEndian.AppendUint32(v, n) }
	icon := func(w, h int, argb uint32) {
		put(uint32(w))
		put(uint32(h))
		for range w * h {
			put(argb)
		}
	}
	icon(16, 16, 0xff0000ff)
	icon(48, 48, 0x80ff0000)
	icon(32, 32, 0xff00ff00)
	img := decodeNetWMIcon(v)
	if img == nil || img.Bounds().Dx() != 32 {
		t.Fatalf("want the 32 px icon, got %v", img)
	}
	if r, g, b, a := img.At(0, 0).RGBA(); r != 0 || g != 0xffff || b != 0 || a != 0xffff {
		t.Fatalf("pixel = %x %x %x %x, want opaque green", r, g, b, a)
	}
	if decodeNetWMIcon(v[:20]) != nil { // truncated
		t.Fatal("decoded a truncated icon")
	}
	// Cut off in the second icon: the first one is still good.
	if img := decodeNetWMIcon(v[:(2+16*16+2+100)*4]); img == nil || img.Bounds().Dx() != 16 {
		t.Fatalf("want the 16 px icon before the cut, got %v", img)
	}
}

// List leaves out the desktop's own windows: the icons layer (Cinnamon's
// "Desktop", nemo-desktop) and panels. Opt-in, on a test X server without a
// window manager (it sets the root's _NET_CLIENT_LIST itself):
//
//	Xvfb :77 & DISPLAY=:77 BLITE_X11_TEST=1 go test -run TestListSkipsDesktop ./internal/capture
func TestListSkipsDesktop(t *testing.T) {
	if os.Getenv("BLITE_X11_TEST") == "" {
		t.Skip("set BLITE_X11_TEST=1 with DISPLAY pointing at a test X server")
	}
	c, err := xgb.NewConn()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	scr := xproto.Setup(c).DefaultScreen(c)
	intern := func(name string) xproto.Atom {
		r, err := xproto.InternAtom(c, false, uint16(len(name)), name).Reply()
		if err != nil {
			t.Fatal(err)
		}
		return r.Atom
	}
	u32 := func(vs ...uint32) []byte {
		var b []byte
		for _, v := range vs {
			b = binary.LittleEndian.AppendUint32(b, v)
		}
		return b
	}
	var ids []uint32
	mk := func(title, class, typ string) xproto.Window {
		w, _ := xproto.NewWindowId(c)
		xproto.CreateWindow(c, scr.RootDepth, w, scr.Root, 0, 0, 64, 64, 0, xproto.WindowClassInputOutput, scr.RootVisual, 0, nil)
		xproto.ChangeProperty(c, xproto.PropModeReplace, w, xproto.AtomWmName, xproto.AtomString, 8, uint32(len(title)), []byte(title))
		xproto.ChangeProperty(c, xproto.PropModeReplace, w, xproto.AtomWmClass, xproto.AtomString, 8, uint32(len(class)), []byte(class))
		if typ != "" {
			xproto.ChangeProperty(c, xproto.PropModeReplace, w, intern("_NET_WM_WINDOW_TYPE"), xproto.AtomAtom, 32, 1, u32(uint32(intern(typ))))
		}
		ids = append(ids, uint32(w))
		return w
	}
	app := mk("Notes", "notes\x00Notes\x00", "_NET_WM_WINDOW_TYPE_NORMAL")
	mk("Desktop", "nemo-desktop\x00Nemo-desktop\x00", "_NET_WM_WINDOW_TYPE_DESKTOP")
	mk("nemo-desktop", "nemo-desktop\x00Nemo-desktop\x00", "") // no type (yet)
	mk("Panel", "cinnamon\x00Cinnamon\x00", "_NET_WM_WINDOW_TYPE_DOCK")
	list := intern("_NET_CLIENT_LIST")
	xproto.ChangeProperty(c, xproto.PropModeReplace, scr.Root, list, xproto.AtomWindow, 32, uint32(len(ids)), u32(ids...))
	c.Sync()
	defer func() {
		xproto.DeleteProperty(c, scr.Root, list)
		for _, w := range ids {
			xproto.DestroyWindow(c, xproto.Window(w))
		}
		c.Sync()
	}()
	ts, err := List()
	if err != nil {
		t.Fatal(err)
	}
	var wins []string
	for _, tg := range ts {
		if tg.Kind == "window" {
			wins = append(wins, tg.Name)
		}
	}
	if len(wins) != 1 || wins[0] != "Notes" {
		t.Fatalf("windows %q, want only Notes (window %#x)", wins, app)
	}
}
