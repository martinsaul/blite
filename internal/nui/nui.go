// Package nui is Bellion Share's native window: a menu bar with the settings
// and actions, the room and the people in the call below it, and the tray icon.
// It replaces opening the local web UI in a browser.
package nui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"fyne.io/fyne/v2"
	fapp "fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"

	"blite/internal/app"
	"blite/internal/audio"
	"blite/internal/session"
	"blite/internal/tray"
	"blite/internal/ui"
)

// Options are the hooks main needs.
type Options struct {
	// OnReady runs once the window and tray exist.
	OnReady func()
	// Hidden starts with the window hidden (only the tray icon).
	Hidden bool
}

// UI is the running native interface.
type UI struct {
	a   ui.Backend
	fa  fyne.App
	win fyne.Window

	closed  atomic.Bool // Run returned: the driver is gone
	mu      sync.Mutex
	st      app.UIState
	menuKey string
	ownLink string // the share link we copied for our own session
	devices []audio.Device

	view     *fyne.Container
	viewKind string // lobby | call
	lobby    *lobbyView
	call     *callView
}

var (
	cur   *UI
	curMu sync.Mutex
)

// Run shows the window and the tray icon and blocks the calling goroutine
// (the main one) until Quit.
func Run(a ui.Backend, o Options) {
	fa := fapp.NewWithID("com.bellion.share")
	icon := fyne.NewStaticResource("icon.png", tray.PNG())
	fa.SetIcon(icon)
	u := &UI{a: a, fa: fa}
	curMu.Lock()
	cur = u
	curMu.Unlock()

	u.win = fa.NewWindow("Bellion Share")
	u.win.SetIcon(icon)
	u.win.SetMaster()
	u.win.SetCloseIntercept(func() { u.win.Hide() }) // keeps running in the tray
	u.win.Resize(fyne.NewSize(1100, 720))
	u.view = container.NewStack()
	u.win.SetContent(u.view)

	if d, ok := fa.(desktop.App); ok {
		d.SetSystemTrayIcon(icon)
		d.SetSystemTrayMenu(fyne.NewMenu("Bellion Share",
			fyne.NewMenuItem("Open Bellion Share", u.Show),
			fyne.NewMenuItem("Leave session", a.Leave),
		))
	}

	go u.loadDevices()
	ch, stop := a.Subscribe()
	go func() {
		for range ch {
			fyne.Do(u.refresh)
		}
	}()
	fa.Lifecycle().SetOnEnteredForeground(func() {
		if u.lobby != nil {
			u.lobby.checkClipboard(u) // the user may have copied a link meanwhile
		}
	})
	fa.Lifecycle().SetOnStarted(func() {
		fyne.Do(u.refresh) // the first draw, on Fyne's thread
		if o.OnReady != nil {
			go o.OnReady()
		}
	})
	if !o.Hidden {
		u.win.Show()
	}
	fa.Run()
	stop()
	u.closed.Store(true)
	// Closed: Show and Quit from other goroutines (a signal, a second
	// launch) must not touch the torn-down driver (85 crashed on quit).
	curMu.Lock()
	cur = nil
	curMu.Unlock()
}

// Show brings the window to the front (the tray, a second launch).
func Show() {
	if u := get(); u != nil {
		fyne.Do(u.Show)
	}
}

// Quit closes the window and the tray icon and makes Run return.
func Quit() {
	if u := get(); u != nil {
		fyne.Do(u.fa.Quit)
	}
}

func get() *UI {
	curMu.Lock()
	defer curMu.Unlock()
	return cur
}

// Show implements the tray's "Open".
func (u *UI) Show() {
	u.win.Show()
	u.win.RequestFocus()
}

func (u *UI) loadDevices() {
	devs, _ := audio.ListDevices()
	fyne.Do(func() {
		u.devices = devs
		u.menuKey = "" // rebuild the Share menu with them
		u.refresh()
	})
}

