// Package uiscale picks the window's UI scale before Fyne starts.
//
// Fyne 2.8 on X11 ignores the desktop's scaling setting: its scale is
// FYNE_SCALE (or the "scale" in Fyne's own settings.json) times the DPI it
// derives from the RandR output's physical size, over a 120 dpi baseline.
// Xft.dpi, GDK_SCALE and Cinnamon's/GNOME's scaling are never read, so a
// monitor that reports no (or a bogus) physical size gets scale 1.0 however
// the desktop is scaled. Apply works the desktop scale out and hands it to
// Fyne through FYNE_SCALE, with Fyne's own DPI detection switched off so
// the two don't multiply.
package uiscale

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Prefs are the choices the app offers ("" = auto). Values are percent,
// in 10% steps because Fyne rounds its scale to 0.1 (125% would draw at
// 130%); 160 and 190 are left out to keep the menu short.
var Prefs = []string{"", "100", "110", "120", "130", "140", "150", "170", "200"}

// Result is what Apply decided, for the log.
type Result struct {
	Scale  float64 // 0: left to Fyne
	Source string  // where Scale came from
	Probe  Probe   // what the desktop told us
}

// Probe is the desktop's scaling state (Linux/X11; empty elsewhere).
type Probe struct {
	Session        string  // XDG_SESSION_TYPE
	Desktop        string  // XDG_CURRENT_DESKTOP
	GDKScale       string  // GDK_SCALE
	GDKDPIScale    string  // GDK_DPI_SCALE
	XftDPI         float64 // Xft.dpi from the X resource database (0: unset)
	CinnamonScale  int     // org.cinnamon.desktop.interface scaling-factor (0: auto / unknown)
	CinnamonText   float64 // org.cinnamon.desktop.interface text-scaling-factor (0: unknown)
	Monitors       string  // RandR monitors: name WxH+X+Y mm [mode, xrandr scale] (what Fyne/GLFW see)
	FyneDetectHint string  // what Fyne's own detection would make of them
	XrandrScale    float64 // largest logical/mode size ratio (>1: xrandr downscales, as Cinnamon's fractional scaling does)
}

// sources are where decide reads the desktop's state (fakes in tests).
type sources struct {
	probe   func(getenv func(string) string) Probe // environment and X11: cheap
	desktop func(*Probe)                           // gsettings: a subprocess, only as a fallback
}

// Parse turns a preference ("150", "150%", "1.5") into a scale (0: auto).
func Parse(pref string) float64 {
	p := strings.TrimSuffix(strings.TrimSpace(pref), "%")
	if p == "" || strings.EqualFold(p, "auto") {
		return 0
	}
	v, err := strconv.ParseFloat(p, 64)
	if err != nil || !(v > 0) || math.IsInf(v, 0) { // also NaN
		return 0
	}
	if v > 8 { // percent
		v /= 100
	}
	return clamp(v)
}

// clamp keeps a scale within 1..4, rounded to 0.1 as Fyne rounds it
// (calculateScale), so the scale we log and label is the one drawn.
func clamp(v float64) float64 {
	v = math.Max(1, math.Min(4, v))
	return math.Round(v*10) / 10
}

// Label is a preference as the menu shows it: the percentage that
// actually applies ("125" shows as 130%); "" for automatic.
func Label(pref string) string {
	s := Parse(pref)
	if s == 0 {
		return ""
	}
	return strconv.Itoa(int(math.Round(s*100))) + "%"
}

// Same reports whether two preferences give the same scale.
func Same(a, b string) bool { return Parse(a) == Parse(b) }

