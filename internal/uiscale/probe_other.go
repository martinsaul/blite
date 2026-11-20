//go:build !linux

package uiscale

// Windows and macOS scale through the OS (Fyne reads the per-monitor DPI
// on Windows and the backing scale on macOS): nothing to probe, and the
// manual setting is X11-only.
var sys = sources{
	probe:   func(func(string) string) Probe { return Probe{Session: "os"} },
	desktop: func(*Probe) {},
}

func session(func(string) string) string { return "os" }
