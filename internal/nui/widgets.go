package nui

import (
	"image/color"
	"unicode"
	"unicode/utf8"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"blite/internal/feeds"
)

// newCard wraps body in a rounded dark card with a subtle border and optional
// wrapping title.
func newCard(title string, body fyne.CanvasObject) fyne.CanvasObject {
	bg := canvas.NewRectangle(colCardBg)
	bg.StrokeColor = colCardBorder
	bg.StrokeWidth = 1
	bg.CornerRadius = 12
	inner := body
	if title != "" {
		hdr := label(title)
		hdr.TextStyle.Bold = true
		hdr.SizeName = theme.SizeNameSubHeadingText
		inner = container.NewBorder(hdr, nil, nil, nil, body)
	}
	return container.NewStack(bg, container.New(&insetLayout{pad: 14}, inner))
}

type insetLayout struct {
	pad      float32
	padRight float32
}

func (l *insetLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	right := l.pad
	if l.padRight > 0 {
		right = l.padRight
	}
	w := max(0, size.Width-l.pad-right)
	h := max(0, size.Height-2*l.pad)
	for _, o := range objs {
		o.Move(fyne.NewPos(l.pad, l.pad))
		o.Resize(fyne.NewSize(w, h))
	}
}

func (l *insetLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	right := l.pad
	if l.padRight > 0 {
		right = l.padRight
	}
	var w, h float32
	for _, o := range objs {
		if !o.Visible() {
			continue
		}
		m := o.MinSize()
		w = max(w, m.Width)
		h = max(h, m.Height)
	}
	return fyne.NewSize(w+l.pad+right, h+2*l.pad)
}

// avatarCircle draws a deterministic coloured circle with the person's initial.
type avatarCircle struct {
	widget.BaseWidget
	name string
	diam float32
}

func newAvatar(name string, diam float32) *avatarCircle {
	a := &avatarCircle{name: name, diam: diam}
	a.ExtendBaseWidget(a)
	return a
}

func (a *avatarCircle) SetName(name string) {
	if a.name == name {
		return
	}
	a.name = name
	a.Refresh()
}

func initialRune(name string) string {
	for _, r := range name {
		if !unicode.IsSpace(r) {
			return string(unicode.ToUpper(r))
		}
	}
	r, _ := utf8.DecodeRuneInString(name)
	if r == utf8.RuneError || r == 0 {
		return "?"
	}
	return string(unicode.ToUpper(r))
}

func (a *avatarCircle) CreateRenderer() fyne.WidgetRenderer {
	circ := canvas.NewCircle(avatarColor(a.name))
	txt := canvas.NewText(initialRune(a.name), colWhite)
	txt.Alignment = fyne.TextAlignCenter
	txt.TextStyle = fyne.TextStyle{Bold: true}
	txt.TextSize = a.diam * 0.44
	return &avatarRenderer{a: a, circ: circ, txt: txt, objs: []fyne.CanvasObject{circ, txt}}
}

type avatarRenderer struct {
	a    *avatarCircle
	circ *canvas.Circle
	txt  *canvas.Text
	objs []fyne.CanvasObject
}

func (r *avatarRenderer) Layout(size fyne.Size) {
	d := min(aSize(r.a.diam), min(size.Width, size.Height))
	x := (size.Width - d) / 2
	y := (size.Height - d) / 2
	r.circ.Move(fyne.NewPos(x, y))
	r.circ.Resize(fyne.NewSize(d, d))
	ts := r.txt.MinSize()
	r.txt.Move(fyne.NewPos(x, y+(d-ts.Height)/2))
	r.txt.Resize(fyne.NewSize(d, ts.Height))
}

func aSize(d float32) float32 {
	if d <= 0 {
		return 34
	}
	return d
}

func (r *avatarRenderer) MinSize() fyne.Size {
	d := aSize(r.a.diam)
	return fyne.NewSize(d, d)
}

func (r *avatarRenderer) Refresh() {
	r.circ.FillColor = avatarColor(r.a.name)
	r.txt.Text = initialRune(r.a.name)
	r.txt.TextSize = aSize(r.a.diam) * 0.44
	r.circ.Refresh()
	r.txt.Refresh()
}

func (r *avatarRenderer) Objects() []fyne.CanvasObject { return r.objs }
func (r *avatarRenderer) Destroy()                     {}

