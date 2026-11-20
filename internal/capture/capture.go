// Package capture lists what can be shared: monitors and application windows.
// A Target's Source string is what media.SourceKind understands.
package capture

import (
	"fmt"
	"image"
	"strconv"
	"strings"
)

// Target is one shareable monitor or window.
type Target struct {
	Kind   string `json:"kind"`   // monitor | window
	Name   string `json:"name"`   // monitor name and size, or window title
	Source string `json:"source"` // media source kind, e.g. "window:0x3a00007"
	// App is the program that owns a window (exe name, or X11 class).
	App string `json:"app,omitempty"`
	// Primary marks the primary monitor (the one with the taskbar or panel).
	Primary bool `json:"primary,omitempty"`
}

// Label is what to call the target for people: its name, else its app.
// Never the handle in Source.
func (t Target) Label() string {
	if n := strings.TrimSpace(t.Name); n != "" {
		return n
	}
	return strings.TrimSpace(t.App)
}

// shellWindowClass reports whether a Windows window class is the desktop
// (the icons layer) rather than an app: "Program Manager" (Progman) passes
// every other test of a window Alt-Tab lists. WorkerW (the wallpaper) has
// no title, so it never got listed.
func shellWindowClass(class string) bool {
	return strings.EqualFold(class, "Progman") || strings.EqualFold(class, "WorkerW")
}

// SourceName is the label of the monitor or window a source string shares,
// or "" when it can't tell (the window is gone, or another kind of source).
func SourceName(source string) string {
	ts, _ := List()
	for _, t := range ts {
		if t.Source == source {
			return t.Label()
		}
	}
	if arg, ok := strings.CutPrefix(source, "window:"); ok { // untitled, so not listed
		if id, err := ParseWindow(arg); err == nil {
			return windowApp(id)
		}
	}
	return ""
}

// Icons returns the app icons of the windows among ts, by Source; windows
// without one (and monitors) are left out. Icons are cached, so asking again
// is cheap.
func Icons(ts []Target) map[string]image.Image {
	var ws []Target
	for _, t := range ts {
		if t.Kind == "window" {
			ws = append(ws, t)
		}
	}
	if len(ws) == 0 {
		return map[string]image.Image{}
	}
	return icons(ws)
}

// PrimarySource is the source for the primary monitor, or "screen" (all of
// them) when there is only one monitor or it can't tell.
func PrimarySource() string {
	ts, err := List()
	if err != nil {
		return "screen"
	}
	var mons []Target
	for _, t := range ts {
		if t.Kind == "monitor" {
			mons = append(mons, t)
		}
	}
	if len(mons) <= 1 {
		return "screen"
	}
	for _, m := range mons {
		if m.Primary {
			return m.Source
		}
	}
	return mons[0].Source
}

// Region is a monitor's rectangle on the virtual screen.
type Region struct{ X, Y, W, H int }

func monitorSource(r Region) string { return fmt.Sprintf("monitor:%d,%d,%d,%d", r.X, r.Y, r.W, r.H) }

// ParseMonitor parses the argument of an X11 "monitor:" source (x,y,w,h).
func ParseMonitor(arg string) (Region, error) {
	f := strings.Split(arg, ",")
	if len(f) != 4 {
		return Region{}, fmt.Errorf("bad monitor %q", arg)
	}
	var v [4]int
	for i, s := range f {
		n, err := strconv.Atoi(s)
		if err != nil || n < -65536 || n > 65536 || (i >= 2 && n <= 0) {
			return Region{}, fmt.Errorf("bad monitor %q", arg)
		}
		v[i] = n
	}
	return Region{v[0], v[1], v[2], v[3]}, nil
}

// ParseWindow parses the argument of a "window:" source (an X11 window id or a
// Windows HWND, decimal or 0x hex); Windows "monitor:" sources (HMONITOR) too.
func ParseWindow(arg string) (uint64, error) {
	id, err := strconv.ParseUint(arg, 0, 64)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("bad window %q", arg)
	}
	return id, nil
}

// WindowPID returns the process that owns a "window:" source's window, or 0
// (other sources, or it can't tell).
func WindowPID(source string) int {
	arg, ok := strings.CutPrefix(source, "window:")
	if !ok {
		return 0
	}
	id, err := ParseWindow(arg)
	if err != nil {
		return 0
	}
	return windowPID(id)
}

// WindowGone reports whether a "window:" source's window no longer exists.
// Other sources, and errors asking, report false.
func WindowGone(source string) bool {
	arg, ok := strings.CutPrefix(source, "window:")
	if !ok {
		return false
	}
	id, err := ParseWindow(arg)
	if err != nil {
		return false
	}
	return windowGone(id)
}
