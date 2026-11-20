package capture

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func writePNG(t *testing.T, p string, side int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	img := image.NewNRGBA(image.Rect(0, 0, side, side))
	img.Set(0, 0, color.NRGBA{255, 0, 0, 255})
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A window without _NET_WM_ICON gets the icon its app's .desktop file names,
// found by WM_CLASS: StartupWMClass, the file name, or a reverse-DNS name's
// last part (Flatpak).
func TestAppIconFromDesktopFile(t *testing.T) {
	user, sys := t.TempDir(), t.TempDir()
	dirs := []string{user, sys}
	// Discord's .deb: Discord.desktop, Icon=discord, a pixmap.
	writeFile(t, filepath.Join(sys, "applications", "discord.desktop"),
		"[Desktop Entry]\nName=Discord\nIcon=discord\nStartupWMClass=discord\n[Desktop Action New]\nIcon=other\n")
	writePNG(t, filepath.Join(sys, "pixmaps", "discord.png"), 256)
	// A Flatpak: no StartupWMClass, reverse-DNS name, hicolor icons.
	writeFile(t, filepath.Join(user, "applications", "org.example.Notes.desktop"), "[Desktop Entry]\nIcon=org.example.Notes\n")
	writePNG(t, filepath.Join(user, "icons", "hicolor", "256x256", "apps", "org.example.Notes.png"), 256)
	writePNG(t, filepath.Join(sys, "icons", "hicolor", "48x48", "apps", "org.example.Notes.png"), 48)
	// An absolute Icon= path (Snap).
	writePNG(t, filepath.Join(sys, "snap", "icon.png"), 32)
	writeFile(t, filepath.Join(sys, "applications", "snapapp_snapapp.desktop"),
		"[Desktop Entry]\nIcon="+filepath.Join(sys, "snap", "icon.png")+"\nStartupWMClass=SnapApp\n")
	// SVG only: no icon.
	writeFile(t, filepath.Join(sys, "applications", "vector.desktop"), "[Desktop Entry]\nIcon=vector\n")
	writeFile(t, filepath.Join(sys, "icons", "hicolor", "scalable", "apps", "vector.svg"), "<svg/>")

	idx := desktopIndex(dirs)
	for _, c := range []struct {
		key, want string
	}{
		{"discord", "discord"},
		{"notes", "org.example.Notes"},
		{"org.example.notes", "org.example.Notes"},
		{"snapapp", filepath.Join(sys, "snap", "icon.png")},
	} {
		if got := idx[c.key]; got != c.want {
			t.Errorf("index[%q] = %q, want %q", c.key, got, c.want)
		}
	}
	for _, c := range []struct {
		icon string
		side int
	}{
		{"discord", 256},
		{"org.example.Notes", 48}, // 48 px is preferred over 256
		{filepath.Join(sys, "snap", "icon.png"), 32},
		{"vector", 0},
		{"../etc/passwd", 0},
	} {
		img := loadThemeIcon(c.icon, dirs)
		switch {
		case c.side == 0 && img != nil:
			t.Errorf("%q: got an icon, want none", c.icon)
		case c.side != 0 && (img == nil || img.Bounds().Dx() != c.side):
			t.Errorf("%q: got %v, want %d px", c.icon, img, c.side)
		}
	}
}

func TestShellClass(t *testing.T) {
	for _, c := range []string{"nemo-desktop", "Nemo-desktop", "desktop_window", "xfdesktop"} {
		if !shellClass(c) {
			t.Errorf("%q is the desktop", c)
		}
	}
	for _, c := range []string{"discord", "Nemo", "Desktop", ""} {
		if shellClass(c) {
			t.Errorf("%q is an app", c)
		}
	}
}
