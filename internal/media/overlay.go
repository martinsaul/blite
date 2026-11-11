package media

import (
	"image"
	"image/color"
	"math"
)

// The controls on a video tile and its waiting spinner
// are drawn here, at the size they show on the canvas, and
// composed over the tile: crisp, of even size, and faded through the
// compositor pad's alpha.

// The bar, in units of its height h: mute and pause buttons, then the
// volume slider (0..maxVolume, a tick at 100%).
const (
	barPad       = 0.3
	barMuteX     = barPad + 0.5 // button centres
	barPauseX    = barPad + 1.5
	barSliderX0  = barPad + 2.35
	barSliderX1  = barPad + 5.25
	barUnits     = barSliderX1 + 0.4
	maxVolume    = 2.0
	barMinH      = 24
	barMaxH      = 48
	spinnerMinPx = 32
	spinnerMaxPx = 88
)

// barSize is the bar's size on a tile th lines tall.
func barSize(th int) (w, h int) {
	h = min(max(th/12, barMinH), barMaxH) &^ 1
	return int(barUnits*float64(h)) &^ 1, h
}

// spinnerSize is the spinner's side on a tile th lines tall.
func spinnerSize(th int) int { return min(max(th/7, spinnerMinPx), spinnerMaxPx) &^ 1 }

// barHit is what a point x, y of the bar (pixels from its top left) hits:
// a button, or the slider with the volume there.
func barHit(x, y float64, h int) (Control, float64, bool) {
	u, v := x/float64(h), y/float64(h)
	if v < 0 || v > 1 || u < 0 || u > barUnits {
		return "", 0, false
	}
	switch {
	case u < barPad+1:
		return ControlMute, 0, true
	case u < barPad+2:
		return ControlPause, 0, true
	case u >= barSliderX0-0.25:
		return ControlVolume, sliderValue(u), true
	}
	return "", 0, false
}

func sliderValue(u float64) float64 {
	f := (u - barSliderX0) / (barSliderX1 - barSliderX0)
	return math.Round(min(max(f, 0), 1)*maxVolume*20) / 20 // steps of 5%
}

type canvas struct {
	img *image.NRGBA
	s   float64 // pixels per unit
}

// fill paints color c where sdf (a signed distance in units, < 0 inside)
// covers each pixel, antialiased over one pixel.
func (cv canvas) fill(c color.NRGBA, sdf func(x, y float64) float64) {
	b := cv.img.Bounds()
	for py := b.Min.Y; py < b.Max.Y; py++ {
		for px := b.Min.X; px < b.Max.X; px++ {
			d := sdf((float64(px)+0.5)/cv.s, (float64(py)+0.5)/cv.s) * cv.s
			a := min(max(0.5-d, 0), 1) * float64(c.A) / 255
			if a <= 0 {
				continue
			}
			i := cv.img.PixOffset(px, py)
			p := cv.img.Pix[i : i+4]
			da := float64(p[3]) / 255
			oa := a + da*(1-a)
			for k, sc := range []uint8{c.R, c.G, c.B} {
				p[k] = uint8((float64(sc)*a + float64(p[k])*da*(1-a)) / oa)
			}
			p[3] = uint8(oa*255 + 0.5)
		}
	}
}

func circle(cx, cy, r float64) func(x, y float64) float64 {
	return func(x, y float64) float64 { return math.Hypot(x-cx, y-cy) - r }
}

// capsule is a segment a-b with round ends of radius r.
func capsule(ax, ay, bx, by, r float64) func(x, y float64) float64 {
	return func(x, y float64) float64 {
		px, py, dx, dy := x-ax, y-ay, bx-ax, by-ay
		t := min(max((px*dx+py*dy)/(dx*dx+dy*dy), 0), 1)
		return math.Hypot(px-dx*t, py-dy*t) - r
	}
}

// polygon is a closed polygon (any shape, even-odd inside).
func polygon(pts ...[2]float64) func(x, y float64) float64 {
	return func(x, y float64) float64 {
		d := math.Inf(1)
		in := false
		for i, j := 0, len(pts)-1; i < len(pts); j, i = i, i+1 {
			a, b := pts[i], pts[j]
			ex, ey := b[0]-a[0], b[1]-a[1]
			wx, wy := x-a[0], y-a[1]
			t := min(max((wx*ex+wy*ey)/(ex*ex+ey*ey), 0), 1)
			d = min(d, math.Hypot(wx-ex*t, wy-ey*t))
			if (a[1] > y) != (b[1] > y) && x < a[0]+(y-a[1])*(b[0]-a[0])/(b[1]-a[1]) {
				in = !in
			}
		}
		if in {
			return -d
		}
		return d
	}
}

