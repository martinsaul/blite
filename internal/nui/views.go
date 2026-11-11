package nui

import (
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"blite/internal/app"
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
	shown   string // the update result we already showed
}

func newNotices(u *UI) *notices {
	n := &notices{err: label(""), updText: label(""), reach: label("")}
	n.err.Importance = widget.DangerImportance
	n.reach.Importance = widget.WarningImportance
	n.updBtn = widget.NewButtonWithIcon("Install and restart", theme.DownloadIcon(), func() {
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
	n.err.SetText(st.Error)
	show(n.err, st.Error != "")
	up := st.Update
	if up != nil && up.Result != "" && up.Result != n.shown {
		n.shown = up.Result
		res := up.Result
		u.info("Update", res, func() { u.a.DismissUpdateResult() })
	}
	switch {
	case up != nil && up.Problem != "":
		n.updText.SetText("Update problem: " + up.Problem)
		n.updText.Importance = widget.DangerImportance
		n.updBtn.Hide()
		show(n.update, true)
	case up != nil && up.Available != "":
		t := "Bellion Share " + up.Available + " is available."
		if up.Mandatory {
			t = "Bellion Share " + up.Available + " is required: you can't start or join a session until you install it."
		}
		if up.Notes != "" {
			t += " " + up.Notes
		}
		if st.Session != nil {
			t += " Installing restarts and rejoins the call in a few seconds."
		}
		n.updText.SetText(t)
		n.updText.Importance = widget.MediumImportance
		show(n.updBtn, up.CanApply)
		show(n.update, true)
	case st.Session != nil && st.Session.UpdateRecommended:
		n.updText.SetText("A newer Bellion Share is available. Please update when you can (Help > Check for updates).")
		n.updText.Importance = widget.WarningImportance
		n.updBtn.Hide()
		show(n.update, true)
	default:
		show(n.update, false)
	}
	n.updText.Refresh()
	show(n.reach, st.Reach != nil && !st.Reach.OK)
	if st.Reach != nil {
		n.reach.SetText(st.Reach.Text)
	}
}

// lobbyView: not in a session. One click to go live, one to watch.
type lobbyView struct {
	u        *UI
	root     fyne.CanvasObject
	who      *widget.Label
	notes    *notices
	live     *widget.Button
	liveWin  *widget.Button
	needPw   *widget.Check
	pw       *widget.Entry
	e2ee     *widget.Check
	link     *widget.Entry
	join     *widget.Button
	clipJoin *widget.Button
	clipLink string
	dc       *fyne.Container // the Discord voice channel row
	dcText   *widget.Label
	dcBtn    *widget.Button
	dcAct    func()
}

func newLobby(u *UI) *lobbyView {
	v := &lobbyView{u: u, who: widget.NewLabel(""), notes: newNotices(u)}
	v.who.TextStyle.Bold = true
	v.pw = widget.NewEntry()
	v.pw.SetPlaceHolder("Password")
	v.pw.Hide()
	v.needPw = widget.NewCheck("Require a password", func(on bool) {
		if on {
			v.pw.Show()
		} else {
			v.pw.Hide()
		}
	})
	v.e2ee = widget.NewCheck("Encrypt end to end (everyone needs build 72 or newer)", nil)
	v.e2ee.SetChecked(true)
	golive := func(pick bool) func() {
		return func() {
			pw := ""
			if v.needPw.Checked {
				pw = strings.TrimSpace(v.pw.Text)
			}
			u.goLive(pw, v.e2ee.Checked, pick)
		}
	}
	v.live = widget.NewButtonWithIcon("Go live: main monitor", theme.MediaRecordIcon(), golive(false))
	v.live.Importance = widget.HighImportance
	v.liveWin = widget.NewButton("Share a window instead...", golive(true))
	v.liveWin.Importance = widget.LowImportance

	v.clipJoin = widget.NewButtonWithIcon("", theme.LoginIcon(), func() {
		l := v.clipLink
		u.busyDo(v.clipJoin, func() error { return u.a.JoinLink(ctx(), l) })
	})
	v.clipJoin.Importance = widget.HighImportance
	v.clipJoin.Hide()
	v.link = widget.NewEntry()
	v.link.SetPlaceHolder("Paste a share link")
	v.join = widget.NewButtonWithIcon("Watch", theme.VisibilityIcon(), func() {
		l := strings.TrimSpace(v.link.Text)
		u.busyDo(v.join, func() error { return u.a.JoinLink(ctx(), l) })
	})
	v.link.OnSubmitted = func(string) { v.join.OnTapped() }

	opts := widget.NewAccordion(widget.NewAccordionItem("Options", container.NewVBox(v.needPw, v.pw, v.e2ee)))
	start := widget.NewCard("Share your screen", "Starts a session, shares your screen and copies the link to send.",
		container.NewVBox(container.NewHBox(v.live, v.liveWin), opts))
	watch := widget.NewCard("Watch someone", "Click a share link in Discord, or paste it here.",
		container.NewVBox(v.clipJoin, container.NewBorder(nil, nil, nil, v.join, v.link)))
	change := widget.NewButton("Change name...", u.nameDialog)
	head := container.NewHBox(v.who, change)
	v.dcText = widget.NewLabel("")
	v.dcText.TextStyle.Bold = true
	v.dcBtn = widget.NewButtonWithIcon("", theme.MediaRecordIcon(), func() {
		if v.dcAct != nil {
			v.dcAct()
		}
	})
	v.dcBtn.Importance = widget.HighImportance
	v.dc = container.NewVBox(widget.NewCard("", "", container.NewBorder(nil, nil, nil, v.dcBtn, v.dcText)))
	v.dc.Hide()
	v.root = container.NewPadded(container.NewVBox(head, v.notes.box, v.dc, container.NewGridWithColumns(2, start, watch)))
	v.checkClipboard(u)
	return v
}

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
	v.clipJoin.SetText("Watch session " + code + " (link you copied)")
	v.clipJoin.Show()
}

func (v *lobbyView) update(st app.UIState) {
	v.who.SetText("You are " + displayName(st.Name, st.Tag))
	v.notes.set(v.u, st)
	v.updateDiscord(v.u, st.Discord)
	if st.Update != nil && st.Update.Mandatory {
		v.live.Disable()
		v.liveWin.Disable()
		v.join.Disable()
		v.clipJoin.Disable()
	}
}

// callView: the room on top, the people below.
type callView struct {
	u       *UI
	root    fyne.CanvasObject
	notes   *notices
	room    *widget.Label
	enc     *widget.Label
	safety  *widget.Label
	warn    *widget.Label // our share or the mesh needs attention
	relay   *widget.Label
	alone   *widget.Label
	rows    *fyne.Container
	people  map[string]*peerRow
	order   string
	share   *widget.Button
	pick    *widget.Button
	sharing bool
}

func newCall(u *UI) *callView {
	v := &callView{u: u, notes: newNotices(u), people: map[string]*peerRow{}}
	v.room = widget.NewLabel("")
	v.room.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}
	v.room.SizeName = theme.SizeNameHeadingText
	v.enc = widget.NewLabel("")
	v.safety = label("")
	v.warn = label("")
	v.warn.Importance = widget.WarningImportance
	v.relay = caption("")
	v.alone = widget.NewLabel("Nobody else yet. Send them the share link (Room > Copy share link).")
	copyBtn := widget.NewButtonWithIcon("Copy share link", theme.ContentCopyIcon(), u.copyLink)
	v.share = widget.NewButtonWithIcon("Go live: main monitor", theme.MediaRecordIcon(), func() {
		if v.sharing {
			u.do(func() error { return u.a.SetSource("none") })
		} else {
			u.do(func() error { return u.a.SetSource("primary") })
		}
	})
	v.pick = widget.NewButton("Share a window...", u.sourceDialog)
	leave := widget.NewButtonWithIcon("Leave", theme.LogoutIcon(), u.leave)

	roomLine := container.NewBorder(nil, nil,
		container.NewHBox(widget.NewLabel("Room"), v.room, v.enc),
		container.NewHBox(v.share, v.pick, copyBtn, leave))
	header := container.NewVBox(roomLine, v.safety)

	v.rows = container.NewVBox()
	head := peerGrid(bold("Name"), bold("Status"), bold("You send"), bold("Connection"), bold("Their video"), bold("Volume"), widget.NewLabel(""))
	people := widget.NewCard("People", "", container.NewBorder(container.NewVBox(head, widget.NewSeparator()), v.relay, nil, nil,
		container.NewVScroll(container.NewVBox(v.alone, v.rows))))
	v.root = container.NewPadded(container.NewBorder(container.NewVBox(v.notes.box, header, v.warn), nil, nil, nil, people))
	return v
}

