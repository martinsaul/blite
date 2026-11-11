package media

import (
	"os"
	"testing"
	"time"
)

// A manual check of the native preview window alone (no video): run with
// BLITE_PREVIEW_WINDOW_TEST=<seconds> and inspect the window meanwhile.
func TestPreviewWindowManual(t *testing.T) {
	d, err := time.ParseDuration(os.Getenv("BLITE_PREVIEW_WINDOW_TEST"))
	if err != nil {
		t.Skip("set BLITE_PREVIEW_WINDOW_TEST to a duration to run")
	}
	w, err := openPreviewWindow()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.Closed():
		t.Log("closed by the user")
	case <-time.After(d):
	}
	w.Destroy()
}
