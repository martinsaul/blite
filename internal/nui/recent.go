package nui

import (
	"image/color"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"blite/internal/app"
	"blite/internal/i18n"
)

// recentSection renders the "Recent rooms" card on Home and hides itself when
// the list is empty.
type recentSection struct {
	u    *UI
	root fyne.CanvasObject
	box  *fyne.Container
	rows map[string]*recentRow
	keys []string
}

func newRecentSection(u *UI) *recentSection {
	s := &recentSection{
		u:    u,
		box:  container.NewVBox(),
		rows: map[string]*recentRow{},
	}
	s.root = newCard(i18n.L("Recent rooms"), s.box)
	s.root.Hide()
	return s
}

func (s *recentSection) update(st app.UIState) {
	if len(st.Recent) == 0 {
		for _, r := range s.rows {
			r.stopFade()
		}
		s.rows = map[string]*recentRow{}
		s.keys = nil
		s.box.Objects = nil
		s.root.Hide()
		return
	}
	mandatory := st.Update != nil && st.Update.Mandatory
	seen := map[string]bool{}
	var newKeys []string
	objs := make([]fyne.CanvasObject, 0, len(st.Recent))
	for _, item := range st.Recent {
		k := item.Server + "|" + item.Code
		seen[k] = true
		newKeys = append(newKeys, k)
		row, ok := s.rows[k]
		if !ok {
			row = newRecentRow(s.u, item)
			s.rows[k] = row
		}
		row.set(item, mandatory)
		objs = append(objs, row.wrap)
	}
	for k, r := range s.rows {
		if !seen[k] {
			r.stopFade()
			delete(s.rows, k)
		}
	}
	sameOrder := len(newKeys) == len(s.keys)
	if sameOrder {
		for i := range newKeys {
			if newKeys[i] != s.keys[i] {
				sameOrder = false
				break
			}
		}
	}
	if !sameOrder {
		s.keys = newKeys
		s.box.Objects = objs
		s.box.Refresh()
	}
	s.root.Show()
}

type recentRow struct {
	widget.BaseWidget
	u         *UI
	item      app.RecentRoom
	mandatory bool
	hover     bool
	alpha     uint8
	fadeMu    sync.Mutex
	fadeStop  chan struct{}
	removeBtn *recentRemoveBtn
	wrap      fyne.CanvasObject
}

func newRecentRow(u *UI, item app.RecentRoom) *recentRow {
	r := &recentRow{u: u, item: item, alpha: 255}
	r.ExtendBaseWidget(r)
	r.removeBtn = newRecentRemoveBtn(func() {
		srv, code := r.item.Server, r.item.Code
		u.do(func() error { return u.a.RemoveRecentRoom(srv, code) })
	})
	r.wrap = container.NewBorder(nil, nil, nil, r.removeBtn, r)
	return r
}

func (r *recentRow) clickable() bool {
	return !r.mandatory && r.item.Status == "ready"
}

func (r *recentRow) set(item app.RecentRoom, mandatory bool) {
	prevStatus := r.item.Status
	r.item = item
	r.mandatory = mandatory
	r.removeBtn.setVisible(item.Status != "gone")
	if item.Status == "gone" {
		if prevStatus != "gone" {
			r.startFade()
		}
	} else {
		r.stopFade()
		r.fadeMu.Lock()
		r.alpha = 255
		r.fadeMu.Unlock()
	}
	r.Refresh()
}

func (r *recentRow) startFade() {
	r.stopFade()
	stop := make(chan struct{})
	r.fadeMu.Lock()
	r.fadeStop = stop
	r.alpha = 255
	r.fadeMu.Unlock()

	go func() {
		const steps = 15
		tick := time.NewTicker(45 * time.Millisecond)
		defer tick.Stop()
		for i := 1; i <= steps; i++ {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			if r.u.closed.Load() {
				return
			}
			a := uint8(max(20, 255-(i*235)/steps))
			r.fadeMu.Lock()
			r.alpha = a
			r.fadeMu.Unlock()
			r.u.later(r.Refresh)
		}
	}()
}

func (r *recentRow) stopFade() {
	r.fadeMu.Lock()
	stop := r.fadeStop
	r.fadeStop = nil
	r.fadeMu.Unlock()
	if stop != nil {
		close(stop)
	}
}

func (r *recentRow) curAlpha() uint8 {
	r.fadeMu.Lock()
	defer r.fadeMu.Unlock()
	return r.alpha
}

