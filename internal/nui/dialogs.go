package nui

import (
	"image"
	"image/color"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"blite/internal/app"
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

// confirm asks before f: the buttons name the action and the way out, so
// they read right without the title. imp styles the action's button.
func (u *UI) confirm(title, text, act, dismiss string, imp widget.Importance, f func() error) {
	d := dialog.NewCustomConfirm(title, act, dismiss, label(text), func(ok bool) {
		if ok {
			u.do(f)
		}
	}, u.win)
	d.SetConfirmImportance(imp)
	d.Resize(fyne.NewSize(420, 0))
	d.Show()
}

// busyDo disables b while f runs.
func (u *UI) busyDo(b *widget.Button, f func() error) {
	b.Disable()
	u.bg.Add(1)
	go func() {
		err := f()
		u.laterDone(func() {
			b.Enable()
			if err != nil {
				u.showErr(err)
			}
		})
	}()
}

func (u *UI) leave() {
	u.confirm(i18n.L("Leave the session?"), i18n.L("You can rejoin later with the same link while the session is open."),
		i18n.L("Leave"), i18n.L("Stay"), widget.HighImportance, func() error {
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

// goLiveWith runs create, then shares the configured source and copies the link.
func (u *UI) goLiveWith(create func() error, pick bool) {
	src := normalizeShareState(u.st).Source
	u.bg.Add(1)
	go func() {
		err := create()
		if err == nil && !pick && src != "" {
			err = u.a.SetSource(src)
		}
		u.laterDone(func() {
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

// listTargets and targetIcons are what the source picker lists (blite-uidemo
// and tests show made-up ones).
var (
	listTargets = capture.List
	targetIcons = capture.Icons
)

// sourceDialog picks a monitor or window to share: the displays, then the
// windows, under a header each. Windows show their app's icon (a generic one
// when they have none), displays a monitor.
func (u *UI) sourceDialog() {
	var d dialog.Dialog
	rows := map[string]*pickRow{} // by Source, for the icons
	box := container.NewVBox()
	status := widget.NewLabel(i18n.L("Loading..."))
	pick := func(src string) {
		u.do(func() error { return u.a.SetSource(src) })
		d.Hide()
	}
	show := func(ts []capture.Target) {
		rows = map[string]*pickRow{}
		box.RemoveAll()
		for _, sec := range []struct {
			kind, title string
			icon        fyne.Resource
		}{
			{"monitor", i18n.L("Displays"), screenWhiteRes},
			{"window", i18n.L("Windows"), windowWhiteRes},
		} {
			first := true
			for _, t := range ts {
				if t.Kind != sec.kind {
					continue
				}
				if first {
					box.Add(pickHeader(sec.title))
					first = false
				}
				src := t.Source
				r := newPickRow(sec.icon, t.Label(), func() { pick(src) })
				rows[src] = r
				box.Add(r)
			}
		}
	}
	load := func() {
		go func() {
			ts, err := listTargets()
			u.later(func() {
				show(ts)
				status.SetText("")
				if err != nil {
					status.SetText(i18n.Msg(err.Error()))
				} else if len(ts) == 0 {
					status.SetText(i18n.L("No monitors or windows found."))
				}
			})
			// The icons after the list: a slow app doesn't hold it up.
			imgs := targetIcons(ts)
			u.later(func() {
				for src, img := range imgs {
					if r := rows[src]; r != nil {
						r.SetImage(img)
					}
				}
			})
		}()
	}
	body := container.NewBorder(nil, container.NewHBox(widget.NewButton(i18n.L("Refresh"), load), status), nil, nil, container.NewVScroll(box))
	d = dialog.NewCustom(i18n.L("Share a monitor or window"), i18n.L("Cancel"), body, u.win)
	d.Resize(fyne.NewSize(620, 460))
	load()
	d.Show()
}

// pickHeader heads a section of the source picker.
func pickHeader(text string) fyne.CanvasObject {
	t := canvas.NewText(text, colTextDim)
	t.TextSize = theme.CaptionTextSize()
	t.TextStyle = fyne.TextStyle{Bold: true}
	return container.New(&insetLayout{pad: 6}, t)
}

// pickRow is one display or window in the source picker: its icon and
// name, lit under the pointer, picked with a click.
type pickRow struct {
	widget.BaseWidget
	bg    *canvas.Rectangle
	img   *canvas.Image
	text  *widget.Label
	onTap func()
}

func newPickRow(icon fyne.Resource, text string, onTap func()) *pickRow {
	r := &pickRow{bg: canvas.NewRectangle(color.Transparent), img: canvas.NewImageFromResource(icon), text: widget.NewLabel(text), onTap: onTap}
	r.bg.CornerRadius = 6
	r.img.FillMode = canvas.ImageFillContain
	r.img.SetMinSize(fyne.NewSize(20, 20))
	r.text.Truncation = fyne.TextTruncateEllipsis
	r.ExtendBaseWidget(r)
	return r
}

// SetImage shows the app's icon instead of the generic one.
func (r *pickRow) SetImage(img image.Image) {
	r.img.Resource, r.img.Image = nil, img
	r.img.Refresh()
}

func (r *pickRow) Tapped(*fyne.PointEvent)        { r.onTap() }
func (r *pickRow) Cursor() desktop.Cursor         { return desktop.PointerCursor }
func (r *pickRow) MouseMoved(*desktop.MouseEvent) {}

func (r *pickRow) MouseIn(*desktop.MouseEvent) {
	r.bg.FillColor = colRowHover
	r.bg.Refresh()
}

func (r *pickRow) MouseOut() {
	r.bg.FillColor = color.Transparent
	r.bg.Refresh()
}

func (r *pickRow) CreateRenderer() fyne.WidgetRenderer {
	row := container.NewBorder(nil, nil, container.New(&insetLayout{pad: 6}, r.img), nil, r.text)
	return widget.NewSimpleRenderer(container.NewStack(r.bg, row))
}

// audioDialog picks the apps whose audio we share.
func (u *UI) audioDialog() {
	curApps := normalizeShareState(u.st).Audio.Apps
	group := widget.NewCheckGroup(nil, nil)
	keys := map[string]string{} // label -> key
	status := widget.NewLabel(i18n.L("Loading..."))
	load := func() {
		go func() {
			apps, err := audio.ListApps()
			u.later(func() {
				var opts, sel []string
				keys = map[string]string{}
				for _, a := range apps {
					l := a.Name
					if a.PID > 0 {
						l += " (" + strconv.Itoa(a.PID) + ")"
					}
					keys[l] = a.Key
					opts = append(opts, l)
					for _, k := range curApps {
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

func hinted(f *widget.FormItem, hint string) *widget.FormItem {
	f.HintText = hint
	return f
}

func (u *UI) relayDialog() {
	u.info(i18n.L("How relays work"), i18n.L("Everyone sends their video to everyone (full mesh). Members with a fast upload can volunteer as relays: the others then send their video only to a relay, which forwards it. Big rooms use several relays."), nil)
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
			u.later(func() {
				if err != nil {
					u.showErr(err)
				} else {
					u.info(i18n.L("Feedback sent"), i18n.L("Thanks for telling us. Your feedback was sent."), nil)
				}
			})
		}()
	}, u.win)
	d.Resize(fyne.NewSize(560, 0))
	d.Show()
}

// sendLogs asks whether to anonymize (remembered), then sends.
func (u *UI) sendLogs() {
	anon := widget.NewCheck(i18n.L("Anonymize: leave out names, room codes, servers and this computer's name"), nil)
	anon.SetChecked(u.st.AnonymizeLogs)
	note := widget.NewLabel(i18n.L("Anonymized logs may keep us from finding the problem."))
	note.Wrapping = fyne.TextWrapWord
	note.Importance = widget.LowImportance
	d := dialog.NewCustomConfirm(i18n.L("Send logs to the developers"), i18n.L("Send"), i18n.L("Cancel"),
		container.NewVBox(anon, note), func(ok bool) {
			if ok {
				u.sendLogsNow(anon.Checked)
			}
		}, u.win)
	d.Resize(fyne.NewSize(520, 0))
	d.Show()
}

func (u *UI) sendLogsNow(anonymize bool) {
	go func() {
		err := u.a.SendLogsNow(ctx(), anonymize)
		st, _ := u.a.State().(app.UIState) // u.st may not have the report yet
		u.later(func() {
			if err != nil {
				u.info(i18n.L("Couldn't send your logs"), upperFirst(i18n.Err(err).Error())+"\n\n"+i18n.L("Check your connection and try again."), nil)
				return
			}
			// The report says "sent 15:04" (the time it went).
			text := i18n.L("Thanks. Your logs were sent and will help us find the problem.")
			if t, ok := strings.CutPrefix(logsOf(st).Report, "sent "); ok && t != "" {
				text = i18n.L("Thanks. Your logs were sent at {{.Time}} and will help us find the problem.", i18n.D{"Time": t})
			}
			u.info(i18n.L("Logs sent"), text, nil)
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
