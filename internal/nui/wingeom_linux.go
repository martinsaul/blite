package nui

import (
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// X11 only (Wayland gives no handle and lets no client place itself).
var (
	geomX    *xgb.Conn
	geomOnce sync.Once
)

func geomConn() *xgb.Conn {
	geomOnce.Do(func() {
		if c, err := xgb.NewConn(); err == nil {
			geomX = c
		}
	})
	return geomX
}

// getWinGeom is the window's client area on the root window.
func getWinGeom(handle uintptr) (winGeom, bool) {
	c := geomConn()
	if c == nil || handle == 0 {
		return winGeom{}, false
	}
	w := xproto.Window(handle)
	geo, err := xproto.GetGeometry(c, xproto.Drawable(w)).Reply()
	if err != nil {
		return winGeom{}, false
	}
	at, err := xproto.TranslateCoordinates(c, w, geo.Root, 0, 0).Reply()
	if err != nil {
		return winGeom{}, false
	}
	return winGeom{X: int(at.DstX), Y: int(at.DstY), W: int(geo.Width), H: int(geo.Height)}, true
}

// setWinGeom asks the window manager to place the client area at g (it may
// keep the window on a screen, or add its frame).
func setWinGeom(handle uintptr, g winGeom) {
	c := geomConn()
	if c == nil || handle == 0 {
		return
	}
	xproto.ConfigureWindow(c, xproto.Window(handle),
		xproto.ConfigWindowX|xproto.ConfigWindowY|xproto.ConfigWindowWidth|xproto.ConfigWindowHeight,
		[]uint32{uint32(int32(g.X)), uint32(int32(g.Y)), uint32(g.W), uint32(g.H)})
	c.Sync()
}