func bold(t string) *widget.Label {
	l := widget.NewLabel(t)
	l.TextStyle.Bold = true
	return l
}

// peerGrid lays a row out in the People table's columns.
func peerGrid(cells ...fyne.CanvasObject) *fyne.Container {
	return container.New(&columns{weights: []float32{1.4, 1.1, 1, 1.6, 0.9, 1.3, 0.5}}, cells...)
}

func (v *callView) update(st app.UIState) {
	s := st.Session
	v.notes.set(v.u, st)
	v.room.SetText(s.Room)
	v.sharing = s.Source != "" && s.Source != "none"
	if v.sharing {
		v.share.SetText("Stop sharing")
		v.share.SetIcon(theme.MediaStopIcon())
		v.share.Importance = widget.DangerImportance
		v.pick.SetText("Share something else...")
	} else {
		v.share.SetText("Go live: main monitor")
		v.share.SetIcon(theme.MediaRecordIcon())
		v.share.Importance = widget.HighImportance
		v.pick.SetText("Share a window...")
	}
	v.share.Refresh()
	if s.Encrypted {
		v.enc.SetText("End-to-end encrypted")
	} else {
		v.enc.SetText("Not end-to-end encrypted")
	}
	connected := 0
	for _, m := range s.Members {
		if m.State == "connected" {
			connected++
		}
	}
	if connected > 0 && s.Safety != "" {
		v.safety.SetText("Safety code " + s.Safety + " (everyone should see the same; Room > Safety code explains)")
		v.safety.Show()
	} else {
		v.safety.Hide()
	}
	var warns []string
	if s.Reconnecting {
		warns = append(warns, "Reconnecting to the server... the call continues meanwhile.")
	}
	if s.SourceNote != "" {
		warns = append(warns, s.SourceNote)
	} else if s.Source == "" || s.Source == "none" {
		warns = append(warns, "You're watching. Press Go live to share your main monitor.")
	}
	if s.ResolutionNote != "" {
		warns = append(warns, s.ResolutionNote)
	}
	var bad []string
	for _, m := range s.Members {
		if m.State == "untrusted" {
			bad = append(bad, m.Name)
		}
	}
	if len(bad) > 0 {
		warns = append(warns, "Refused to connect to "+strings.Join(bad, ", ")+": their encryption keys didn't match their identity. The connection may be intercepted.")
	}
	people := len(s.Members) + 1
	if len(s.Relay) == 0 && people > s.MeshComfortPeers && s.MeshComfortPeers > 0 {
		warns = append(warns, fmt.Sprintf("%d people in a full mesh: everyone who shares uploads their screen %d times. Expect lower quality, or have someone with a fast upload volunteer as relay (People menu).", people, people-1))
	}
	if s.RelayRefused {
		warns = append(warns, "Relay mode needs everyone in the room on Bellion Share build 31 or newer.")
	}
	v.warn.SetText(strings.Join(warns, "\n"))
	if len(warns) > 0 {
		v.warn.Show()
	} else {
		v.warn.Hide()
	}
	v.relay.SetText(relayText(s))

	// Rows: rebuild only when people come or go, so a slider being
	// dragged survives the stats updates.
	var ids []string
	for _, m := range s.Members {
		ids = append(ids, m.ID)
	}
	if order := strings.Join(ids, ","); order != v.order {
		v.order = order
		next := map[string]*peerRow{}
		var objs []fyne.CanvasObject
		for _, m := range s.Members {
			r := v.people[m.ID]
			if r == nil {
				r = newPeerRow(v.u, m.ID)
			}
			next[m.ID] = r
			objs = append(objs, r.root, widget.NewSeparator())
		}
		v.people = next
		v.rows.Objects = objs
		v.rows.Refresh()
	}
	for _, m := range s.Members {
		v.people[m.ID].update(m, s.IsOwner)
	}
	if len(s.Members) == 0 {
		v.alone.Show()
	} else {
		v.alone.Hide()
	}
}