// nameTag renders "Name #tag" with a dimmed #tag and optional badge ("(you)", "(owner)").
type nameTag struct {
	widget.BaseWidget
	name        string
	tag         string
	badge       string
	accentBadge bool
}

func newNameTag() *nameTag {
	n := &nameTag{}
	n.ExtendBaseWidget(n)
	return n
}

func (n *nameTag) Set(name, tag, badge string, accentBadge bool) {
	if n.name == name && n.tag == tag && n.badge == badge && n.accentBadge == accentBadge {
		return
	}
	n.name, n.tag, n.badge, n.accentBadge = name, tag, badge, accentBadge
	n.Refresh()
}

func (n *nameTag) CreateRenderer() fyne.WidgetRenderer {
	nm := canvas.NewText("", colText)
	nm.TextStyle = fyne.TextStyle{Bold: true}
	nm.TextSize = theme.TextSize()
	tg := canvas.NewText("", colTextDim)
	tg.TextSize = theme.CaptionTextSize()
	bd := canvas.NewText("", colTextDim)
	bd.TextSize = theme.CaptionTextSize()
	re := &nameTagRenderer{n: n, nm: nm, tg: tg, bd: bd, objs: []fyne.CanvasObject{nm, tg, bd}}
	re.Refresh()
	return re
}

type nameTagRenderer struct {
	n          *nameTag
	nm, tg, bd *canvas.Text
	objs       []fyne.CanvasObject
}

func (r *nameTagRenderer) Layout(size fyne.Size) {
	ns := r.nm.MinSize()
	x := float32(0)
	r.nm.Move(fyne.NewPos(x, (size.Height-ns.Height)/2))
	r.nm.Resize(ns)
	x += ns.Width
	if r.tg.Visible() {
		x += 5
		ts := r.tg.MinSize()
		r.tg.Move(fyne.NewPos(x, (size.Height-ts.Height)/2+1))
		r.tg.Resize(ts)
		x += ts.Width
	}
	if r.bd.Visible() {
		x += 8
		bs := r.bd.MinSize()
		r.bd.Move(fyne.NewPos(x, (size.Height-bs.Height)/2+1))
		r.bd.Resize(bs)
	}
}

func (r *nameTagRenderer) MinSize() fyne.Size {
	ns := r.nm.MinSize()
	w, h := ns.Width, ns.Height
	if r.tg.Visible() {
		w += 5 + r.tg.MinSize().Width
	}
	if r.bd.Visible() {
		w += 8 + r.bd.MinSize().Width
	}
	return fyne.NewSize(min(w, 220), h+2)
}

func (r *nameTagRenderer) Refresh() {
	r.nm.Text = r.n.name
	r.nm.TextSize = theme.TextSize()
	if r.n.tag != "" {
		r.tg.Text = "#" + r.n.tag
		r.tg.TextSize = theme.CaptionTextSize()
		r.tg.Show()
	} else {
		r.tg.Hide()
	}
	if r.n.badge != "" {
		r.bd.Text = r.n.badge
		r.bd.TextSize = theme.CaptionTextSize()
		if r.n.accentBadge {
			r.bd.Color = colAccent
		} else {
			r.bd.Color = colTextDim
		}
		r.bd.Show()
	} else {
		r.bd.Hide()
	}
	r.nm.Refresh()
	r.tg.Refresh()
	r.bd.Refresh()
}

func (r *nameTagRenderer) Objects() []fyne.CanvasObject { return r.objs }
func (r *nameTagRenderer) Destroy()                     {}

// eyeToggle is the 3-state watching toggle (white open, white closed, grey not sharing).
type eyeToggle struct {
	widget.BaseWidget
	eye   feeds.Eye
	hover bool
	onTap func()
	tips  *tooltips
	tip   string // the tooltip: what a tap does, or why it can't
}

func newEyeToggle(tips *tooltips, onTap func()) *eyeToggle {
	e := &eyeToggle{eye: feeds.EyeGrey, onTap: onTap, tips: tips}
	e.ExtendBaseWidget(e)
	return e
}

// SetTip sets the tooltip.
func (e *eyeToggle) SetTip(tip string) {
	if e.tip == tip {
		return
	}
	e.tip = tip
	if e.hover {
		e.tips.Show(e, tip)
	}
}

func (e *eyeToggle) SetEye(st feeds.Eye) {
	if st == "" {
		st = feeds.EyeGrey
	}
	if e.eye == st {
		return
	}
	e.eye = st
	e.Refresh()
}

