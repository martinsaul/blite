package register

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Register installs a per-user .desktop entry for blite:// and makes it the
// default handler (xdg-mime). exe is the absolute path of the blite binary.
func Register(exe string) error {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	dir := filepath.Join(dataHome, "applications")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	desktop := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=Blite
Comment=Peer-to-peer screen sharing
Exec="%s" %%u
Terminal=false
Categories=Network;
MimeType=x-scheme-handler/%s;
`, exe, Scheme)
	if err := os.WriteFile(filepath.Join(dir, "blite.desktop"), []byte(desktop), 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("xdg-mime", "default", "blite.desktop", "x-scheme-handler/"+Scheme).CombinedOutput(); err != nil {
		return fmt.Errorf("xdg-mime: %v: %s", err, out)
	}
	exec.Command("update-desktop-database", dir).Run() // best effort
	return nil
}
