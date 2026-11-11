package media

import (
	"os"
	"testing"
	"time"
	"unsafe"
)

// Logs the preview window's client size every 500 ms while
// TestPreviewWindowManual-like checks run (BLITE_PREVIEW_WINDOW_TEST).
func TestPreviewWindowSizeLog(t *testing.T) {
	d, err := time.ParseDuration(os.Getenv("BLITE_PREVIEW_WINDOW_TEST"))
	if err != nil {
		t.Skip("set BLITE_PREVIEW_WINDOW_TEST to a duration to run")
	}
	w, err := openPreviewWindow()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Destroy()
	getClient := user32.NewProc("GetClientRect")
	getWindow := user32.NewProc("GetWindowRect")
	end := time.After(d)
	for {
		select {
		case <-end:
			return
		case <-w.Closed():
			t.Log("closed by the user")
			return
		case <-time.After(500 * time.Millisecond):
		}
		var c, r winRect
		getClient.Call(w.Handle(), uintptr(unsafe.Pointer(&c)))
		getWindow.Call(w.Handle(), uintptr(unsafe.Pointer(&r)))
		t.Logf("client %dx%d window %d,%d-%d,%d", c.Right, c.Bottom, r.Left, r.Top, r.Right, r.Bottom)
	}
}