func (e *eyeToggle) active() bool {
	return e.eye == feeds.EyeOpen || e.eye == feeds.EyeClosed
}

func (e *eyeToggle) Tapped(*fyne.PointEvent) {
	if e.active() && e.onTap != nil {
		e.onTap()
	}
}

func (e *eyeToggle) Cursor() desktop.Cursor {
	if e.active() {
		return desktop.PointerCursor
	}
	return desktop.DefaultCursor
}

func (e *eyeToggle) MouseIn(*desktop.MouseEvent) {
	e.hover = true
	e.tips.Show(e, e.tip)
	if e.active() {
		e.Refresh()
	}
}

func (e *eyeToggle) MouseMoved(*desktop.MouseEvent) {}

func (e *eyeToggle) MouseOut() {
	e.tips.Hide(e)
	if e.hover {
		e.hover = false
		e.Refresh()
	}
}

func (e *eyeToggle) CreateRenderer() fyne.WidgetRenderer {
	bg := canvas.NewRectangle(color.Transparent)
	bg.CornerRadius = 8
	img := canvas.NewImageFromResource(eyeGreyRes)
	img.FillMode = canvas.ImageFillContain
	re := &eyeRenderer{e: e, bg: bg, img: img, objs: []fyne.CanvasObject{bg, img}}
	re.Refresh()
	return re
}

type eyeRenderer struct {
	e    *eyeToggle
	bg   *canvas.Rectangle
	img  *canvas.Image
	objs []fyne.CanvasObject
}

func (r *eyeRenderer) Layout(size fyne.Size) {
	r.bg.Move(fyne.NewPos(0, 0))
	r.bg.Resize(size)
	const ic = 20
	r.img.Move(fyne.NewPos((size.Width-ic)/2, (size.Height-ic)/2))
	r.img.Resize(fyne.NewSize(ic, ic))
}

func (r *eyeRenderer) MinSize() fyne.Size { return fyne.NewSize(34, 34) }

func (r *eyeRenderer) Refresh() {
	if r.e.hover && r.e.active() {
		r.bg.FillColor = colButton
	} else {
		r.bg.FillColor = color.Transparent
	}
	switch r.e.eye {
	case feeds.EyeOpen:
		r.img.Resource = eyeOpenWhiteRes
	case feeds.EyeClosed:
		r.img.Resource = eyeClosedWhiteRes
	default:
		r.img.Resource = eyeGreyRes
	}
	r.bg.Refresh()
	r.img.Refresh()
}

func (r *eyeRenderer) Objects() []fyne.CanvasObject { return r.objs }
func (r *eyeRenderer) Destroy()                     { r.e.tips.Hide(r.e) }

// iconDotButton is a compact header icon button (Gear, "?") with an optional
// notification dot at top right.
type iconDotButton struct {
	widget.BaseWidget
	icon  fyne.Resource
	dot   bool
	hover bool
	onTap func(*iconDotButton)
}

func newIconDotButton(icon fyne.Resource, onTap func(*iconDotButton)) *iconDotButton {
	b := &iconDotButton{icon: icon, onTap: onTap}
	b.ExtendBaseWidget(b)
	return b
}

func (b *iconDotButton) SetDot(on bool) {
	if b.dot == on {
		return
	}
	b.dot = on
	b.Refresh()
}

func (b *iconDotButton) Tapped(*fyne.PointEvent) {
	if b.onTap != nil {
		b.onTap(b)
	}
}

func (b *iconDotButton) Cursor() desktop.Cursor { return desktop.PointerCursor }

func (b *iconDotButton) MouseIn(*desktop.MouseEvent) {
	b.hover = true
	b.Refresh()
}

func (b *iconDotButton) MouseMoved(*desktop.MouseEvent) {}

func (b *iconDotButton) MouseOut() {
	b.hover = false
	b.Refresh()
}

func (b *iconDotButton) CreateRenderer() fyne.WidgetRenderer {
	bg := canvas.NewRectangle(colButton)
	bg.StrokeColor = colCardBorder
	bg.StrokeWidth = 1
	bg.CornerRadius = 8
	img := canvas.NewImageFromResource(b.icon)
	img.FillMode = canvas.ImageFillContain
	dot := canvas.NewCircle(colAccent)
	dot.StrokeColor = colBg
	dot.StrokeWidth = 2
	re := &iconDotRenderer{b: b, bg: bg, img: img, dot: dot, objs: []fyne.CanvasObject{bg, img, dot}}
	re.Refresh()
	return re
}

