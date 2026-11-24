package nui

import (
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"blite/internal/i18n"
	"blite/internal/session"
)

// personPanel shows the controls, connection stats and owner actions for the
// selected person on the right side of the People list.
type personPanel struct {
	u       *UI
	root    fyne.CanvasObject
	id      string
	m       session.Member
	owner   bool
	setting bool

	avatar   *avatarCircle
	ident    *nameTag
	client   *widget.Label
	notice   *widget.Label
	vol      *widget.Slider
	volPct   *widget.Label
	mute     *widget.Check
	dragging bool
	quality  *segmented

	connState  *widget.Label
	sendLine   *widget.Label
	uploadLine *widget.Label
	statsLine  *widget.Label
	pathLine   *widget.Label

	ownerBox *fyne.Container
	makeOwn  *widget.Button
	kickBtn  *widget.Button
	banBtn   *widget.Button
}

func newPersonPanel(u *UI, onClose func()) *personPanel {
	p := &personPanel{
		u:          u,
		avatar:     newAvatar("?", 38),
		ident:      newNameTag(),
		client:     caption(""),
		notice:     caption(""),
		volPct:     caption("100%"),
		connState:  label(""),
		sendLine:   caption(""),
		uploadLine: caption(""),
		statsLine:  caption(""),
		pathLine:   caption(""),
	}
	p.volPct.Wrapping = fyne.TextWrapOff
	p.notice.Importance = widget.WarningImportance

	closeBtn := newIconDotButton(theme.CancelIcon(), func(*iconDotButton) {
		if onClose != nil {
			onClose()
		}
	})

	applyAudio := func() {
		if p.setting || p.id == "" {
			return
		}
		id, q := p.id, p.m.Quality
		if q == "" {
			q = "auto"
		}
		vol, muted := p.vol.Value/100, p.mute.Checked
		u.do(func() error { return u.a.SetPeer(id, q, vol, muted) })
	}

	p.vol = widget.NewSlider(0, 200)
	p.vol.Step = 5
	p.vol.OnChanged = func(v float64) {
		p.volPct.SetText(fmt.Sprintf("%.0f%%", v))
		p.dragging = !p.setting
	}
	p.vol.OnChangeEnded = func(float64) {
		p.dragging = false
		applyAudio()
	}
	p.mute = widget.NewCheck(i18n.L("Mute"), func(bool) { applyAudio() })

	p.quality = newSegmented([]segOption{
		{Value: "auto", Label: i18n.L("Best (automatic)")},
		{Value: "low", Label: i18n.L("Low (saves data)")},
	}, func(q string) {
		if p.setting || p.id == "" {
			return
		}
		id, vol, muted := p.id, p.m.Volume, p.m.Muted
		u.do(func() error { return u.a.SetPeer(id, q, vol, muted) })
	})

	p.makeOwn = widget.NewButton(i18n.L("Make owner..."), func() {
		id, name := p.id, p.m.Name
		u.confirm(i18n.L("Make {{.Name}} the owner?", i18n.D{"Name": name}),
			i18n.L("You can't take it back yourself."), i18n.L("Make owner"), i18n.L("Cancel"), widget.HighImportance,
			func() error { return u.a.Transfer(id) })
	})
	p.kickBtn = widget.NewButton(i18n.L("Disconnect..."), func() {
		id, name := p.id, p.m.Name
		u.confirm(i18n.L("Disconnect {{.Name}}?", i18n.D{"Name": name}),
			i18n.L("They can rejoin with the link."), i18n.L("Disconnect"), i18n.L("Cancel"), widget.HighImportance,
			func() error { return u.a.Kick(id, false) })
	})
	p.banBtn = widget.NewButton(i18n.L("Ban..."), func() {
		id, name := p.id, p.m.Name
		u.confirm(i18n.L("Ban {{.Name}}?", i18n.D{"Name": name}),
			i18n.L("They won't be able to rejoin this room."), i18n.L("Ban"), i18n.L("Cancel"), widget.DangerImportance,
			func() error { return u.a.Kick(id, true) })
	})
	p.banBtn.Importance = widget.DangerImportance

	p.ownerBox = container.NewVBox(
		widget.NewSeparator(),
		bold(i18n.L("Owner actions")),
		p.makeOwn,
		container.NewGridWithColumns(2, p.kickBtn, p.banBtn),
	)

	head := container.NewBorder(nil, nil, p.avatar, closeBtn,
		container.NewVBox(p.ident, p.client))

	volHdr := container.NewBorder(nil, nil, bold(i18n.L("Volume")), p.volPct)
	volRow := container.NewBorder(nil, nil, nil, p.mute, p.vol)

	body := container.NewVBox(
		head,
		p.notice,
		widget.NewSeparator(),
		volHdr,
		volRow,
		bold(i18n.L("Their video")),
		p.quality,
		widget.NewSeparator(),
		bold(i18n.L("Connection details")),
		p.connState,
		p.sendLine,
		p.uploadLine,
		p.statsLine,
		p.pathLine,
		p.ownerBox,
	)
	p.root = newCard("", container.NewVScroll(container.New(&insetLayout{padRight: 12}, body)))
	return p
}

