package nui

import (
	"fmt"
	"image/color"
	"net/url"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"blite/internal/app"
	"blite/internal/feeds"
	"blite/internal/i18n"
	"blite/internal/session"
)

// notices are the warning lines above both views: errors, updates, reach.
type notices struct {
	box     *fyne.Container
	err     *widget.Label
	update  *fyne.Container
	updText *widget.Label
	updBtn  *widget.Button
	reach   *widget.Label
}

func newNotices(u *UI) *notices {
	n := &notices{err: label(""), updText: label(""), reach: label("")}
	n.err.Importance = widget.DangerImportance
	n.reach.Importance = widget.WarningImportance
	n.updBtn = widget.NewButtonWithIcon(i18n.L("Install and restart"), theme.DownloadIcon(), func() {
		u.do(func() error { return u.a.InstallUpdate(ctx()) })
	})
	n.updBtn.Importance = widget.HighImportance
	n.update = container.NewBorder(nil, nil, nil, n.updBtn, n.updText)
	n.box = container.NewVBox(n.err, n.update, n.reach)
	return n
}

func (n *notices) set(u *UI, st app.UIState) {
	show := func(o fyne.CanvasObject, on bool) {
		if on {
			o.Show()
		} else {
			o.Hide()
		}
	}
	n.err.SetText(i18n.Msg(st.Error))
	show(n.err, st.Error != "")
	up := st.Update
	switch {
	case up != nil && up.Downloading:
		n.updText.SetText(i18n.L("Downloading Bellion Share {{.Version}}: {{.Percent}}%", i18n.D{"Version": up.Available, "Percent": up.Progress}))
		n.updText.Importance = widget.MediumImportance
		n.updBtn.Hide()
		show(n.update, true)
	case up != nil && up.Problem != "":
		n.updText.SetText(i18n.L("Update problem: {{.Err}}", i18n.D{"Err": i18n.Msg(up.Problem)}))
		n.updText.Importance = widget.DangerImportance
		show(n.updBtn, up.Available != "" && up.CanApply) // try again
		show(n.update, true)
	case up != nil && up.Available != "" && up.Mandatory:
		// A plain update is the corner notice and the button by the gear;
		// one we can't do without says so here.
		n.updText.SetText(i18n.L("Bellion Share {{.Version}} is required: you can't start or join a session until you install it.", i18n.D{"Version": up.Available}))
		n.updText.Importance = widget.MediumImportance
		show(n.updBtn, up.CanApply)
		show(n.update, true)
	default:
		show(n.update, false)
	}
	n.updText.Refresh()
	show(n.reach, st.Reach != nil && !st.Reach.OK)
	if st.Reach != nil {
		n.reach.SetText(i18n.Msg(st.Reach.Text))
	}
	show(n.box, n.err.Visible() || n.update.Visible() || n.reach.Visible())
}

// lobbyView: not in a session. Share card, Join card, Discord banner, and
// Recent rooms.
type lobbyView struct {
	u           *UI
	root        fyne.CanvasObject
	who         *widget.Label
	gear        *iconDotButton
	upd         *iconDotButton // shows while an update waits
	help        *iconDotButton
	notes       *notices
	tileScreen  *sourceTile
	tileWindow  *sourceTile
	presets     *presetBar
	chipAudio   *chipButton
	chipSource  *chipButton
	chipSec     *chipButton
	chipsBox    *fyne.Container
	noWinAudio  *widget.Label // the shared window's app audio can't be shared
	live        *widget.Button
	link        *widget.Entry
	join        *widget.Button
	clipJoin    *widget.Button
	clipLink    string
	dc          *fyne.Container // the Discord voice channel banner
	dcAvatars   *fyne.Container
	dcTitle     *widget.Label
	dcSub       *widget.Label
	dcPrimary   *widget.Button
	dcSecondary *widget.Button
	dcPriAct    func()
	dcSecAct    func()
	recent      *recentSection
	server      *serverPicker // bottom right, once the user added a server
}

func resDisplay(res string) string {
	if res == "2160p" {
		return "4K"
	}
	if res == "" {
		return "720p"
	}
	return res
}