type iconDotRenderer struct {
	b    *iconDotButton
	bg   *canvas.Rectangle
	img  *canvas.Image
	dot  *canvas.Circle
	objs []fyne.CanvasObject
}

func (r *iconDotRenderer) Layout(size fyne.Size) {
	r.bg.Move(fyne.NewPos(0, 0))
	r.bg.Resize(size)
	const ic = 18
	r.img.Move(fyne.NewPos((size.Width-ic)/2, (size.Height-ic)/2))
	r.img.Resize(fyne.NewSize(ic, ic))
	r.dot.Move(fyne.NewPos(size.Width-10, 2))
	r.dot.Resize(fyne.NewSize(8, 8))
}

func (r *iconDotRenderer) MinSize() fyne.Size { return fyne.NewSize(36, 36) }

func (r *iconDotRenderer) Refresh() {
	if r.b.hover {
		r.bg.FillColor = colCardBorder
	} else {
		r.bg.FillColor = colButton
	}
	r.img.Resource = r.b.icon
	if r.b.dot {
		r.dot.Show()
	} else {
		r.dot.Hide()
	}
	r.bg.Refresh()
	r.img.Refresh()
	r.dot.Refresh()
}

func (r *iconDotRenderer) Objects() []fyne.CanvasObject { return r.objs }
func (r *iconDotRenderer) Destroy()                     {}

// chipButton is a compact rounded badge/chip button (lock badge in header,
// settings chips on Home).
type chipButton struct {
	widget.BaseWidget
	icon   fyne.Resource
	text   string
	fill   color.Color
	border color.Color
	fg     color.Color
	hover  bool
	onTap  func()
}

func newChip(icon fyne.Resource, text string, onTap func()) *chipButton {
	c := &chipButton{icon: icon, text: text, fill: colButton, border: colCardBorder, fg: colText, onTap: onTap}
	c.ExtendBaseWidget(c)
	return c
}

func (c *chipButton) SetContent(icon fyne.Resource, text string, fill, border, fg color.Color) {
	c.icon, c.text, c.fill, c.border, c.fg = icon, text, fill, border, fg
	c.Refresh()
}

func (c *chipButton) Tapped(*fyne.PointEvent) {
	if c.onTap != nil {
		c.onTap()
	}
}

func (c *chipButton) Cursor() desktop.Cursor {
	if c.onTap != nil {
		return desktop.PointerCursor
	}
	return desktop.DefaultCursor
}

func (c *chipButton) MouseIn(*desktop.MouseEvent) {
	if c.onTap != nil {
		c.hover = true
		c.Refresh()
	}
}

func (c *chipButton) MouseMoved(*desktop.MouseEvent) {}

func (c *chipButton) MouseOut() {
	if c.hover {
		c.hover = false
		c.Refresh()
	}
}

func (c *chipButton) CreateRenderer() fyne.WidgetRenderer {
	bg := canvas.NewRectangle(c.fill)
	bg.StrokeColor = c.border
	bg.StrokeWidth = 1
	bg.CornerRadius = 8
	var img *canvas.Image
	if c.icon != nil {
		img = canvas.NewImageFromResource(c.icon)
	} else {
		img = canvas.NewImageFromResource(lockClosedRes)
	}
	img.FillMode = canvas.ImageFillContain
	txt := canvas.NewText(c.text, c.fg)
	txt.TextSize = theme.CaptionTextSize()
	txt.TextStyle = fyne.TextStyle{Bold: true}
	re := &chipRenderer{c: c, bg: bg, img: img, txt: txt, objs: []fyne.CanvasObject{bg, img, txt}}
	re.Refresh()
	return re
}

type chipRenderer struct {
	c    *chipButton
	bg   *canvas.Rectangle
	img  *canvas.Image
	txt  *canvas.Text
	objs []fyne.CanvasObject
}

func (r *chipRenderer) Layout(size fyne.Size) {
	r.bg.Move(fyne.NewPos(0, 0))
	r.bg.Resize(size)
	x := float32(10)
	if r.img.Visible() {
		const ic = 15
		r.img.Move(fyne.NewPos(x, (size.Height-ic)/2))
		r.img.Resize(fyne.NewSize(ic, ic))
		x += ic + 6
	}
	ts := r.txt.MinSize()
	r.txt.Move(fyne.NewPos(x, (size.Height-ts.Height)/2))
	r.txt.Resize(ts)
}

