package nui

import (
	"slices"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"blite/internal/app"
	"blite/internal/i18n"
)

func presetLabel(p app.QualityPreset) string {
	return i18n.L("{{.Resolution}}, {{.FPS}} fps", i18n.D{"Resolution": resDisplay(p.Resolution), "FPS": p.FPS})
}

// presetEntry is one chip of the preset scroller.
type presetEntry struct {
	p         app.QualityPreset
	removable bool // the user's own
}

// presetEntries lists the built-in presets then the user's; when what we
// share at matches none (a custom frame rate from Advanced), it comes first
// so the selection always shows. active is the index of the current one.
func presetEntries(sh app.ShareConfig) (es []presetEntry, active int) {
	cur := app.QualityPreset{Resolution: sh.Resolution, FPS: sh.FPS}
	if cur.Resolution == "" {
		cur.Resolution = "720p"
	}
	for _, p := range app.BuiltinPresets {
		es = append(es, presetEntry{p: p})
	}
	for _, p := range sh.Presets {
		es = append(es, presetEntry{p: p, removable: true})
	}
	active = slices.IndexFunc(es, func(e presetEntry) bool { return e.p == cur })
	if active < 0 {
		es = append([]presetEntry{{p: cur}}, es...)
		active = 0
	}
	return es, active
}

// presetBar is a horizontal scroller of quality presets with "+" at its
// right end (outside the scroller, so it always shows).
type presetBar struct {
	root     fyne.CanvasObject
	box      *fyne.Container
	scroll   *container.Scroll
	add      *widget.Button
	chips    []*presetChip
	entries  []presetEntry
	onPick   func(app.QualityPreset)
	onRemove func(app.QualityPreset)
}

func newPresetBar(onPick, onRemove func(app.QualityPreset), onAdd func()) *presetBar {
	b := &presetBar{onPick: onPick, onRemove: onRemove}
	b.box = container.NewHBox()
	b.scroll = container.NewHScroll(b.box)
	b.add = widget.NewButtonWithIcon("", theme.ContentAddIcon(), onAdd)
	b.add.Importance = widget.LowImportance
	b.root = container.NewBorder(nil, nil, nil, b.add, b.scroll)
	return b
}

func (b *presetBar) update(sh app.ShareConfig) {
	es, active := presetEntries(sh)
	if !slices.Equal(es, b.entries) {
		b.entries = es
		b.chips = make([]*presetChip, len(es))
		objs := make([]fyne.CanvasObject, len(es))
		for i, e := range es {
			p := e.p
			var remove func()
			if e.removable {
				remove = func() { b.onRemove(p) }
			}
			b.chips[i] = newPresetChip(presetLabel(p), func() {
				b.setActive(p)
				b.onPick(p)
			}, remove)
			objs[i] = b.chips[i]
		}
		b.box.Objects = objs
		b.box.Refresh()
	}
	for i, c := range b.chips {
		c.setActive(i == active)
	}
}

// setActive highlights p right away, before the state comes back.
func (b *presetBar) setActive(p app.QualityPreset) {
	for i, c := range b.chips {
		c.setActive(b.entries[i].p == p)
	}
}

// presetChip is a selectable preset; the user's own have an x to remove them.
type presetChip struct {
	widget.BaseWidget
	text     string
	active   bool
	hover    bool
	onTap    func()
	onRemove func() // nil: built in
}

const presetX = 22 // the remove area at a chip's right

func newPresetChip(text string, onTap, onRemove func()) *presetChip {
	c := &presetChip{text: text, onTap: onTap, onRemove: onRemove}
	c.ExtendBaseWidget(c)
	return c
}

func (c *presetChip) setActive(on bool) {
	if c.active != on {
		c.active = on
		c.Refresh()
	}
}

func (c *presetChip) Tapped(e *fyne.PointEvent) {
	if c.onRemove != nil && e.Position.X >= c.Size().Width-presetX {
		c.onRemove()
		return
	}
	c.onTap()
}

func (c *presetChip) Cursor() desktop.Cursor { return desktop.PointerCursor }

func (c *presetChip) MouseIn(*desktop.MouseEvent) {
	c.hover = true
	c.Refresh()
}

func (c *presetChip) MouseMoved(*desktop.MouseEvent) {}

func (c *presetChip) MouseOut() {
	c.hover = false
	c.Refresh()
}

func (c *presetChip) CreateRenderer() fyne.WidgetRenderer {
	bg := canvas.NewRectangle(colInputBg)
	bg.CornerRadius = 8
	txt := canvas.NewText(c.text, colText)
	txt.TextSize = theme.CaptionTextSize()
	x := canvas.NewImageFromResource(closeDimRes)
	x.FillMode = canvas.ImageFillContain
	r := &presetChipRenderer{c: c, bg: bg, txt: txt, x: x, objs: []fyne.CanvasObject{bg, txt, x}}
	r.Refresh()
	return r
}

