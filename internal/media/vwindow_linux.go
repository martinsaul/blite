package media

import (
	"encoding/binary"
	"sync"
	"sync/atomic"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// Both scale to the window; xvimagesink needs an Xv port, which some
// drivers and VMs lack.
var videoWindowSinks = []string{"xvimagesink", "glimagesink"}

func atom(c *xgb.Conn, name string) xproto.Atom {
	r, err := xproto.InternAtom(c, false, uint16(len(name)), name).Reply()
	if err != nil {
		return 0
	}
	return r.Atom
}

func u32s(vs ...uint32) []byte {
	b := make([]byte, 4*len(vs))
	for i, v := range vs {
		binary.LittleEndian.PutUint32(b[4*i:], v)
	}
	return b
}

type x11Video struct {
	c         *xgb.Conn
	win       xproto.Window
	closed    chan struct{}
	closeReq  chan struct{}
	intercept atomic.Bool
	closeOnce sync.Once
	destroy   sync.Once
	// onButton gets the pointer button events on the window (see
	// SetButtonHandler); buttonMu also keeps it from running after it is
	// replaced.
	buttonMu  sync.Mutex
	onButton  func(press bool, button int, x, y float64)
	buttonGen uint64
}

func openVideoWindow(title string, w, h int) (Window, error) {
	c, err := xgb.NewConn()
	if err != nil {
		return nil, err
	}
	scr := xproto.Setup(c).DefaultScreen(c)
	wid, err := xproto.NewWindowId(c)
	if err != nil {
		c.Close()
		return nil, err
	}
	if err := xproto.CreateWindowChecked(c, scr.RootDepth, wid, scr.Root, 0, 0, uint16(w), uint16(h), 0,
		xproto.WindowClassInputOutput, scr.RootVisual, xproto.CwBackPixel|xproto.CwEventMask,
		[]uint32{scr.BlackPixel, xproto.EventMaskStructureNotify | xproto.EventMaskButtonPress | xproto.EventMaskButtonRelease | xproto.EventMaskButtonMotion}).Check(); err != nil {
		c.Close()
		return nil, err
	}
	v := &x11Video{c: c, win: wid, closed: make(chan struct{}), closeReq: make(chan struct{}, 1)}
	v.SetTitle(title)
	class := []byte("bellionshare-video\x00BellionShare\x00")
	xproto.ChangeProperty(c, xproto.PropModeReplace, wid, atom(c, "WM_CLASS"), xproto.AtomString, 8, uint32(len(class)), class)
	del := atom(c, "WM_DELETE_WINDOW")
	xproto.ChangeProperty(c, xproto.PropModeReplace, wid, atom(c, "WM_PROTOCOLS"), xproto.AtomAtom, 32, 1, u32s(uint32(del)))
	xproto.MapWindow(c, wid)
	c.Sync()
	go func() {
		for {
			ev, err := c.WaitForEvent()
			if ev == nil && err == nil {
				return // connection closed
			}
			switch e := ev.(type) {
			case xproto.ButtonPressEvent:
				v.button(true, int(e.Detail), float64(e.EventX), float64(e.EventY))
				continue
			case xproto.ButtonReleaseEvent:
				v.button(false, int(e.Detail), float64(e.EventX), float64(e.EventY))
				continue
			case xproto.MotionNotifyEvent:
				// Only while a button is held: X then reports the
				// pointer to us alone (the press grabbed it), so the
				// sink sees no moves (a volume slider drag) unless we
				// pass them on.
				v.button(false, 0, float64(e.EventX), float64(e.EventY))
				continue
			}
			if cm, ok := ev.(xproto.ClientMessageEvent); ok && cm.Format == 32 && xproto.Atom(cm.Data.Data32[0]) == del {
				if v.intercept.Load() {
					select {
					case v.closeReq <- struct{}{}:
					default:
					}
					continue
				}
				xproto.UnmapWindow(c, wid) // hide now, the owner destroys it
				v.closeOnce.Do(func() { close(v.closed) })
			}
		}
	}()
	return v, nil
}

// The window takes the pointer button events itself: xvimagesink (and
// ximagesink) select only pointer moves on a window they are given, never
// clicks, which X lets a single client select on a window. Without this, the
// tile controls lit up under the pointer but clicks on them went nowhere.
// glimagesink draws into a child window that takes its own clicks, so none
// reach this one twice.
func (v *x11Video) button(press bool, button int, x, y float64) {
	v.buttonMu.Lock()
	defer v.buttonMu.Unlock()
	if v.onButton != nil {
		v.onButton(press, button, x, y)
	}
}

// SetButtonHandler makes f get the window's pointer button events (x, y in
// the window's pixels; button 0: a move while a button is held) until the returned func removes it (if no other
// handler replaced it since). Once that returns, f isn't running and won't
// run again.
func (v *x11Video) SetButtonHandler(f func(press bool, button int, x, y float64)) (remove func()) {
	v.buttonMu.Lock()
	defer v.buttonMu.Unlock()
	v.onButton = f
	v.buttonGen++
	gen := v.buttonGen
	return func() {
		v.buttonMu.Lock()
		defer v.buttonMu.Unlock()
		if v.buttonGen == gen {
			v.onButton = nil
		}
	}
}

func (v *x11Video) Handle() uintptr                 { return uintptr(v.win) }
func (v *x11Video) Closed() <-chan struct{}         { return v.closed }
func (v *x11Video) SetInterceptClose(on bool)       { v.intercept.Store(on) }
func (v *x11Video) CloseRequested() <-chan struct{} { return v.closeReq }

func (v *x11Video) SetTitle(title string) {
	for _, p := range []struct {
		name string
		typ  xproto.Atom
	}{{"WM_NAME", xproto.AtomString}, {"_NET_WM_NAME", atom(v.c, "UTF8_STRING")}} {
		xproto.ChangeProperty(v.c, xproto.PropModeReplace, v.win, atom(v.c, p.name), p.typ, 8, uint32(len(title)), []byte(title))
	}
	v.c.Sync()
}

func (v *x11Video) PointerInside() bool {
	p, err := xproto.QueryPointer(v.c, v.win).Reply()
	if err != nil {
		return true // unknown: don't hide the buttons
	}
	g, err := xproto.GetGeometry(v.c, xproto.Drawable(v.win)).Reply()
	if err != nil || !p.SameScreen {
		return err != nil
	}
	return p.WinX >= 0 && p.WinY >= 0 && int(p.WinX) < int(g.Width) && int(p.WinY) < int(g.Height) && !v.Minimized()
}

func (v *x11Video) Minimized() bool {
	a, err := xproto.GetWindowAttributes(v.c, v.win).Reply()
	return err == nil && a.MapState != xproto.MapStateViewable
}

func (v *x11Video) Destroy() {
	v.destroy.Do(func() {
		xproto.DestroyWindow(v.c, v.win)
		v.c.Sync()
		v.c.Close()
	})
}