func (r *chipRenderer) MinSize() fyne.Size {
	ts := r.txt.MinSize()
	w := ts.Width + 20
	if r.c.icon != nil {
		w += 21
	}
	return fyne.NewSize(w, max(32, ts.Height+12))
}

func (r *chipRenderer) Refresh() {
	r.bg.FillColor = r.c.fill
	r.bg.StrokeColor = r.c.border
	if r.c.hover {
		r.bg.StrokeColor = colAccent
	}
	if r.c.icon != nil {
		r.img.Resource = r.c.icon
		r.img.Show()
	} else {
		r.img.Hide()
	}
	r.txt.Text = r.c.text
	r.txt.Color = r.c.fg
	r.txt.TextSize = theme.CaptionTextSize()
	r.bg.Refresh()
	r.img.Refresh()
	r.txt.Refresh()
}

func (r *chipRenderer) Objects() []fyne.CanvasObject { return r.objs }
func (r *chipRenderer) Destroy()                     {}

// segOption is one choice in a segmented button group.
type segOption struct {
	Value      string
	Label      string
	Retappable bool // fire onChange even when tapping the already-selected option
}

// segmented highlights the selected option with the accent border and fill.
type segmented struct {
	widget.BaseWidget
	cols     int
	opts     []segOption
	selected string
	onChange func(string)
	items    []*segButton
	box      *fyne.Container
}

func newSegmented(opts []segOption, onChange func(string)) *segmented {
	return newSegmentedCols(0, opts, onChange)
}

func newSegmentedCols(cols int, opts []segOption, onChange func(string)) *segmented {
	s := &segmented{cols: cols, opts: opts, onChange: onChange}
	s.rebuild()
	s.ExtendBaseWidget(s)
	return s
}

func (s *segmented) gridCols(n int) int {
	if s.cols > 0 {
		return s.cols
	}
	return max(1, n)
}

func (s *segmented) rebuild() {
	s.items = make([]*segButton, len(s.opts))
	objs := make([]fyne.CanvasObject, len(s.opts))
	for i, o := range s.opts {
		val, retap := o.Value, o.Retappable
		b := newSegButton(o.Label, o.Value == s.selected, func() {
			if s.selected == val && !retap {
				return
			}
			s.selected = val
			s.syncButtons()
			if s.onChange != nil {
				s.onChange(val)
			}
		})
		s.items[i] = b
		objs[i] = b
	}
	cols := s.gridCols(len(objs))
	if s.box == nil {
		s.box = container.NewGridWithColumns(cols, objs...)
	} else {
		s.box.Layout = container.NewGridWithColumns(cols).Layout
		s.box.Objects = objs
		s.box.Refresh()
	}
}

func (s *segmented) SetOptions(opts []segOption, selected string) {
	same := len(opts) == len(s.opts)
	if same {
		for i := range opts {
			if opts[i] != s.opts[i] {
				same = false
				break
			}
		}
	}
	if same {
		s.SetSelected(selected)
		return
	}
	s.opts = opts
	s.selected = selected
	s.rebuild()
	s.Refresh()
}

func (s *segmented) syncButtons() {
	for i, b := range s.items {
		b.setActive(s.opts[i].Value == s.selected)
	}
}

func (s *segmented) SetSelected(val string) {
	if s.selected == val {
		return
	}
	s.selected = val
	s.syncButtons()
}

func (s *segmented) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(s.box)
}

type segButton struct {
	widget.BaseWidget
	lbl    *widget.Label
	bg     *canvas.Rectangle
	active bool
	hover  bool
	onTap  func()
}

func newSegButton(text string, active bool, onTap func()) *segButton {
	lbl := label(text)
	lbl.Alignment = fyne.TextAlignCenter
	bg := canvas.NewRectangle(colInputBg)
	bg.CornerRadius = 8
	b := &segButton{lbl: lbl, bg: bg, active: active, onTap: onTap}
	b.applyStyle()
	b.ExtendBaseWidget(b)
	return b
}

func (b *segButton) setActive(on bool) {
	if b.active == on {
		return
	}
	b.active = on
	b.applyStyle()
	b.Refresh()
}