func newLobby(u *UI) *lobbyView {
	v := &lobbyView{u: u, who: widget.NewLabel(""), notes: newNotices(u)}
	v.who.TextStyle.Bold = true
	var chrome *fyne.Container
	v.gear, v.upd, v.help, chrome = u.newChromeButtons()

	v.tileScreen = newSourceTile(screenWhiteRes, screenAccentRes, i18n.L("Entire screen"), true, func() {
		u.do(func() error { return u.a.SetSource("primary") })
	})
	v.tileWindow = newSourceTile(windowWhiteRes, windowAccentRes, i18n.L("A window"), false, u.sourceDialog)

	v.presets = newPresetBar(u.applyPreset, u.removePreset, func() { u.presetDialog(u.applyPreset) })
	v.chipAudio = newChip(soundOffRes, "", u.showShareSheet)
	v.chipSource = newChip(nil, "", func() { u.openShareSheetAt(true, false) }) // Advanced has these sources
	v.chipSource.Hide()
	v.chipSec = newChip(lockClosedRes, "", u.showShareSheet)
	v.chipSec.Hide()
	v.noWinAudio = caption(i18n.L("This window's app audio isn't available."))
	v.noWinAudio.Hide()
	v.chipsBox = container.New(&flowWrapLayout{hGap: 8, vGap: 6},
		v.chipSource, v.chipAudio, v.chipSec)

	v.live = widget.NewButtonWithIcon(i18n.L("Go live and copy link"), theme.MediaRecordIcon(), func() {
		sh := normalizeShareState(u.st)
		pw := ""
		if sh.RequirePassword {
			pw = strings.TrimSpace(sh.RoomPassword)
		}
		u.goLive(pw, sh.E2EE, false)
	})
	v.live.Importance = widget.HighImportance

	v.clipJoin = widget.NewButtonWithIcon("", theme.LoginIcon(), func() {
		l := v.clipLink
		u.busyDo(v.clipJoin, func() error { return u.a.JoinLink(ctx(), l) })
	})
	v.clipJoin.Importance = widget.HighImportance
	v.clipJoin.Hide()
	v.link = widget.NewEntry()
	v.link.SetPlaceHolder(i18n.L("Share link or room code (e.g. K7PQ-2M4X)"))
	v.join = widget.NewButtonWithIcon(i18n.L("Watch"), theme.VisibilityIcon(), func() {
		v.joinInput(u)
	})
	v.link.OnSubmitted = func(string) { v.join.OnTapped() }

	tilesGrid := container.NewGridWithColumns(2, v.tileScreen, v.tileWindow)
	start := newCard(i18n.L("Share your screen"),
		container.NewVBox(
			tilesGrid,
			v.presets.root,
			v.chipsBox,
			v.noWinAudio,
			v.live,
		))
	watch := newCard(i18n.L("Watch someone"),
		container.NewVBox(
			label(i18n.L("Paste a share link or enter a room code.")),
			v.clipJoin,
			container.NewBorder(nil, nil, nil, v.join, v.link),
		))
	change := widget.NewButton(i18n.L("Change name..."), u.nameDialog)
	head := container.NewBorder(nil, nil, container.NewHBox(v.who, change), chrome)

	v.dcAvatars = container.NewHBox()
	v.dcAvatars.Hide()
	v.dcTitle = label("")
	v.dcTitle.TextStyle.Bold = true
	v.dcSub = caption("")
	v.dcSub.Hide()
	v.dcPrimary = widget.NewButtonWithIcon("", theme.LoginIcon(), func() {
		if v.dcPriAct != nil {
			v.dcPriAct()
		}
	})
	v.dcPrimary.Importance = widget.HighImportance
	v.dcSecondary = widget.NewButtonWithIcon("", theme.MediaRecordIcon(), func() {
		if v.dcSecAct != nil {
			v.dcSecAct()
		}
	})
	v.dcSecondary.Hide()
	dcLeft := container.NewBorder(nil, nil, v.dcAvatars, nil, container.NewVBox(v.dcTitle, v.dcSub))
	dcBtns := container.NewHBox(v.dcPrimary, v.dcSecondary)
	v.dc = container.NewVBox(newCard("", container.NewBorder(nil, nil, nil, dcBtns, dcLeft)))
	v.dc.Hide()

	v.recent = newRecentSection(u)
	v.server = newServerPicker(u)
	bodyScroll := container.NewVScroll(container.NewVBox(
		v.dc,
		container.NewGridWithColumns(2, start, watch),
		v.recent.root,
	))
	top := container.NewVBox(head, v.notes.box)
	v.root = container.NewPadded(container.NewBorder(top, v.server.box, nil, nil, bodyScroll))
	v.checkClipboard(u)
	return v
}

func (v *lobbyView) joinInput(u *UI) {
	raw := strings.TrimSpace(v.link.Text)
	v.join.Disable()
	u.bg.Add(1)
	go func() {
		err := u.a.JoinLink(ctx(), raw)
		u.laterDone(func() {
			v.join.Enable()
			if err == nil {
				return
			}
			if !strings.Contains(raw, "#") && err.Error() == "wrong password for that room" {
				u.passwordDialog(func(pw string) {
					u.busyDo(v.join, func() error {
						return u.a.JoinLink(ctx(), raw+"#"+url.PathEscape(pw))
					})
				})
				return
			}
			u.showErr(err)
		})
	}()
}

// tapLabel is a label that runs tap when tapped.
type tapLabel struct {
	widget.Label
	tap func()
}

func (l *tapLabel) Tapped(*fyne.PointEvent) { l.tap() }

// checkClipboard offers a one-click join when a share link was copied.
func (v *lobbyView) checkClipboard(u *UI) {
	c := strings.TrimSpace(u.fa.Clipboard().Content())
	code, _, err := session.ParseLink(c)
	if err != nil || len(c) > 512 || c == u.ownLink || !strings.Contains(c, "/j/") && !strings.Contains(c, "://join") {
		v.clipLink = ""
		v.clipJoin.Hide()
		return
	}
	v.clipLink = c
	v.clipJoin.SetText(i18n.L("Watch session {{.Code}} (link you copied)", i18n.D{"Code": code}))
	v.clipJoin.Show()
}

func (v *lobbyView) update(st app.UIState) {
	v.who.SetText(i18n.L("You are {{.Name}}", i18n.D{"Name": displayName(st.Name, st.Tag)}))
	setUpdateButton(v.upd, st)
	v.notes.set(v.u, st)
	v.server.update(st)
	v.updateShareCard(st)
	v.updateDiscord(v.u, st.Discord)
	v.recent.update(st)
	mandatory := st.Update != nil && st.Update.Mandatory
	v.tileScreen.SetDisabled(mandatory)
	v.tileWindow.SetDisabled(mandatory)
	if mandatory {
		v.live.Disable()
		v.join.Disable()
		v.clipJoin.Disable()
		v.dcPrimary.Disable()
		v.dcSecondary.Disable()
	} else {
		v.live.Enable()
		v.join.Enable()
		v.clipJoin.Enable()
	}
}

