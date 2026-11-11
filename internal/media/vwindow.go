package media

// Window is a native window a grid draws into. The session
// owns it, so it outlives grid restarts: a layout switch rebuilds the
// pipelines and keeps the windows.
type Window interface {
	Handle() uintptr
	// Closed is closed when the user closes the window (it hides at once).
	Closed() <-chan struct{}
	SetTitle(title string)
	// PointerInside reports whether the pointer is over the window.
	PointerInside() bool
	// Minimized reports whether the window is minimized or hidden.
	Minimized() bool
	Destroy()
}

// OpenVideoWindow opens a resizable window of w x h pixels (client area)
// titled title, without taking the focus. Not every platform has one
// (Wayland): grids then let their sink open its own window.
func OpenVideoWindow(title string, w, h int) (Window, error) { return openVideoWindow(title, w, h) }

// VideoWindowSinks are the sinks that can draw into a Window, best first.
func VideoWindowSinks() []string { return previewSinks }
