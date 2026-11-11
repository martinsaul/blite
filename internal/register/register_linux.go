package register

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Register installs a per-user .desktop entry for our schemes and makes it
// the default handler (xdg-mime). exe is the absolute path of the blite binary.
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
Name=Bellion Share
Comment=Peer-to-peer screen sharing
Exec="%s" %%u
Terminal=false
Categories=Network;
MimeType=%s
`, exe, mimeTypes())
	if err := os.WriteFile(filepath.Join(dir, "blite.desktop"), []byte(desktop), 0o644); err != nil {
		return err
	}
	for _, sch := range Schemes {
		if out, err := exec.Command("xdg-mime", "default", "blite.desktop", "x-scheme-handler/"+sch).CombinedOutput(); err != nil {
			return fmt.Errorf("xdg-mime: %v: %s", err, out)
		}
	}
	exec.Command("update-desktop-database", dir).Run() // best effort
	return nil
}

func mimeTypes() string {
	var b strings.Builder
	for _, s := range Schemes {
		b.WriteString("x-scheme-handler/" + s + ";")
	}
	return b.String()
}
