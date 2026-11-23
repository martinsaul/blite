package nui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver"
)

// winGeom is where the main window was and how big, in screen pixels, so it
// comes back there after a restart (an update restarts the app). Fyne has
// no window position, so the OS's own calls do it (wingeom_*.go).
type winGeom struct {
	X, Y, W, H int
	Maximized  bool `json:",omitempty"`
}

func (g winGeom) valid() bool { return g.W >= 200 && g.H >= 150 }

func winGeomFile() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "blite", "window.json"), nil
}

func loadWinGeom() (winGeom, bool) {
	var g winGeom
	p, err := winGeomFile()
	if err != nil {
		return g, false
	}
	b, err := os.ReadFile(p)
	if err != nil || json.Unmarshal(b, &g) != nil {
		return g, false
	}
	return g, g.valid()
}

func saveWinGeom(g winGeom) {
	p, err := winGeomFile()
	if err != nil {
		return
	}
	b, _ := json.Marshal(g)
	os.MkdirAll(filepath.Dir(p), 0o700)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, p)
	}
}

// nativeGeom runs f with the window's native handle (0: none, e.g. Wayland
// or not yet shown). On the UI goroutine.
func nativeGeom(w fyne.Window, f func(handle uintptr)) {
	nw, ok := w.(driver.NativeWindow)
	if !ok {
		return
	}
	nw.RunNative(func(ctx any) {
		switch c := ctx.(type) {
		case driver.WindowsWindowContext:
			f(c.HWND)
		case driver.X11WindowContext:
			f(c.WindowHandle)
		}
	})
}

// geomEvery is how often we note where the window is.
const geomEvery = 2 * time.Second

// keepWinGeom puts the window back where it was and then notes where it is
// while it shows (a restart can come any time: an update, a crash).
func (u *UI) keepWinGeom() {
	var last winGeom
	go func() {
		// After the driver has made and placed the window.
		time.Sleep(300 * time.Millisecond)
		if g, ok := loadWinGeom(); ok {
			u.later(func() {
				last = g // last: only on the UI goroutine
				nativeGeom(u.win, func(h uintptr) { setWinGeom(h, g) })
			})
		}
		for range time.Tick(geomEvery) {
			if u.closed.Load() {
				return
			}
			u.later(func() {
				nativeGeom(u.win, func(h uintptr) {
					if g, ok := getWinGeom(h); ok && g.valid() && g != last {
						last = g
						go saveWinGeom(g)
					}
				})
			})
		}
	}()
}