func (r *recentRow) Tapped(*fyne.PointEvent) {
	if !r.clickable() {
		return
	}
	srv, code := r.item.Server, r.item.Code
	r.u.do(func() error { return r.u.a.JoinRecentRoom(ctx(), srv, code) })
}

func (r *recentRow) Cursor() desktop.Cursor {
	if r.clickable() {
		return desktop.PointerCursor
	}
	return desktop.DefaultCursor
}

func (r *recentRow) MouseIn(*desktop.MouseEvent) {
	if r.clickable() {
		r.hover = true
		r.Refresh()
	}
}

func (r *recentRow) MouseMoved(*desktop.MouseEvent) {}

func (r *recentRow) MouseOut() {
	if r.hover {
		r.hover = false
		r.Refresh()
	}
}

func (r *recentRow) CreateRenderer() fyne.WidgetRenderer {
	bg := canvas.NewRectangle(colInputBg)
	bg.CornerRadius = 8
	bg.StrokeWidth = 1
	code := canvas.NewText("", colText)
	code.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}
	code.TextSize = theme.TextSize()
	lock := canvas.NewImageFromResource(lockClosedRes)
	lock.FillMode = canvas.ImageFillContain
	host := canvas.NewText("", colTextDim)
	host.TextSize = theme.CaptionTextSize()
	stat := canvas.NewText("", colTextDim)
	stat.TextSize = theme.CaptionTextSize()
	re := &recentRowRenderer{
		r:    r,
		bg:   bg,
		code: code,
		lock: lock,
		host: host,
		stat: stat,
		objs: []fyne.CanvasObject{bg, code, lock, host, stat},
	}
	re.Refresh()
	return re
}

type recentRowRenderer struct {
	r                *recentRow
	bg               *canvas.Rectangle
	code, host, stat *canvas.Text
	lock             *canvas.Image
	objs             []fyne.CanvasObject
}

func (re *recentRowRenderer) Layout(size fyne.Size) {
	re.bg.Move(fyne.NewPos(0, 0))
	re.bg.Resize(size)

	x := float32(12)
	cs := re.code.MinSize()
	re.code.Move(fyne.NewPos(x, (size.Height-cs.Height)/2))
	re.code.Resize(cs)
	x += cs.Width + 8

	if re.lock.Visible() {
		const lc = 14
		re.lock.Move(fyne.NewPos(x, (size.Height-lc)/2))
		re.lock.Resize(fyne.NewSize(lc, lc))
		x += lc + 8
	}

	rightEdge := size.Width - 12
	if re.stat.Visible() {
		ss := re.stat.MinSize()
		re.stat.Move(fyne.NewPos(max(x, rightEdge-ss.Width), (size.Height-ss.Height)/2))
		re.stat.Resize(ss)
		rightEdge -= ss.Width + 10
	}

	hs := re.host.MinSize()
	availW := max(0, rightEdge-x)
	re.host.Move(fyne.NewPos(x, (size.Height-hs.Height)/2))
	re.host.Resize(fyne.NewSize(min(hs.Width, availW), hs.Height))
}

func (re *recentRowRenderer) MinSize() fyne.Size {
	cs := re.code.MinSize()
	w := float32(24) + cs.Width + 80
	if re.lock.Visible() {
		w += 22
	}
	return fyne.NewSize(min(w, 260), max(38, cs.Height+16))
}

func scaleAlpha(c color.NRGBA, a uint8) color.NRGBA {
	return color.NRGBA{
		R: c.R,
		G: c.G,
		B: c.B,
		A: uint8((uint16(c.A) * uint16(a)) / 255),
	}
}

