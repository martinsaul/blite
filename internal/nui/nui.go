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
	"time"
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
	"blite/internal/ui"
)

// Options are the hooks main needs.
type Options struct {
	// OnReady runs once the window and tray exist.
	OnReady func()
	// Hidden starts with the window hidden (only the tray icon).
	Hidden bool
	// Scene opens a secondary UI surface after the initial draw (for blite-uidemo):
	// "sharesheet", "advanced", "panel", "gear", "help".
	Scene string
}

// UI is the running native interface.
type UI struct {
	a     ui.Backend
	fa    fyne.App
	win   fyne.Window
	shown bool // the window showed once (Fyne thread)

	// closed: a quit was asked for (or Run returned). From then on nothing
	// may queue work for the driver: once it has quit, fyne.Do runs the
	// function inline on the calling goroutine, against a terminated GLFW
	//.
	closed atomic.Bool
	// bg counts goroutines that report back to the UI (Go live, busy
	// buttons), until their UI work ran, so tests can wait for them.
	bg      sync.WaitGroup
	mu      sync.Mutex
	st      app.UIState
	ownLink string // the share link we copied for our own session
	devices []audio.Device

	icon             fyne.Resource
	view             *fyne.Container
	viewKind         string // lobby | call
	lobby            *lobbyView
	call             *callView
	sheet            *shareSheet
	leaveDialogShown bool
}

var (
	cur   *UI
	curMu sync.Mutex
)

// Run shows the window and the tray icon and blocks the calling goroutine
// (the main one) until Quit.
func Run(a ui.Backend, o Options) {
	fa := fapp.NewWithID("com.bellion.share")
	icon := fyne.NewStaticResource("icon.png", iconPNG)
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
	winH := float32(760)
	if o.Scene == "advanced" {
		winH = 960
	}
	u.win.Resize(fyne.NewSize(1100, winH))
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
			u.later(u.refresh)
		}
	}()
	fa.Lifecycle().SetOnEnteredForeground(func() {
		if u.lobby != nil {
			u.lobby.checkClipboard(u) // the user may have copied a link meanwhile
		}
	})
	fa.Lifecycle().SetOnStarted(func() {
		fyne.Do(u.refresh) // the first draw, on Fyne's thread
		if o.Scene != "" {
			go func() {
				time.Sleep(250 * time.Millisecond)
				u.later(func() { u.applyScene(o.Scene) })
			}()
		}
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
		u.later(u.Show)
	}
}

// Quit closes the window and the tray icon and makes Run return.
func Quit() {
	if u := get(); u != nil && !u.closed.Swap(true) {
		fyne.Do(u.fa.Quit)
	}
}

// quit is the tray's Quit, on the UI goroutine. It must be the app's Quit,
// not the driver's (what Fyne gives an IsQuit item without an Action): that
// one terminates GLFW but leaves the windows registered and the settings
// watcher running, whose theme re-apply then crashed.
func (u *UI) quit() {
	if !u.closed.Swap(true) {
		u.fa.Quit()
	}
}

// later runs f on the UI goroutine unless the UI is closing. Use it, not
// fyne.Do, from other goroutines.
func (u *UI) later(f func()) {
	if u.closed.Load() {
		return
	}
	fyne.Do(func() {
		if !u.closed.Load() {
			f()
		}
	})
}

// laterDone is later for a goroutine counted in bg: it counts as done once
// f ran (or won't run).
func (u *UI) laterDone(f func()) {
	if u.closed.Load() {
		u.bg.Done()
		return
	}
	fyne.Do(func() {
		defer u.bg.Done()
		if !u.closed.Load() {
			f()
		}
	})
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
	quit := u.quitItem()
	d.SetSystemTrayMenu(fyne.NewMenu("Bellion Share",
		fyne.NewMenuItem(i18n.L("Open Bellion Share"), u.Show),
		fyne.NewMenuItem(i18n.L("Leave session"), u.a.Leave),
		fyne.NewMenuItemSeparator(),
		quit,
	))
}

// Show implements the tray's "Open".
func (u *UI) Show() {
	u.win.Show()
	u.win.RequestFocus()
	if !u.shown {
		u.shown = true
		u.askAutostart() // the first time the window shows
	} else if u.viewKind == "lobby" && u.lobby != nil {
		u.lobby.checkClipboard(u)
		go u.a.ProbeRecentRooms()
	}
}

func (u *UI) loadDevices() {
	devs, _ := audio.ListDevices()
	u.later(func() {
		u.devices = devs
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
	if u.sheet != nil {
		u.sheet.update(st)
	}
	if st.Session == nil {
		u.leaveDialogShown = false
		enteredLobby := false
		if u.viewKind != "lobby" {
			u.lobby = newLobby(u)
			u.call = nil
			u.view.Objects = []fyne.CanvasObject{u.lobby.root}
			u.viewKind = "lobby"
			u.view.Refresh()
			enteredLobby = true
		}
		u.lobby.update(st)
		if enteredLobby {
			go u.a.ProbeRecentRooms()
		}
		return
	}
	if !st.Session.LeavePrompt {
		u.leaveDialogShown = false
	} else if !u.leaveDialogShown {
		u.leaveDialogShown = true
		u.showLeaveStreamDialog()
	}
	if u.viewKind != "call" {
		if u.lobby != nil && u.lobby.recent != nil {
			for _, r := range u.lobby.recent.rows {
				r.stopFade()
			}
		}
		u.call = newCall(u)
		u.lobby = nil
		u.view.Objects = []fyne.CanvasObject{u.call.root}
		u.viewKind = "call"
		u.view.Refresh()
	}
	u.call.update(st)
}

// applyScene opens a secondary UI surface on the UI goroutine.
func (u *UI) applyScene(scene string) {
	switch scene {
	case "sharesheet":
		u.openShareSheet(false)
	case "advanced":
		u.openShareSheet(true)
	case "panel":
		if u.call != nil && u.st.Session != nil && len(u.st.Session.Members) > 0 {
			u.call.selectPeer(u.st.Session.Members[0].ID)
		}
	case "gear":
		if btn := u.activeGearButton(); btn != nil {
			showMenuBelow(btn, u.gearMenuItems()...)
		}
	case "help":
		if btn := u.activeHelpButton(); btn != nil {
			showMenuBelow(btn, u.helpMenuItems()...)
		}
	}
}

func (u *UI) activeGearButton() *iconDotButton {
	if u.viewKind == "call" && u.call != nil {
		return u.call.gear
	}
	if u.lobby != nil {
		return u.lobby.gear
	}
	return nil
}

func (u *UI) activeHelpButton() *iconDotButton {
	if u.viewKind == "call" && u.call != nil {
		return u.call.help
	}
	if u.lobby != nil {
		return u.lobby.help
	}
	return nil
}

// do runs f off the UI goroutine and shows its error, if any.
func (u *UI) do(f func() error) {
	go func() {
		if err := f(); err != nil {
			u.later(func() { u.showErr(err) })
		}
	}()
}

func (u *UI) showErr(err error) {
	d := dialogError(i18n.Err(err), u.win)
	d.Show()
}

// quitItem is our own Quit, so it's in our language (Fyne adds its own
// where a menu has none). Its Action must be set: Fyne fills a nil one with
// the driver's Quit (see quit).
func (u *UI) quitItem() *fyne.MenuItem {
	q := fyne.NewMenuItem(i18n.L("Quit"), u.quit)
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
	case "upload":
		return i18n.L("upload limit")
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
	EnterChannel(ctx context.Context, password string, e2ee bool) error
	JoinChannel(ctx context.Context, password string) error
}