func (v *lobbyView) updateShareCard(st app.UIState) {
	sh := normalizeShareState(st)
	isWindowPick := strings.HasPrefix(sh.Source, "window:") || strings.HasPrefix(sh.Source, "monitor:")
	v.tileScreen.SetActive(sh.Source == "primary")
	v.tileWindow.SetActive(isWindowPick)
	if isWindowPick {
		v.tileWindow.SetSubtitle(sh.SourceName) // the title, never the handle
	} else {
		v.tileWindow.SetSubtitle("")
	}

	v.presets.update(sh)

	if sh.NoWindowAudio && (sh.Audio.Mode == "" || sh.Audio.Mode == "none") {
		v.noWinAudio.Show()
	} else {
		v.noWinAudio.Hide()
	}
	switch sh.Audio.Mode {
	case "", "none":
		v.chipAudio.SetContent(soundOffRes, i18n.L("No audio"), colButton, colCardBorder, colTextDim)
	case "test": // never mistaken for real sound
		v.chipAudio.SetContent(soundOnRes, i18n.L("Test tone"), colAccentFill, colAccent, colText)
	default:
		v.chipAudio.SetContent(soundOnRes, i18n.L("With sound"), colButton, colCardBorder, colText)
	}

	switch sh.Source {
	case "screen":
		v.chipSource.SetContent(screenAccentRes, i18n.L("All screens combined"), colAccentFill, colAccent, colText)
		v.chipSource.Show()
	case "test":
		v.chipSource.SetContent(nil, i18n.L("Test pattern"), colAccentFill, colAccent, colText)
		v.chipSource.Show()
	case "none":
		v.chipSource.SetContent(nil, i18n.L("Nothing (watch only)"), colAccentFill, colAccent, colText)
		v.chipSource.Show()
	default:
		v.chipSource.Hide()
	}

	switch {
	case !sh.E2EE:
		v.chipSec.SetContent(lockOpenRes, i18n.L("Unencrypted"), colWarningBg, colWarning, colWarning)
		v.chipSec.Show()
	case sh.RequirePassword:
		v.chipSec.SetContent(lockClosedRes, i18n.L("Password"), colSuccessBg, colSuccess, colText)
		v.chipSec.Show()
	default:
		v.chipSec.Hide()
	}
	v.chipsBox.Refresh()
}

// callView: the room header and share controls on top, the People list (plus
// optional person side panel) below.
type callView struct {
	u          *UI
	root       fyne.CanvasObject
	notes      *notices
	room       *widget.Label
	lockBadge  *chipButton
	gear       *iconDotButton
	upd        *iconDotButton
	help       *iconDotButton
	warn       *widget.Label
	share      *widget.Button
	pick       *widget.Button
	opts       *widget.Button
	sharing    bool
	self       *selfRow
	alone      *widget.Label
	rows       *fyne.Container
	people     map[string]*peerRow
	order      string
	selectedID string
	panel      *personPanel
	split      *fyne.Container
	relay      *widget.Label
	relaySw    *toggleSwitch
	relayLbl   *tapLabel
	relayOn    bool
	sounds     *iconToggle
	autoWatch  *iconToggle
	qr         *iconToggle
}

func newCall(u *UI) *callView {
	v := &callView{u: u, notes: newNotices(u), people: map[string]*peerRow{}}
	v.room = widget.NewLabel("")
	v.room.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}
	v.room.SizeName = theme.SizeNameHeadingText

	v.lockBadge = newChip(lockClosedRes, "", func() {
		if s := u.st.Session; s != nil && s.Encrypted {
			u.safetyDialog()
		}
	})

	var chrome *fyne.Container
	v.gear, v.upd, v.help, chrome = u.newChromeButtons()
	copyBtn := widget.NewButtonWithIcon(i18n.L("Copy link"), theme.ContentCopyIcon(), u.copyLink)
	leave := widget.NewButtonWithIcon(i18n.L("Leave"), theme.LogoutIcon(), u.leave)
	header := container.NewBorder(nil, nil,
		container.NewHBox(v.room, v.lockBadge),
		container.NewHBox(copyBtn, leave, chrome))

	v.warn = label("")
	v.warn.Importance = widget.WarningImportance

	v.share = widget.NewButtonWithIcon(i18n.L("Go live: main monitor"), theme.MediaRecordIcon(), func() {
		if v.sharing {
			u.do(func() error { return u.a.SetSource("none") })
		} else {
			u.do(func() error { return u.a.SetSource("primary") })
		}
	})
	v.pick = widget.NewButton(i18n.L("Share a window..."), u.sourceDialog)
	v.opts = widget.NewButtonWithIcon(i18n.L("Share options..."), theme.SettingsIcon(), u.showShareSheet)
	shareBar := newCard("", container.NewHBox(v.share, v.pick, v.opts))

	v.self = newSelfRow(u)
	v.alone = caption(i18n.L("Nobody else yet. Copy the link above to invite friends."))
	v.rows = container.NewVBox()

	v.relay = caption("")
	toggleRelay := func(wantOn bool) {
		u.do(func() error { return u.a.Volunteer(wantOn) })
	}
	v.relaySw = newToggleSwitch(toggleRelay)
	v.relayLbl = &tapLabel{tap: func() {
		if !v.relaySw.disabled {
			want := !v.relayOn
			v.relaySw.SetChecked(want)
			toggleRelay(want)
		}
	}}
	v.relayLbl.Text = i18n.L("Volunteer as relay")
	v.relayLbl.Wrapping = fyne.TextWrapOff // in a row with About relays: a wrapping label would shrink to nothing
	v.relayLbl.ExtendBaseWidget(v.relayLbl)

	aboutRelays := widget.NewButton(i18n.L("About relays"), u.relayDialog)
	aboutRelays.Importance = widget.LowImportance
	v.sounds = newIconToggle(u.tips, musicWhiteRes, func() {
		on := !u.st.PlaySounds
		u.do(func() error { return u.a.SetPlaySounds(on) })
	})
	v.autoWatch = newIconToggle(u.tips, autoWatchOnRes, func() {
		on := !u.st.AutoWatch
		u.do(func() error { return u.a.SetAutoWatch(on) })
	})
	v.autoWatch.aspect = 38.0 / 24
	v.qr = newIconToggle(u.tips, qrWhiteRes, u.qrDialog)
	v.qr.meaning = i18n.L("Show a QR code to join from a phone")
	relayFooter := container.NewVBox(
		widget.NewSeparator(),
		v.relay,
		container.NewBorder(nil, nil,
			container.NewHBox(v.relaySw, v.relayLbl, aboutRelays),
			container.NewHBox(v.qr, v.sounds, v.autoWatch)),
	)

	peopleList := container.NewVScroll(container.NewVBox(v.self.root, widget.NewSeparator(), v.alone, v.rows))
	peopleCard := newCard(i18n.L("People"), container.NewBorder(nil, relayFooter, nil, nil, peopleList))

	v.panel = newPersonPanel(u, func() {
		v.selectPeer("")
	})
	v.panel.root.Hide()
	v.split = container.New(sideSplitLayout{}, peopleCard, v.panel.root)

	top := container.NewVBox(v.notes.box, header, v.warn, shareBar)
	v.root = container.NewPadded(container.NewBorder(top, nil, nil, nil, v.split))
	return v
}

