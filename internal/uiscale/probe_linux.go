package uiscale

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"
)

// sys reads the real desktop.
var sys = sources{probe: probe, desktop: cinnamonProbe}

// probe reads the cheap scaling sources: the environment and, on X11, the
// root window's resources and the RandR monitors. The gsettings fallback
// (a subprocess) is cinnamonProbe, run only when these give nothing.
func probe(getenv func(string) string) Probe {
	p := Probe{
		Session:     session(getenv),
		Desktop:     getenv("XDG_CURRENT_DESKTOP"),
		GDKScale:    getenv("GDK_SCALE"),
		GDKDPIScale: getenv("GDK_DPI_SCALE"),
	}
	if p.Session == "x11" {
		type x11 struct {
			dpi, xr        float64
			monitors, hint string
		}
		ch := make(chan x11, 1)
		go func() { // xgb.NewConn has no timeout; a dead TCP DISPLAY mustn't stall startup
			var r x11
			r.dpi, r.monitors, r.hint, r.xr = x11Probe()
			ch <- r
		}()
		select {
		case r := <-ch:
			p.XftDPI, p.Monitors, p.FyneDetectHint, p.XrandrScale = r.dpi, r.monitors, r.hint, r.xr
		case <-time.After(2 * time.Second):
			p.Monitors = "X11 probe timed out"
		}
	}
	return p
}

