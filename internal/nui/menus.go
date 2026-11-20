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

// updateWaiting reports whether the gear button should show its update dot.
func updateWaiting(st app.UIState) bool {
	if st.Update != nil && st.Update.Available != "" {
		return true
	}
	return st.Session != nil && st.Session.UpdateRecommended
}

// newChromeButtons creates the top-right Gear and "?" icon buttons shared by
// the Home and Call headers.
func (u *UI) newChromeButtons() (*iconDotButton, *iconDotButton, *fyne.Container) {
	gear := newIconDotButton(theme.SettingsIcon(), func(b *iconDotButton) {
		showMenuBelow(b, u.gearMenuItems()...)
	})
	help := newIconDotButton(theme.HelpIcon(), func(b *iconDotButton) {
		showMenuBelow(b, u.helpMenuItems()...)
	})
	return gear, help, container.NewHBox(gear, help)
}

// gearMenuItems builds the Gear button's popup menu.
func (u *UI) gearMenuItems() []*fyne.MenuItem {
	up := u.st.Update
	install := fyne.NewMenuItem(i18n.L("Install update and restart"), func() {
		u.do(func() error { return u.a.InstallUpdate(ctx()) })
	})
	install.Disabled = up == nil || up.Available == "" || !up.CanApply
	if up != nil && up.Available != "" {
		install.Label = i18n.L("Install {{.Version}} and restart", i18n.D{"Version": up.Available})
	}
	checkUp := disabled(fyne.NewMenuItem(i18n.L("Check for updates"), func() {
		u.do(func() error { return u.a.CheckUpdate(ctx()) })
	}), up == nil || up.Checking)

	autostart := check(i18n.L("Start Bellion Share when I sign in"), u.st.Autostart != nil && *u.st.Autostart, func() {
		on := !(u.st.Autostart != nil && *u.st.Autostart)
		u.do(func() error { return u.a.SetAutostart(on) })
	})
	autostart.Disabled = u.st.Autostart == nil

	sounds := check(i18n.L("Play sounds"), u.st.PlaySounds, func() {
		on := !u.st.PlaySounds
		u.do(func() error { return u.a.SetPlaySounds(on) })
	})

	autoWatch := check(i18n.L("Start watching new streams automatically"), u.st.AutoWatch, func() {
		on := !u.st.AutoWatch
		u.do(func() error { return u.a.SetAutoWatch(on) })
	})

	return nonNil(
		fyne.NewMenuItem(i18n.L("Your name..."), u.nameDialog),
		fyne.NewMenuItem(i18n.L("Your identity key..."), u.idKeyDialog),
		fyne.NewMenuItem(i18n.L("Servers..."), u.serversDialog),
		u.languageMenu(),
		u.uiScaleMenu(),
		fyne.NewMenuItemSeparator(),
		autostart,
		sounds,
		autoWatch,
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
				u.info(i18n.L("Interface size"), err.Error(), nil)
				return
			}
			u.info(i18n.L("Interface size"), i18n.L("The new size applies when you restart Bellion Share."), nil)
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
