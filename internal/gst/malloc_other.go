//go:build !linux

package gst

func startTrimmer() {}

// MallocStats is unknown here.
func MallocStats() (inuse, free int64) { return -1, -1 }

func setX11ErrorHandler() {}