func (b *segButton) applyStyle() {
	if b.active {
		b.bg.FillColor = colAccentFill
		b.bg.StrokeColor = colAccent
		b.bg.StrokeWidth = 1.5
		b.lbl.TextStyle.Bold = true
	} else {
		if b.hover {
			b.bg.FillColor = colButton
		} else {
			b.bg.FillColor = colInputBg
		}
		b.bg.StrokeColor = colCardBorder
		b.bg.StrokeWidth = 1
		b.lbl.TextStyle.Bold = false
	}
}

func (b *segButton) Tapped(*fyne.PointEvent) {
	if b.onTap != nil {
		b.onTap()
	}
}

func (b *segButton) Cursor() desktop.Cursor { return desktop.PointerCursor }

func (b *segButton) MouseIn(*desktop.MouseEvent) {
	b.hover = true
	b.applyStyle()
	b.bg.Refresh()
}

func (b *segButton) MouseMoved(*desktop.MouseEvent) {}

func (b *segButton) MouseOut() {
	b.hover = false
	b.applyStyle()
	b.bg.Refresh()
}

func (b *segButton) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(container.NewStack(b.bg, container.New(&insetLayout{pad: 4}, b.lbl)))
}

// toggleSwitch is a pill-shaped on/off switch.
type toggleSwitch struct {
	widget.BaseWidget
	checked  bool
	disabled bool
	onChange func(bool)
}

func newToggleSwitch(onChange func(bool)) *toggleSwitch {
	t := &toggleSwitch{onChange: onChange}
	t.ExtendBaseWidget(t)
	return t
}

func (t *toggleSwitch) SetChecked(on bool) {
	if t.checked == on {
		return
	}
	t.checked = on
	t.Refresh()
}

func (t *toggleSwitch) SetDisabled(off bool) {
	if t.disabled == off {
		return
	}
	t.disabled = off
	t.Refresh()
}

func (t *toggleSwitch) Tapped(*fyne.PointEvent) {
	if t.disabled {
		return
	}
	t.checked = !t.checked
	t.Refresh()
	if t.onChange != nil {
		t.onChange(t.checked)
	}
}

func (t *toggleSwitch) Cursor() desktop.Cursor {
	if t.disabled {
		return desktop.DefaultCursor
	}
	return desktop.PointerCursor
}

func (t *toggleSwitch) CreateRenderer() fyne.WidgetRenderer {
	track := canvas.NewRectangle(colInputBg)
	track.CornerRadius = 11
	track.StrokeColor = colInputBord
	track.StrokeWidth = 1
	thumb := canvas.NewCircle(colWhite)
	re := &switchRenderer{t: t, track: track, thumb: thumb, objs: []fyne.CanvasObject{track, thumb}}
	re.Refresh()
	return re
}

type switchRenderer struct {
	t     *toggleSwitch
	track *canvas.Rectangle
	thumb *canvas.Circle
	objs  []fyne.CanvasObject
}

func (r *switchRenderer) Layout(size fyne.Size) {
	const w, h, d = 40, 22, 16
	x := (size.Width - w) / 2
	y := (size.Height - h) / 2
	r.track.Move(fyne.NewPos(x, y))
	r.track.Resize(fyne.NewSize(w, h))
	tx := x + 3
	if r.t.checked {
		tx = x + w - d - 3
	}
	r.thumb.Move(fyne.NewPos(tx, y+(h-d)/2))
	r.thumb.Resize(fyne.NewSize(d, d))
}

func (r *switchRenderer) MinSize() fyne.Size { return fyne.NewSize(40, 24) }

func (r *switchRenderer) Refresh() {
	switch {
	case r.t.disabled:
		r.track.FillColor = colButtonDis
		r.track.StrokeColor = colCardBorder
		r.thumb.FillColor = colTextDis
	case r.t.checked:
		r.track.FillColor = colAccent
		r.track.StrokeColor = colAccent
		r.thumb.FillColor = colWhite
	default:
		r.track.FillColor = colInputBg
		r.track.StrokeColor = colInputBord
		r.thumb.FillColor = colTextDim
	}
	r.track.Refresh()
	r.thumb.Refresh()
	r.Layout(r.t.Size())
}

func (r *switchRenderer) Objects() []fyne.CanvasObject { return r.objs }
func (r *switchRenderer) Destroy()                     {}

// tapArea wraps content to make it clickable and hover-aware without stealing
// events from sibling widgets (such as the eye toggle on the right of a row).
type tapArea struct {
	widget.BaseWidget
	body    fyne.CanvasObject
	onTap   func()
	onHover func(bool)
}

