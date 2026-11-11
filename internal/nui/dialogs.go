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
)

func dialogError(err error, w fyne.Window) dialog.Dialog {
	return dialog.NewError(err, w)
}

func (u *UI) info(title, text string, done func()) {
	d := dialog.NewInformation(title, text, u.win)
	if done != nil { // Fyne calls the callback unchecked: nil crashes on OK
		d.SetOnClosed(done)
	}
	d.Show()
}

func (u *UI) confirm(title, text string, f func() error) {
	dialog.ShowConfirm(title, text, func(ok bool) {
		if ok {
			u.do(f)
		}
	}, u.win)
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
	u.confirm("Leave the session?", "You can rejoin with the link while the session is open.", func() error {
		u.a.Leave()
		return nil
	})
}

func (u *UI) copyLink() {
	if s := u.st.Session; s != nil {
		u.ownLink = s.Link // not offered as "a link you copied" after leaving
		u.fa.Clipboard().SetContent(s.Link)
		u.fa.SendNotification(fyne.NewNotification("Bellion Share", "Share link copied. Anyone with it can join while the session is open."))
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
	pw.SetPlaceHolder("Optional: leave empty for an open room")
	e2ee := widget.NewCheck("Encrypt the call end to end (everyone needs build 72 or newer)", nil)
	e2ee.SetChecked(true)
	dialog.ShowForm("Start a session", "Create", "Cancel", []*widget.FormItem{
		widget.NewFormItem("Password", pw), widget.NewFormItem("", e2ee),
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
	d := dialog.NewForm("Join a session", "Join", "Cancel", []*widget.FormItem{widget.NewFormItem("Share link", link)}, func(ok bool) {
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
	d := dialog.NewForm("Your name", "Save", "Cancel", []*widget.FormItem{
		hinted(widget.NewFormItem("Name", name), "Others see it with your tag (#"+u.st.Tag+"), which comes from your identity key."),
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
	text := "A hosted room's operator can list this key to make you its owner while you're in it."
	if key == "" {
		text += "\n\nIt shows here while you're in a session."
	}
	var items []fyne.CanvasObject
	items = append(items, label(text))
	if key != "" {
		e := widget.NewEntry()
		e.SetText(key)
		items = append(items, e, widget.NewButton("Copy", func() { u.fa.Clipboard().SetContent(key) }))
	}
	d := dialog.NewCustom("Your identity key", "Close", container.NewVBox(items...), u.win)
	d.Resize(fyne.NewSize(520, 0))
	d.Show()
}

func (u *UI) safetyDialog() {
	s := u.st.Session
	if s == nil {
		return
	}
	u.info("Safety code", "Safety code: "+s.Safety+"\n\nRead it aloud once: everyone should see the same numbers. If someone's differs, the connection is being intercepted.", nil)
}

// sourceDialog picks a monitor or window to share.
func (u *UI) sourceDialog() {
	var targets []capture.Target
	list := widget.NewList(func() int { return len(targets) },
		func() fyne.CanvasObject { return widget.NewLabel("") },
		func(i widget.ListItemID, o fyne.CanvasObject) {
			t := targets[i]
			kind := "Window"
			if t.Kind == "monitor" {
				kind = "Monitor"
			}
			o.(*widget.Label).SetText(kind + ": " + t.Name)
		})
	status := widget.NewLabel("Loading...")
	load := func() {
		go func() {
			ts, err := capture.List()
			fyne.Do(func() {
				targets = ts
				status.SetText("")
				if err != nil {
					status.SetText(err.Error())
				} else if len(ts) == 0 {
					status.SetText("No monitors or windows found.")
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
	body := container.NewBorder(nil, container.NewHBox(widget.NewButton("Refresh", load), status), nil, nil, list)
	d = dialog.NewCustom("Share a monitor or window", "Cancel", body, u.win)
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
	status := widget.NewLabel("Loading...")
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
					status.SetText(err.Error())
				} else if len(apps) == 0 {
					status.SetText("No apps are playing audio right now.")
				}
			})
		}()
	}
	body := container.NewBorder(label("Only these apps are heard in the call. Apps appear here while they play audio."),
		container.NewHBox(widget.NewButton("Refresh", load), status), nil, nil, container.NewVScroll(group))
	d := dialog.NewCustomConfirm("Audio you share", "Share these", "Cancel", body, func(ok bool) {
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
	auto := widget.NewCheck("Automatically", func(on bool) {
		if on {
			rows.Disable()
			cols.Disable()
		} else {
			rows.Enable()
			cols.Enable()
		}
	})
	auto.SetChecked(l.Rows == 0 && l.Cols == 0)
	dialog.ShowForm("Arrange the video window", "Apply", "Cancel", []*widget.FormItem{
		widget.NewFormItem("", auto), widget.NewFormItem("Rows", rows), widget.NewFormItem("Columns", cols),
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
	u.info("Relays", "Everyone sends their video to everyone (full mesh). Members with a fast upload can volunteer as relays: "+
		"the others then send their video only to a relay, which forwards it. Big rooms use several relays.", nil)
}

func (u *UI) feedbackDialog() {
	kind := widget.NewRadioGroup([]string{"An idea or something you'd like added", "Something that went wrong or bothers you"}, nil)
	kind.SetSelected(kind.Options[0])
	text := widget.NewMultiLineEntry()
	text.SetPlaceHolder("What would you like, or what happened?")
	text.SetMinRowsVisible(7)
	text.Wrapping = fyne.TextWrapWord
	diag := widget.NewCheck("Include diagnostics (your setup and call stats, no names, keys or links)", nil)
	diag.SetChecked(true)
	d := dialog.NewCustomConfirm("Tell us what you think", "Send", "Cancel", container.NewVBox(kind, text, diag), func(ok bool) {
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
					u.info("Thank you", "Your feedback was sent.", nil)
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
			st := u.logs().Report
			if st == "" {
				st = "Your logs were sent to the developers."
			}
			u.info("Logs", st, nil)
		})
	}()
}

func (u *UI) aboutDialog() {
	l := u.logs()
	text := "Bellion Share " + u.st.Version + "\nScreen sharing between friends, peer to peer."
	if u.st.Update != nil && u.st.Update.Channel != "" {
		text += "\nUpdate channel: " + u.st.Update.Channel
		if u.st.Update.Checked != "" {
			text += ", last checked " + u.st.Update.Checked
		}
	}
	if l.Dir != "" {
		text += "\nLogs: " + l.Dir
	}
	if l.Guard != "" {
		text += "\nCrash guard: " + l.Guard
	}
	u.info("About Bellion Share", text, nil)
}

// passwordDialog asks for a session's password.
func (u *UI) passwordDialog(f func(pw string)) {
	pw := widget.NewPasswordEntry()
	dialog.ShowForm("This session has a password", "Join", "Cancel", []*widget.FormItem{widget.NewFormItem("Password", pw)}, func(ok bool) {
		if ok {
			f(pw.Text)
		}
	}, u.win)
}
