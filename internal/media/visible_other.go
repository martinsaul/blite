//go:build !windows

package media

// VideoWindowsMinimized reports whether every video window is minimized;
// only known on Windows so far.
func VideoWindowsMinimized() bool { return false }
