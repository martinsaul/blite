package media

import (
	"encoding/binary"
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// Both scale to the window; xvimagesink needs an Xv port, which some
// drivers and VMs lack.
var previewSinks = []string{"xvimagesink", "glimagesink"}

type x11Preview struct {
	c         *xgb.Conn
	win       xproto.Window
	closed    chan struct{}
	closeOnce sync.Once
	destroy   sync.Once
}

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

// openPreviewWindow creates an X11 window the window manager keeps 16:9,
// between 144p and 240p, and above other windows.
func openPreviewWindow() (previewWindow, error) {
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
	if err := xproto.CreateWindowChecked(c, scr.RootDepth, wid, scr.Root, 0, 0, previewStartW, previewStartH, 0,
		xproto.WindowClassInputOutput, scr.RootVisual, xproto.CwBackPixel|xproto.CwEventMask,
		[]uint32{scr.BlackPixel, xproto.EventMaskStructureNotify}).Check(); err != nil {
		c.Close()
		return nil, err
	}
	prop := func(name string, typ xproto.Atom, format byte, data []byte) {
		xproto.ChangeProperty(c, xproto.PropModeReplace, wid, atom(c, name), typ, format, uint32(len(data))/uint32(format/8), data)
	}
	prop("WM_NAME", xproto.AtomString, 8, []byte(previewTitle))
	prop("_NET_WM_NAME", atom(c, "UTF8_STRING"), 8, []byte(previewTitle))
	prop("WM_CLASS", xproto.AtomString, 8, []byte("bellionshare-preview\x00BellionShare\x00"))
	// WM_NORMAL_HINTS: PMinSize | PMaxSize | PAspect.
	prop("WM_NORMAL_HINTS", xproto.AtomWmSizeHints, 32, u32s(16|32|128, 0, 0, 0, 0,
		previewMinW, previewMinH, PreviewW, PreviewH, 0, 0, 16, 9, 16, 9, 0, 0, 0))
	del := atom(c, "WM_DELETE_WINDOW")
	prop("WM_PROTOCOLS", xproto.AtomAtom, 32, u32s(uint32(del)))
	prop("_NET_WM_STATE", xproto.AtomAtom, 32, u32s(uint32(atom(c, "_NET_WM_STATE_ABOVE"))))
	xproto.MapWindow(c, wid)
	c.Sync()
	w := &x11Preview{c: c, win: wid, closed: make(chan struct{})}
	go func() {
		for {
			ev, err := c.WaitForEvent()
			if ev == nil && err == nil {
				return // connection closed
			}
			if cm, ok := ev.(xproto.ClientMessageEvent); ok && cm.Format == 32 && xproto.Atom(cm.Data.Data32[0]) == del {
				xproto.UnmapWindow(c, wid) // hide now, Destroy tears it down
				w.closeOnce.Do(func() { close(w.closed) })
			}
		}
	}()
	return w, nil
}

func (w *x11Preview) Handle() uintptr         { return uintptr(w.win) }
func (w *x11Preview) Closed() <-chan struct{} { return w.closed }

func (w *x11Preview) Destroy() {
	w.destroy.Do(func() {
		xproto.DestroyWindow(w.c, w.win)
		w.c.Sync()
		w.c.Close()
	})
}