func (v *callView) selectPeer(id string) {
	if v.selectedID == id {
		v.selectedID = ""
	} else {
		v.selectedID = id
	}
	for pid, r := range v.people {
		r.setSelected(pid == v.selectedID)
	}
	if v.selectedID == "" || v.u.st.Session == nil {
		v.panel.root.Hide()
		v.split.Refresh()
		return
	}
	s := v.u.st.Session
	for _, m := range s.Members {
		if m.ID == v.selectedID {
			v.panel.update(m, s.IsOwner)
			v.panel.root.Show()
			v.split.Refresh()
			return
		}
	}
	v.selectedID = ""
	v.panel.root.Hide()
	v.split.Refresh()
}

// bold is a section header that wraps so translations don't widen the panel.
func bold(t string) *widget.Label {
	l := label(t)
	l.TextStyle.Bold = true
	return l
}

func (v *callView) update(st app.UIState) {
	s := st.Session
	setUpdateButton(v.upd, st)
	v.notes.set(v.u, st)
	v.room.SetText(s.Room)
	v.sharing = s.Source != "" && s.Source != "none"
	if v.sharing {
		v.share.SetText(i18n.L("Stop sharing"))
		v.share.SetIcon(theme.MediaStopIcon())
		v.share.Importance = widget.DangerImportance
		v.pick.SetText(i18n.L("Share something else..."))
	} else {
		v.share.SetText(i18n.L("Go live: main monitor"))
		v.share.SetIcon(theme.MediaRecordIcon())
		v.share.Importance = widget.HighImportance
		v.pick.SetText(i18n.L("Share a window..."))
	}
	v.share.Refresh()

	if s.Encrypted {
		v.lockBadge.SetContent(lockClosedRes, i18n.L("End-to-end encrypted"), colSuccessBg, colSuccess, colSuccess)
	} else {
		v.lockBadge.SetContent(lockOpenRes, i18n.L("Not end-to-end encrypted"), colWarningBg, colWarning, colWarning)
	}

	var warns []string
	if s.Reconnecting {
		warns = append(warns, i18n.L("Reconnecting to the server... the call continues meanwhile."))
	}
	if s.SourceNote != "" {
		warns = append(warns, i18n.Msg(s.SourceNote))
	} else if !v.sharing {
		warns = append(warns, i18n.L("You're watching. Press Go live to share your main monitor."))
	}
	if s.ResolutionNote != "" {
		warns = append(warns, i18n.Msg(s.ResolutionNote))
	}
	if s.ShareNotice != "" {
		warns = append(warns, i18n.Msg(s.ShareNotice))
	}
	var bad []string
	for _, m := range s.Members {
		if m.State == "untrusted" {
			bad = append(bad, m.Name)
		}
	}
	if len(bad) > 0 {
		warns = append(warns, i18n.L("Refused to connect to {{.Names}}: their encryption keys didn't match their identity. The connection may be intercepted.", i18n.D{"Names": strings.Join(bad, ", ")}))
	}
	people := len(s.Members) + 1
	if len(s.Relay) == 0 && people > s.MeshComfortPeers && s.MeshComfortPeers > 0 {
		warns = append(warns, i18n.N("{{.Count}} people in a full mesh: everyone who shares uploads their screen to each of the others. Expect lower quality, or have someone with a fast upload volunteer as relay below.", people))
	}
	if s.RelayRefused {
		warns = append(warns, i18n.L("Relay mode needs everyone in the room on Bellion Share build 31 or newer."))
	}
	v.warn.SetText(strings.Join(warns, "\n"))
	if len(warns) > 0 {
		v.warn.Show()
	} else {
		v.warn.Hide()
	}

	v.self.update(st, s)

	// Footer toggles.
	if st.PlaySounds {
		v.sounds.Set(musicWhiteRes, i18n.L("Sounds on: a chime when someone joins or goes live"), false)
	} else {
		v.sounds.Set(musicOffRes, i18n.L("Sounds off"), false)
	}
	if st.AutoWatch {
		v.autoWatch.Set(autoWatchOnRes, i18n.L("New streams open automatically"), false)
	} else {
		v.autoWatch.Set(autoWatchOffRes, i18n.L("New streams wait until you open them"), false)
	}

	// Relay footer state.
	v.relay.SetText(relayText(s))
	meRelay, meWaiting := false, false
	for _, e := range s.Relay {
		meRelay = meRelay || e.Me
	}
	for _, e := range s.RelayWaiting {
		meWaiting = meWaiting || e.Me
	}
	v.relayOn = meRelay || meWaiting
	v.relaySw.SetChecked(v.relayOn)
	switch {
	case meRelay:
		v.relayLbl.SetText(i18n.L("Stop relaying"))
	case meWaiting:
		v.relayLbl.SetText(i18n.L("Leave the relay queue"))
	default:
		v.relayLbl.SetText(i18n.L("Volunteer as relay"))
	}

	// Rows: rebuild only when people come or go.
	var ids []string
	for _, m := range s.Members {
		ids = append(ids, m.ID)
	}
	if order := strings.Join(ids, ","); order != v.order {
		v.order = order
		next := map[string]*peerRow{}
		var objs []fyne.CanvasObject
		for _, m := range s.Members {
			id := m.ID
			r := v.people[id]
			if r == nil {
				r = newPeerRow(v.u, id, func() { v.selectPeer(id) })
			}
			r.setSelected(id == v.selectedID)
			next[id] = r
			objs = append(objs, r.root)
		}
		v.people = next
		v.rows.Objects = objs
		v.rows.Refresh()
	}

	var selectedMember *session.Member
	for i := range s.Members {
		m := s.Members[i]
		v.people[m.ID].update(m)
		if m.ID == v.selectedID {
			selectedMember = &m
		}
	}
	if v.selectedID != "" {
		if selectedMember != nil {
			v.panel.update(*selectedMember, s.IsOwner)
			if !v.panel.root.Visible() {
				v.panel.root.Show()
				v.split.Refresh()
			}
		} else {
			v.selectedID = ""
			v.panel.root.Hide()
			v.split.Refresh()
		}
	}

	if len(s.Members) == 0 {
		v.alone.Show()
	} else {
		v.alone.Hide()
	}
}

