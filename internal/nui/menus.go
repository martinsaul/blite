package nui

import (
	"fmt"
	"strings"

	"fyne.io/fyne/v2"

	"blite/internal/i18n"
	"blite/internal/session"
)

// updateMenu rebuilds the menu bar when something it shows changed (not on
// every stats tick: that would close an open menu).
func (u *UI) updateMenu() {
	key := u.menuSignature()
	if key == u.menuKey {
		return
	}
	u.menuKey = key
	u.win.SetMainMenu(fyne.NewMainMenu(u.roomMenu(), u.shareMenu(), u.viewMenu(), u.peopleMenu(), u.helpMenu()))
}

func (u *UI) menuSignature() string {
	st := u.st
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%s|%v|%v|", i18n.Current(), st.Name, st.Tag, st.Autostart != nil && *st.Autostart, st.Layout)
	if st.Update != nil {
		fmt.Fprintf(&b, "%s|%v|%v|", st.Update.Available, st.Update.CanApply, st.Update.Checking)
	}
	fmt.Fprintf(&b, "%d|", len(u.devices))
	if s := st.Session; s != nil {
		fmt.Fprintf(&b, "%s|%s|%s|%s|%s|%v|%s|%v|%v|%v|", s.Room, s.Source, s.Resolution, s.Mode, s.Output, s.Audio, s.Audio.Mode, s.IsOwner, s.GridClosed, s.PreviewClosed)
		for _, r := range append(s.Relay, s.RelayWaiting...) {
			fmt.Fprintf(&b, "r%s%v", r.ID, r.Me)
		}
		for _, m := range s.Members {
			fmt.Fprintf(&b, "m%s%s%s%s%v%v%v%s;", m.ID, m.Name, m.Tag, m.State, m.Owner, m.Muted, m.WindowClosed, m.Quality)
		}
	}
	return b.String()
}

func check(label string, on bool, f func()) *fyne.MenuItem {
	i := fyne.NewMenuItem(label, f)
	i.Checked = on
	return i
}

func disabled(i *fyne.MenuItem, off bool) *fyne.MenuItem {
	i.Disabled = off
	return i
}

func sub(label string, items ...*fyne.MenuItem) *fyne.MenuItem {
	i := fyne.NewMenuItem(label, nil)
	i.ChildMenu = fyne.NewMenu("", items...)
	return i
}

func (u *UI) roomMenu() *fyne.Menu {
	s := u.st.Session
	in := s != nil
	return fyne.NewMenu(i18n.L("Room"),
		disabled(fyne.NewMenuItem(i18n.L("Start a session..."), u.createDialog), in),
		disabled(fyne.NewMenuItem(i18n.L("Join a session..."), u.joinDialog), in),
		fyne.NewMenuItemSeparator(),
		disabled(fyne.NewMenuItem(i18n.L("Copy share link"), u.copyLink), !in),
		disabled(fyne.NewMenuItem(i18n.L("Safety code..."), u.safetyDialog), !in),
		disabled(fyne.NewMenuItem(i18n.L("Leave session"), u.leave), !in),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem(i18n.L("Your name..."), u.nameDialog),
		fyne.NewMenuItem(i18n.L("Your identity key..."), u.idKeyDialog),
		fyne.NewMenuItem(i18n.L("Servers..."), u.serversDialog),
		fyne.NewMenuItemSeparator(),
		quitItem(),
	)
}

