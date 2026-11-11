package nui

import (
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"blite/internal/audio"
	"blite/internal/capture"
	"blite/internal/i18n"
)

// dialogError shows err like dialog.NewError, with our language's words
// (Fyne's own follow the system's).
func dialogError(err error, w fyne.Window) dialog.Dialog {
	return dialog.NewCustom(i18n.L("Error"), i18n.L("OK"), label(upperFirst(err.Error())), w)
}

func (u *UI) info(title, text string, done func()) {
	d := dialog.NewCustom(title, i18n.L("OK"), label(text), u.win)
	if done != nil { // Fyne calls the callback unchecked: nil crashes on OK
		d.SetOnClosed(done)
	}
	d.Resize(fyne.NewSize(460, 0))
	d.Show()
}

func (u *UI) confirm(title, text string, f func() error) {
	d := dialog.NewCustomConfirm(title, i18n.L("Yes"), i18n.L("No"), label(text), func(ok bool) {
		if ok {
			u.do(f)
		}
	}, u.win)
	d.Resize(fyne.NewSize(420, 0))
	d.Show()
}

// busyDo disables b while f runs.
func (u *UI) busyDo(b *widget.Button, f func() error) {
	b.Disable()
	go func() {
		err := f()
		fyne.Do(func() {
			b.Enable()
			if err != nil {
				u.showErr(err)
			}
		})
	}()
}

func (u *UI) leave() {
	u.confirm(i18n.L("Leave the session?"), i18n.L("You can rejoin with the link while the session is open."), func() error {
		u.a.Leave()
		return nil
	})
}

func (u *UI) copyLink() {
	if s := u.st.Session; s != nil {
		u.ownLink = s.Link // not offered as "a link you copied" after leaving
		u.fa.Clipboard().SetContent(s.Link)
		u.fa.SendNotification(fyne.NewNotification("Bellion Share", i18n.L("Share link copied. Anyone with it can join while the session is open.")))
	}
}

// goLive starts a session, shares the screen (or lets the user pick a
// window) and copies the link: one click from the lobby.
func (u *UI) goLive(pw string, e2ee, pick bool) {
	u.goLiveWith(func() error { return u.a.Create(ctx(), pw, e2ee) }, pick)
}

// goLiveWith runs create, then shares the screen and copies the link.
func (u *UI) goLiveWith(create func() error, pick bool) {
	go func() {
		err := create()
		if err == nil && !pick {
			err = u.a.SetSource("primary")
		}
		fyne.Do(func() {
			if err != nil {
				u.showErr(err)
				return
			}
			u.refresh()
			u.copyLink()
			if pick {
				u.sourceDialog()
			}
		})
	}()
}

func (u *UI) createDialog() {
	pw := widget.NewEntry()
	pw.SetPlaceHolder(i18n.L("Optional: leave empty for an open room"))
	e2ee := widget.NewCheck(i18n.L("Encrypt the call end to end (everyone needs build 72 or newer)"), nil)
	e2ee.SetChecked(true)
	dialog.ShowForm(i18n.L("Start a session"), i18n.L("Create"), i18n.L("Cancel"), []*widget.FormItem{
		widget.NewFormItem(i18n.L("Password"), pw), widget.NewFormItem("", e2ee),
	}, func(ok bool) {
		if ok {
			p, e := pw.Text, e2ee.Checked
			u.do(func() error { return u.a.Create(ctx(), p, e) })
		}
	}, u.win)
}

func (u *UI) joinDialog() {
	link := widget.NewEntry()
	link.SetPlaceHolder("https://your-server/j/XXXXXXXX#password")
	d := dialog.NewForm(i18n.L("Join a session"), i18n.L("Join"), i18n.L("Cancel"), []*widget.FormItem{widget.NewFormItem(i18n.L("Share link"), link)}, func(ok bool) {
		if ok {
			l := strings.TrimSpace(link.Text)
			u.do(func() error { return u.a.JoinLink(ctx(), l) })
		}
	}, u.win)
	d.Resize(fyne.NewSize(560, 0))
	d.Show()
}

func (u *UI) nameDialog() {
	name := widget.NewEntry()
	name.SetText(u.st.Name)
	d := dialog.NewForm(i18n.L("Your name"), i18n.L("Save"), i18n.L("Cancel"), []*widget.FormItem{
		hinted(widget.NewFormItem(i18n.L("Name"), name), i18n.L("Others see it with your tag (#{{.Tag}}), which comes from your identity key.", i18n.D{"Tag": u.st.Tag})),
	}, func(ok bool) {
		if ok {
			n := strings.TrimSpace(name.Text)
			u.do(func() error { return u.a.SetName(n) })
		}
	}, u.win)
	d.Resize(fyne.NewSize(460, 0))
	d.Show()
}