// arc is a ring of radius r and half thickness w around cx, cy, where the
// angle from the +x axis lies within a0..a1 (radians, a0 < a1).
func arc(cx, cy, r, w, a0, a1 float64) func(x, y float64) float64 {
	return func(x, y float64) float64 {
		a := math.Atan2(y-cy, x-cx)
		for a < a0 {
			a += 2 * math.Pi
		}
		if a <= a1 {
			return math.Abs(math.Hypot(x-cx, y-cy)-r) - w
		}
		e := func(t float64) float64 {
			return math.Hypot(x-cx-r*math.Cos(t), y-cy-r*math.Sin(t)) - w
		}
		return min(e(a0), e(a1))
	}
}

var (
	white    = color.NRGBA{255, 255, 255, 255}
	dimWhite = color.NRGBA{255, 255, 255, 110}
	panel    = color.NRGBA{0, 0, 0, 150}
	hoverBg  = color.NRGBA{255, 255, 255, 46}
)

// drawBar draws the controls h pixels tall: the button under the pointer
// (hover) lit, the speaker crossed out when muted, play when paused.
func drawBar(h int, muted, paused bool, volume float64, hover Control) *image.NRGBA {
	w := int(barUnits*float64(h)) &^ 1
	cv := canvas{image.NewNRGBA(image.Rect(0, 0, w, h)), float64(h)}
	cv.fill(panel, capsule(0.5, 0.5, barUnits-0.5, 0.5, 0.5))
	for _, b := range []struct {
		c Control
		x float64
	}{{ControlMute, barMuteX}, {ControlPause, barPauseX}} {
		if hover == b.c {
			cv.fill(hoverBg, circle(b.x, 0.5, 0.42))
		}
	}
	// speaker: box and cone, then waves or a cross
	x, y, s := barMuteX-0.06, 0.5, 0.5
	cv.fill(white, polygon([2]float64{x - 0.34*s, y - 0.13*s}, [2]float64{x - 0.12*s, y - 0.13*s}, [2]float64{x + 0.14*s, y - 0.4*s},
		[2]float64{x + 0.14*s, y + 0.4*s}, [2]float64{x - 0.12*s, y + 0.13*s}, [2]float64{x - 0.34*s, y + 0.13*s}))
	if muted {
		cv.fill(white, capsule(x+0.3*s, y-0.17*s, x+0.62*s, y+0.17*s, 0.055*s))
		cv.fill(white, capsule(x+0.3*s, y+0.17*s, x+0.62*s, y-0.17*s, 0.055*s))
	} else {
		cv.fill(white, arc(x+0.14*s, y, 0.26*s, 0.05*s, -0.85, 0.85))
		cv.fill(white, arc(x+0.14*s, y, 0.46*s, 0.05*s, -0.85, 0.85))
	}
	// pause bars, or play
	x = barPauseX
	if paused {
		cv.fill(white, polygon([2]float64{x - 0.18*s, y - 0.34*s}, [2]float64{x + 0.34*s, y}, [2]float64{x - 0.18*s, y + 0.34*s}))
	} else {
		cv.fill(white, capsule(x-0.15*s, y-0.3*s, x-0.15*s, y+0.3*s, 0.08*s))
		cv.fill(white, capsule(x+0.15*s, y-0.3*s, x+0.15*s, y+0.3*s, 0.08*s))
	}
	// volume slider
	f := min(max(volume/maxVolume, 0), 1)
	xv := barSliderX0 + f*(barSliderX1-barSliderX0)
	fillC := white
	if muted {
		fillC = dimWhite
	}
	cv.fill(dimWhite, capsule(barSliderX0, y, barSliderX1, y, 0.06))
	unity := barSliderX0 + (barSliderX1-barSliderX0)/maxVolume
	cv.fill(dimWhite, capsule(unity, y-0.17, unity, y+0.17, 0.025))
	cv.fill(fillC, capsule(barSliderX0, y, xv, y, 0.06))
	cv.fill(fillC, circle(xv, y, 0.16))
	return cv.img
}

// drawSpinner draws the waiting spinner s pixels square at phase 0..1.
func drawSpinner(s int, phase float64) *image.NRGBA {
	cv := canvas{image.NewNRGBA(image.Rect(0, 0, s, s)), float64(s)}
	cv.fill(panel, circle(0.5, 0.5, 0.5))
	cv.fill(dimWhite, arc(0.5, 0.5, 0.3, 0.05, -math.Pi, math.Pi))
	a := phase * 2 * math.Pi
	cv.fill(white, arc(0.5, 0.5, 0.3, 0.05, a, a+1.6))
	return cv.img
}