func (u *UI) shareMenu() *fyne.Menu {
	s := u.st.Session
	if s == nil {
		return fyne.NewMenu(i18n.L("Share"), disabled(fyne.NewMenuItem(i18n.L("Start or join a session to share"), nil), true))
	}
	src := func(label, kind string) *fyne.MenuItem {
		return check(label, s.Source == kind, func() { u.do(func() error { return u.a.SetSource(kind) }) })
	}
	other := s.Source != "" && s.Source != "screen" && s.Source != "test" && s.Source != "none"
	res := func(label, v string) *fyne.MenuItem {
		return check(label, s.Resolution == v, func() { u.do(func() error { return u.a.SetResolution(v) }) })
	}
	mode := func(label, v string) *fyne.MenuItem {
		return check(label, s.Mode == v, func() { u.do(func() error { return u.a.SetMode(v) }) })
	}
	l := u.st.Layout
	prev := func(label, v string) *fyne.MenuItem {
		return check(label, l.Preview == v, func() { u.do(func() error { return u.a.SetLayout(l.Mode, l.Rows, l.Cols, v) }) })
	}
	am := func(label, v string) *fyne.MenuItem {
		return check(label, s.Audio.Mode == v, func() { u.do(func() error { return u.a.SetAudio(v, s.Audio.Apps) }) })
	}
	outs := []*fyne.MenuItem{check(i18n.L("System default"), s.Output == "", func() { u.do(func() error { return u.a.SetOutput("") }) })}
	for _, d := range u.devices {
		d := d
		outs = append(outs, check(d.Name, s.Output == d.ID, func() { u.do(func() error { return u.a.SetOutput(d.ID) }) }))
	}
	outs = append(outs, fyne.NewMenuItemSeparator(), fyne.NewMenuItem(i18n.L("Refresh devices"), func() { go u.loadDevices() }))
	return fyne.NewMenu(i18n.L("Share"),
		src(i18n.L("All screens combined"), "screen"),
		check(i18n.L("A monitor or window..."), other, u.sourceDialog),
		src(i18n.L("Test pattern"), "test"),
		src(i18n.L("Nothing (watch only)"), "none"),
		fyne.NewMenuItemSeparator(),
		sub(i18n.L("Resolution"), res("720p", "720p"), res("1080p", "1080p"), res("4K", "2160p")),
		sub(i18n.L("Tuned for"), mode(i18n.L("Desktop: sharp text, 30 fps"), "desktop"), mode(i18n.L("Game: smooth motion, 60 fps"), "game")),
		sub(i18n.L("Preview of your stream"), prev(i18n.L("Off"), ""), prev(i18n.L("Small floating window"), "window"), prev(i18n.L("In a corner of the video window"), "grid")),
		fyne.NewMenuItemSeparator(),
		sub(i18n.L("Audio you share"), am(i18n.L("No audio"), "none"), am(i18n.L("All apps (except Bellion Share)"), "system"),
			check(i18n.L("Only these apps..."), s.Audio.Mode == "apps", u.audioDialog),
			windowAudioItem(s.Audio.Mode == "window")),
		sub(i18n.L("Play the call on"), outs...),
	)
}

func (u *UI) viewMenu() *fyne.Menu {
	l := u.st.Layout
	set := func(mode string) func() {
		return func() { u.do(func() error { return u.a.SetLayout(mode, l.Rows, l.Cols, l.Preview) }) }
	}
	return fyne.NewMenu(i18n.L("View"),
		check(i18n.L("A separate window for each person"), l.Mode != "grid", set("windows")),
		check(i18n.L("All in one window"), l.Mode == "grid", set("grid")),
		disabled(fyne.NewMenuItem(i18n.L("Arrange the window..."), u.arrangeDialog), l.Mode != "grid"),
		fyne.NewMenuItemSeparator(),
		disabled(fyne.NewMenuItem(i18n.L("Show video"), u.showVideo), !anyClosed(u.st.Session)),
		fyne.NewMenuItemSeparator(),
		u.languageMenu(),
	)
}

// peopleMenu has one submenu per person, plus relay.
func (u *UI) peopleMenu() *fyne.Menu {
	s := u.st.Session
	if s == nil {
		return fyne.NewMenu(i18n.L("People"), disabled(fyne.NewMenuItem(i18n.L("Nobody yet"), nil), true))
	}
	var items []*fyne.MenuItem
	for _, m := range s.Members {
		items = append(items, sub(displayName(m.Name, m.Tag), u.personItems(m, s.IsOwner)...))
	}
	if len(items) == 0 {
		items = append(items, disabled(fyne.NewMenuItem(i18n.L("Nobody else yet"), nil), true))
	}
	meRelay, meWaiting := false, false
	for _, e := range s.Relay {
		meRelay = meRelay || e.Me
	}
	for _, e := range s.RelayWaiting {
		meWaiting = meWaiting || e.Me
	}
	relay := i18n.L("Volunteer as relay")
	if meRelay {
		relay = i18n.L("Stop relaying")
	} else if meWaiting {
		relay = i18n.L("Leave the relay queue")
	}
	on := !meRelay && !meWaiting
	items = append(items, fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem(relay, func() { u.do(func() error { return u.a.Volunteer(on) }) }),
		fyne.NewMenuItem(i18n.L("About relays..."), u.relayDialog))
	return fyne.NewMenu(i18n.L("People"), items...)
}

