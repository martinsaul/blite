package register

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Launcher is what the OS should start for Blite.
func Launcher(exe string) string { return exe }

func autostartFile() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "autostart", "blite.desktop"), nil
}

// SetAutostart starts Blite in the background at login (XDG autostart), or
// stops doing so.
func SetAutostart(on bool, exe string) error {
	p, err := autostartFile()
	if err != nil {
		return err
	}
	if !on {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	desktop := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=Blite
Comment=Peer-to-peer screen sharing (runs in the tray)
Exec="%s" -background
Icon=video-display
Terminal=false
X-GNOME-Autostart-enabled=true
`, Launcher(exe))
	return os.WriteFile(p, []byte(desktop), 0o644)
}

// AutostartEnabled reports whether Blite starts at login.
func AutostartEnabled() bool {
	p, err := autostartFile()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}
