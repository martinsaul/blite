package capture

import (
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"
)

func init() { xgb.Logger = log.New(io.Discard, "", 0) } // it reports a missing ~/.Xauthority on every connect

// List returns the X11 monitors (RandR) and the window manager's client
// windows (_NET_CLIENT_LIST), skipping minimized and untitled ones.
func List() ([]Target, error) {
	c, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("X11: %w (Wayland sessions are not supported yet)", err)
	}
	defer c.Close()
	root := xproto.Setup(c).DefaultScreen(c).Root
	var out []Target
	if randr.Init(c) == nil {
		if mons, err := randr.GetMonitors(c, root, true).Reply(); err == nil {
			for _, m := range mons.Monitors {
				name := atomName(c, m.Name)
				r := Region{int(m.X), int(m.Y), int(m.Width), int(m.Height)}
				out = append(out, Target{Kind: "monitor", Name: fmt.Sprintf("%s (%dx%d)", name, r.W, r.H), Source: monitorSource(r)})
			}
		}
	}
	ws := clientWindows(c, root)
	if ws == nil {
		ws = topLevel(c, root) // no EWMH window manager
	}
	for _, w := range ws {
		if hidden(c, w) {
			continue
		}
		title := windowTitle(c, w)
		if title == "" {
			continue
		}
		out = append(out, Target{Kind: "window", Name: title, Source: fmt.Sprintf("window:%#x", uint32(w))})
	}
	return out, nil
}

func atom(c *xgb.Conn, name string) xproto.Atom {
	r, err := xproto.InternAtom(c, true, uint16(len(name)), name).Reply()
	if err != nil {
		return xproto.AtomNone
	}
	return r.Atom
}

func atomName(c *xgb.Conn, a xproto.Atom) string {
	r, err := xproto.GetAtomName(c, a).Reply()
	if err != nil {
		return "monitor"
	}
	return r.Name
}

func prop(c *xgb.Conn, w xproto.Window, name string, typ xproto.Atom) *xproto.GetPropertyReply {
	a := atom(c, name)
	if a == xproto.AtomNone {
		return nil
	}
	r, err := xproto.GetProperty(c, false, w, a, typ, 0, 1<<16).Reply()
	if err != nil || r.Format == 0 {
		return nil
	}
	return r
}

func clientWindows(c *xgb.Conn, root xproto.Window) []xproto.Window {
	r := prop(c, root, "_NET_CLIENT_LIST", xproto.AtomWindow)
	if r == nil || r.Format != 32 {
		return nil
	}
	ws := make([]xproto.Window, 0, r.ValueLen)
	for i := 0; i+4 <= len(r.Value); i += 4 {
		ws = append(ws, xproto.Window(xgb.Get32(r.Value[i:])))
	}
	return ws
}

// topLevel returns the root's mapped children.
func topLevel(c *xgb.Conn, root xproto.Window) []xproto.Window {
	t, err := xproto.QueryTree(c, root).Reply()
	if err != nil {
		return nil
	}
	var ws []xproto.Window
	for _, w := range t.Children {
		if a, err := xproto.GetWindowAttributes(c, w).Reply(); err == nil && a.MapState == xproto.MapStateViewable {
			ws = append(ws, w)
		}
	}
	return ws
}

func hidden(c *xgb.Conn, w xproto.Window) bool {
	r := prop(c, w, "_NET_WM_STATE", xproto.AtomAtom)
	if r == nil || r.Format != 32 {
		return false
	}
	h := atom(c, "_NET_WM_STATE_HIDDEN")
	for i := 0; i+4 <= len(r.Value); i += 4 {
		if xproto.Atom(xgb.Get32(r.Value[i:])) == h {
			return true
		}
	}
	return false
}

func windowTitle(c *xgb.Conn, w xproto.Window) string {
	if r := prop(c, w, "_NET_WM_NAME", xproto.GetPropertyTypeAny); r != nil && len(r.Value) > 0 {
		return strings.TrimSpace(string(r.Value))
	}
	if r := prop(c, w, "WM_NAME", xproto.GetPropertyTypeAny); r != nil {
		return strings.TrimSpace(string(r.Value))
	}
	return ""
}

func windowGone(id uint64) bool {
	c, err := xgb.NewConn()
	if err != nil {
		return false
	}
	defer c.Close()
	_, err = xproto.GetWindowAttributes(c, xproto.Window(id)).Reply()
	_, bad := err.(xproto.WindowError)
	return bad
}
