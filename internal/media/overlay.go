package media

import (
	"image"
	"image/color"
	"math"
)

// The controls on a video tile, its waiting spinner and its texts
// are drawn here, at the size they show on the canvas, and
// composed over the tile: crisp, of even size, and faded through the
// compositor pad's alpha.

// DrawText draws text white on transparent, px pixels high: as wide as it
// advances, as high as the font's line (nil: no text). The UI sets it
// (fonts for every script); without it the tiles show no texts.
var DrawText func(text string, px float64) *image.NRGBA

// The bar, in units of its height h: eye, pop-out/pop-in and pin on every
// tile, plus mute and the volume slider (0..maxVolume, a tick at 100%) on
// remote tiles. Our own preview has its own set (barButtons).
const (
	barPad       = 0.3
	barEyeX      = barPad + 0.5 // button centres
	barPopX      = barPad + 1.5
	barPinX      = barPad + 2.5
	barMuteX     = barPad + 3.5
	barSliderX0  = barPad + 4.35
	barSliderX1  = barPad + 7.15
	barFullUnits = barSliderX1 + 0.4
	maxVolume    = 2.0
	barMinH      = 24
	barMaxH      = 48
	spinnerMinPx = 32
	spinnerMaxPx = 88
)

// barButtons are the buttons on tile t's bar, left to right (remote tiles
// have the volume slider after them). Our preview in the corner: hide,
// pop out, expand to a tile; as a tile: hide, pop out, pin, back to the
// corner; popped out: hide, pop back in, pin.
func barButtons(t Tile) []Control {
	switch {
	case !t.Self:
		return []Control{ControlEye, ControlPopOut, ControlPin, ControlMute}
	case t.PiP:
		return []Control{ControlEye, ControlPopOut, ControlExpand}
	case t.PoppedOut:
		return []Control{ControlEye, ControlPopOut, ControlPin}
	}
	return []Control{ControlEye, ControlPopOut, ControlPin, ControlExpand}
}

// barButtonX is the centre of the k-th button.
func barButtonX(k int) float64 { return barPad + float64(k) + 0.5 }

func barUnits(t Tile) float64 {
	if !t.Self {
		return barFullUnits
	}
	return barPad + float64(len(barButtons(t))) + barPad
}

// barSize is tile t's bar size on a tile tw x th pixels.
func barSize(tw, th int, t Tile) (w, h int) {
	u := barUnits(t)
	h = min(max(th/12, barMinH), barMaxH)
	if maxH := int(float64(max(tw-8, 24)) / u); h > maxH {
		h = max(maxH, 12)
	}
	h &^= 1
	w = min(int(u*float64(h))&^1, tw&^1)
	return max(w, 2), max(h, 2)
}

// spinnerSize is the spinner's side on a tile tw x th pixels.
func spinnerSize(tw, th int) int {
	side := min(tw, th)
	return max(min(max(side/7, spinnerMinPx), min(spinnerMaxPx, side&^1))&^1, 2)
}

