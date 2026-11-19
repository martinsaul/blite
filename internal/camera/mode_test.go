package camera

import (
	"strings"
	"testing"
)

func raw(f string, w, h, n, d int) Mode {
	return Mode{Media: MediaRaw, Format: f, W: w, H: h, FPSNum: n, FPSDen: d}
}
func mjpg(w, h, n, d int) Mode { return Mode{Media: MediaJPEG, W: w, H: h, FPSNum: n, FPSDen: d} }

// rates expands one size into a mode per frame rate (n/1).
func rates(mk func(n int) Mode, ns ...int) []Mode {
	var out []Mode
	for _, n := range ns {
		out = append(out, mk(n))
	}
	return out
}

// Real mode lists (as v4l2-ctl / GStreamer report them).
var (
	// Logitech C920: MJPEG to 1080p30; raw YUY2 only slow at the top.
	c920 = func() []Mode {
		var ms []Mode
		for _, s := range [][2]int{{640, 480}, {160, 90}, {320, 180}, {640, 360}, {800, 448}, {800, 600}, {960, 720}, {1280, 720}, {1600, 896}, {1920, 1080}} {
			ms = append(ms, rates(func(n int) Mode { return mjpg(s[0], s[1], n, 1) }, 30, 24, 20, 15, 10)...)
		}
		ms = append(ms, rates(func(n int) Mode { return raw("YUY2", 640, 480, n, 1) }, 30, 24, 15)...)
		ms = append(ms, raw("YUY2", 960, 720, 15, 1), raw("YUY2", 1280, 720, 10, 1), raw("YUY2", 1600, 896, 15, 2),
			raw("YUY2", 1920, 1080, 5, 1), raw("YUY2", 2304, 1536, 2, 1))
		return ms
	}()
	// MS2109 HDMI capture stick: MJPEG to 1080p30 (incl. a 4:3 1600x1200),
	// raw YUY2 1080p only at 5 fps.
	ms2109 = []Mode{
		mjpg(1920, 1080, 30, 1), mjpg(1600, 1200, 30, 1), mjpg(1360, 768, 30, 1), mjpg(1280, 1024, 30, 1),
		mjpg(1280, 960, 30, 1), mjpg(1280, 720, 30, 1), mjpg(1024, 768, 30, 1), mjpg(800, 600, 30, 1),
		mjpg(720, 576, 30, 1), mjpg(720, 480, 30, 1), mjpg(640, 480, 30, 1),
		raw("YUY2", 1920, 1080, 5, 1), raw("YUY2", 1600, 1200, 5, 1), raw("YUY2", 1280, 720, 10, 1),
		raw("YUY2", 640, 480, 30, 1),
	}
	// Elgato HD60 S+ (Media Foundation): raw NV12 and YUY2 to 1080p60.
	elgato = func() []Mode {
		var ms []Mode
		for _, f := range []string{"YUY2", "NV12"} {
			for _, s := range [][2]int{{1920, 1080}, {1280, 720}, {720, 480}} {
				ms = append(ms, rates(func(n int) Mode { return raw(f, s[0], s[1], n, 1) }, 60, 50, 30, 25)...)
			}
		}
		return ms
	}()
	// v4l2loopback without a producer: any size and rate.
	loopback = []Mode{
		{Media: MediaRaw, Format: "YUY2", W: 2, H: 2, MaxW: 8192, MaxH: 8192, FPSNum: 1, FPSDen: 1, MaxFPSNum: 1000, MaxFPSDen: 1},
		{Media: MediaRaw, Format: "NV12", W: 2, H: 2, MaxW: 8192, MaxH: 8192, FPSNum: 1, FPSDen: 1, MaxFPSNum: 1000, MaxFPSDen: 1},
		{Media: MediaRaw, Format: "I420", W: 2, H: 2, MaxW: 8192, MaxH: 8192, FPSNum: 1, FPSDen: 1, MaxFPSNum: 1000, MaxFPSDen: 1},
	}
	// OBS Virtual Camera (DirectShow): the OBS output size at its rate.
	obs = []Mode{raw("NV12", 1920, 1080, 30, 1), raw("I420", 1920, 1080, 30, 1), raw("YUY2", 1920, 1080, 30, 1)}
	// An old 4:3 webcam.
	vga = []Mode{
		raw("YUY2", 640, 480, 30, 1), raw("YUY2", 320, 240, 30, 1), raw("YUY2", 160, 120, 30, 1),
		mjpg(640, 480, 30, 1), mjpg(320, 240, 30, 1),
	}
	// NTSC-style rates.
	ntsc = []Mode{
		raw("NV12", 1920, 1080, 60000, 1001), raw("NV12", 1920, 1080, 30000, 1001),
		raw("NV12", 1280, 720, 60000, 1001), raw("NV12", 1280, 720, 30000, 1001),
	}
)