func relayText(s *session.State) string {
	if s.RelayPaused {
		return fmt.Sprintf("Relay mode is paused: someone runs an older Bellion Share whose relay mode serves up to %d people. Everyone sends to everyone until the room is smaller or everyone has updated.", s.RelayMaxPeers)
	}
	if len(s.Relay) == 0 {
		return "Everyone sends their video to everyone (full mesh). People > Volunteer as relay if you have a fast upload."
	}
	var parts []string
	for _, e := range s.Relay {
		n := e.Name
		if e.Me {
			n = "You"
		}
		parts = append(parts, fmt.Sprintf("%s relay for %d", n, e.Children))
	}
	for _, e := range s.RelayWaiting {
		n := e.Name
		if e.Me {
			n = "You"
		}
		parts = append(parts, n+" standing by")
	}
	return "Relays: " + strings.Join(parts, "; ") + "."
}

// peerRow is one person in the People table.
type peerRow struct {
	root     *fyne.Container
	id       string
	name     *widget.Label
	client   *widget.Label
	notice   *widget.Label
	status   *widget.Label
	send     *widget.Label
	link     *widget.Label
	linkSub  *widget.Label
	quality  *widget.Select
	vol      *widget.Slider
	mute     *widget.Check
	more     *widget.Button
	m        session.Member
	owner    bool
	setting  bool // update() is setting widgets: don't send their callbacks
	dragging bool
}

