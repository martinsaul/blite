package camera

import (
	"fmt"
	"math"
	"strconv"
)

// Media types a camera mode can have that we can open.
const (
	MediaRaw  = "video/x-raw"
	MediaJPEG = "image/jpeg"
)

// Mode is one way a camera can deliver video. A size or frame rate is
// either fixed, or a range from the fixed field up to the Max field
// (v4l2loopback, PipeWire and some virtual cameras take any size).
type Mode struct {
	Media  string // MediaRaw or MediaJPEG
	Format string // raw: "NV12", "YUY2", ...; "" for MJPEG
	W, H   int

	FPSNum, FPSDen int

	MaxW, MaxH           int // > W, H: a size range; 0 when fixed
	MaxFPSNum, MaxFPSDen int // > 0: a frame rate range up to this; 0 when fixed
}

// JPEG reports whether the mode is MJPEG.
func (m Mode) JPEG() bool { return m.Media == MediaJPEG }

// FPS is the (lowest) frame rate as a number; 0 if unknown.
func (m Mode) FPS() float64 { return frac(m.FPSNum, m.FPSDen) }

// TopFPS is the highest frame rate the mode offers.
func (m Mode) TopFPS() float64 {
	if m.MaxFPSNum > 0 {
		return frac(m.MaxFPSNum, m.MaxFPSDen)
	}
	return m.FPS()
}

// TopH is the largest height the mode offers.
func (m Mode) TopH() int { return max(m.H, m.MaxH) }

// Label is the format as people know it: "MJPEG", "NV12", "YUY2".
func (m Mode) Label() string {
	switch {
	case m.JPEG():
		return "MJPEG"
	case m.Format != "":
		return m.Format
	}
	return m.Media
}

// Size is "1920x1080", or "2-8192 x 2-8192" for a range.
func (m Mode) Size() string {
	if m.MaxW == 0 && m.MaxH == 0 {
		return fmt.Sprintf("%dx%d", m.W, m.H)
	}
	return span(m.W, m.MaxW) + " x " + span(m.H, m.MaxH)
}

// Variable reports a fixed 0/1 frame rate: the device sends frames when
// it has them.
func (m Mode) Variable() bool { return m.FPSNum == 0 && m.FPSDen > 0 && m.MaxFPSNum == 0 }

// Rate is "30 fps", "29.97 fps", "1-60 fps" or "variable".
func (m Mode) Rate() string {
	if m.Variable() {
		return "variable"
	}
	if m.FPSDen == 0 && m.MaxFPSNum == 0 {
		return "? fps"
	}
	if m.MaxFPSNum > 0 {
		return FormatFPS(m.FPS()) + "-" + FormatFPS(m.TopFPS()) + " fps"
	}
	return FormatFPS(m.FPS()) + " fps"
}

func (m Mode) String() string { return m.Label() + " " + m.Size() + " @ " + m.Rate() }

// FormatFPS prints a frame rate with up to two decimals: 30, 29.97, 7.5.
func FormatFPS(f float64) string { return strconv.FormatFloat(math.Round(f*100)/100, 'f', -1, 64) }

func span(lo, hi int) string {
	if hi <= lo {
		return strconv.Itoa(lo)
	}
	return strconv.Itoa(lo) + "-" + strconv.Itoa(hi)
}