func TestPickModeAndFit(t *testing.T) {
	cases := []struct {
		name      string
		modes     []Mode
		h, fps    int
		want      Mode
		shareH    int
		shareFPS  int
		noteMatch string // "" = no note
	}{
		{"C920 720p30", c920, 720, 30, mjpg(1280, 720, 30, 1), 720, 30, ""},
		{"C920 720p60", c920, 720, 60, mjpg(1280, 720, 30, 1), 720, 30, "at most 30 fps"},
		{"C920 1080p30", c920, 1080, 30, mjpg(1920, 1080, 30, 1), 1080, 30, ""},
		{"C920 1080p60", c920, 1080, 60, mjpg(1920, 1080, 30, 1), 1080, 30, "at most 30 fps"},
		{"C920 2160p30 (not the 5 fps raw 2304x1536)", c920, 2160, 30, mjpg(1920, 1080, 30, 1), 1080, 30, "at most 1080p at 30 fps"},
		{"MS2109 1080p60", ms2109, 1080, 60, mjpg(1920, 1080, 30, 1), 1080, 30, "at most 30 fps"},
		{"MS2109 2160p30 (16:9 1080p over 4:3 1200p)", ms2109, 2160, 30, mjpg(1920, 1080, 30, 1), 1080, 30, "at most 1080p"},
		{"MS2109 720p30 (exact 720 over 768)", ms2109, 720, 30, mjpg(1280, 720, 30, 1), 720, 30, ""},
		{"Elgato 1080p60", elgato, 1080, 60, raw("NV12", 1920, 1080, 60, 1), 1080, 60, ""},
		{"Elgato 1080p30", elgato, 1080, 30, raw("NV12", 1920, 1080, 30, 1), 1080, 30, ""},
		{"Elgato 720p60", elgato, 720, 60, raw("NV12", 1280, 720, 60, 1), 720, 60, ""},
		{"Elgato 2160p60", elgato, 2160, 60, raw("NV12", 1920, 1080, 60, 1), 1080, 60, "at most 1080p at 60 fps"},
		{"loopback 720p30", loopback, 720, 30, raw("NV12", 1280, 720, 30, 1), 720, 30, ""},
		{"loopback 1080p60", loopback, 1080, 60, raw("NV12", 1920, 1080, 60, 1), 1080, 60, ""},
		{"loopback 2160p60", loopback, 2160, 60, raw("NV12", 3840, 2160, 60, 1), 2160, 60, ""},
		{"OBS 1080p60", obs, 1080, 60, raw("NV12", 1920, 1080, 30, 1), 1080, 30, "at most 30 fps"},
		{"OBS 720p30", obs, 720, 30, raw("NV12", 1920, 1080, 30, 1), 720, 30, ""},
		{"4:3 480p 720p30 (raw, scaled up)", vga, 720, 30, raw("YUY2", 640, 480, 30, 1), 720, 30, "480p at 30 fps; sharing at 720p at 30 fps, scaled up"},
		{"4:3 480p 1080p60", vga, 1080, 60, raw("YUY2", 640, 480, 30, 1), 720, 30, "scaled up"},
		{"29.97 1080p30", ntsc, 1080, 30, raw("NV12", 1920, 1080, 30000, 1001), 1080, 30, ""},
		{"59.94 1080p60", ntsc, 1080, 60, raw("NV12", 1920, 1080, 60000, 1001), 1080, 60, ""},
		{"59.94 720p30", ntsc, 720, 30, raw("NV12", 1280, 720, 30000, 1001), 720, 30, ""},
	}
	for _, c := range cases {
		got := PickMode(c.modes, c.h, c.fps)
		if got != c.want {
			t.Errorf("%s: PickMode = %v, want %v", c.name, got, c.want)
		}
		f := Fit(Camera{Modes: c.modes}, c.h, c.fps)
		if f.Mode != got || f.Height != c.shareH || f.FPS != c.shareFPS {
			t.Errorf("%s: Fit = %v %dp%d, want %v %dp%d", c.name, f.Mode, f.Height, f.FPS, c.want, c.shareH, c.shareFPS)
		}
		if (c.noteMatch == "") != (f.Note == "") || !strings.Contains(f.Note, c.noteMatch) {
			t.Errorf("%s: note %q, want one containing %q", c.name, f.Note, c.noteMatch)
		}
	}
}