var qualityLabels = []string{"Best", "Low", "Off (audio only)"}
var qualityValues = []string{"auto", "low", "off"}

func newPeerRow(u *UI, id string) *peerRow {
	r := &peerRow{id: id, name: widget.NewLabel(""), client: widget.NewLabel(""), notice: label(""),
		status: widget.NewLabel(""), send: label(""), link: widget.NewLabel(""), linkSub: caption("")}
	r.name.TextStyle.Bold = true
	r.client.SizeName = theme.SizeNameCaptionText
	r.notice.Importance = widget.WarningImportance
	r.notice.SizeName = theme.SizeNameCaptionText
	set := func() {
		if r.setting {
			return
		}
		q := r.m.Quality
		for i, l := range qualityLabels {
			if l == r.quality.Selected {
				q = qualityValues[i]
			}
		}
		vol, muted := r.vol.Value/100, r.mute.Checked
		u.do(func() error { return u.a.SetPeer(id, q, vol, muted) })
	}
	r.quality = widget.NewSelect(qualityLabels, func(string) { set() })
	r.vol = widget.NewSlider(0, 200)
	r.vol.Step = 5
	r.vol.OnChanged = func(float64) { r.dragging = !r.setting }
	r.vol.OnChangeEnded = func(float64) { r.dragging = false; set() }
	r.mute = widget.NewCheck("Mute", func(bool) { set() })
	r.more = widget.NewButtonWithIcon("", theme.MoreVerticalIcon(), func() {
		menu := fyne.NewMenu("", u.personItems(r.m, r.owner)...)
		c := fyne.CurrentApp().Driver().CanvasForObject(r.more)
		pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(r.more)
		widget.ShowPopUpMenuAtPosition(menu, c, pos.Add(fyne.NewPos(0, r.more.Size().Height)))
	})
	r.root = peerGrid(container.NewVBox(r.name, r.client, r.notice), r.status, r.send, container.NewVBox(r.link, r.linkSub),
		container.NewCenter(r.quality), container.NewBorder(nil, nil, nil, r.mute, r.vol), container.NewCenter(r.more))
	return r
}