func frac(n, d int) float64 {
	if d <= 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// fpsSlack lets 29.97 count as 30 and 59.94 as 60.
const fpsSlack = 0.1

// concrete fixes a mode's ranges to what is wanted: the height clamped
// to the range, a 16:9 width for it clamped likewise, and the wanted
// frame rate clamped to the rate range.
func concrete(m Mode, wantH, wantFPS int) Mode {
	if m.MaxH > m.H {
		m.H = min(max(wantH, m.H), m.MaxH)
	}
	if m.MaxW > m.W {
		w := (m.H*16/9 + 1) &^ 1
		m.W = min(max(w, m.W), m.MaxW)
	}
	m.MaxW, m.MaxH = 0, 0
	if m.MaxFPSNum > 0 {
		lo, hi := m.FPS(), m.TopFPS()
		switch want := float64(wantFPS); {
		case want > hi:
			m.FPSNum, m.FPSDen = m.MaxFPSNum, m.MaxFPSDen
		case want >= lo:
			m.FPSNum, m.FPSDen = wantFPS, 1
		}
		m.MaxFPSNum, m.MaxFPSDen = 0, 0
	}
	return m
}

func usable(m Mode) bool {
	switch m.Media {
	case MediaJPEG:
	case MediaRaw:
		if m.Format == "" {
			return false
		}
	default:
		return false
	}
	return max(m.W, m.MaxW) >= minW && m.TopH() >= minH
}

// The smallest size worth opening (160x90, the smallest 16:9 webcam mode);
// below it are placeholders such as 1x1.
const minW, minH = 160, 90

// PickMode chooses the mode to open for sharing at wantH (720, 1080, 2160)
// and wantFPS, before the pipeline starts, so the choice can be tested and
// logged. Ranges come back clamped to the wanted size and rate. The
// ranking, in order:
//
//  1. a frame rate of at least min(wantFPS, 30) (motion matters; USB 2
//     cameras offer raw 1080p only at about 5 fps);
//  2. the smallest height at or above wantH, else the largest (no
//     decoding 4K MJPEG to send 720p); heights are compared by the share
//     height (720p, 1080p, 2160p) they give, the exact height last;
//  3. the frame rate nearest wantFPS, at or above it first;
//  4. 16:9 before other shapes;
//  5. raw before MJPEG;
//  6. NV12, then YUY2, then other raw formats.
//
// The zero Mode means the list had nothing we can open.
func PickMode(ms []Mode, wantH, wantFPS int) Mode {
	if wantH <= 0 {
		wantH = 720
	}
	if wantFPS <= 0 {
		wantFPS = 30
	}
	var all, fast []Mode
	floor := float64(min(wantFPS, 30)) - fpsSlack
	for _, m := range ms {
		if !usable(m) {
			continue
		}
		c := concrete(m, wantH, wantFPS)
		all = append(all, c)
		if c.FPS() >= floor {
			fast = append(fast, c)
		}
	}
	pool := fast
	if len(pool) == 0 {
		pool = all
	}
	var best Mode
	for i, m := range pool {
		if i == 0 || better(m, best, wantH, wantFPS) {
			best = m
		}
	}
	return best
}

func better(a, b Mode, wantH, wantFPS int) bool {
	ka, kb := rank(a, wantH, wantFPS), rank(b, wantH, wantFPS)
	for i := range ka {
		if ka[i] != kb[i] {
			return ka[i] < kb[i]
		}
	}
	return false
}

// bucket is the height we would share a mode at: the largest share
// height at or below h (h itself below 720p).
func bucket(h int) int {
	b := h
	for _, s := range shareHeights {
		if s <= h {
			b = s
		}
	}
	return b
}

func rank(m Mode, wantH, wantFPS int) [9]float64 {
	var k [9]float64
	// Heights are compared by the share height they give, so a 4:3
	// 1600x1200 doesn't win over 1920x1080; the exact height breaks ties
	// last (k[7], k[8]).
	if b, w := bucket(m.H), bucket(wantH); b >= w {
		k[0], k[1] = 0, float64(b-w)
	} else {
		k[0], k[1] = 1, float64(w-b)
	}
	// At or above the wanted rate first, then the nearest: 30 beats
	// 29.97 beats 60 for 30 fps. A variable rate (0/1) counts as 0.
	if f, w := m.FPS(), float64(wantFPS); f >= w-fpsSlack {
		k[2], k[3] = 0, math.Abs(f-w)
	} else {
		k[2], k[3] = 1, w-f
	}
	if !wide(m.W, m.H) {
		k[4] = 1
	}
	if m.JPEG() {
		k[5] = 1
	}
	switch m.Format {
	case "NV12":
	case "YUY2", "YUYV":
		k[6] = 1
	default:
		if !m.JPEG() {
			k[6] = 2
		}
	}
	if m.H >= wantH {
		k[7], k[8] = 0, float64(m.H-wantH)
	} else {
		k[7], k[8] = 1, float64(wantH-m.H)
	}
	return k
}

// wide reports a 16:9 shape (within 1%).
func wide(w, h int) bool { d := w*9 - h*16; return h > 0 && max(d, -d) <= h*16/100 }

// Heights we share at, smallest first.
var shareHeights = []int{720, 1080, 2160}

// Fitted is a camera fitted to a share setting.
type Fitted struct {
	Mode   Mode   // the camera mode to open (zero: the camera listed none)
	Height int    // what we share at: 720, 1080 or 2160
	FPS    int    // what we share at: 10..60, a multiple of 5
	Note   string // why it is less than asked; "" when it isn't
}

// Fit fits the share setting (wantH, wantFPS) to what cam gives: the
// height becomes the largest of 720p, 1080p and 2160p at or below the
// mode's (and the wanted) height, 720p at least (a 480p webcam is scaled
// up); the frame rate becomes min(wantFPS, the mode's), rounded to a step
// of 5 so 29.97 becomes 30.
func Fit(cam Camera, wantH, wantFPS int) Fitted {
	if wantH < shareHeights[0] {
		wantH = shareHeights[0]
	}
	if wantFPS <= 0 {
		wantFPS = 30
	}
	m := PickMode(cam.Modes, wantH, wantFPS)
	f := Fitted{Mode: m, Height: wantH, FPS: wantFPS}
	if m == (Mode{}) {
		return f
	}
	f.Height = shareHeights[0]
	for _, h := range shareHeights[1:] {
		if h <= m.H && h <= wantH {
			f.Height = h
		}
	}
	modeFPS := int(math.Round(m.FPS()/5)) * 5
	if modeFPS > 0 {
		f.FPS = min(max(min(wantFPS, modeFPS), 10), 60)
	}
	real := FormatFPS(m.FPS())
	if m.Variable() || m.FPS() <= 0 {
		real = strconv.Itoa(f.FPS)
	}
	then := "sharing at that."
	if math.Abs(m.FPS()-float64(f.FPS)) > fpsSlack && !m.Variable() {
		then = fmt.Sprintf("sharing at %dp at %d fps.", f.Height, f.FPS)
	}
	switch {
	case m.H < f.Height:
		f.Note = fmt.Sprintf("Your camera gives at most %dp at %s fps; sharing at %dp at %d fps, scaled up.", m.H, real, f.Height, f.FPS)
	case f.Height < wantH:
		f.Note = fmt.Sprintf("Your camera gives at most %dp at %s fps; %s", f.Height, real, then)
	case f.FPS < wantFPS:
		f.Note = fmt.Sprintf("Your camera gives at most %s fps at %dp; %s", real, f.Height, then)
	}
	return f
}