func (u *UI) idKeyDialog() {
	key := ""
	if s := u.st.Session; s != nil {
		key = s.IDKey
	}
	text := i18n.L("A hosted room's operator can list this key to make you its owner while you're in it.")
	if key == "" {
		text += "\n\n" + i18n.L("It shows here while you're in a session.")
	}
	var items []fyne.CanvasObject
	items = append(items, label(text))
	if key != "" {
		e := widget.NewEntry()
		e.SetText(key)
		items = append(items, e, widget.NewButton(i18n.L("Copy"), func() { u.fa.Clipboard().SetContent(key) }))
	}
	d := dialog.NewCustom(i18n.L("Your identity key"), i18n.L("Close"), container.NewVBox(items...), u.win)
	d.Resize(fyne.NewSize(520, 0))
	d.Show()
}

func (u *UI) safetyDialog() {
	s := u.st.Session
	if s == nil {
		return
	}
	u.info(i18n.L("Safety code"), i18n.L("Safety code: {{.Code}}", i18n.D{"Code": s.Safety})+"\n\n"+
		i18n.L("Read it aloud once: everyone should see the same numbers. If someone's differs, the connection is being intercepted."), nil)
}

// sourceDialog picks a monitor or window to share.
func (u *UI) sourceDialog() {
	var targets []capture.Target
	list := widget.NewList(func() int { return len(targets) },
		func() fyne.CanvasObject { return widget.NewLabel("") },
		func(i widget.ListItemID, o fyne.CanvasObject) {
			t := targets[i]
			text := i18n.L("Window: {{.Name}}", i18n.D{"Name": t.Name})
			if t.Kind == "monitor" {
				text = i18n.L("Monitor: {{.Name}}", i18n.D{"Name": t.Name})
			}
			o.(*widget.Label).SetText(text)
		})
	status := widget.NewLabel(i18n.L("Loading..."))
	load := func() {
		go func() {
			ts, err := capture.List()
			fyne.Do(func() {
				targets = ts
				status.SetText("")
				if err != nil {
					status.SetText(i18n.Msg(err.Error()))
				} else if len(ts) == 0 {
					status.SetText(i18n.L("No monitors or windows found."))
				}
				list.Refresh()
			})
		}()
	}
	var d dialog.Dialog
	list.OnSelected = func(i widget.ListItemID) {
		src := targets[i].Source
		u.do(func() error { return u.a.SetSource(src) })
		d.Hide()
	}
	body := container.NewBorder(nil, container.NewHBox(widget.NewButton(i18n.L("Refresh"), load), status), nil, nil, list)
	d = dialog.NewCustom(i18n.L("Share a monitor or window"), i18n.L("Cancel"), body, u.win)
	d.Resize(fyne.NewSize(620, 460))
	load()
	d.Show()
}

// audioDialog picks the apps whose audio we share.
func (u *UI) audioDialog() {
	s := u.st.Session
	if s == nil {
		return
	}
	group := widget.NewCheckGroup(nil, nil)
	keys := map[string]string{} // label -> key
	status := widget.NewLabel(i18n.L("Loading..."))
	load := func() {
		go func() {
			apps, err := audio.ListApps()
			fyne.Do(func() {
				var opts, sel []string
				keys = map[string]string{}
				for _, a := range apps {
					l := a.Name
					if a.PID > 0 {
						l += " (" + strconv.Itoa(a.PID) + ")"
					}
					keys[l] = a.Key
					opts = append(opts, l)
					for _, k := range s.Audio.Apps {
						if k == a.Key {
							sel = append(sel, l)
						}
					}
				}
				group.Options = opts
				group.SetSelected(sel)
				status.SetText("")
				if err != nil {
					status.SetText(i18n.Msg(err.Error()))
				} else if len(apps) == 0 {
					status.SetText(i18n.L("No apps are playing audio right now."))
				}
			})
		}()
	}
	body := container.NewBorder(label(i18n.L("Only these apps are heard in the call. Apps appear here while they play audio.")),
		container.NewHBox(widget.NewButton(i18n.L("Refresh"), load), status), nil, nil, container.NewVScroll(group))
	d := dialog.NewCustomConfirm(i18n.L("Audio you share"), i18n.L("Share these"), i18n.L("Cancel"), body, func(ok bool) {
		if !ok {
			return
		}
		var picked []string
		for _, l := range group.Selected {
			picked = append(picked, keys[l])
		}
		u.do(func() error { return u.a.SetAudio("apps", picked) })
	}, u.win)
	d.Resize(fyne.NewSize(520, 420))
	load()
	d.Show()
}

