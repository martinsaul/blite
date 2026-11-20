package capture

import (
	"bufio"
	"image"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Some apps never set _NET_WM_ICON on their windows (Discord, for one, or
// sets it only once it has loaded), so the picker showed a generic icon for
// them. Their .desktop file names the icon the menus show: we find it by the
// window's WM_CLASS and read it from the icon theme. PNG only; an app that
// installs only an SVG keeps the generic icon.
//
// Not WM_HINTS' icon_pixmap: only old X11 toolkits set it (Electron, GTK 3+
// and Qt set _NET_WM_ICON), and reading it means GetImage on a pixmap of the
// app's depth and visual plus its 1-bit mask, for no app we know needs it.

var (
	appIconsMu sync.Mutex
	appIndex   map[string]string          // lower-case WM_CLASS or desktop file name -> Icon=
	appIcons   = map[string]image.Image{} // by Icon= value (nil: none)
)

// appIcon is the icon of the app whose window has WM_CLASS inst, class
// (nil: no .desktop file or no PNG icon for it). Lookups are cached.
func appIcon(inst, class string) image.Image {
	appIconsMu.Lock()
	defer appIconsMu.Unlock()
	if appIndex == nil {
		appIndex = desktopIndex(dataDirs())
	}
	for _, k := range []string{inst, class} {
		name := appIndex[strings.ToLower(strings.TrimSpace(k))]
		if name == "" {
			continue
		}
		img, ok := appIcons[name]
		if !ok {
			img = loadThemeIcon(name, dataDirs())
			appIcons[name] = img
		}
		if img != nil {
			return img
		}
	}
	return nil
}

// dataDirs are the XDG data directories, most important first, plus where
// Flatpak and Snap put their apps' .desktop files and icons.
func dataDirs() []string {
	home, _ := os.UserHomeDir()
	var dirs []string
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		dirs = append(dirs, d)
	} else if home != "" {
		dirs = append(dirs, filepath.Join(home, ".local", "share"))
	}
	if home != "" {
		dirs = append(dirs, filepath.Join(home, ".local", "share", "flatpak", "exports", "share"))
	}
	sys := os.Getenv("XDG_DATA_DIRS")
	if sys == "" {
		sys = "/usr/local/share:/usr/share"
	}
	dirs = append(dirs, filepath.SplitList(sys)...)
	return append(dirs, "/var/lib/flatpak/exports/share", "/var/lib/snapd/desktop")
}

// desktopIndex maps lower-case keys to the Icon= of the .desktop files in
// dirs/applications: their StartupWMClass, their file name, and the last
// part of a reverse-DNS file name (com.discordapp.Discord: "discord").
// Earlier dirs win, and StartupWMClass wins over names.
func desktopIndex(dirs []string) map[string]string {
	byClass, byName := map[string]string{}, map[string]string{}
	for _, d := range dirs {
		root := filepath.Join(d, "applications")
		filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || !strings.HasSuffix(p, ".desktop") {
				return nil
			}
			icon, wmClass := readDesktop(p)
			if icon == "" {
				return nil
			}
			if k := strings.ToLower(wmClass); k != "" && byClass[k] == "" {
				byClass[k] = icon
			}
			name := strings.ToLower(strings.TrimSuffix(e.Name(), ".desktop"))
			for _, k := range []string{name, name[strings.LastIndexByte(name, '.')+1:]} {
				if byName[k] == "" {
					byName[k] = icon
				}
			}
			return nil
		})
	}
	for k, v := range byClass {
		byName[k] = v
	}
	return byName
}

// readDesktop reads Icon= and StartupWMClass= of a .desktop file's
// [Desktop Entry] group.
func readDesktop(p string) (icon, wmClass string) {
	f, err := os.Open(p)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	in := false
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(l, "[") {
			if in {
				break
			}
			in = l == "[Desktop Entry]"
			continue
		}
		if !in {
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Icon":
			icon = strings.TrimSpace(v)
		case "StartupWMClass":
			wmClass = strings.TrimSpace(v)
		}
	}
	return icon, wmClass
}

// iconSizes are the hicolor sizes we look in, best first: enough pixels for
// the picker's 20 points on a HiDPI screen, little to decode.
var iconSizes = []string{"48x48", "64x64", "96x96", "128x128", "256x256", "32x32", "512x512", "24x24"}

// loadThemeIcon reads icon name (an Icon= value: a theme name, or an
// absolute path) as a PNG from the hicolor theme or pixmaps under dirs.
func loadThemeIcon(name string, dirs []string) image.Image {
	if filepath.IsAbs(name) {
		return loadPNG(name)
	}
	if strings.ContainsAny(name, `/\`) {
		return nil
	}
	name = strings.TrimSuffix(name, ".png")
	var paths []string
	for _, size := range iconSizes {
		for _, d := range dirs {
			paths = append(paths, filepath.Join(d, "icons", "hicolor", size, "apps", name+".png"))
		}
	}
	for _, d := range dirs {
		paths = append(paths, filepath.Join(d, "pixmaps", name+".png"))
	}
	for _, p := range paths {
		if img := loadPNG(p); img != nil {
			return img
		}
	}
	return nil
}

// maxIconFile bounds the icon files we decode.
const maxIconFile = 4 << 20

func loadPNG(p string) image.Image {
	if !strings.EqualFold(filepath.Ext(p), ".png") {
		return nil
	}
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.Size() > maxIconFile {
		return nil
	}
	if cfg, err := png.DecodeConfig(f); err != nil || cfg.Width > 1024 || cfg.Height > 1024 {
		return nil
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil
	}
	img, err := png.Decode(f)
	if err != nil {
		return nil
	}
	return img
}
