// Package capture lists what can be shared: monitors and application windows.
// A Target's Source string is what media.SourceKind understands.
package capture

import (
	"fmt"
	"strconv"
	"strings"
)

// Target is one shareable monitor or window.
type Target struct {
	Kind   string `json:"kind"`   // monitor | window
	Name   string `json:"name"`   // monitor name and size, or window title
	Source string `json:"source"` // media source kind, e.g. "window:0x3a00007"
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