func relayText(s *session.State) string {
	if s.RelayPaused {
		return i18n.N("Relay mode is paused: someone runs an older Bellion Share whose relay mode serves up to {{.Count}} people. Everyone sends to everyone until the room is smaller or everyone has updated.", s.RelayMaxPeers)
	}
	if len(s.Relay) == 0 {
		return i18n.L("Everyone sends their video to everyone (full mesh).")
	}
	var parts []string
	for _, e := range s.Relay {
		if e.Me {
			parts = append(parts, i18n.N("You relay for {{.Count}} people", e.Children))
		} else {
			parts = append(parts, i18n.N("{{.Name}} relays for {{.Count}} people", e.Children, i18n.D{"Name": e.Name}))
		}
	}
	for _, e := range s.RelayWaiting {
		if e.Me {
			parts = append(parts, i18n.L("You are standing by"))
		} else {
			parts = append(parts, i18n.L("{{.Name}} is standing by", i18n.D{"Name": e.Name}))
		}
	}
	return i18n.L("Relays: {{.List}}.", i18n.D{"List": strings.Join(parts, "; ")})
}

// selfRow is our own row at the top of the People list, with the preview eye.
type selfRow struct {
	root   fyne.CanvasObject
	avatar *avatarCircle
	ident  *nameTag
	status *widget.Label
	eye    *eyeToggle
}

func newSelfRow(u *UI) *selfRow {
	r := &selfRow{
		avatar: newAvatar("?", 34),
		ident:  newNameTag(),
		status: caption(""),
		eye: newEyeToggle(u.tips, func() {
			u.do(func() error { return u.a.ToggleWatch(feeds.Self) })
		}),
	}
	info := container.NewVBox(r.ident, r.status)
	gap := canvas.NewRectangle(color.Transparent) // where peers' speaker goes: the eyes line up
	gap.SetMinSize(fyne.NewSize(34, 34))
	row := container.NewBorder(nil, nil, r.avatar, container.NewHBox(gap, r.eye), container.New(&insetLayout{pad: 4}, info))
	r.root = container.New(&insetLayout{pad: 4}, row)
	return r
}

