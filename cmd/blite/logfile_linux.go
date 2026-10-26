package main

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func logDir() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "blite", "logs"), nil
}

// keepStderr: a terminal keeps its stderr.
func keepStderr() bool {
	_, err := unix.IoctlGetTermios(2, unix.TCGETS)
	return err == nil
}

// redirectStderr makes f fd 2, which C code (GStreamer) writes to as well.
func redirectStderr(f *os.File) error { return unix.Dup3(int(f.Fd()), 2, 0) }
