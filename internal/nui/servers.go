package nui

import (
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"blite/internal/app"
)

// serversBackend is what the app adds for the user's signaling servers.
type serversBackend interface {
	AddServer(s string) error
	RemoveServer(url string) error
	SetDefaultServer(url string) error
	SelectServer(url string) error
}

// serverName is how a server shows in the window.
func serverName(s app.Server) string {
	if s.BuiltIn {
		return s.Host + " (Bellion Share)"
	}
	return s.Host
}

// serverPicker is the lobby's drop-down, bottom right, for the server new
// sessions start on. It shows only once the user added a server.
type serverPicker struct {
	box  *fyne.Container
	sel  *widget.Select
	urls map[string]string // shown name -> URL
	key  string            // the list the drop-down shows
	set  bool              // updating: not the user's choice
}

func newServerPicker(u *UI) *serverPicker {
	p := &serverPicker{urls: map[string]string{}}
	p.sel = widget.NewSelect(nil, func(name string) {
		sb, ok := u.a.(serversBackend)
		if p.set || !ok || p.urls[name] == "" {
			return
		}
		url := p.urls[name]
		u.do(func() error { return sb.SelectServer(url) })
	})
	l := widget.NewLabel("Start sessions on")
	l.SizeName = theme.SizeNameCaptionText
	sel := container.NewGridWrap(fyne.NewSize(300, p.sel.MinSize().Height), p.sel) // names don't fit its minimum
	p.box = container.NewHBox(layout.NewSpacer(), l, sel)
	p.box.Hide()
	return p
}

func (p *serverPicker) update(st app.UIState) {
	s := st.Servers
	if !s.Custom() {
		p.box.Hide()
		return
	}
	var names []string
	p.urls = map[string]string{}
	sel := ""
	for _, srv := range s.List {
		n := serverName(srv)
		names = append(names, n)
		p.urls[n] = srv.URL
		if srv.URL == s.Selected {
			sel = n
		}
	}
	p.set = true
	if k := strings.Join(names, "\n"); k != p.key {
		p.key = k
		p.sel.SetOptions(names)
	}
	if p.sel.Selected != sel {
		p.sel.SetSelected(sel)
	}
	p.set = false
	p.box.Show()
}

// discordServerNote says why the Discord channel actions are off while a
// server of the user's is picked; "" = they work.
func discordServerNote(s *app.Servers) string {
	if s.SelectedBuiltIn() {
		return ""
	}
	return "Discord works on " + s.List[0].Host + " only for now."
}

// serversDialog lists the servers: add and remove the user's own, and
// choose the default for new sessions.
func (u *UI) serversDialog() {
	sb, ok := u.a.(serversBackend)
	if !ok {
		u.info("Servers", "This build always uses its built-in server.", nil)
		return
	}
	rows := container.NewVBox()
	msg := label("")
	msg.Importance = widget.DangerImportance
	msg.Hide()
	var fill func()
	act := func(f func() error) {
		if err := f(); err != nil {
			msg.SetText(err.Error())
			msg.Show()
		} else {
			msg.Hide()
		}
		fill()
	}
	fill = func() {
		st, _ := u.a.State().(app.UIState)
		rows.RemoveAll()
		if st.Servers == nil {
			return
		}
		for _, srv := range st.Servers.List {
			srv := srv
			name := widget.NewLabel(serverName(srv))
			var right []fyne.CanvasObject
			if srv.URL == st.Servers.Default {
				d := widget.NewLabel("Default")
				d.TextStyle.Bold = true
				right = append(right, d)
			} else {
				b := widget.NewButton("Make default", func() { act(func() error { return sb.SetDefaultServer(srv.URL) }) })
				b.Importance = widget.LowImportance
				right = append(right, b)
			}
			if !srv.BuiltIn {
				rm := widget.NewButtonWithIcon("", theme.DeleteIcon(), func() { act(func() error { return sb.RemoveServer(srv.URL) }) })
				rm.Importance = widget.LowImportance
				right = append(right, rm)
			}
			rows.Add(container.NewBorder(nil, nil, nil, container.NewHBox(right...), name))
		}
	}
	fill()
	entry := widget.NewEntry()
	entry.SetPlaceHolder("share.example.com or wss://share.example.com/ws")
	add := widget.NewButtonWithIcon("Add", theme.ContentAddIcon(), func() {
		s := strings.TrimSpace(entry.Text)
		act(func() error {
			err := sb.AddServer(s)
			if err == nil {
				entry.SetText("")
			}
			return err
		})
	})
	entry.OnSubmitted = func(string) { add.OnTapped() }
	intro := label("Sessions you start run on the default server. Links you click always join on the server they came from, " +
		"so you don't need to add a server to watch someone. Add a server of your own to start sessions there: " +
		"a server picker then shows at the bottom right of the window.")
	content := container.NewVBox(intro, widget.NewSeparator(), rows, widget.NewSeparator(),
		container.NewBorder(nil, nil, nil, add, entry), msg)
	d := dialog.NewCustom("Servers", "Close", content, u.win)
	d.Resize(fyne.NewSize(600, 0))
	d.Show()
}

// ConfirmServer asks once before joining a session on host, a server not
// run by Bellion Share, and waits for the answer (App.ConfirmServer).
func ConfirmServer(host string) bool {
	u := get()
	if u == nil {
		return false
	}
	ch := make(chan bool, 1)
	fyne.Do(func() {
		u.Show()
		d := dialog.NewCustomConfirm("Join a session on "+host+"?", "Join", "Cancel",
			label("This server isn't run by Bellion Share."), func(ok bool) { ch <- ok }, u.win)
		d.Show()
	})
	select {
	case ok := <-ch:
		return ok
	case <-time.After(5 * time.Minute):
		return false
	}
}