func (u *UI) arrangeDialog() {
	l := u.st.Layout
	rows, cols := widget.NewEntry(), widget.NewEntry()
	if l.Rows > 0 {
		rows.SetText(strconv.Itoa(l.Rows))
	}
	if l.Cols > 0 {
		cols.SetText(strconv.Itoa(l.Cols))
	}
	auto := widget.NewCheck(i18n.L("Automatically"), func(on bool) {
		if on {
			rows.Disable()
			cols.Disable()
		} else {
			rows.Enable()
			cols.Enable()
		}
	})
	auto.SetChecked(l.Rows == 0 && l.Cols == 0)
	dialog.ShowForm(i18n.L("Arrange the video window"), i18n.L("Apply"), i18n.L("Cancel"), []*widget.FormItem{
		widget.NewFormItem("", auto), widget.NewFormItem(i18n.L("Rows"), rows), widget.NewFormItem(i18n.L("Columns"), cols),
	}, func(ok bool) {
		if !ok {
			return
		}
		r, c := 0, 0
		if !auto.Checked {
			r, _ = strconv.Atoi(rows.Text)
			c, _ = strconv.Atoi(cols.Text)
			r, c = clamp(r), clamp(c)
		}
		u.do(func() error { return u.a.SetLayout(l.Mode, r, c, l.Preview) })
	}, u.win)
}

func hinted(f *widget.FormItem, hint string) *widget.FormItem {
	f.HintText = hint
	return f
}

func clamp(n int) int { return max(1, min(n, 8)) }

func (u *UI) relayDialog() {
	u.info(i18n.L("Relays"), i18n.L("Everyone sends their video to everyone (full mesh). Members with a fast upload can volunteer as relays: the others then send their video only to a relay, which forwards it. Big rooms use several relays."), nil)
}

func (u *UI) feedbackDialog() {
	kind := widget.NewRadioGroup([]string{i18n.L("An idea or something you'd like added"), i18n.L("Something that went wrong or bothers you")}, nil)
	kind.SetSelected(kind.Options[0])
	text := widget.NewMultiLineEntry()
	text.SetPlaceHolder(i18n.L("What would you like, or what happened?"))
	text.SetMinRowsVisible(7)
	text.Wrapping = fyne.TextWrapWord
	diag := widget.NewCheck(i18n.L("Include diagnostics (your setup and call stats, no names, keys or links)"), nil)
	diag.SetChecked(true)
	d := dialog.NewCustomConfirm(i18n.L("Tell us what you think"), i18n.L("Send"), i18n.L("Cancel"), container.NewVBox(kind, text, diag), func(ok bool) {
		if !ok || strings.TrimSpace(text.Text) == "" {
			return
		}
		k := "idea"
		if kind.Selected == kind.Options[1] {
			k = "problem"
		}
		t, dg := text.Text, diag.Checked
		go func() {
			err := u.a.Feedback(ctx(), k, t, dg)
			fyne.Do(func() {
				if err != nil {
					u.showErr(err)
				} else {
					u.info(i18n.L("Thank you"), i18n.L("Your feedback was sent."), nil)
				}
			})
		}()
	}, u.win)
	d.Resize(fyne.NewSize(560, 0))
	d.Show()
}

func (u *UI) sendLogs() {
	go func() {
		err := u.a.SendLogsNow(ctx())
		fyne.Do(func() {
			if err != nil {
				u.showErr(err)
				return
			}
			st := i18n.Msg(u.logs().Report)
			if st == "" {
				st = i18n.L("Your logs were sent to the developers.")
			}
			u.info(i18n.L("Logs"), st, nil)
		})
	}()
}

func (u *UI) aboutDialog() {
	l := u.logs()
	text := "Bellion Share " + u.st.Version + "\n" + i18n.L("Screen sharing between friends, peer to peer.")
	if up := u.st.Update; up != nil && up.Channel != "" {
		if up.Checked != "" {
			text += "\n" + i18n.L("Update channel: {{.Channel}}, last checked {{.Time}}", i18n.D{"Channel": up.Channel, "Time": up.Checked})
		} else {
			text += "\n" + i18n.L("Update channel: {{.Channel}}", i18n.D{"Channel": up.Channel})
		}
	}
	if l.Dir != "" {
		text += "\n" + i18n.L("Logs: {{.Path}}", i18n.D{"Path": l.Dir})
	}
	if l.Guard != "" {
		text += "\n" + i18n.L("Crash guard: {{.State}}", i18n.D{"State": i18n.Msg(l.Guard)})
	}
	u.info(i18n.L("About Bellion Share"), text, nil)
}

// passwordDialog asks for a session's password.
func (u *UI) passwordDialog(f func(pw string)) {
	pw := widget.NewPasswordEntry()
	dialog.ShowForm(i18n.L("This session has a password"), i18n.L("Join"), i18n.L("Cancel"), []*widget.FormItem{widget.NewFormItem(i18n.L("Password"), pw)}, func(ok bool) {
		if ok {
			f(pw.Text)
		}
	}, u.win)
}