func (r *selfRow) update(st app.UIState, s *session.State) {
	name, tag := st.Name, st.Tag
	if s.Name != "" {
		name = s.Name
	}
	if s.Tag != "" {
		tag = s.Tag
	}
	r.avatar.SetName(name)
	badge := i18n.L("(you)")
	if s.IsOwner {
		badge += " · " + i18n.L("(owner)")
	}
	r.ident.Set(name, tag, badge, true)
	sharing := s.Source != "" && s.Source != "none"
	if sharing {
		res := s.Resolution
		switch res {
		case "":
			res = "720p"
		case "2160p":
			res = "4K"
		}
		if s.SelfEye == feeds.EyeClosed {
			r.status.SetText(i18n.L("Sharing {{.Resolution}} (preview hidden)", i18n.D{"Resolution": res}))
		} else {
			r.status.SetText(i18n.L("Sharing {{.Resolution}}", i18n.D{"Resolution": res}))
		}
		r.eye.SetEye(s.SelfEye)
		if s.SelfEye == feeds.EyeClosed {
			r.eye.SetTip(i18n.L("Show your preview"))
		} else {
			r.eye.SetTip(i18n.L("Hide your preview"))
		}
	} else {
		r.status.SetText(i18n.L("Watching only"))
		r.eye.SetEye(feeds.EyeGrey)
		r.eye.SetTip(i18n.L("You aren't sharing"))
	}
	if s.Audio.Mode == "test" { // so it isn't left on unnoticed
		r.status.SetText(r.status.Text + " · " + i18n.L("Test tone"))
	}
}

// peerRow is one remote person in the People list.
type peerRow struct {
	root     fyne.CanvasObject
	id       string
	bg       *canvas.Rectangle
	avatar   *avatarCircle
	ident    *nameTag
	status   *widget.Label
	eye      *eyeToggle
	speaker  *iconToggle
	m        session.Member
	selected bool
	hovered  bool
}

func newPeerRow(u *UI, id string, onSelect func()) *peerRow {
	bg := canvas.NewRectangle(color.Transparent)
	bg.CornerRadius = 10
	r := &peerRow{
		id:     id,
		bg:     bg,
		avatar: newAvatar("?", 34),
		ident:  newNameTag(),
		status: caption(""),
		eye: newEyeToggle(u.tips, func() {
			u.do(func() error { return u.a.ToggleWatch(id) })
		}),
	}
	r.speaker = newIconToggle(u.tips, speakerNoneRes, func() {
		q, vol, muted := r.m.Quality, r.m.Volume, !r.m.Muted
		if q == "" {
			q = "auto"
		}
		if !muted && vol == 0 { // unmuting someone turned all the way down
			vol = 1
		}
		u.do(func() error { return u.a.SetPeer(id, q, vol, muted) })
	})
	mainBody := container.NewBorder(nil, nil, r.avatar, nil, container.New(&insetLayout{pad: 4}, container.NewVBox(r.ident, r.status)))
	tap := newTapArea(mainBody, onSelect, func(hov bool) {
		r.hovered = hov
		r.refreshBg()
	})
	inner := container.NewBorder(nil, nil, nil, container.NewHBox(r.speaker, r.eye), tap)
	r.root = container.NewStack(r.bg, container.New(&insetLayout{pad: 6}, inner))
	return r
}

func (r *peerRow) setSelected(sel bool) {
	if r.selected == sel {
		return
	}
	r.selected = sel
	r.refreshBg()
}

func (r *peerRow) refreshBg() {
	switch {
	case r.selected:
		r.bg.FillColor = colAccentFill
		r.bg.StrokeColor = colAccent
		r.bg.StrokeWidth = 1.5
	case r.hovered:
		r.bg.FillColor = colRowHover
		r.bg.StrokeColor = colCardBorder
		r.bg.StrokeWidth = 1
	default:
		r.bg.FillColor = color.Transparent
		r.bg.StrokeColor = color.Transparent
		r.bg.StrokeWidth = 0
	}
	r.bg.Refresh()
}

func (r *peerRow) update(m session.Member) {
	r.m = m
	res, glow := speakerIcon(m)
	r.speaker.Set(res, speakerTip(m), glow)
	r.avatar.SetName(m.Name)
	badge := ""
	if m.Owner {
		badge = i18n.L("(owner)")
	}
	r.ident.Set(m.Name, m.Tag, badge, false)
	txt, imp := peerStatus(m)
	r.status.SetText(txt)
	r.status.Importance = imp
	r.status.Refresh()
	eye := m.Eye
	if eye == "" {
		switch {
		case m.Sharing && m.Watching:
			eye = feeds.EyeOpen
		case m.Sharing:
			eye = feeds.EyeClosed
		default:
			eye = feeds.EyeGrey
		}
	}
	r.eye.SetEye(eye)
	r.eye.SetTip(eyeTip(eye, m.Name))
}

// speakerIcon is the person row's audio state: white while their audio
// reaches us (glowing while it isn't silence), struck through while we
// muted them, a dim speaker with a dot while they send none.
func speakerIcon(m session.Member) (res fyne.Resource, glow bool) {
	switch {
	case m.Muted && m.Heard:
		return speakerMutedRes, false
	case m.Muted:
		return speakerMutedDimRes, false
	case !m.Heard:
		return speakerNoneRes, false
	case m.Loud:
		return speakerLoudRes, true
	}
	return speakerWhiteRes, false
}

func speakerTip(m session.Member) string {
	name := i18n.D{"Name": m.Name}
	switch {
	case m.Muted:
		return i18n.L("Unmute {{.Name}}", name)
	case !m.Heard:
		return i18n.L("No audio from {{.Name}}. Click to mute them anyway.", name)
	}
	return i18n.L("Mute {{.Name}}", name)
}

// eyeTip is a person's eye tooltip.
func eyeTip(eye feeds.Eye, name string) string {
	switch eye {
	case feeds.EyeOpen:
		return i18n.L("Stop watching {{.Name}}", i18n.D{"Name": name})
	case feeds.EyeClosed:
		return i18n.L("Watch {{.Name}}", i18n.D{"Name": name})
	}
	return i18n.L("{{.Name}} isn't sharing", i18n.D{"Name": name})
}

