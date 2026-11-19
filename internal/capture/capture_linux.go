package capture

import (
	"fmt"
	"image"
	"io"
	"log"
	"strings"
	"sync"
	"time"

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
				n := fmt.Sprintf("%s (%dx%d)", name, r.W, r.H)
				if m.Primary {
					n += ", primary"
				}
				out = append(out, Target{Kind: "monitor", Name: n, Source: monitorSource(r), Primary: m.Primary})
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
		out = append(out, Target{Kind: "window", Name: title, Source: fmt.Sprintf("window:%#x", uint32(w)), App: windowClass(c, w)})
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

// windowClass is the class half of WM_CLASS ("Firefox"), else its instance.
func windowClass(c *xgb.Conn, w xproto.Window) string {
	r := prop(c, w, "WM_CLASS", xproto.AtomString)
	if r == nil {
		return ""
	}
	parts := strings.Split(strings.TrimRight(string(r.Value), "\x00"), "\x00")
	for i := len(parts) - 1; i >= 0; i-- {
		if p := strings.TrimSpace(parts[i]); p != "" {
			return p
		}
	}
	return ""
}

func windowApp(id uint64) string {
	c, err := xgb.NewConn()
	if err != nil {
		return ""
	}
	defer c.Close()
	return windowClass(c, xproto.Window(id))
}

type cachedIcon struct {
	img image.Image // nil: none (yet; apps often set it after mapping)
	at  time.Time
}

// noIconRetry is how long "no icon" is believed.
const noIconRetry = 10 * time.Second

var (
	iconMu    sync.Mutex
	iconCache = map[uint64]cachedIcon{} // by window id
)

func icons(ts []Target) map[string]image.Image {
	out := map[string]image.Image{}
	keep := map[uint64]bool{}
	defer func() { // forget windows that closed (ids get reused)
		iconMu.Lock()
		for id := range iconCache {
			if !keep[id] {
				delete(iconCache, id)
			}
		}
		iconMu.Unlock()
	}()
	var c *xgb.Conn
	for _, t := range ts {
		id, err := ParseWindow(strings.TrimPrefix(t.Source, "window:"))
		if err != nil {
			continue
		}
		keep[id] = true
		iconMu.Lock()
		ci, ok := iconCache[id]
		iconMu.Unlock()
		img := ci.img
		if !ok || img == nil && time.Since(ci.at) > noIconRetry {
			if c == nil {
				if c, err = xgb.NewConn(); err != nil {
					return out
				}
				defer c.Close()
			}
			img = netWMIcon(c, xproto.Window(id))
			iconMu.Lock()
			iconCache[id] = cachedIcon{img, time.Now()}
			iconMu.Unlock()
		}
		if img != nil {
			out[t.Source] = img
		}
	}
	return out
}

// netWMIcon decodes _NET_WM_ICON (width, height, then ARGB pixels, for each
// size), picking the smallest size of at least 32 pixels, else the largest.
func netWMIcon(c *xgb.Conn, w xproto.Window) image.Image {
	a := atom(c, "_NET_WM_ICON")
	if a == xproto.AtomNone {
		return nil
	}
	// Ask for its size first: some apps set several big icons (megabytes).
	r, err := xproto.GetProperty(c, false, w, a, xproto.AtomCardinal, 0, 0).Reply()
	if err != nil || r.Format != 32 || r.BytesAfter == 0 {
		return nil
	}
	n := min((r.BytesAfter+3)/4, maxNetWMIcon)
	if r, err = xproto.GetProperty(c, false, w, a, xproto.AtomCardinal, 0, n).Reply(); err != nil || r.Format != 32 {
		return nil
	}
	return decodeNetWMIcon(r.Value)
}

// maxNetWMIcon bounds what we read of _NET_WM_ICON, in 32-bit values (16 MB);
// icons cut off past it are skipped.
const maxNetWMIcon = 1 << 22

func decodeNetWMIcon(v []byte) image.Image {
	n := len(v) / 4
	best, bestW, bestH := -1, 0, 0
	for i := 0; i+2 <= n; {
		w, h := int(xgb.Get32(v[4*i:])), int(xgb.Get32(v[4*i+4:]))
		if w <= 0 || h <= 0 || w > 1024 || h > 1024 || i+2+w*h > n {
			break // bad, or cut off: pick from the ones before
		}
		better := best < 0 ||
			(bestW < 32 && w > bestW) || // too small so far: anything bigger
			(w >= 32 && w < bestW) // big enough: the smaller the better
		if better {
			best, bestW, bestH = i+2, w, h
		}
		i += 2 + w*h
	}
	if best < 0 {
		return nil
	}
	img := image.NewNRGBA(image.Rect(0, 0, bestW, bestH))
	for p := 0; p < bestW*bestH; p++ {
		argb := xgb.Get32(v[4*(best+p):])
		img.Pix[4*p] = byte(argb >> 16)
		img.Pix[4*p+1] = byte(argb >> 8)
		img.Pix[4*p+2] = byte(argb)
		img.Pix[4*p+3] = byte(argb >> 24)
	}
	return img
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

// windowPID reads _NET_WM_PID, which most toolkits set.
func windowPID(id uint64) int {
	c, err := xgb.NewConn()
	if err != nil {
		return 0
	}
	defer c.Close()
	r := prop(c, xproto.Window(id), "_NET_WM_PID", xproto.AtomCardinal)
	if r == nil || r.Format != 32 || len(r.Value) < 4 {
		return 0
	}
	return int(xgb.Get32(r.Value))
}