// refresh redraws from the app's state; it runs on the UI goroutine.
func (u *UI) refresh() {
	if u.closed.Load() {
		return // a late state change after the window shut down
	}
	st, _ := u.a.State().(app.UIState)
	u.st = st
	u.updateMenu()
	if st.Session == nil {
		if u.viewKind != "lobby" {
			u.lobby = newLobby(u)
			u.call = nil
			u.view.Objects = []fyne.CanvasObject{u.lobby.root}
			u.viewKind = "lobby"
			u.view.Refresh()
		}
		u.lobby.update(st)
		return
	}
	if u.viewKind != "call" {
		u.call = newCall(u)
		u.lobby = nil
		u.view.Objects = []fyne.CanvasObject{u.call.root}
		u.viewKind = "call"
		u.view.Refresh()
	}
	u.call.update(st)
}

// do runs f off the UI goroutine and shows its error, if any.
func (u *UI) do(f func() error) {
	go func() {
		if err := f(); err != nil {
			fyne.Do(func() { u.showErr(err) })
		}
	}()
}

func (u *UI) showErr(err error) {
	d := dialogError(err, u.win)
	d.Show()
}

func ctx() context.Context { return context.Background() }

// logInfo mirrors what main reports as UIState.Logs.
type logInfo struct {
	Dir, File, Error, Guard, Report string
}

func (u *UI) logs() logInfo {
	var l logInfo
	if u.st.Logs != nil {
		b, _ := json.Marshal(u.st.Logs)
		json.Unmarshal(b, &l)
	}
	return l
}

func displayName(name, tag string) string {
	if tag == "" {
		return name
	}
	return name + "#" + tag
}

func kbps(k int) string {
	if k >= 1000 {
		return fmt.Sprintf("%.1f Mbit/s", float64(k)/1000)
	}
	return fmt.Sprintf("%d kbit/s", k)
}

var limitText = map[string]string{"viewer": "their choice", "bandwidth": "their link", "cpu": "your CPU"}

var layerText = map[string]string{"high": "full quality", "low": "low quality", "off": "nothing"}

func sendText(m session.Member) string {
	t := m.Layer
	if l, ok := layerText[t]; ok {
		t = l
	}
	if t != "" && m.Limit != "" {
		l := limitText[m.Limit]
		if l == "" {
			l = m.Limit
		}
		t += " (" + l + ")"
	}
	if m.Auto != "" {
		if t != "" {
			t += ", "
		}
		if m.Auto == "off" {
			t += "paused: windows minimized"
		} else {
			t += "low: small tile"
		}
	}
	if t == "" {
		t = "-"
	}
	return strings.ToUpper(t[:1]) + t[1:]
}

func stateText(m session.Member) string {
	switch m.State {
	case "unreachable":
		return "can't connect, retrying"
	case "untrusted":
		return "refused: keys don't match"
	}
	return m.State
}

func linkText(m session.Member) string {
	var parts []string
	if m.RTTms > 0 {
		parts = append(parts, fmt.Sprintf("%d ms", m.RTTms))
	}
	if m.Loss > 0 {
		parts = append(parts, fmt.Sprintf("%.1f%% loss", m.Loss*100))
	}
	if m.TxKbps > 0 || m.RxKbps > 0 {
		parts = append(parts, "↑ "+kbps(m.TxKbps)+"  ↓ "+kbps(m.RxKbps))
	}
	return strings.Join(parts, " · ")
}

// label is a wrapping label for longer text.
func label(text string) *widget.Label {
	l := widget.NewLabel(text)
	l.Wrapping = fyne.TextWrapWord
	return l
}

// discordBackend is what the app adds for Discord voice channels.
type discordBackend interface {
	DiscordLogin()
	GoLiveInChannel(ctx context.Context, password string, e2ee bool) error
	JoinChannel(ctx context.Context, password string) error
}