func (p *personPanel) update(m session.Member, isOwner bool) {
	p.id, p.m, p.owner = m.ID, m, isOwner
	p.setting = true
	defer func() { p.setting = false }()

	p.avatar.SetName(m.Name)
	badge := ""
	if m.Owner {
		badge = i18n.L("(owner)")
	}
	p.ident.Set(m.Name, m.Tag, badge, false)
	if m.Client != "" {
		p.client.SetText(m.Client)
		p.client.Show()
	} else {
		p.client.Hide()
	}

	var notes []string
	if m.KeyChanged {
		notes = append(notes, i18n.L("New device or key since you last saw this name."))
	}
	if m.Notice != "" {
		notes = append(notes, i18n.Msg(m.Notice))
	}
	if m.Hint != "" {
		notes = append(notes, i18n.Msg(m.Hint))
	}
	if len(notes) > 0 {
		p.notice.SetText(strings.Join(notes, " "))
		p.notice.Show()
	} else {
		p.notice.Hide()
	}

	if !p.dragging {
		pct := m.Volume * 100
		p.vol.SetValue(pct)
		p.volPct.SetText(fmt.Sprintf("%.0f%%", pct))
	}
	p.mute.SetChecked(m.Muted)

	q := m.Quality
	if q != "low" {
		q = "auto"
	}
	p.quality.SetSelected(q)

	word, imp := linkQuality(m)
	st := upperFirst(stateText(m))
	if m.State == "connected" && word != "-" {
		st = word + " · " + st
	}
	p.connState.SetText(st)
	p.connState.Importance = imp
	p.connState.Refresh()

	p.sendLine.SetText(i18n.L("You send") + ": " + sendText(m))
	if s := p.u.st.Session; s != nil && s.Source != "" && s.Source != "none" && (s.UploadLimitKbps > 0 || s.AutoUploadKbps > 0) {
		if s.UploadLimitKbps > 0 {
			p.uploadLine.SetText(i18n.L("Total upload: {{.Rate}} / {{.Limit}}", i18n.D{"Rate": kbps(s.TxKbps), "Limit": kbps(s.UploadLimitKbps)}))
		} else {
			p.uploadLine.SetText(i18n.L("Total upload: {{.Rate}} / {{.Limit}} (automatic)", i18n.D{"Rate": kbps(s.TxKbps), "Limit": kbps(s.AutoUploadKbps)}))
		}
		p.uploadLine.Show()
	} else {
		p.uploadLine.Hide()
	}

	var stats []string
	if lt := linkText(m); lt != "" {
		stats = append(stats, lt)
	}
	if m.Recv > 0 {
		res := fmt.Sprintf("%dp", m.Recv)
		if m.Recv == 2160 {
			res = "4K"
		}
		if m.FPSIn > 0 {
			res = res + " " + i18n.L("{{.FPS}} fps", i18n.D{"FPS": m.FPSIn})
		}
		stats = append(stats, "↓ "+res)
	}
	if len(stats) > 0 {
		p.statsLine.SetText(strings.Join(stats, " · "))
		p.statsLine.Show()
	} else {
		p.statsLine.Hide()
	}

	if m.Pair != "" {
		p.pathLine.SetText(i18n.L("Network path: {{.Path}}", i18n.D{"Path": m.Pair}))
		p.pathLine.Show()
	} else {
		p.pathLine.Hide()
	}

	if isOwner {
		if m.State == "connected" {
			p.makeOwn.Enable()
		} else {
			p.makeOwn.Disable()
		}
		p.ownerBox.Show()
	} else {
		p.ownerBox.Hide()
	}
}

// sideSplitLayout places the People card on the left and, when visible, the
// person side panel on the right.
type sideSplitLayout struct{}

func (sideSplitLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	if len(objs) < 2 || !objs[1].Visible() {
		if len(objs) > 0 {
			objs[0].Move(fyne.NewPos(0, 0))
			objs[0].Resize(size)
		}
		return
	}
	const gap = 12
	panelMin := max(320, objs[1].MinSize().Width)
	pw := min(max(panelMin, (size.Width-gap)*0.40), size.Width*0.48)
	lw := max(0, size.Width-gap-pw)
	objs[0].Move(fyne.NewPos(0, 0))
	objs[0].Resize(fyne.NewSize(lw, size.Height))
	objs[1].Move(fyne.NewPos(lw+gap, 0))
	objs[1].Resize(fyne.NewSize(pw, size.Height))
}

func (sideSplitLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	if len(objs) == 0 {
		return fyne.NewSize(0, 0)
	}
	m0 := objs[0].MinSize()
	if len(objs) < 2 || !objs[1].Visible() {
		return m0
	}
	m1 := objs[1].MinSize()
	return fyne.NewSize(m0.Width+12+m1.Width, max(m0.Height, m1.Height))
}