// Apply sets FYNE_SCALE (and FYNE_DISABLE_DPI_DETECTION) from pref (the
// app's "Interface size" setting, "" = auto) or the desktop, before Fyne
// creates a window. FYNE_SCALE set by the user always wins. Call it once,
// before nui.Run.
func Apply(pref string, log *slog.Logger) Result {
	r := decide(pref, os.Getenv, sys)
	if r.Scale > 0 {
		os.Setenv("FYNE_SCALE", strconv.FormatFloat(r.Scale, 'f', -1, 64))
		os.Setenv("FYNE_DISABLE_DPI_DETECTION", "1") // FYNE_SCALE is the whole scale, not a factor on Fyne's guess
	}
	if log != nil {
		p := r.Probe
		log.Info("ui scale", "scale", r.Scale, "source", r.Source, "session", p.Session, "desktop", p.Desktop,
			"gdkScale", p.GDKScale, "gdkDPIScale", p.GDKDPIScale, "xftDPI", p.XftDPI,
			"cinnamonScale", p.CinnamonScale, "cinnamonText", p.CinnamonText,
			"monitors", p.Monitors, "xrandrScale", p.XrandrScale, "fyneWouldPick", p.FyneDetectHint)
	}
	return r
}

// decide is Apply without side effects (tests).
func decide(pref string, getenv func(string) string, src sources) Result {
	p := src.probe(getenv)
	r := Result{Probe: p}
	if v := getenv("FYNE_SCALE"); v != "" { // the user's: Fyne multiplies it by its own guess
		r.Source = "FYNE_SCALE=" + v + " (user)"
		return r
	}
	if !strings.EqualFold(p.Session, "x11") && p.Session != "" {
		// Wayland, Windows, macOS: Fyne follows the compositor's or the OS's
		// (per-monitor) scale itself, and FYNE_SCALE would multiply it.
		r.Source = "left to Fyne (" + p.Session + ")"
		return r
	}
	if s := Parse(pref); s > 0 {
		r.Scale, r.Source = s, "setting "+pref
		return r
	}
	// Priority: GDK_SCALE (an explicit per-session override that GTK obeys
	// over everything else), then Xft.dpi (what Cinnamon, GNOME, MATE, Xfce
	// and ~/.Xresources all set, and what every X11 toolkit reads; it
	// includes the text scaling factor), then Cinnamon's gsettings.
	//
	// Under Cinnamon's fractional scaling on X11 (muffin's default "scale
	// UI down" mode), Xft.dpi is 96 x text factor x the integer UI scale
	// (cinnamon-settings-daemon's xsettings plugin uses muffin's
	// legacy-ui-scaling-factor, ceil of the largest monitor scale), so 192
	// at 150%, and xrandr shrinks the whole screen by 0.75. Drawing at 2
	// then shows at 1.5, like GTK apps; drawing at 1.5 would show at 1.125.
	// So Xft.dpi/96 is right there too; the log's xrandrScale tells such
	// setups apart.
	if g, err := strconv.Atoi(strings.TrimSpace(p.GDKScale)); err == nil && g >= 1 {
		f := 1.0
		if d, err := strconv.ParseFloat(strings.TrimSpace(p.GDKDPIScale), 64); err == nil && d > 0 {
			f = d
		}
		r.Scale, r.Source = clamp(float64(g)*f), fmt.Sprintf("GDK_SCALE=%d GDK_DPI_SCALE=%g", g, f)
		return r
	}
	if p.XftDPI >= 48 && p.XftDPI <= 960 {
		r.Scale, r.Source = clamp(p.XftDPI/96), fmt.Sprintf("Xft.dpi=%g", p.XftDPI)
		return r
	}
	src.desktop(&p) // gsettings: only now that GDK_SCALE and Xft.dpi gave nothing
	r.Probe = p
	// scaling-factor 0 is "auto" (muffin picks): with text at 1 that says
	// nothing, so Fyne's detection is no worse.
	if p.CinnamonScale > 0 || (p.CinnamonText > 0 && p.CinnamonText != 1) {
		s, t := float64(max(p.CinnamonScale, 1)), p.CinnamonText
		if !(t > 0) || math.IsInf(t, 0) { // also NaN
			t = 1
		}
		r.Scale, r.Source = clamp(s*t), fmt.Sprintf("cinnamon scaling-factor=%d text-scaling-factor=%g", p.CinnamonScale, t)
		return r
	}
	r.Source = "left to Fyne (no desktop scale found)"
	return r
}

// ManualSupported reports whether the "Interface size" setting applies
// here: on X11 only (elsewhere the OS or compositor scales Fyne).
func ManualSupported() bool {
	return runtime.GOOS == "linux" && session(os.Getenv) == "x11"
}
