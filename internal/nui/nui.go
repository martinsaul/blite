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
	"unicode"
	"unicode/utf8"

	"fyne.io/fyne/v2"
	fapp "fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"

	"blite/internal/app"
	"blite/internal/audio"
	"blite/internal/i18n"
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

	icon     fyne.Resource
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
	u := &UI{a: a, fa: fa, icon: icon}
	setFyneLanguage()
	fa.Settings().SetTheme(themeFor())
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
		u.setTrayMenu()
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

// setTrayMenu sets the tray icon's menu (again after a language change).
func (u *UI) setTrayMenu() {
	d, ok := u.fa.(desktop.App)
	if !ok {
		return
	}
	quit := quitItem()
	d.SetSystemTrayMenu(fyne.NewMenu("Bellion Share",
		fyne.NewMenuItem(i18n.L("Open Bellion Share"), u.Show),
		fyne.NewMenuItem(i18n.L("Show video"), u.showVideo),
		fyne.NewMenuItem(i18n.L("Leave session"), u.a.Leave),
		fyne.NewMenuItemSeparator(),
		quit,
	))
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

// showVideo reopens every video window the user closed (nothing outside
// a call).
func (u *UI) showVideo() {
	go u.a.ShowWindow("*")
}

func (u *UI) showErr(err error) {
	d := dialogError(i18n.Err(err), u.win)
	d.Show()
}

// quitItem is our own Quit, so it's in our language (Fyne adds its own
// where a menu has none; it quits the app).
func quitItem() *fyne.MenuItem {
	q := fyne.NewMenuItem(i18n.L("Quit"), nil)
	q.IsQuit = true
	return q
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

func limitText(limit string) string {
	switch limit {
	case "viewer":
		return i18n.L("their choice")
	case "bandwidth":
		return i18n.L("their link")
	case "cpu":
		return i18n.L("your CPU")
	}
	return limit
}

func layerText(layer string) string {
	switch layer {
	case "high":
		return i18n.L("full quality")
	case "low":
		return i18n.L("low quality")
	case "off":
		return i18n.L("nothing")
	}
	return layer
}

func sendText(m session.Member) string {
	t := layerText(m.Layer)
	if t != "" && m.Limit != "" {
		t = i18n.L("{{.What}} ({{.Why}})", i18n.D{"What": t, "Why": limitText(m.Limit)})
	}
	if m.Auto != "" {
		a := i18n.L("low: small tile")
		if m.Auto == "off" {
			a = i18n.L("paused: windows minimized")
		}
		if t != "" {
			a = i18n.L("{{.First}}, {{.Second}}", i18n.D{"First": t, "Second": a})
		}
		t = a
	}
	if t == "" {
		return "-"
	}
	return upperFirst(t)
}

// upperFirst capitalizes the first letter (scripts without case keep it).
func upperFirst(t string) string {
	r, n := utf8.DecodeRuneInString(t)
	return string(unicode.ToUpper(r)) + t[n:]
}

func stateText(m session.Member) string {
	switch m.State {
	case "unreachable":
		return i18n.L("can't connect, retrying")
	case "untrusted":
		return i18n.L("refused: keys don't match")
	case "connected":
		return i18n.L("connected")
	case "connecting":
		return i18n.L("connecting")
	case "reconnecting":
		return i18n.L("reconnecting")
	}
	return m.State
}

func linkText(m session.Member) string {
	var parts []string
	if m.RTTms > 0 {
		parts = append(parts, fmt.Sprintf("%d ms", m.RTTms))
	}
	if m.Loss > 0 {
		parts = append(parts, i18n.L("{{.Percent}}% loss", i18n.D{"Percent": fmt.Sprintf("%.1f", m.Loss*100)}))
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