func (re *recentRowRenderer) Refresh() {
	it := re.r.item
	re.code.Text = it.Display
	re.code.TextSize = theme.TextSize()
	re.host.Text = it.Host
	re.host.TextSize = theme.CaptionTextSize()
	re.stat.TextSize = theme.CaptionTextSize()

	if it.HasPassword && it.Status != "gone" {
		re.lock.Show()
	} else {
		re.lock.Hide()
	}

	switch it.Status {
	case "ready":
		re.code.Color = colWhite
		re.host.Color = colTextDim
		re.stat.Text = ""
		re.stat.Hide()
		if re.r.hover && re.r.clickable() {
			re.bg.FillColor = colButton
			re.bg.StrokeColor = colAccent
		} else {
			re.bg.FillColor = colInputBg
			re.bg.StrokeColor = colCardBorder
		}
	case "gone":
		a := re.r.curAlpha()
		danger := scaleAlpha(colDanger, a)
		re.code.Color = danger
		re.host.Color = scaleAlpha(colTextDim, a)
		re.stat.Text = i18n.L("Ended")
		re.stat.Color = danger
		re.stat.Show()
		re.bg.FillColor = color.NRGBA{R: colDanger.R, G: colDanger.G, B: colDanger.B, A: uint8((uint16(0x22) * uint16(a)) / 255)}
		re.bg.StrokeColor = danger
	default: // "checking"
		re.code.Color = colTextDis
		re.host.Color = colTextDis
		re.stat.Text = i18n.L("Checking...")
		re.stat.Color = colTextDis
		re.stat.Show()
		re.bg.FillColor = colButtonDis
		re.bg.StrokeColor = colCardBorder
	}

	re.bg.Refresh()
	re.code.Refresh()
	re.lock.Refresh()
	re.host.Refresh()
	re.stat.Refresh()
}

func (re *recentRowRenderer) Objects() []fyne.CanvasObject { return re.objs }
func (re *recentRowRenderer) Destroy()                     { re.r.stopFade() }

// recentRemoveBtn is the compact "x" button that removes a recent room entry.
type recentRemoveBtn struct {
	widget.BaseWidget
	hover   bool
	enabled bool
	onTap   func()
}

func newRecentRemoveBtn(onTap func()) *recentRemoveBtn {
	b := &recentRemoveBtn{enabled: true, onTap: onTap}
	b.ExtendBaseWidget(b)
	return b
}

func (b *recentRemoveBtn) setVisible(on bool) {
	if b.enabled == on {
		return
	}
	b.enabled = on
	b.Refresh()
}

func (b *recentRemoveBtn) Tapped(*fyne.PointEvent) {
	if b.enabled && b.onTap != nil {
		b.onTap()
	}
}

func (b *recentRemoveBtn) Cursor() desktop.Cursor {
	if b.enabled {
		return desktop.PointerCursor
	}
	return desktop.DefaultCursor
}

func (b *recentRemoveBtn) MouseIn(*desktop.MouseEvent) {
	if b.enabled {
		b.hover = true
		b.Refresh()
	}
}

func (b *recentRemoveBtn) MouseMoved(*desktop.MouseEvent) {}

func (b *recentRemoveBtn) MouseOut() {
	if b.hover {
		b.hover = false
		b.Refresh()
	}
}

func (b *recentRemoveBtn) CreateRenderer() fyne.WidgetRenderer {
	bg := canvas.NewRectangle(color.Transparent)
	bg.CornerRadius = 6
	img := canvas.NewImageFromResource(closeDimRes)
	img.FillMode = canvas.ImageFillContain
	re := &recentRemoveRenderer{b: b, bg: bg, img: img, objs: []fyne.CanvasObject{bg, img}}
	re.Refresh()
	return re
}

type recentRemoveRenderer struct {
	b    *recentRemoveBtn
	bg   *canvas.Rectangle
	img  *canvas.Image
	objs []fyne.CanvasObject
}

func (r *recentRemoveRenderer) Layout(size fyne.Size) {
	r.bg.Move(fyne.NewPos(2, (size.Height-30)/2))
	r.bg.Resize(fyne.NewSize(30, 30))
	const ic = 15
	r.img.Move(fyne.NewPos(2+(30-ic)/2, (size.Height-ic)/2))
	r.img.Resize(fyne.NewSize(ic, ic))
}

func (r *recentRemoveRenderer) MinSize() fyne.Size { return fyne.NewSize(34, 38) }

func (r *recentRemoveRenderer) Refresh() {
	if !r.b.enabled {
		r.bg.Hide()
		r.img.Hide()
		return
	}
	r.bg.Show()
	r.img.Show()
	if r.b.hover {
		r.bg.FillColor = colButton
		r.img.Resource = closeWhiteRes
	} else {
		r.bg.FillColor = color.Transparent
		r.img.Resource = closeDimRes
	}
	r.bg.Refresh()
	r.img.Refresh()
}

func (r *recentRemoveRenderer) Objects() []fyne.CanvasObject { return r.objs }
func (r *recentRemoveRenderer) Destroy()                     {}