func TestPickModeEdges(t *testing.T) {
	if m := PickMode(nil, 720, 30); m != (Mode{}) {
		t.Errorf("no modes: %v", m)
	}
	// Only formats we can't open (H.264, raw without a format).
	odd := []Mode{{Media: "video/x-h264", W: 1920, H: 1080, FPSNum: 30, FPSDen: 1}, {Media: MediaRaw, W: 1280, H: 720, FPSNum: 30, FPSDen: 1}}
	if m := PickMode(odd, 720, 30); m != (Mode{}) {
		t.Errorf("unusable modes: %v", m)
	}
	f := Fit(Camera{Modes: odd}, 1080, 60)
	if f.Height != 1080 || f.FPS != 60 || f.Note != "" {
		t.Errorf("Fit without modes changed the setting: %+v", f)
	}
	// Nothing fast enough: the fastest of the best height is still picked.
	slow := []Mode{raw("YUY2", 1920, 1080, 5, 1), raw("YUY2", 1280, 720, 10, 1)}
	if m := PickMode(slow, 720, 30); m != raw("YUY2", 1280, 720, 10, 1) {
		t.Errorf("slow: %v", m)
	}
	if f := Fit(Camera{Modes: slow}, 720, 30); f.FPS != 10 {
		t.Errorf("slow fit fps %d", f.FPS)
	}
	// A rate range below the wanted rate gives its top.
	rr := []Mode{{Media: MediaRaw, Format: "NV12", W: 1280, H: 720, FPSNum: 5, FPSDen: 1, MaxFPSNum: 25, MaxFPSDen: 1}}
	if m := PickMode(rr, 720, 30); m.FPSNum != 25 || m.FPSDen != 1 || m.MaxFPSNum != 0 {
		t.Errorf("rate range: %v", m)
	}
	// Defaults: 0 means 720p30.
	if m := PickMode(c920, 0, 0); m != mjpg(1280, 720, 30, 1) {
		t.Errorf("defaults: %v", m)
	}
}

func TestModeText(t *testing.T) {
	for m, want := range map[Mode]string{
		mjpg(1920, 1080, 30, 1):                  "MJPEG 1920x1080 @ 30 fps",
		raw("NV12", 1920, 1080, 30000, 1001):     "NV12 1920x1080 @ 29.97 fps",
		raw("YUY2", 1600, 896, 15, 2):            "YUY2 1600x896 @ 7.5 fps",
		loopback[0]:                              "YUY2 2-8192 x 2-8192 @ 1-1000 fps",
		{Media: MediaRaw, Format: "NV12", W: 64}: "NV12 64x0 @ ? fps",
	} {
		if got := m.String(); got != want {
			t.Errorf("%#v: %q, want %q", m, got, want)
		}
	}
}