func peerStatus(m session.Member) (string, widget.Importance) {
	switch m.State {
	case "untrusted":
		return upperFirst(stateText(m)), widget.DangerImportance
	case "unreachable":
		if m.Hint != "" {
			return i18n.L("Can't connect: {{.Why}}", i18n.D{"Why": i18n.Msg(m.Hint)}), widget.DangerImportance
		}
		return upperFirst(stateText(m)), widget.DangerImportance
	case "connected":
	default:
		return upperFirst(stateText(m)), widget.WarningImportance
	}

	word, imp := linkQuality(m)
	var text string
	switch {
	case m.Notice != "":
		text = i18n.Msg(m.Notice)
		imp = widget.WarningImportance
	case m.Sharing && (!m.Watching || m.Eye == feeds.EyeClosed):
		text = i18n.L("{{.Quality}}, not watching", i18n.D{"Quality": word})
	case m.Sharing && m.Recv > 0:
		res := fmt.Sprintf("%dp", m.Recv)
		if m.Recv == 2160 {
			res = "4K"
		}
		if m.Loss >= 0.05 {
			text = i18n.L("{{.Quality}}, {{.Resolution}}, dropping frames", i18n.D{"Quality": word, "Resolution": res})
		} else {
			text = i18n.L("{{.Quality}}, watching in {{.Resolution}}", i18n.D{"Quality": word, "Resolution": res})
		}
	case m.Sharing:
		text = i18n.L("{{.Quality}}, starting stream", i18n.D{"Quality": word})
	default:
		text = word
	}
	if m.Muted && m.Sharing && m.Watching {
		text += " " + i18n.L("(muted)")
	}
	if m.KeyChanged {
		text += " · " + i18n.L("New device or key since you last saw this name.")
		imp = widget.WarningImportance
	}
	return text, imp
}

// caption is small secondary text that stays readable.
func caption(t string) *widget.Label {
	l := label(t)
	l.SizeName = theme.SizeNameCaptionText
	return l
}

// linkQuality sums the link up in a word.
func linkQuality(m session.Member) (string, widget.Importance) {
	if m.State != "connected" {
		return "-", widget.MediumImportance
	}
	switch {
	case m.Loss >= 0.05 || m.RTTms >= 300:
		return i18n.L("Unstable"), widget.DangerImportance
	case m.Loss >= 0.01 || m.RTTms >= 150:
		return i18n.L("Fair"), widget.WarningImportance
	}
	return i18n.L("Good"), widget.SuccessImportance
}

