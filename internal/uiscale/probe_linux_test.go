package uiscale

import "testing"

func TestParseXftDPI(t *testing.T) {
	db := "Xcursor.size:\t48\nXft.antialias:\t1\nXft.dpi:\t144\nXft.hinting:\t1\n"
	if got := parseXftDPI(db); got != 144 {
		t.Fatalf("parseXftDPI = %v, want 144", got)
	}
	if got := parseXftDPI("Xcursor.size: 24\n"); got != 0 {
		t.Fatalf("no Xft.dpi: %v", got)
	}
}

func TestFyneDetected(t *testing.T) {
	// 27" 5K (597 mm): 218 dpi -> 1.8; no physical size: GLFW assumes 96 dpi -> 1.0.
	if got := fyneDetected(597, 5120); got != 1.8 {
		t.Errorf("5K 27in: %v", got)
	}
	if got := fyneDetected(0, 5120); got != 1 {
		t.Errorf("0 mm: %v", got)
	}
}

func TestParseGUint(t *testing.T) {
	for in, want := range map[string]int{"uint32 2": 2, "2": 2, "uint32 0": 0, "": 0, "x": 0} {
		if got := parseGUint(in); got != want {
			t.Errorf("parseGUint(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestMonitorDesc(t *testing.T) {
	// Cinnamon 150% fractional on a 1080p panel: laid out at 2x, shrunk by xrandr.
	s, xr := monitorDesc("eDP-1", 2560, 1440, 0, 0, 344, 194, 1920, 1080)
	if s != "eDP-1 2560x1440+0+0 344x194mm mode 1920x1080 xrandr-scale 1.33" || xr < 1.33 || xr > 1.34 {
		t.Errorf("fractional: %q %v", s, xr)
	}
	if s, xr := monitorDesc("HDMI-1", 1920, 1080, 0, 0, 527, 296, 1920, 1080); s != "HDMI-1 1920x1080+0+0 527x296mm" || xr != 1 {
		t.Errorf("plain: %q %v", s, xr)
	}
}

func TestSession(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"XDG_SESSION_TYPE": "x11", "DISPLAY": ":0"}, "x11"},
		{map[string]string{"XDG_SESSION_TYPE": "wayland", "WAYLAND_DISPLAY": "wayland-0", "DISPLAY": ":0"}, "wayland"},
		{map[string]string{"XDG_SESSION_TYPE": "wayland", "DISPLAY": ":0"}, "x11"}, // GLFW falls back to X11
		{map[string]string{"XDG_SESSION_TYPE": "wayland", "WAYLAND_DISPLAY": "wayland-0", "DISPLAY": ":0", "FYNE_PLATFORM": "x11"}, "x11"},
		{map[string]string{"XDG_SESSION_TYPE": "x11", "DISPLAY": ":0", "FYNE_PLATFORM": "wayland"}, "x11"}, // no Wayland display
	} {
		if got := session(env(c.env)); got != c.want {
			t.Errorf("session(%v) = %q, want %q", c.env, got, c.want)
		}
	}
}