func TestPickModeExactRateBeatsNTSC(t *testing.T) {
	for _, c := range []struct {
		modes []Mode
		h     int
		fps   int
		want  Mode
	}{
		{[]Mode{raw("NV12", 1920, 1080, 30000, 1001), raw("NV12", 1920, 1080, 30, 1)}, 1080, 30, raw("NV12", 1920, 1080, 30, 1)},
		{[]Mode{raw("NV12", 1920, 1080, 30, 1), raw("NV12", 1920, 1080, 30000, 1001)}, 1080, 30, raw("NV12", 1920, 1080, 30, 1)},
		{[]Mode{raw("NV12", 1920, 1080, 60000, 1001), raw("NV12", 1920, 1080, 60, 1)}, 1080, 60, raw("NV12", 1920, 1080, 60, 1)},
		{[]Mode{raw("NV12", 1920, 1080, 60, 1), raw("NV12", 1920, 1080, 60000, 1001)}, 1080, 60, raw("NV12", 1920, 1080, 60, 1)},
		// 29.97 is still nearer 30 than 60 is.
		{[]Mode{raw("NV12", 1920, 1080, 60, 1), raw("NV12", 1920, 1080, 30000, 1001)}, 1080, 30, raw("NV12", 1920, 1080, 30000, 1001)},
	} {
		if got := PickMode(c.modes, c.h, c.fps); got != c.want {
			t.Errorf("%v at %dp%d: %v, want %v", c.modes, c.h, c.fps, got, c.want)
		}
	}
}

func TestVariableRateAndTinyModes(t *testing.T) {
	variable := raw("NV12", 1280, 720, 0, 1)
	if !variable.Variable() || variable.Rate() != "variable" {
		t.Errorf("0/1: Variable %v Rate %q", variable.Variable(), variable.Rate())
	}
	// Alone: picked, shared at the wanted rate, no note.
	if f := Fit(Camera{Modes: []Mode{variable}}, 720, 30); f.Mode != variable || f.Height != 720 || f.FPS != 30 || f.Note != "" {
		t.Errorf("0/1 alone: %+v", f)
	}
	// Mixed: a fixed 30 fps mode wins.
	mixed := []Mode{variable, raw("NV12", 1280, 720, 30, 1), raw("NV12", 1280, 720, 15, 1)}
	if m := PickMode(mixed, 720, 30); m != raw("NV12", 1280, 720, 30, 1) {
		t.Errorf("0/1 mixed: %v", m)
	}
	// A 0/1 range: the wanted rate inside it, its top above it.
	vr := Mode{Media: MediaRaw, Format: "NV12", W: 1280, H: 720, FPSNum: 0, FPSDen: 1, MaxFPSNum: 30, MaxFPSDen: 1}
	if m := PickMode([]Mode{vr}, 720, 30); m.FPSNum != 30 || m.FPSDen != 1 {
		t.Errorf("0/1 range at 30: %v", m)
	}
	if f := Fit(Camera{Modes: []Mode{vr}}, 720, 60); f.FPS != 30 || f.Mode.FPSNum != 30 {
		t.Errorf("0/1 range at 60: %+v", f)
	}
	if got := modeGroups([]Mode{raw("NV12", 1280, 720, 30, 1), variable}); len(got) != 1 || got[0].rates != "30 fps, variable rate" {
		t.Errorf("groups %+v", got)
	}
	// 1x1 placeholders aren't modes.
	tiny := raw("NV12", 1, 1, 30, 1)
	if usable(tiny) || PickMode([]Mode{tiny}, 720, 30) != (Mode{}) {
		t.Error("1x1 mode usable")
	}
	if !usable(raw("YUY2", 160, 90, 30, 1)) || usable(raw("YUY2", 120, 90, 30, 1)) {
		t.Error("minimum size")
	}
	if ms := modesOf(nil); ms != nil {
		t.Errorf("modesOf(nil) = %v", ms)
	}
}

func TestFitNoteGivesRealRate(t *testing.T) {
	f := Fit(Camera{Modes: []Mode{mjpg(1920, 1080, 24, 1)}}, 1080, 30)
	if f.FPS != 25 || !strings.Contains(f.Note, "at most 24 fps at 1080p") || !strings.Contains(f.Note, "sharing at 1080p at 25 fps") {
		t.Errorf("24 fps: %+v", f)
	}
	f = Fit(Camera{Modes: []Mode{raw("NV12", 1280, 720, 30000, 1001)}}, 1080, 30)
	if f.Note != "Your camera gives at most 720p at 29.97 fps; sharing at that." {
		t.Errorf("29.97: %q", f.Note)
	}
}
