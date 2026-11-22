package nui

import (
	"image/color"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"
)

// iconToggle is a compact icon button like the eye: the caller sets its
// icon per state. While glowing (someone makes sound) an accent halo
// pulses behind the icon. Hovering shows its meaning as a tooltip.
type iconToggle struct {
	widget.BaseWidget
	res     fyne.Resource
	meaning string
	glow    bool
	hover   bool
	onTap   func()
	tips    *tooltips
	aspect  float32 // the icon's width over its height; 0 = square

	pulse *fyne.Animation
	halo  float32 // 0..1, the pulse's phase
}

func newIconToggle(tips *tooltips, res fyne.Resource, onTap func()) *iconToggle {
	t := &iconToggle{res: res, onTap: onTap, tips: tips}
	t.ExtendBaseWidget(t)
	return t
}

// Set shows res; meaning, the tooltip, says what it shows or a tap does.
func (t *iconToggle) Set(res fyne.Resource, meaning string, glow bool) {
	if t.res == res && t.meaning == meaning && t.glow == glow {
		return
	}
	t.res, t.meaning = res, meaning
	if t.hover {
		t.tips.Show(t, meaning)
	}
	t.setGlow(glow)
	t.Refresh()
}

func (t *iconToggle) setGlow(on bool) {
	if t.glow == on {
		return
	}
	t.glow = on
	if !on {
		if t.pulse != nil {
			t.pulse.Stop()
			t.pulse = nil
		}
		t.halo = 0
		return
	}
	t.pulse = &fyne.Animation{
		Duration:    600 * time.Millisecond,
		AutoReverse: true,
		RepeatCount: fyne.AnimationRepeatForever,
		Curve:       fyne.AnimationEaseInOut,
		Tick: func(f float32) {
			t.halo = f
			t.Refresh()
		},
	}
	t.pulse.Start()
}

func (t *iconToggle) Tapped(*fyne.PointEvent) {
	if t.onTap != nil {
		t.onTap()
	}
}

func (t *iconToggle) Cursor() desktop.Cursor {
	if t.onTap != nil {
		return desktop.PointerCursor
	}
	return desktop.DefaultCursor
}

func (t *iconToggle) MouseIn(*desktop.MouseEvent) {
	t.hover = true
	t.tips.Show(t, t.meaning)
	t.Refresh()
}

func (t *iconToggle) MouseMoved(*desktop.MouseEvent) {}

func (t *iconToggle) MouseOut() {
	t.hover = false
	t.tips.Hide(t)
	t.Refresh()
}

func (t *iconToggle) CreateRenderer() fyne.WidgetRenderer {
	bg := canvas.NewRectangle(color.Transparent)
	bg.CornerRadius = 8
	halo := canvas.NewCircle(color.Transparent)
	img := canvas.NewImageFromResource(t.res)
	img.FillMode = canvas.ImageFillContain
	r := &iconToggleRenderer{t: t, bg: bg, halo: halo, img: img, objs: []fyne.CanvasObject{bg, halo, img}}
	r.Refresh()
	return r
}

type iconToggleRenderer struct {
	t    *iconToggle
	bg   *canvas.Rectangle
	halo *canvas.Circle
	img  *canvas.Image
	objs []fyne.CanvasObject
}

func (r *iconToggleRenderer) Layout(size fyne.Size) {
	r.bg.Move(fyne.NewPos(0, 0))
	r.bg.Resize(size)
	const ic, hs = 20, 28
	r.halo.Move(fyne.NewPos((size.Width-hs)/2, (size.Height-hs)/2))
	r.halo.Resize(fyne.NewSize(hs, hs))
	w := ic * max(1, r.t.aspect)
	r.img.Move(fyne.NewPos((size.Width-w)/2, (size.Height-ic)/2))
	r.img.Resize(fyne.NewSize(w, ic))
}