func (u *UI) personItems(m session.Member, owner bool) []*fyne.MenuItem {
	set := func(q string, vol float64, muted bool) {
		u.do(func() error { return u.a.SetPeer(m.ID, q, vol, muted) })
	}
	q := func(label, v string) *fyne.MenuItem {
		return check(label, m.Quality == v, func() { set(v, m.Volume, m.Muted) })
	}
	items := []*fyne.MenuItem{
		sub(i18n.L("Their video"), q(i18n.L("Best"), "auto"), q(i18n.L("Low"), "low"), q(i18n.L("Off (audio only)"), "off")),
		check(i18n.L("Mute"), m.Muted, func() { set(m.Quality, m.Volume, !m.Muted) }),
	}
	if m.Pair != "" {
		items = append(items, disabled(fyne.NewMenuItem(i18n.L("Network path: {{.Path}}", i18n.D{"Path": m.Pair}), nil), true))
	}
	if m.WindowClosed {
		items = append(items, fyne.NewMenuItem(i18n.L("Show their window"), func() { u.do(func() error { return u.a.ShowWindow(m.ID) }) }))
	}
	if owner {
		name := m.Name
		items = append(items, fyne.NewMenuItemSeparator(),
			disabled(fyne.NewMenuItem(i18n.L("Make owner..."), func() {
				u.confirm(i18n.L("Make {{.Name}} the owner?", i18n.D{"Name": name}), i18n.L("You can't take it back yourself."), func() error { return u.a.Transfer(m.ID) })
			}), m.State != "connected"),
			fyne.NewMenuItem(i18n.L("Disconnect..."), func() {
				u.confirm(i18n.L("Disconnect {{.Name}}?", i18n.D{"Name": name}), i18n.L("They can rejoin with the link."), func() error { return u.a.Kick(m.ID, false) })
			}),
			fyne.NewMenuItem(i18n.L("Ban..."), func() {
				u.confirm(i18n.L("Ban {{.Name}}?", i18n.D{"Name": name}), i18n.L("They won't be able to rejoin this room."), func() error { return u.a.Kick(m.ID, true) })
			}))
	}
	return items
}

func (u *UI) helpMenu() *fyne.Menu {
	up := u.st.Update
	install := fyne.NewMenuItem(i18n.L("Install update and restart"), func() { u.do(func() error { return u.a.InstallUpdate(ctx()) }) })
	install.Disabled = up == nil || up.Available == "" || !up.CanApply
	if up != nil && up.Available != "" {
		install.Label = i18n.L("Install {{.Version}} and restart", i18n.D{"Version": up.Available})
	}
	checkUp := disabled(fyne.NewMenuItem(i18n.L("Check for updates"), func() { u.do(func() error { return u.a.CheckUpdate(ctx()) }) }), up == nil || up.Checking)
	auto := check(i18n.L("Start Bellion Share when I sign in"), u.st.Autostart != nil && *u.st.Autostart, func() {
		on := !(u.st.Autostart != nil && *u.st.Autostart)
		u.do(func() error { return u.a.SetAutostart(on) })
	})
	auto.Disabled = u.st.Autostart == nil
	return fyne.NewMenu(i18n.L("Help"),
		fyne.NewMenuItem(i18n.L("Send feedback..."), u.feedbackDialog),
		sub(i18n.L("Logs and problem reports"),
			fyne.NewMenuItem(i18n.L("Send logs to the developers"), u.sendLogs),
			fyne.NewMenuItem(i18n.L("Open log folder"), func() { u.do(u.a.OpenLogFolder) }),
			fyne.NewMenuItem(i18n.L("Copy diagnostics"), func() {
				go func() {
					t := u.a.Diagnostics()
					fyne.Do(func() { u.fa.Clipboard().SetContent(t) })
				}()
			})),
		fyne.NewMenuItemSeparator(),
		checkUp, install, auto,
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem(i18n.L("About Bellion Share"), u.aboutDialog),
	)
}

// windowAudioItem shows that the audio follows the shared window; choosing
// another audio option overrides it.
func windowAudioItem(on bool) *fyne.MenuItem {
	it := fyne.NewMenuItem(i18n.L("The shared window's app (automatic)"), nil)
	it.Checked, it.Disabled = on, true
	return it
}

// languageMenu picks the app's language: the system's, or one by hand.
// The window and menus switch at once.
func (u *UI) languageMenu() *fyne.MenuItem {
	pref := i18n.Preference()
	sys, _ := i18n.Lookup(i18n.System())
	items := []*fyne.MenuItem{
		check(i18n.L("Automatic: {{.Language}}", i18n.D{"Language": sys.Name}), pref == "", func() { u.setLanguage("") }),
		fyne.NewMenuItemSeparator(),
	}
	for _, l := range i18n.Languages {
		tag, name := l.Tag, l.Name
		if l.RTL { // Fyne can't lay right-to-left text out yet: mixed lines read wrong
			name = i18n.L("{{.Language}} (preview)", i18n.D{"Language": name})
		}
		items = append(items, check(name, pref == tag, func() { u.setLanguage(tag) }))
	}
	return sub(i18n.L("Language"), items...)
}