// barHit is what a point x, y of tile t's bar (pixels from its top left)
// hits: a button, or the slider with the volume there.
func barHit(x, y float64, h int, t Tile) (Control, float64, bool) {
	u, v := x/float64(h), y/float64(h)
	if v < 0 || v > 1 || u < 0 || u > barUnits(t) {
		return "", 0, false
	}
	bs := barButtons(t)
	if k := int(max(u-barPad, 0)); k < len(bs) {
		return bs[k], 0, true
	}
	if !t.Self && u >= barSliderX0-0.25 {
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

// roundRect is the rectangle x0,y0 - x1,y1 with corners of radius r.
func roundRect(x0, y0, x1, y1, r float64) func(x, y float64) float64 {
	return func(x, y float64) float64 {
		dx := max(x0+r-x, x-(x1-r), 0)
		dy := max(y0+r-y, y-(y1-r), 0)
		if dx > 0 && dy > 0 {
			return math.Hypot(dx, dy) - r
		}
		return max(x0-x, x-x1, y0-y, y-y1)
	}
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
	accentBg = color.NRGBA{108, 123, 255, 190}
)

// drawBar draws tile t's control bar w x h pixels (barButtons, and the
// volume slider on remote tiles).
func drawBar(w, h int, t Tile, hover Control) *image.NRGBA {
	u := barUnits(t)
	cv := canvas{image.NewNRGBA(image.Rect(0, 0, w, h)), float64(w) / u}
	cv.fill(panel, capsule(0.5, 0.5, u-0.5, 0.5, 0.5))
	bs := barButtons(t)
	for k, c := range bs {
		if c == ControlPin && t.Pinned {
			cv.fill(accentBg, circle(barButtonX(k), 0.5, 0.42))
		}
		if hover == c {
			cv.fill(hoverBg, circle(barButtonX(k), 0.5, 0.42))
		}
	}
	for k, c := range bs {
		cv.drawButton(c, barButtonX(k), t)
	}
	if t.Self {
		return cv.img
	}

	// volume slider
	y := 0.5
	f := min(max(t.Volume/maxVolume, 0), 1)
	xv := barSliderX0 + f*(barSliderX1-barSliderX0)
	fillC := white
	if t.Muted {
		fillC = dimWhite
	}
	cv.fill(dimWhite, capsule(barSliderX0, y, barSliderX1, y, 0.06))
	unity := barSliderX0 + (barSliderX1-barSliderX0)/maxVolume
	cv.fill(dimWhite, capsule(unity, y-0.17, unity, y+0.17, 0.025))
	cv.fill(fillC, capsule(barSliderX0, y, xv, y, 0.06))
	cv.fill(fillC, circle(xv, y, 0.16))
	return cv.img
}

// drawButton draws control c's icon centred at x.
func (cv canvas) drawButton(c Control, x float64, t Tile) {
	y, s := 0.5, 0.5
	switch c {
	case ControlEye: // watching; clicking stops watching
		cv.fill(white, arc(x, y+0.20*s, 0.44*s, 0.055*s, -2.32, -0.82))
		cv.fill(white, arc(x, y-0.20*s, 0.44*s, 0.055*s, 0.82, 2.32))
		cv.fill(white, circle(x, y, 0.13*s))

	case ControlPopOut: // arrow out of the frame, or back into it
		cv.fill(white, capsule(x-0.34*s, y-0.20*s, x-0.34*s, y+0.34*s, 0.05*s))
		cv.fill(white, capsule(x-0.34*s, y+0.34*s, x+0.20*s, y+0.34*s, 0.05*s))
		cv.fill(white, capsule(x-0.34*s, y-0.20*s, x-0.06*s, y-0.20*s, 0.05*s))
		cv.fill(white, capsule(x+0.20*s, y+0.06*s, x+0.20*s, y+0.34*s, 0.05*s))
		if t.PoppedOut {
			cv.fill(white, capsule(x+0.34*s, y-0.34*s, x-0.04*s, y+0.04*s, 0.05*s))
			cv.fill(white, capsule(x-0.04*s, y+0.04*s, x+0.20*s, y+0.04*s, 0.05*s))
			cv.fill(white, capsule(x-0.04*s, y+0.04*s, x-0.04*s, y-0.20*s, 0.05*s))
		} else {
			cv.fill(white, capsule(x-0.04*s, y+0.04*s, x+0.34*s, y-0.34*s, 0.05*s))
			cv.fill(white, capsule(x+0.10*s, y-0.34*s, x+0.34*s, y-0.34*s, 0.05*s))
			cv.fill(white, capsule(x+0.34*s, y-0.34*s, x+0.34*s, y-0.10*s, 0.05*s))
		}

	case ControlPin: // pushpin
		cv.fill(white, capsule(x-0.18*s, y-0.30*s, x+0.18*s, y-0.30*s, 0.055*s))
		cv.fill(white, polygon([2]float64{x - 0.14*s, y - 0.28*s}, [2]float64{x + 0.14*s, y - 0.28*s},
			[2]float64{x + 0.24*s, y + 0.06*s}, [2]float64{x - 0.24*s, y + 0.06*s}))
		cv.fill(white, capsule(x-0.26*s, y+0.06*s, x+0.26*s, y+0.06*s, 0.05*s))
		cv.fill(white, capsule(x, y+0.08*s, x, y+0.38*s, 0.045*s))

	case ControlExpand:
		if t.PiP { // expand: two arrows out to the corners
			for _, d := range []float64{-1, 1} {
				tx, ty := x+d*0.36*s, y-d*0.36*s
				cv.fill(white, capsule(x+d*0.08*s, y-d*0.08*s, tx, ty, 0.05*s))
				cv.fill(white, capsule(tx, ty, tx-d*0.22*s, ty, 0.05*s))
				cv.fill(white, capsule(tx, ty, tx, ty+d*0.22*s, 0.05*s))
			}
			return
		}
		// back to the corner: a frame with a small picture in its corner
		cv.fill(white, capsule(x-0.40*s, y-0.30*s, x+0.40*s, y-0.30*s, 0.05*s))
		cv.fill(white, capsule(x+0.40*s, y-0.30*s, x+0.40*s, y+0.30*s, 0.05*s))
		cv.fill(white, capsule(x+0.40*s, y+0.30*s, x-0.40*s, y+0.30*s, 0.05*s))
		cv.fill(white, capsule(x-0.40*s, y+0.30*s, x-0.40*s, y-0.30*s, 0.05*s))
		cv.fill(white, polygon([2]float64{x + 0.02*s, y - 0.20*s}, [2]float64{x + 0.30*s, y - 0.20*s},
			[2]float64{x + 0.30*s, y + 0.00*s}, [2]float64{x + 0.02*s, y + 0.00*s}))

	case ControlMute: // speaker: box and cone, then waves or a cross
		x -= 0.06
		cv.fill(white, polygon([2]float64{x - 0.34*s, y - 0.13*s}, [2]float64{x - 0.12*s, y - 0.13*s}, [2]float64{x + 0.14*s, y - 0.4*s},
			[2]float64{x + 0.14*s, y + 0.4*s}, [2]float64{x - 0.12*s, y + 0.13*s}, [2]float64{x - 0.34*s, y + 0.13*s}))
		if t.Muted {
			cv.fill(white, capsule(x+0.3*s, y-0.17*s, x+0.62*s, y+0.17*s, 0.055*s))
			cv.fill(white, capsule(x+0.3*s, y+0.17*s, x+0.62*s, y-0.17*s, 0.055*s))
		} else {
			cv.fill(white, arc(x+0.14*s, y, 0.26*s, 0.05*s, -0.85, 0.85))
			cv.fill(white, arc(x+0.14*s, y, 0.46*s, 0.05*s, -0.85, 0.85))
		}
	}
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

// textBox draws text px pixels high, on a shaded rounded panel when shade,
// no wider than maxW: a text too wide for it is drawn smaller (nil: no
// DrawText, or no text).
func textBox(text string, px float64, maxW int, shade bool) *image.NRGBA {
	if DrawText == nil {
		return nil
	}
	t := DrawText(text, px)
	if t == nil {
		return nil
	}
	padX, padY := int(math.Ceil(px*0.4)), int(math.Ceil(px*0.1))
	if w := t.Rect.Dx() + 2*padX; w > maxW && maxW > 2*padX {
		px *= float64(maxW) / float64(w)
		if t = DrawText(text, px); t == nil {
			return nil
		}
		padX, padY = int(math.Ceil(px*0.4)), int(math.Ceil(px*0.1))
	}
	w := min((t.Rect.Dx()+2*padX+1)&^1, max(maxW&^1, 2))
	h := (t.Rect.Dy() + 2*padY + 1) &^ 1
	cv := canvas{image.NewNRGBA(image.Rect(0, 0, w, h)), 1}
	if shade {
		cv.fill(panel, roundRect(0, 0, float64(w), float64(h), min(float64(h)/2, px*0.35)))
	}
	cv.over(t, padX, (h-t.Rect.Dy())/2)
	return cv.img
}

// over draws src over the canvas with its top left at x, y.
func (cv canvas) over(src *image.NRGBA, x, y int) {
	r := src.Rect.Add(image.Pt(x, y)).Intersect(cv.img.Rect)
	for py := r.Min.Y; py < r.Max.Y; py++ {
		for px := r.Min.X; px < r.Max.X; px++ {
			s := src.Pix[src.PixOffset(px-x, py-y):]
			a := float64(s[3]) / 255
			if a <= 0 {
				continue
			}
			p := cv.img.Pix[cv.img.PixOffset(px, py):]
			da := float64(p[3]) / 255
			oa := a + da*(1-a)
			for k := range 3 {
				p[k] = uint8((float64(s[k])*a + float64(p[k])*da*(1-a)) / oa)
			}
			p[3] = uint8(oa*255 + 0.5)
		}
	}
}

// placed is a w x h layer with box in it: in its top left corner, or
// centred.
func placed(w, h int, box *image.NRGBA, centre bool) *image.NRGBA {
	cv := canvas{image.NewNRGBA(image.Rect(0, 0, w, h)), 1}
	if box == nil {
		return cv.img
	}
	x, y := 0, 0
	if centre {
		x, y = (w-box.Rect.Dx())/2, (h-box.Rect.Dy())/2
	}
	cv.over(box, x, y)
	return cv.img
}