type presetChipRenderer struct {
	c    *presetChip
	bg   *canvas.Rectangle
	txt  *canvas.Text
	x    *canvas.Image
	objs []fyne.CanvasObject
}

func (r *presetChipRenderer) Layout(size fyne.Size) {
	r.bg.Resize(size)
	ts := r.txt.MinSize()
	r.txt.Move(fyne.NewPos(12, (size.Height-ts.Height)/2))
	r.txt.Resize(ts)
	const xs = 12
	r.x.Move(fyne.NewPos(size.Width-presetX+(presetX-xs)/2-2, (size.Height-xs)/2))
	r.x.Resize(fyne.NewSize(xs, xs))
}

func (r *presetChipRenderer) MinSize() fyne.Size {
	ts := r.txt.MinSize()
	w := ts.Width + 24
	if r.c.onRemove != nil {
		w += presetX - 6
	}
	return fyne.NewSize(w, max(32, ts.Height+12))
}

func (r *presetChipRenderer) Refresh() {
	switch {
	case r.c.active:
		r.bg.FillColor = colAccentFill
		r.bg.StrokeColor = colAccent
		r.bg.StrokeWidth = 1.5
	case r.c.hover:
		r.bg.FillColor = colButton
		r.bg.StrokeColor = colCardBorder
		r.bg.StrokeWidth = 1
	default:
		r.bg.FillColor = colInputBg
		r.bg.StrokeColor = colCardBorder
		r.bg.StrokeWidth = 1
	}
	r.txt.Text = r.c.text
	r.txt.TextStyle = fyne.TextStyle{Bold: r.c.active}
	r.txt.TextSize = theme.CaptionTextSize()
	if r.c.onRemove != nil {
		r.x.Show()
	} else {
		r.x.Hide()
	}
	r.bg.Refresh()
	r.txt.Refresh()
	r.x.Refresh()
}

func (r *presetChipRenderer) Objects() []fyne.CanvasObject { return r.objs }
func (r *presetChipRenderer) Destroy()                     {}

// applyPreset shares at p from now on (Home); it keeps the other share
// options as they are.
func (u *UI) applyPreset(p app.QualityPreset) {
	sh := normalizeShareState(u.st)
	u.do(func() error {
		if err := u.a.SetResolution(p.Resolution); err != nil {
			return err
		}
		return u.a.SetShareOptions(p.FPS, sh.SlowViewer, sh.MinHeight, sh.SmallerCopy, sh.UploadKbps)
	})
}

func (u *UI) removePreset(p app.QualityPreset) {
	u.do(func() error { return u.a.RemoveQualityPreset(p.Resolution, p.FPS) })
}

// presetDialog asks for a resolution and frame rate, keeps them as a preset
// and then runs picked with it.
func (u *UI) presetDialog(picked func(app.QualityPreset)) {
	sh := normalizeShareState(u.st)
	res := sh.Resolution
	resSeg := newSegmented([]segOption{
		{Value: "720p", Label: "720p"},
		{Value: "1080p", Label: "1080p"},
		{Value: "2160p", Label: "4K"},
	}, func(v string) { res = v })
	resSeg.SetSelected(res)
	var labels []string
	fpsByLabel := map[string]int{}
	for fps := 10; fps <= 60; fps += 5 {
		l := i18n.L("{{.FPS}} fps", i18n.D{"FPS": fps})
		labels = append(labels, l)
		fpsByLabel[l] = fps
	}
	fpsSel := widget.NewSelect(labels, nil)
	fpsSel.SetSelected(i18n.L("{{.FPS}} fps", i18n.D{"FPS": sh.FPS}))
	body := container.NewVBox(bold(i18n.L("Resolution")), resSeg, bold(i18n.L("Frame rate")), fpsSel)
	d := dialog.NewCustomConfirm(i18n.L("New quality preset"), i18n.L("Add"), i18n.L("Cancel"), body, func(ok bool) {
		fps, known := fpsByLabel[fpsSel.Selected]
		if !ok || !known || strings.TrimSpace(res) == "" {
			return
		}
		p := app.QualityPreset{Resolution: res, FPS: fps}
		go func() {
			if err := u.a.AddQualityPreset(p.Resolution, p.FPS); err != nil {
				u.later(func() { u.showErr(err) })
				return
			}
			u.later(func() { picked(p) })
		}()
	}, u.win)
	d.Resize(fyne.NewSize(420, 280))
	d.Show()
}
