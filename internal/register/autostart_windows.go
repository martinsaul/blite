package register

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// Launcher is what the OS should start for Blite: the crash guard
// (<root>\blite-guard.exe) when it sits next to bin\, else exe.
func Launcher(exe string) string {
	g := filepath.Join(filepath.Dir(filepath.Dir(exe)), "blite-guard.exe")
	if _, err := os.Stat(g); err == nil {
		return g
	}
	return exe
}

// SetAutostart starts Blite in the background at sign-in (HKCU Run; no
// admin needed), or stops doing so.
func SetAutostart(on bool, exe string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		if err := k.DeleteValue("Blite"); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}
	return k.SetStringValue("Blite", `"`+Launcher(exe)+`" -background`)
}

// AutostartEnabled reports whether Blite starts at sign-in.
func AutostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue("Blite")
	return err == nil
}
