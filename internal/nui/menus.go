package nui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"

	"blite/internal/app"
	"blite/internal/i18n"
	"blite/internal/uiscale"
)

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

// updateWaiting reports whether the update button shows.
func updateWaiting(st app.UIState) bool {
	if st.Update != nil && st.Update.Available != "" {
		return true
	}
	return st.Session != nil && st.Session.UpdateRecommended
}

// newChromeButtons creates the top-right Gear, update and "?" icon buttons
// shared by the Home and Call headers. The update button shows only while an
// update waits (setUpdateButton).
func (u *UI) newChromeButtons() (gear, upd, help *iconDotButton, chrome *fyne.Container) {
	gear = newIconDotButton(theme.SettingsIcon(), func(b *iconDotButton) {
		showMenuBelow(b, u.gearMenuItems()...)
	})
	upd = newIconDotButton(theme.DownloadIcon(), func(*iconDotButton) { u.showUpdate() })
	upd.tips = u.tips
	upd.dot = true
	upd.Hide()
	help = newIconDotButton(theme.HelpIcon(), func(b *iconDotButton) {
		showMenuBelow(b, u.helpMenuItems()...)
	})
	return gear, upd, help, container.NewHBox(gear, upd, help)
}

// setUpdateButton shows the update button while an update waits, its
// tooltip saying which.
func setUpdateButton(b *iconDotButton, st app.UIState) {
	if !updateWaiting(st) {
		b.Hide()
		return
	}
	up := st.Update
	switch {
	case up == nil || up.Available == "":
		b.SetTip(i18n.L("Check for updates")) // the server says ours is old
	case up.Downloading:
		b.SetTip(i18n.L("Downloading Bellion Share {{.Version}}: {{.Percent}}%", i18n.D{"Version": up.Available, "Percent": up.Progress}))
	default:
		b.SetTip(i18n.L("Bellion Share {{.Version}} is available.", i18n.D{"Version": up.Available}))
	}
	b.Show()
}

// showUpdate brings back the corner notice of the waiting update (or looks
// for one, when only the server said ours is old).
func (u *UI) showUpdate() {
	up := u.st.Update
	if up == nil || up.Available == "" {
		u.do(func() error { return u.a.CheckUpdate(ctx()) })
		return
	}
	if u.toasts == nil {
		return
	}
	u.toasts.Objects = nil
	u.toasts.Add(u.newUpdateNotice(up))
	u.toasts.Refresh()
}

// checkUpdate is the menu's "Check for updates": it always answers, with the
// update's notice or a note that this is the newest build.
func (u *UI) checkUpdate() {
	u.do(func() error {
		if err := u.a.CheckUpdate(ctx()); err != nil {
			return err
		}
		st, ok := u.a.State().(app.UIState)
		if !ok || st.Update == nil {
			return nil
		}
		up := *st.Update
		u.later(func() {
			switch {
			case up.Available != "":
				u.st.Update = &up
				u.showUpdate()
			case up.Problem != "": // the banner shows it
			case up.Offline:
				u.info(i18n.L("Couldn't check for updates"), i18n.L("Couldn't reach the update server. Check your connection and try again."), nil)
			default:
				u.info(i18n.L("You're up to date"), i18n.L("You have the newest version of Bellion Share."), nil)
			}
		})
		return nil
	})
}

// gearMenuItems builds the Gear button's popup menu.
func (u *UI) gearMenuItems() []*fyne.MenuItem {
	up := u.st.Update
	install := fyne.NewMenuItem(i18n.L("Install update and restart"), func() {
		u.do(func() error { return u.a.InstallUpdate(ctx()) })
	})
	install.Disabled = up == nil || up.Available == "" || !up.CanApply || up.Downloading
	if up != nil && up.Available != "" {
		install.Label = i18n.L("Install {{.Version}} and restart", i18n.D{"Version": up.Available})
	}
	checkUp := disabled(fyne.NewMenuItem(i18n.L("Check for updates"), u.checkUpdate), up == nil || up.Checking)

	autostart := check(i18n.L("Open at sign-in"), u.st.Autostart != nil && *u.st.Autostart, func() {
		on := !(u.st.Autostart != nil && *u.st.Autostart)
		u.do(func() error { return u.a.SetAutostart(on) })
	})
	autostart.Disabled = u.st.Autostart == nil

	var unlink *fyne.MenuItem
	if dc, ok := u.a.(discordBackend); ok && u.st.Discord != nil && u.st.Discord.Via == "bot" && u.st.Discord.Status == "ready" {
		unlink = fyne.NewMenuItem(i18n.L("Disconnect Discord ({{.User}})", i18n.D{"User": u.st.Discord.User}), dc.DiscordUnlink)
	}

	return nonNil(
		fyne.NewMenuItem(i18n.L("Your name..."), u.nameDialog),
		fyne.NewMenuItem(i18n.L("Your identity key..."), u.idKeyDialog),
		fyne.NewMenuItem(i18n.L("Servers..."), u.serversDialog),
		u.languageMenu(),
		u.uiScaleMenu(),
		unlink,
		fyne.NewMenuItemSeparator(),
		autostart,
		fyne.NewMenuItemSeparator(),
		checkUp,
		install,
	)
}

// nonNil drops absent (nil) items: Fyne crashes on a nil menu item.
func nonNil(items ...*fyne.MenuItem) []*fyne.MenuItem {
	out := items[:0]
	for _, i := range items {
		if i != nil {
			out = append(out, i)
		}
	}
	return out
}

// helpMenuItems builds the "?" button's popup menu.
func (u *UI) helpMenuItems() []*fyne.MenuItem {
	return []*fyne.MenuItem{
		fyne.NewMenuItem(i18n.L("Send feedback..."), u.feedbackDialog),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem(i18n.L("Send logs to the developers"), u.sendLogs),
		fyne.NewMenuItem(i18n.L("Open log folder"), func() { u.do(u.a.OpenLogFolder) }),
		fyne.NewMenuItem(i18n.L("Copy diagnostics"), func() {
			go func() {
				t := u.a.Diagnostics()
				u.later(func() { u.fa.Clipboard().SetContent(t) })
			}()
		}),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem(i18n.L("What's new"), u.whatsNew),
		fyne.NewMenuItem(i18n.L("About Bellion Share"), u.aboutDialog),
	}
}

// uiScaleMenu picks the interface size (X11 only): automatic (the
// desktop's scale) or a fixed one. Applies on the next start.
func (u *UI) uiScaleMenu() *fyne.MenuItem {
	if !uiscale.ManualSupported() {
		return nil
	}
	pref := app.LoadUIScale()
	var items []*fyne.MenuItem
	for _, p := range uiscale.Prefs {
		label := uiscale.Label(p) // the percentage Fyne applies
		if label == "" {
			label = i18n.L("Automatic (follow the desktop)")
		}
		items = append(items, check(label, uiscale.Same(pref, p), func() {
			if err := app.SaveUIScale(p); err != nil {
				u.info(i18n.L("Couldn't change the interface size"), upperFirst(i18n.Err(err).Error()), nil)
				return
			}
			u.info(i18n.L("Interface size changed"), i18n.L("The new size applies when you restart Bellion Share."), nil)
		}))
		if p == "" {
			items = append(items, fyne.NewMenuItemSeparator())
		}
	}
	return sub(i18n.L("Interface size"), items...)
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