func newTapArea(body fyne.CanvasObject, onTap func(), onHover func(bool)) *tapArea {
	t := &tapArea{body: body, onTap: onTap, onHover: onHover}
	t.ExtendBaseWidget(t)
	return t
}

func (t *tapArea) Tapped(*fyne.PointEvent) {
	if t.onTap != nil {
		t.onTap()
	}
}

func (t *tapArea) Cursor() desktop.Cursor {
	if t.onTap != nil {
		return desktop.PointerCursor
	}
	return desktop.DefaultCursor
}

func (t *tapArea) MouseIn(*desktop.MouseEvent) {
	if t.onHover != nil && t.onTap != nil {
		t.onHover(true)
	}
}

func (t *tapArea) MouseMoved(*desktop.MouseEvent) {}

func (t *tapArea) MouseOut() {
	if t.onHover != nil {
		t.onHover(false)
	}
}

func (t *tapArea) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(t.body)
}

// showMenuBelow opens items as a popup menu aligned below btn without clipping
// off the right edge of the window.
func showMenuBelow(btn fyne.CanvasObject, items ...*fyne.MenuItem) {
	drv := fyne.CurrentApp().Driver()
	c := drv.CanvasForObject(btn)
	if c == nil {
		return
	}
	pop := widget.NewPopUpMenu(fyne.NewMenu("", items...), c)
	pos := drv.AbsolutePositionForObject(btn)
	sz := pop.MinSize()
	x := pos.X + btn.Size().Width - sz.Width
	if x < 6 {
		x = pos.X
	}
	pop.ShowAtPosition(fyne.NewPos(x, pos.Y+btn.Size().Height+4))
}

// flowWrapLayout places visible items left-to-right, wrapping onto additional
// rows when they exceed the available width, while keeping MinSize().Width
// bounded to the widest single item so 2-column cards never overflow in long
// languages.
type flowWrapLayout struct {
	hGap, vGap float32
	minH       float32
}

func (l *flowWrapLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	hGap, vGap := l.hGap, l.vGap
	if hGap <= 0 {
		hGap = 8
	}
	if vGap <= 0 {
		vGap = 6
	}
	var x, y, rowH float32
	for _, o := range objs {
		if !o.Visible() {
			continue
		}
		ms := o.MinSize()
		if x > 0 && size.Width > 0 && x+ms.Width > size.Width {
			x = 0
			y += rowH + vGap
			rowH = 0
		}
		o.Move(fyne.NewPos(x, y))
		o.Resize(ms)
		x += ms.Width + hGap
		rowH = max(rowH, ms.Height)
	}
	if rowH > 0 {
		l.minH = y + rowH
	} else {
		l.minH = 0
	}
}

func (l *flowWrapLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	var maxW, rowH float32
	for _, o := range objs {
		if !o.Visible() {
			continue
		}
		ms := o.MinSize()
		maxW = max(maxW, ms.Width)
		rowH = max(rowH, ms.Height)
	}
	return fyne.NewSize(maxW, max(rowH, l.minH))
}

// sourceTile is a selectable card tile on Home's Share card ("Entire screen" /
// "A window") with an icon and wrapping label.
type sourceTile struct {
	widget.BaseWidget
	iconOff  fyne.Resource
	iconOn   fyne.Resource
	title    string
	sub      string
	active   bool
	disabled bool
	hover    bool
	onTap    func()
}

func newSourceTile(iconOff, iconOn fyne.Resource, title string, active bool, onTap func()) *sourceTile {
	t := &sourceTile{iconOff: iconOff, iconOn: iconOn, title: title, active: active, onTap: onTap}
	t.ExtendBaseWidget(t)
	return t
}

func (t *sourceTile) SetActive(on bool) {
	if t.active == on {
		return
	}
	t.active = on
	t.Refresh()
}

func (t *sourceTile) SetSubtitle(sub string) {
	if t.sub == sub {
		return
	}
	t.sub = sub
	t.Refresh()
}

func (t *sourceTile) SetDisabled(dis bool) {
	if t.disabled == dis {
		return
	}
	t.disabled = dis
	t.Refresh()
}

func (t *sourceTile) Tapped(*fyne.PointEvent) {
	if !t.disabled && t.onTap != nil {
		t.onTap()
	}
}

