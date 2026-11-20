package media

import (
	"os"
	"strings"
)

// Window is a native window a grid draws into. The session
// owns it, so it outlives grid restarts: tile changes rebuild the
// pipelines and keep the windows.
type Window interface {
	Handle() uintptr
	// Closed is closed when the user closes a non-intercepted window (it hides at once).
	Closed() <-chan struct{}
	// SetInterceptClose asks the window not to hide or close when the user
	// clicks its close button, signaling CloseRequested instead.
	SetInterceptClose(on bool)
	// CloseRequested receives a value when the user tries to close an
	// intercepted window.
	CloseRequested() <-chan struct{}
	SetTitle(title string)
	// PointerInside reports whether the pointer is over the window.
	PointerInside() bool
	// Minimized reports whether the window is minimized or hidden.
	Minimized() bool
	Destroy()
}

// buttonWindow is a Window that hands its pointer button events (x, y in
// the window's pixels; button 0: a move with a button held) to the grid drawing into it, for sinks that don't
// take them themselves (X11; see x11Video.button).
type buttonWindow interface {
	SetButtonHandler(f func(press bool, button int, x, y float64)) (remove func())
}

// OpenVideoWindow opens a resizable window of w x h pixels (client area)
// titled title, without taking the focus. Not every platform has one
// (Wayland): grids then let their sink open its own window.
func OpenVideoWindow(title string, w, h int) (Window, error) { return openVideoWindow(title, w, h) }

// VideoWindowSinks are the sinks that can draw into a Window, best first.
// BLITE_VIDEO_SINKS (space separated) replaces them, to try another sink
// on a machine where the usual one misbehaves, or for screenshots under
// Xvfb (ximagesink: glimagesink's picture doesn't show in them there).
func VideoWindowSinks() []string {
	if v := strings.Fields(os.Getenv("BLITE_VIDEO_SINKS")); len(v) > 0 {
		return v
	}
	return videoWindowSinks
}
