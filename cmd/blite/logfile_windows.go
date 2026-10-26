package main

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Next to blite-guard's own segments, which it writes when it runs Blite.
// Asked of Windows, not the LOCALAPPDATA variable (testers had no logs and
// the variable was the one thing both writers shared).
func logDir() (string, error) {
	d, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil || d == "" {
		if d = os.Getenv("LOCALAPPDATA"); d == "" {
			return "", errors.New("can't find the local AppData folder")
		}
	}
	return filepath.Join(d, "Blite", "logs"), nil
}

// keepStderr: under blite-guard, which keeps our output (and dumps crashes).
// Unguarded, nothing did, so a crash left no trace.
func keepStderr() bool { return os.Getenv("BLITE_GUARD") != "" }

// redirectStderr points the process's standard error, which the Go runtime
// writes a crash to, and os.Stderr at f. (The C runtime's stderr, used by
// GStreamer, keeps the handle it had at start.)
func redirectStderr(f *os.File) error {
	if err := windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(f.Fd())); err != nil {
		return err
	}
	os.Stderr = f
	return nil
}