func (t *sourceTile) Cursor() desktop.Cursor {
	if !t.disabled && t.onTap != nil {
		return desktop.PointerCursor
	}
	return desktop.DefaultCursor
}

func (t *sourceTile) MouseIn(*desktop.MouseEvent) {
	if !t.disabled {
		t.hover = true
		t.Refresh()
	}
}

func (t *sourceTile) MouseMoved(*desktop.MouseEvent) {}

func (t *sourceTile) MouseOut() {
	if t.hover {
		t.hover = false
		t.Refresh()
	}
}

func (t *sourceTile) CreateRenderer() fyne.WidgetRenderer {
	bg := canvas.NewRectangle(colInputBg)
	bg.CornerRadius = 10
	img := canvas.NewImageFromResource(t.iconOff)
	img.FillMode = canvas.ImageFillContain
	lbl := label(t.title)
	lbl.Alignment = fyne.TextAlignCenter
	sub := caption(t.sub)
	sub.Alignment = fyne.TextAlignCenter
	sub.Truncation = fyne.TextTruncateEllipsis
	sub.Wrapping = fyne.TextWrapOff
	re := &sourceTileRenderer{t: t, bg: bg, img: img, lbl: lbl, sub: sub, objs: []fyne.CanvasObject{bg, img, lbl, sub}}
	re.Refresh()
	return re
}

type sourceTileRenderer struct {
	t    *sourceTile
	bg   *canvas.Rectangle
	img  *canvas.Image
	lbl  *widget.Label
	sub  *widget.Label
	objs []fyne.CanvasObject
}

func (r *sourceTileRenderer) Layout(size fyne.Size) {
	r.bg.Move(fyne.NewPos(0, 0))
	r.bg.Resize(size)
	const ic = 22
	padX := float32(8)
	w := max(0, size.Width-2*padX)
	r.lbl.Resize(fyne.NewSize(w, 0))
	ls := r.lbl.MinSize()
	subH := float32(0)
	if r.sub.Visible() {
		subH = r.sub.MinSize().Height
	}
	totalH := ic + 4 + ls.Height
	if subH > 0 {
		totalH += 2 + subH
	}
	y := max(float32(8), (size.Height-totalH)/2)
	r.img.Move(fyne.NewPos((size.Width-ic)/2, y))
	r.img.Resize(fyne.NewSize(ic, ic))
	y += ic + 4
	r.lbl.Move(fyne.NewPos(padX, y))
	r.lbl.Resize(fyne.NewSize(w, ls.Height))
	if r.sub.Visible() {
		y += ls.Height + 2
		r.sub.Move(fyne.NewPos(padX, y))
		r.sub.Resize(fyne.NewSize(w, subH))
	}
}

func (r *sourceTileRenderer) MinSize() fyne.Size {
	ls := r.lbl.MinSize()
	h := float32(22+4+16) + ls.Height
	if r.sub.Visible() {
		h += 2 + r.sub.MinSize().Height
	}
	return fyne.NewSize(min(ls.Width+24, 150), max(72, h))
}

func (r *sourceTileRenderer) Refresh() {
	r.lbl.SetText(r.t.title)
	if r.t.sub != "" {
		r.sub.SetText(r.t.sub)
		r.sub.Show()
	} else {
		r.sub.Hide()
	}
	switch {
	case r.t.disabled:
		r.bg.FillColor = colButtonDis
		r.bg.StrokeColor = colCardBorder
		r.bg.StrokeWidth = 1
		r.img.Resource = r.t.iconOff
		r.lbl.TextStyle.Bold = false
	case r.t.active:
		r.bg.FillColor = colAccentFill
		r.bg.StrokeColor = colAccent
		r.bg.StrokeWidth = 1.5
		r.img.Resource = r.t.iconOn
		r.lbl.TextStyle.Bold = true
	default:
		if r.t.hover {
			r.bg.FillColor = colButton
		} else {
			r.bg.FillColor = colInputBg
		}
		r.bg.StrokeColor = colCardBorder
		r.bg.StrokeWidth = 1
		r.img.Resource = r.t.iconOff
		r.lbl.TextStyle.Bold = false
	}
	r.bg.Refresh()
	r.img.Refresh()
	r.lbl.Refresh()
	r.sub.Refresh()
}

func (r *sourceTileRenderer) Objects() []fyne.CanvasObject { return r.objs }
func (r *sourceTileRenderer) Destroy()                     {}