func (r *iconToggleRenderer) MinSize() fyne.Size {
	return fyne.NewSize(14+20*max(1, r.t.aspect), 34)
}

func (r *iconToggleRenderer) Refresh() {
	if r.t.hover && r.t.onTap != nil {
		r.bg.FillColor = colButton
	} else {
		r.bg.FillColor = color.Transparent
	}
	if r.t.glow {
		a := colAccent
		a.A = uint8(0x18 + 0x40*r.t.halo)
		r.halo.FillColor = a
	} else {
		r.halo.FillColor = color.Transparent
	}
	r.img.Resource = r.t.res
	r.bg.Refresh()
	r.halo.Refresh()
	r.img.Refresh()
}

func (r *iconToggleRenderer) Objects() []fyne.CanvasObject { return r.objs }

func (r *iconToggleRenderer) Destroy() {
	r.t.tips.Hide(r.t)
	if r.t.pulse != nil {
		r.t.pulse.Stop()
		r.t.pulse = nil
		r.t.glow = false
	}
}

// slideBox reveals or hides its content by growing or shrinking its
// height, clipped, instead of popping it in.
type slideBox struct {
	widget.BaseWidget
	content fyne.CanvasObject
	clip    *container.Scroll
	open    bool
	f       float32 // 0 hidden .. 1 shown
	anim    *fyne.Animation
	// relayout refreshes the container holding the box: Fyne doesn't
	// lay a parent out again when a child's MinSize changes.
	relayout func()
}

func newSlideBox(content fyne.CanvasObject) *slideBox {
	b := &slideBox{content: content}
	b.clip = container.NewVScroll(content) // clips; never scrolled by hand
	b.ExtendBaseWidget(b)
	b.Hide()
	return b
}

// SetOpen slides the content in or out; off screen it just jumps.
func (b *slideBox) SetOpen(open bool) {
	if b.open == open {
		return
	}
	b.open = open
	if b.anim != nil {
		b.anim.Stop()
		b.anim = nil
	}
	if fyne.CurrentApp() == nil || fyne.CurrentApp().Driver().CanvasForObject(b) == nil { // not on screen yet: no slide
		b.f = 0
		if open {
			b.f = 1
			b.Show()
		} else {
			b.Hide()
		}
		b.Refresh()
		return
	}
	if !b.Visible() && !open {
		return
	}
	from, to := b.f, float32(0)
	if open {
		to = 1
		b.Show()
	}
	b.anim = fyne.NewAnimation(160*time.Millisecond, func(p float32) {
		b.f = from + (to-from)*p
		if p == 1 {
			b.anim = nil
			if !b.open {
				b.Hide()
			}
		}
		b.Refresh()
		if b.relayout != nil {
			b.relayout()
		}
	})
	b.anim.Curve = fyne.AnimationEaseOut
	b.anim.Start()
}

func (b *slideBox) CreateRenderer() fyne.WidgetRenderer {
	return &slideRenderer{b: b}
}

type slideRenderer struct{ b *slideBox }

func (r *slideRenderer) Layout(size fyne.Size) {
	r.b.clip.Move(fyne.NewPos(0, 0))
	r.b.clip.Resize(size)
	// The content keeps its full height, its bottom in view: it slides
	// down from under the row above rather than squashing.
	r.b.clip.Offset = fyne.NewPos(0, max(0, r.b.content.MinSize().Height-size.Height))
	r.b.clip.Refresh()
}

func (r *slideRenderer) MinSize() fyne.Size {
	m := r.b.content.MinSize()
	return fyne.NewSize(m.Width, m.Height*r.b.f)
}

func (r *slideRenderer) Refresh() { r.Layout(r.b.Size()) }

func (r *slideRenderer) Objects() []fyne.CanvasObject { return []fyne.CanvasObject{r.b.clip} }
func (r *slideRenderer) Destroy() {
	if r.b.anim != nil {
		r.b.anim.Stop()
	}
}