func (r *peerRow) update(m session.Member, owner bool) {
	r.m, r.owner = m, owner
	r.setting = true
	defer func() { r.setting = false }()
	n := displayName(m.Name, m.Tag)
	if m.Owner {
		n += "  (owner)"
	}
	r.name.SetText(n)
	c := m.Client
	if m.Recv > 0 {
		res := fmt.Sprintf("%dp", m.Recv)
		if m.Recv == 2160 {
			res = "4K"
		}
		c += ", you get " + res
	}
	r.client.SetText(c)
	var notes []string
	if m.KeyChanged {
		notes = append(notes, "New device or key since you last saw this name.")
	}
	if m.Notice != "" {
		notes = append(notes, m.Notice)
	}
	if m.Hint != "" {
		notes = append(notes, m.Hint)
	}
	if m.WindowClosed {
		notes = append(notes, "You closed their window: People > "+m.Name+" > Show their window.")
	}
	r.notice.SetText(strings.Join(notes, " "))
	if len(notes) > 0 {
		r.notice.Show()
	} else {
		r.notice.Hide()
	}
	r.status.SetText(stateText(m))
	switch m.State {
	case "connected":
		r.status.Importance = widget.SuccessImportance
	case "unreachable", "untrusted":
		r.status.Importance = widget.DangerImportance
	case "reconnecting", "connecting":
		r.status.Importance = widget.WarningImportance
	default:
		r.status.Importance = widget.MediumImportance
	}
	r.status.Refresh()
	r.send.SetText(sendText(m))
	word, imp := linkQuality(m)
	r.link.SetText(word)
	r.link.Importance = imp
	r.link.Refresh()
	r.linkSub.SetText(linkText(m))
	for i, v := range qualityValues {
		if v == m.Quality && r.quality.Selected != qualityLabels[i] {
			r.quality.SetSelected(qualityLabels[i])
		}
	}
	if !r.dragging {
		r.vol.SetValue(m.Volume * 100)
	}
	r.mute.SetChecked(m.Muted)
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
		return "Poor", widget.DangerImportance
	case m.Loss >= 0.01 || m.RTTms >= 150:
		return "Fair", widget.WarningImportance
	}
	return "Good", widget.SuccessImportance
}

// updateDiscord shows one click to watch or go live in the user's Discord
// voice channel.
func (v *lobbyView) updateDiscord(u *UI, d *app.Discord) {
	dc, ok := u.a.(discordBackend)
	if ok && d != nil && d.Status == "off" {
		v.dc.Show()
		v.dcText.SetText("Discord isn't running on this computer (or not found). Start it to watch or go live in your voice channel.")
		v.dcBtn.Hide()
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
	v.dcBtn.Show()
	v.dcBtn.Enable()
	// One blue button at a time: the Discord action leads when it's the
	// likely one, the lobby's Go live otherwise.
	lead := d.Status == "ready" && d.ChannelID != ""
	v.dcBtn.Importance = widget.MediumImportance
	v.live.Importance = widget.HighImportance
	if lead {
		v.dcBtn.Importance = widget.HighImportance
		v.live.Importance = widget.MediumImportance
	}
	defer v.dcBtn.Refresh()
	defer v.live.Refresh()
	where := "#" + d.Channel
	if d.Guild != "" {
		where += " (" + d.Guild + ")"
	}
	switch {
	case d.Status == "login":
		t := "Use your Discord voice channel: watch or go live there in one click."
		if d.Error != "" {
			t += "\nLast try: " + d.Error
		}
		v.dcText.SetText(t)
		v.dcBtn.SetText("Connect Discord")
		v.dcBtn.SetIcon(theme.LoginIcon())
		v.dcAct = dc.DiscordLogin
	case d.Status == "waiting":
		v.dcText.SetText("Approve Bellion in the Discord window that just opened.")
		v.dcBtn.Hide()
	case d.ChannelID == "":
		v.dcText.SetText("Join a Discord voice channel to watch or go live there in one click.")
		v.dcBtn.Hide()
	case d.Room != "":
		people := fmt.Sprintf("%d people", d.People)
		if d.People == 1 {
			people = "1 person"
		}
		v.dcText.SetText("Someone is sharing in " + where + " (" + people + ").")
		v.dcBtn.SetText("Watch")
		v.dcBtn.SetIcon(theme.VisibilityIcon())
		open := d.Open
		v.dcAct = func() {
			if open {
				u.busyDo(v.dcBtn, func() error { return dc.JoinChannel(ctx(), "") })
				return
			}
			u.passwordDialog(func(pw string) { u.busyDo(v.dcBtn, func() error { return dc.JoinChannel(ctx(), pw) }) })
		}
	default:
		v.dcText.SetText("You're in " + where + " on Discord.")
		v.dcBtn.SetText("Go live in #" + d.Channel)
		v.dcBtn.SetIcon(theme.MediaRecordIcon())
		v.dcAct = func() {
			pw, e := "", v.e2ee.Checked
			if v.needPw.Checked {
				pw = strings.TrimSpace(v.pw.Text)
			}
			u.goLiveWith(func() error { return dc.GoLiveInChannel(ctx(), pw, e) }, false)
		}
	}
}
