package nui

import (
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"blite/internal/app"
	"blite/internal/changelog"
	"blite/internal/i18n"
	"blite/internal/update"
)

// autoClose is how long the update notice and the changes panel stay up.
// The time doesn't run out while the mouse is over them.
const autoClose = 30 * time.Second

// hoverBox wraps content and tracks whether the mouse is over it. Tappable
// so that clicks on the update notice don't reach the view below it.
type hoverBox struct {
	widget.BaseWidget
	content fyne.CanvasObject
	hovered bool
}

func newHoverBox(content fyne.CanvasObject) *hoverBox {
	b := &hoverBox{content: content}
	b.ExtendBaseWidget(b)
	return b
}

func (b *hoverBox) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(b.content)
}

func (b *hoverBox) Tapped(*fyne.PointEvent)        {}
func (b *hoverBox) MouseIn(*desktop.MouseEvent)    { b.hovered = true }
func (b *hoverBox) MouseMoved(*desktop.MouseEvent) {}
func (b *hoverBox) MouseOut()                      { b.hovered = false }

// closeLater runs hide (on the Fyne thread) after d, or once the mouse has
// left box.
func (u *UI) closeLater(d time.Duration, box *hoverBox, hide func()) {
	time.AfterFunc(d, func() {
		u.later(func() {
			if box.hovered {
				u.closeLater(5*time.Second, box, hide)
				return
			}
			hide()
		})
	})
}

// changesList shows entries, newest first: "Build N (date)" and its changes.
func changesList(entries []changelog.Entry) fyne.CanvasObject {
	box := container.NewVBox()
	for i, e := range entries {
		if i > 0 {
			box.Add(widget.NewSeparator())
		}
		box.Add(bold(i18n.L("Build {{.Build}} ({{.Date}})", i18n.D{"Build": e.Build, "Date": e.Date})))
		for _, c := range e.Changes {
			box.Add(label("- " + c))
		}
	}
	return box
}

// changesPanel shows what changed in entries, with "Don't show change logs
// again". auto: it also closes by itself. done runs once when it closes.
func (u *UI) changesPanel(title string, entries []changelog.Entry, auto bool, done func()) {
	never := widget.NewCheck(i18n.L("Don't show change logs again"), nil)
	never.SetChecked(u.st.HideChangelog)
	never.OnChanged = func(on bool) { u.do(func() error { return u.a.SetHideChangelog(on) }) }

	list := container.NewVScroll(changesList(entries))
	list.SetMinSize(fyne.NewSize(0, 320))
	box := newHoverBox(container.NewBorder(nil, never, nil, nil, list))
	d := dialog.NewCustom(title, i18n.L("OK"), box, u.win)
	closed := false
	d.SetOnClosed(func() {
		if closed {
			return
		}
		closed = true
		if done != nil {
			done()
		}
	})
	d.Resize(fyne.NewSize(520, 0))
	d.Show()
	if auto {
		u.closeLater(autoClose, box, func() {
			if !closed {
				d.Hide()
			}
		})
	}
}

// whatsNew is Help > What's new: every change up to our build.
func (u *UI) whatsNew() {
	entries := changelog.All(int64(u.st.Build))
	if len(entries) == 0 {
		u.info(i18n.L("What's new"), i18n.L("There's no change log for this build."), nil)
		return
	}
	u.changesPanel(i18n.L("What's new"), entries, false, nil)
}

// updateResult shows the outcome of the last update once: after a
// successful one the changes since the build it replaced, else (or with
// change logs turned off) the plain message.
func (u *UI) updateResult(st app.UIState) {
	up := st.Update
	if up == nil || up.Result == "" || up.Result == u.shownResult {
		return
	}
	u.shownResult = up.Result
	dismiss := func() { u.a.DismissUpdateResult() }
	if up.UpdatedTo > 0 && !st.HideChangelog {
		if entries := changelog.Between(up.UpdatedFrom, up.UpdatedTo); len(entries) > 0 {
			u.changesPanel(i18n.Msg(up.Result), entries, true, dismiss)
			return
		}
	}
	u.info(i18n.L("Update"), i18n.Msg(up.Result), dismiss)
}

// updateNotice shows a newly found update and its notes in the corner of
// the window for a while (once per update, not with change logs turned off).
func (u *UI) updateNotice(st app.UIState) {
	up := st.Update
	if up == nil || up.Available == "" || up.Available == u.noticed || u.toasts == nil {
		return
	}
	u.noticed = up.Available
	if st.HideChangelog || up.Downloading {
		return
	}
	u.toasts.Objects = nil
	u.toasts.Add(u.newUpdateNotice(up))
	u.toasts.Refresh()
}

func (u *UI) newUpdateNotice(up *update.Info) fyne.CanvasObject {
	var box *hoverBox
	hide := func() {
		u.toasts.Remove(box)
		u.toasts.Refresh()
	}
	body := container.NewVBox(bold(i18n.L("Bellion Share {{.Version}} is available.", i18n.D{"Version": up.Available})))
	if up.Notes != "" {
		body.Add(label(up.Notes))
	}
	btns := container.NewHBox()
	if up.CanApply {
		install := widget.NewButtonWithIcon(i18n.L("Install and restart"), theme.DownloadIcon(), func() {
			hide()
			u.do(func() error { return u.a.InstallUpdate(ctx()) })
		})
		install.Importance = widget.HighImportance
		btns.Add(install)
	}
	btns.Add(widget.NewButton(i18n.L("Close"), hide))
	body.Add(btns)

	bg := canvas.NewRectangle(colElevated)
	bg.StrokeColor = colAccent
	bg.StrokeWidth = 1
	bg.CornerRadius = 12
	box = newHoverBox(container.NewStack(bg, container.New(&insetLayout{pad: 14}, body)))
	u.closeLater(autoClose, box, hide)
	return box
}

// cornerLayout puts its objects at the bottom-right corner, w wide, stacked
// upwards. It asks for no room: the notices float over the view.
type cornerLayout struct{ w, pad float32 }

func (l *cornerLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	w := min(l.w, size.Width-2*l.pad)
	y := size.Height - l.pad
	for _, o := range objs {
		if !o.Visible() {
			continue
		}
		o.Resize(fyne.NewSize(w, 1)) // wrapped text measures at this width
		h := o.MinSize().Height
		y -= h
		o.Resize(fyne.NewSize(w, h))
		o.Move(fyne.NewPos(size.Width-l.pad-w, y))
		y -= l.pad
	}
}

func (l *cornerLayout) MinSize([]fyne.CanvasObject) fyne.Size { return fyne.Size{} }
