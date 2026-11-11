package nui

import (
	"fmt"
	"strings"

	"fyne.io/fyne/v2"

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
	fmt.Fprintf(&b, "%s|%s|%v|%v|", st.Name, st.Tag, st.Autostart != nil && *st.Autostart, st.Layout)
	if st.Update != nil {
		fmt.Fprintf(&b, "%s|%v|%v|", st.Update.Available, st.Update.CanApply, st.Update.Checking)
	}
	fmt.Fprintf(&b, "%d|", len(u.devices))
	if s := st.Session; s != nil {
		fmt.Fprintf(&b, "%s|%s|%s|%s|%s|%v|%s|%v|%v|", s.Room, s.Source, s.Resolution, s.Mode, s.Output, s.Audio, s.Audio.Mode, s.IsOwner, s.GridClosed)
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
	return fyne.NewMenu("Room",
		disabled(fyne.NewMenuItem("Start a session...", u.createDialog), in),
		disabled(fyne.NewMenuItem("Join a session...", u.joinDialog), in),
		fyne.NewMenuItemSeparator(),
		disabled(fyne.NewMenuItem("Copy share link", u.copyLink), !in),
		disabled(fyne.NewMenuItem("Safety code...", u.safetyDialog), !in),
		disabled(fyne.NewMenuItem("Leave session", u.leave), !in),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Your name...", u.nameDialog),
		fyne.NewMenuItem("Your identity key...", u.idKeyDialog),
	)
}

func (u *UI) shareMenu() *fyne.Menu {
	s := u.st.Session
	if s == nil {
		return fyne.NewMenu("Share", disabled(fyne.NewMenuItem("Start or join a session to share", nil), true))
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
	outs := []*fyne.MenuItem{check("System default", s.Output == "", func() { u.do(func() error { return u.a.SetOutput("") }) })}
	for _, d := range u.devices {
		d := d
		outs = append(outs, check(d.Name, s.Output == d.ID, func() { u.do(func() error { return u.a.SetOutput(d.ID) }) }))
	}
	outs = append(outs, fyne.NewMenuItemSeparator(), fyne.NewMenuItem("Refresh devices", func() { go u.loadDevices() }))
	return fyne.NewMenu("Share",
		src("All screens combined", "screen"),
		check("A monitor or window...", other, u.sourceDialog),
		src("Test pattern", "test"),
		src("Nothing (watch only)", "none"),
		fyne.NewMenuItemSeparator(),
		sub("Resolution", res("720p", "720p"), res("1080p", "1080p"), res("4K", "2160p")),
		sub("Tuned for", mode("Desktop: sharp text, 30 fps", "desktop"), mode("Game: smooth motion, 60 fps", "game")),
		sub("Preview of your stream", prev("Off", ""), prev("Small floating window", "window"), prev("In a corner of the video window", "grid")),
		fyne.NewMenuItemSeparator(),
		sub("Audio you share", am("No audio", "none"), am("All apps (except Bellion Share)", "system"),
			check("Only these apps...", s.Audio.Mode == "apps", u.audioDialog),
			windowAudioItem(s.Audio.Mode == "window")),
		sub("Play the call on", outs...),
	)
}

func (u *UI) viewMenu() *fyne.Menu {
	l := u.st.Layout
	set := func(mode string) func() {
		return func() { u.do(func() error { return u.a.SetLayout(mode, l.Rows, l.Cols, l.Preview) }) }
	}
	s := u.st.Session
	return fyne.NewMenu("View",
		check("A separate window for each person", l.Mode != "grid", set("windows")),
		check("All in one window", l.Mode == "grid", set("grid")),
		disabled(fyne.NewMenuItem("Arrange the window...", u.arrangeDialog), l.Mode != "grid"),
		fyne.NewMenuItemSeparator(),
		disabled(fyne.NewMenuItem("Show the video window", func() { u.do(func() error { return u.a.ShowWindow("") }) }),
			s == nil || !s.GridClosed || l.Mode != "grid"),
	)
}

// peopleMenu has one submenu per person, plus relay.
func (u *UI) peopleMenu() *fyne.Menu {
	s := u.st.Session
	if s == nil {
		return fyne.NewMenu("People", disabled(fyne.NewMenuItem("Nobody yet", nil), true))
	}
	var items []*fyne.MenuItem
	for _, m := range s.Members {
		items = append(items, sub(displayName(m.Name, m.Tag), u.personItems(m, s.IsOwner)...))
	}
	if len(items) == 0 {
		items = append(items, disabled(fyne.NewMenuItem("Nobody else yet", nil), true))
	}
	meRelay, meWaiting := false, false
	for _, e := range s.Relay {
		meRelay = meRelay || e.Me
	}
	for _, e := range s.RelayWaiting {
		meWaiting = meWaiting || e.Me
	}
	relay := "Volunteer as relay"
	if meRelay {
		relay = "Stop relaying"
	} else if meWaiting {
		relay = "Leave the relay queue"
	}
	on := !meRelay && !meWaiting
	items = append(items, fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem(relay, func() { u.do(func() error { return u.a.Volunteer(on) }) }),
		fyne.NewMenuItem("About relays...", u.relayDialog))
	return fyne.NewMenu("People", items...)
}

func (u *UI) personItems(m session.Member, owner bool) []*fyne.MenuItem {
	set := func(q string, vol float64, muted bool) {
		u.do(func() error { return u.a.SetPeer(m.ID, q, vol, muted) })
	}
	q := func(label, v string) *fyne.MenuItem {
		return check(label, m.Quality == v, func() { set(v, m.Volume, m.Muted) })
	}
	items := []*fyne.MenuItem{
		sub("Their video", q("Best", "auto"), q("Low", "low"), q("Off (audio only)", "off")),
		check("Mute", m.Muted, func() { set(m.Quality, m.Volume, !m.Muted) }),
	}
	if m.Pair != "" {
		items = append(items, disabled(fyne.NewMenuItem("Network path: "+m.Pair, nil), true))
	}
	if m.WindowClosed {
		items = append(items, fyne.NewMenuItem("Show their window", func() { u.do(func() error { return u.a.ShowWindow(m.ID) }) }))
	}
	if owner {
		name := m.Name
		items = append(items, fyne.NewMenuItemSeparator(),
			disabled(fyne.NewMenuItem("Make owner...", func() {
				u.confirm("Make "+name+" the owner?", "You can't take it back yourself.", func() error { return u.a.Transfer(m.ID) })
			}), m.State != "connected"),
			fyne.NewMenuItem("Disconnect...", func() {
				u.confirm("Disconnect "+name+"?", "They can rejoin with the link.", func() error { return u.a.Kick(m.ID, false) })
			}),
			fyne.NewMenuItem("Ban...", func() {
				u.confirm("Ban "+name+"?", "They won't be able to rejoin this room.", func() error { return u.a.Kick(m.ID, true) })
			}))
	}
	return items
}

func (u *UI) helpMenu() *fyne.Menu {
	up := u.st.Update
	install := fyne.NewMenuItem("Install update and restart", func() { u.do(func() error { return u.a.InstallUpdate(ctx()) }) })
	install.Disabled = up == nil || up.Available == "" || !up.CanApply
	if up != nil && up.Available != "" {
		install.Label = "Install " + up.Available + " and restart"
	}
	checkUp := disabled(fyne.NewMenuItem("Check for updates", func() { u.do(func() error { return u.a.CheckUpdate(ctx()) }) }), up == nil || up.Checking)
	auto := check("Start Bellion Share when I sign in", u.st.Autostart != nil && *u.st.Autostart, func() {
		on := !(u.st.Autostart != nil && *u.st.Autostart)
		u.do(func() error { return u.a.SetAutostart(on) })
	})
	auto.Disabled = u.st.Autostart == nil
	return fyne.NewMenu("Help",
		fyne.NewMenuItem("Send feedback...", u.feedbackDialog),
		sub("Logs and problem reports",
			fyne.NewMenuItem("Send logs to the developers", u.sendLogs),
			fyne.NewMenuItem("Open log folder", func() { u.do(u.a.OpenLogFolder) }),
			fyne.NewMenuItem("Copy diagnostics", func() {
				go func() {
					t := u.a.Diagnostics()
					fyne.Do(func() { u.fa.Clipboard().SetContent(t) })
				}()
			})),
		fyne.NewMenuItemSeparator(),
		checkUp, install, auto,
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("About Bellion Share", u.aboutDialog),
	)
}

// windowAudioItem shows that the audio follows the shared window; choosing
// another audio option overrides it.
func windowAudioItem(on bool) *fyne.MenuItem {
	it := fyne.NewMenuItem("The shared window's app (automatic)", nil)
	it.Checked, it.Disabled = on, true
	return it
}
