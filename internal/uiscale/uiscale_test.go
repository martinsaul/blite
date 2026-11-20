package uiscale

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// fake is a desktop: p from the cheap probe, gs from gsettings (counted).
type fake struct {
	p       Probe
	gs      Probe // CinnamonScale/CinnamonText gsettings would give
	gsCalls int
}

func (f *fake) sources() sources {
	return sources{
		probe: func(func(string) string) Probe { return f.p },
		desktop: func(p *Probe) {
			f.gsCalls++
			p.CinnamonScale, p.CinnamonText = f.gs.CinnamonScale, f.gs.CinnamonText
		},
	}
}

func x11(p Probe) Probe { p.Session = "x11"; return p }

// TestDecide walks the priority order: user FYNE_SCALE, then (X11 only)
// the setting, GDK_SCALE x GDK_DPI_SCALE, Xft.dpi, Cinnamon gsettings,
// Fyne's own detection.
func TestDecide(t *testing.T) {
	cinnamon150 := Probe{CinnamonScale: 1, CinnamonText: 1.5}
	for _, c := range []struct {
		name   string
		pref   string
		env    map[string]string
		probe  Probe
		gs     Probe
		want   float64
		source string // prefix
		gs1    bool   // gsettings must run (else it mustn't)
	}{
		{"user FYNE_SCALE wins", "200", map[string]string{"FYNE_SCALE": "1.5"}, x11(Probe{XftDPI: 192}), cinnamon150, 0, "FYNE_SCALE=1.5", false},
		{"wayland: leave to Fyne", "150", nil, Probe{Session: "wayland", XftDPI: 144}, cinnamon150, 0, "left to Fyne (wayland)", false},
		{"windows/macOS: leave to Fyne", "150", nil, Probe{Session: "os"}, cinnamon150, 0, "left to Fyne (os)", false},
		{"setting beats desktop", "120", nil, x11(Probe{GDKScale: "2", XftDPI: 192}), cinnamon150, 1.2, "setting 120", false},
		{"setting rounds as Fyne does", "125", nil, x11(Probe{XftDPI: 192}), cinnamon150, 1.3, "setting 125", false},
		{"GDK_SCALE x GDK_DPI_SCALE before Xft.dpi", "", nil, x11(Probe{GDKScale: "2", GDKDPIScale: "0.75", XftDPI: 96}), cinnamon150, 1.5, "GDK_SCALE=2", false},
		{"GDK_SCALE alone", "", nil, x11(Probe{GDKScale: "2"}), cinnamon150, 2, "GDK_SCALE=2", false},
		{"bad GDK_SCALE: Xft.dpi", "", nil, x11(Probe{GDKScale: "x", XftDPI: 144}), cinnamon150, 1.5, "Xft.dpi=144", false},
		{"Xft.dpi 144 (150%)", "", nil, x11(Probe{XftDPI: 144}), cinnamon150, 1.5, "Xft.dpi=144", false},
		{"Xft.dpi 192 (200%, or Cinnamon fractional 150% with xrandr downscale)", "", nil, x11(Probe{XftDPI: 192, XrandrScale: 4.0 / 3}), cinnamon150, 2, "Xft.dpi=192", false},
		{"Xft.dpi 120 rounds to 1.3", "", nil, x11(Probe{XftDPI: 120}), cinnamon150, 1.3, "Xft.dpi=120", false},
		{"Xft.dpi 96 is 1", "", nil, x11(Probe{XftDPI: 96}), cinnamon150, 1, "Xft.dpi=96", false},
		{"cinnamon gsettings fallback", "", nil, x11(Probe{Desktop: "X-Cinnamon"}), cinnamon150, 1.5, "cinnamon scaling-factor=1", true},
		{"cinnamon scaling-factor 2", "", nil, x11(Probe{Desktop: "X-Cinnamon"}), Probe{CinnamonScale: 2, CinnamonText: 1}, 2, "cinnamon scaling-factor=2", true},
		{"cinnamon auto (0) x text 1.25", "", nil, x11(Probe{Desktop: "X-Cinnamon"}), Probe{CinnamonScale: 0, CinnamonText: 1.25}, 1.3, "cinnamon scaling-factor=0", true},
		{"cinnamon auto x text 1: leave to Fyne", "", nil, x11(Probe{Desktop: "X-Cinnamon"}), Probe{CinnamonScale: 0, CinnamonText: 1}, 0, "left to Fyne (no desktop scale", true},
		{"nothing: leave to Fyne", "", nil, x11(Probe{}), Probe{}, 0, "left to Fyne (no desktop scale", true},
		{"no session info: X11 rules", "", nil, Probe{XftDPI: 144}, Probe{}, 1.5, "Xft.dpi=144", false},
	} {
		f := &fake{p: c.probe, gs: c.gs}
		r := decide(c.pref, env(c.env), f.sources())
		if r.Scale != c.want {
			t.Errorf("%s: scale %v, want %v", c.name, r.Scale, c.want)
		}
		if !strings.HasPrefix(r.Source, c.source) {
			t.Errorf("%s: source %q, want %q...", c.name, r.Source, c.source)
		}
		if got := f.gsCalls > 0; got != c.gs1 {
			t.Errorf("%s: gsettings ran %d times", c.name, f.gsCalls)
		}
	}
}

func TestParse(t *testing.T) {
	for in, want := range map[string]float64{"": 0, "auto": 0, "150": 1.5, "150%": 1.5, "1.75": 1.8, "125": 1.3, "50": 1, "900": 4, "x": 0, "-2": 0, "NaN": 0, "Inf": 0} {
		if got := Parse(in); got != want {
			t.Errorf("Parse(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestPrefsRoundCleanly: every offered size is drawn exactly as labelled.
func TestPrefsRoundCleanly(t *testing.T) {
	for _, p := range Prefs[1:] {
		if got := Label(p); got != p+"%" {
			t.Errorf("Label(%q) = %q, want %s%%", p, got, p)
		}
	}
	if Label("") != "" || Label("125") != "130%" {
		t.Errorf("Label: %q %q", Label(""), Label("125"))
	}
	if !Same("125", "130") || Same("", "100") || !Same("", "auto") {
		t.Error("Same")
	}
}