// cinnamonProbe reads Cinnamon's scaling-factor and text-scaling-factor
// with gsettings, within a second for both.
func cinnamonProbe(p *Probe) {
	if !strings.Contains(strings.ToLower(p.Desktop), "cinnamon") {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if v, ok := gsetting(ctx, "org.cinnamon.desktop.interface", "scaling-factor"); ok {
		p.CinnamonScale = parseGUint(v)
	}
	if v, ok := gsetting(ctx, "org.cinnamon.desktop.interface", "text-scaling-factor"); ok {
		if t, err := strconv.ParseFloat(v, 64); err == nil {
			p.CinnamonText = t
		}
	}
}

// parseGUint reads a gsettings uint ("uint32 2" or "2"); 0 if it isn't one.
func parseGUint(v string) int {
	f := strings.Fields(v)
	if len(f) == 0 {
		return 0
	}
	n, err := strconv.Atoi(f[len(f)-1])
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// session is the windowing system GLFW will pick: Wayland when the session
// is Wayland and a compositor socket is named, else X11 when DISPLAY is set.
// FYNE_PLATFORM (Fyne's own override) wins when its display is there.
func session(getenv func(string) string) string {
	st := strings.ToLower(getenv("XDG_SESSION_TYPE"))
	switch fp := getenv("FYNE_PLATFORM"); {
	case fp == "x11" && getenv("DISPLAY") != "":
		return "x11"
	case fp == "wayland" && getenv("WAYLAND_DISPLAY") != "":
		return "wayland"
	}
	switch {
	case st == "wayland" && getenv("WAYLAND_DISPLAY") != "":
		return "wayland"
	case getenv("DISPLAY") != "":
		return "x11"
	}
	return st
}

// x11Probe returns Xft.dpi from the root window's RESOURCE_MANAGER (what
// XResourceManagerString gives Xlib clients), the RandR monitors with their
// physical size and mode, the scale Fyne's own detection would derive from
// each, and the largest xrandr scale transform (logical size / mode size,
// 1 when none; see monitorDesc).
func x11Probe() (xftDPI float64, monitors, fyneHint string, xrandrScale float64) {
	c, err := xgb.NewConn()
	if err != nil {
		return 0, "", "", 0
	}
	defer c.Close()
	root := xproto.Setup(c).DefaultScreen(c).Root
	if r, err := xproto.GetProperty(c, false, root, xproto.AtomResourceManager, xproto.AtomString, 0, 1<<20).Reply(); err == nil {
		xftDPI = parseXftDPI(string(r.Value))
	}
	if randr.Init(c) != nil {
		return xftDPI, "", "", 0
	}
	mons, err := randr.GetMonitors(c, root, true).Reply()
	if err != nil {
		return xftDPI, "", "", 0
	}
	modes := map[randr.Mode][2]int{}
	if res, err := randr.GetScreenResourcesCurrent(c, root).Reply(); err == nil {
		for _, m := range res.Modes {
			modes[randr.Mode(m.Id)] = [2]int{int(m.Width), int(m.Height)}
		}
	}
	var ms, hs []string
	for _, m := range mons.Monitors {
		name := ""
		if a, err := xproto.GetAtomName(c, m.Name).Reply(); err == nil {
			name = a.Name
		}
		// The mode (physical pixels, what GLFW reports) of the first output's CRTC.
		modeW, modeH := 0, 0
		if len(m.Outputs) > 0 {
			if o, err := randr.GetOutputInfo(c, m.Outputs[0], 0).Reply(); err == nil && o.Crtc != 0 {
				if cr, err := randr.GetCrtcInfo(c, o.Crtc, 0).Reply(); err == nil {
					wh := modes[cr.Mode]
					modeW, modeH = wh[0], wh[1]
					if cr.Rotation&(randr.RotationRotate90|randr.RotationRotate270) != 0 {
						modeW, modeH = modeH, modeW
					}
				}
			}
		}
		desc, xr := monitorDesc(name, int(m.Width), int(m.Height), int(m.X), int(m.Y), int(m.WidthInMillimeters), int(m.HeightInMillimeters), modeW, modeH)
		ms = append(ms, desc)
		xrandrScale = max(xrandrScale, xr)
		px := modeW
		if px == 0 {
			px = int(m.Width)
		}
		hs = append(hs, fmt.Sprintf("%s=%.1f", name, fyneDetected(int(m.WidthInMillimeters), px)))
	}
	return xftDPI, strings.Join(ms, "; "), strings.Join(hs, " "), xrandrScale
}

// monitorDesc describes one RandR monitor for the log: its logical
// geometry, physical size, and mode when that differs from the geometry.
// A logical size larger than the mode is an xrandr --scale downscale, as
// Cinnamon's (muffin's) fractional scaling does by default on X11 (the
// "scale UI down" mode): at 150% the screen is laid out at 2x (Xft.dpi
// 192) and the CRTC shrinks it by 0.75. X clients must then draw at the
// integer scale, Xft.dpi/96, which is what we do; the returned factor
// (logical/mode, 1 when none) is for the log, to tell such setups apart.
func monitorDesc(name string, w, h, x, y, mmW, mmH, modeW, modeH int) (string, float64) {
	s := fmt.Sprintf("%s %dx%d+%d+%d %dx%dmm", name, w, h, x, y, mmW, mmH)
	if modeW <= 0 || modeH <= 0 {
		return s, 1
	}
	if modeW == w && modeH == h {
		return s, 1
	}
	xr := float64(w) / float64(modeW)
	return s + fmt.Sprintf(" mode %dx%d xrandr-scale %.3g", modeW, modeH, xr), xr
}

// fyneDetected mirrors Fyne 2.8's X11 detection (calculateDetectedScale on
// the GLFW video mode's width), including GLFW's guess of 96 dpi for an
// output with no physical size.
func fyneDetected(mm, px int) float64 {
	dpi := 96.0
	if mm > 0 {
		dpi = float64(px) / (float64(mm) / 25.4)
	}
	if dpi > 1000 || dpi < 10 {
		dpi = 120
	}
	return float64(int(max(dpi/120, 1)*10+0.5)) / 10
}

// parseXftDPI finds "Xft.dpi:\t144" in an X resource database string.
func parseXftDPI(db string) float64 {
	for _, l := range strings.Split(db, "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "Xft.dpi") {
			continue
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return 0
}

// gsetting reads one GSettings key with the gsettings tool (no cgo GIO).
func gsetting(ctx context.Context, schema, key string) (string, bool) {
	out, err := exec.CommandContext(ctx, "gsettings", "get", schema, key).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}