// updateDiscord shows the Discord voice channel banner: Join (without sharing)
// and Share when nobody is sharing yet, or "NAME is sharing their screen" with
// member avatars and Watch when someone is sharing.
func (v *lobbyView) updateDiscord(u *UI, d *app.Discord) {
	dc, ok := u.a.(discordBackend)
	if ok && d != nil && d.Status == "off" {
		v.dc.Show()
		v.dcAvatars.Hide()
		v.dcTitle.SetText(i18n.L("Discord isn't running on this computer (or not found). Start it to watch or go live in your voice channel."))
		v.dcSub.Hide()
		v.dcPrimary.Hide()
		v.dcSecondary.Hide()
		if v.live.Importance != widget.HighImportance {
			v.live.Importance = widget.HighImportance
			v.live.Refresh()
		}
		return
	}
	if !ok || d == nil {
		v.dc.Hide()
		if v.live.Importance != widget.HighImportance {
			v.live.Importance = widget.HighImportance
			v.live.Refresh()
		}
		return
	}

	v.dc.Show()
	v.dcSub.Hide()
	v.dcPrimary.Show()
	v.dcPrimary.Enable()
	v.dcSecondary.Hide()
	v.dcSecondary.Enable()
	v.syncDiscordAvatars(nil)

	lead := d.Status == "ready" && d.ChannelID != ""
	v.dcPrimary.Importance = widget.MediumImportance
	v.dcSecondary.Importance = widget.MediumImportance
	v.live.Importance = widget.HighImportance
	if lead {
		v.dcPrimary.Importance = widget.HighImportance
		v.live.Importance = widget.MediumImportance
	}
	defer v.dcPrimary.Refresh()
	defer v.dcSecondary.Refresh()
	defer v.live.Refresh()

	where := "#" + d.Channel
	if d.Guild != "" {
		where += " (" + d.Guild + ")"
	}

	someoneSharing := len(d.Sharers) > 0 || (d.Room != "" && len(d.Sharers) == 0 && len(d.Members) == 0)

	switch {
	case d.Status == "login":
		v.dcTitle.SetText(i18n.L("Use your Discord voice channel: watch or go live there in one click."))
		if d.Error != "" {
			v.dcSub.SetText(i18n.L("Last try: {{.Err}}", i18n.D{"Err": i18n.Msg(d.Error)}))
			v.dcSub.Show()
		}
		v.dcPrimary.SetText(i18n.L("Connect Discord"))
		v.dcPrimary.SetIcon(theme.LoginIcon())
		v.dcPriAct = dc.DiscordLogin
	case d.Status == "waiting":
		v.dcTitle.SetText(i18n.L("Approve Bellion in the Discord window that just opened."))
		v.dcPrimary.Hide()
	case d.Status == "linking":
		v.dcTitle.SetText(i18n.L("Approve Bellion in the browser tab that just opened."))
		v.dcPrimary.SetText(i18n.L("Open it again"))
		v.dcPrimary.SetIcon(theme.LoginIcon())
		v.dcPriAct = dc.DiscordLogin
	case d.ChannelID == "":
		v.dcTitle.SetText(i18n.L("Join a Discord voice channel to watch or go live there in one click."))
		v.dcPrimary.Hide()
		if d.BotURL != "" {
			// The bot only sees servers it was added to.
			v.dcSub.SetText(i18n.L("Already in one? That Discord server doesn't have the Bellion bot, so Bellion can't see you there. Ask a server admin to add it."))
			v.dcSub.Show()
			v.dcPrimary.SetText(i18n.L("Copy link for the admin"))
			v.dcPrimary.SetIcon(theme.ContentCopyIcon())
			v.dcPrimary.Show()
			link := d.BotURL
			v.dcPriAct = func() {
				u.fa.Clipboard().SetContent(link)
				v.dcPrimary.SetText(i18n.L("Link copied"))
				v.dcPrimary.SetIcon(theme.ConfirmIcon())
			}
		}
	case someoneSharing:
		v.syncDiscordAvatars(d.Members)
		switch {
		case len(d.Sharers) == 1:
			v.dcTitle.SetText(i18n.L("{{.Name}} is sharing their screen", i18n.D{"Name": d.Sharers[0]}))
			v.dcSub.SetText(where)
			v.dcSub.Show()
		case len(d.Sharers) > 1:
			v.dcTitle.SetText(i18n.N("{{.Name}} and {{.Count}} others are sharing", len(d.Sharers)-1, i18n.D{"Name": d.Sharers[0]}))
			v.dcSub.SetText(where)
			v.dcSub.Show()
		default:
			v.dcTitle.SetText(i18n.N("Someone is sharing in {{.Where}} ({{.Count}} people).", d.People, i18n.D{"Where": where}))
		}
		v.dcPrimary.SetText(i18n.L("Watch"))
		v.dcPrimary.SetIcon(theme.VisibilityIcon())
		open := d.Open
		v.dcPriAct = func() {
			if open {
				u.busyDo(v.dcPrimary, func() error { return dc.JoinChannel(ctx(), "") })
				return
			}
			u.passwordDialog(func(pw string) {
				u.busyDo(v.dcPrimary, func() error { return dc.JoinChannel(ctx(), pw) })
			})
		}
	default:
		v.syncDiscordAvatars(d.Members)
		v.dcTitle.SetText(i18n.L("You're in {{.Where}} on Discord.", i18n.D{"Where": where}))
		if d.Room != "" && len(d.Members) > 0 {
			v.dcSub.SetText(i18n.N("{{.Count}} people in the room", len(d.Members)))
			v.dcSub.Show()
		}
		hasRoom, open := d.Room != "", d.Open

		v.dcPrimary.SetText(i18n.L("Join"))
		v.dcPrimary.SetIcon(theme.LoginIcon())
		v.dcPriAct = func() {
			if hasRoom && !open {
				u.passwordDialog(func(pw string) {
					u.busyDo(v.dcPrimary, func() error { return dc.EnterChannel(ctx(), pw, true) })
				})
				return
			}
			sh := normalizeShareState(u.st)
			pw := ""
			if !hasRoom && sh.RequirePassword {
				pw = strings.TrimSpace(sh.RoomPassword)
			}
			u.busyDo(v.dcPrimary, func() error { return dc.EnterChannel(ctx(), pw, sh.E2EE) })
		}

		v.dcSecondary.SetText(i18n.L("Share"))
		v.dcSecondary.SetIcon(theme.MediaRecordIcon())
		v.dcSecondary.Show()
		v.dcSecAct = func() {
			sh := normalizeShareState(u.st)
			if hasRoom && !open {
				u.passwordDialog(func(pw string) {
					u.goLiveWith(func() error { return dc.GoLiveInChannel(ctx(), pw, sh.E2EE) }, false)
				})
				return
			}
			pw := ""
			if !hasRoom && sh.RequirePassword {
				pw = strings.TrimSpace(sh.RoomPassword)
			}
			u.goLiveWith(func() error { return dc.GoLiveInChannel(ctx(), pw, sh.E2EE) }, false)
		}
	}

	if note := discordServerNote(u.st.Servers); note != "" && d.Status == "ready" && d.ChannelID != "" {
		if v.dcSub.Visible() {
			v.dcSub.SetText(v.dcSub.Text + " · " + note)
		} else {
			v.dcSub.SetText(note)
			v.dcSub.Show()
		}
		v.dcPrimary.Disable()
		v.dcSecondary.Disable()
		v.dcPrimary.Importance, v.live.Importance = widget.MediumImportance, widget.HighImportance
	}
}

func (v *lobbyView) syncDiscordAvatars(members []string) {
	if len(members) == 0 {
		v.dcAvatars.Objects = nil
		v.dcAvatars.Hide()
		return
	}
	const maxCircles = 5
	limit := min(len(members), maxCircles)
	objs := make([]fyne.CanvasObject, 0, limit+1)
	for i := 0; i < limit; i++ {
		objs = append(objs, newAvatar(members[i], 28))
	}
	if extra := len(members) - limit; extra > 0 {
		objs = append(objs, newChip(nil, fmt.Sprintf("+%d", extra), nil))
	}
	v.dcAvatars.Objects = objs
	v.dcAvatars.Show()
	v.dcAvatars.Refresh()
}
