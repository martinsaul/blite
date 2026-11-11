package nui

import (
	"context"
	"os"
	"testing"

	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/theme"

	"blite/internal/app"
	"blite/internal/i18n"
	"blite/internal/session"
	"blite/internal/ui"
	"blite/internal/update"
)

// fakeBackend shows a fixed state; the window only reads it here.
type fakeBackend struct {
	ui.Backend
	st app.UIState
}

func (f *fakeBackend) State() any { return f.st }

func (f *fakeBackend) DiscordLogin()                                       {}
func (f *fakeBackend) GoLiveInChannel(context.Context, string, bool) error { return nil }
func (f *fakeBackend) JoinChannel(context.Context, string) error           { return nil }

func demoState(inCall bool) app.UIState {
	auto := true
	st := app.UIState{Version: "0.1.0 (84) linux", Name: "Alice", Tag: "4417", Autostart: &auto,
		Update:  &update.Info{Channel: "beta", CanApply: true, Available: "0.1.0 (85)"},
		Reach:   &app.Reach{OK: false, Text: "Not reachable: your router doesn't allow port mapping (UPnP, NAT-PMP or PCP). Calls with people behind strict routers may fail."},
		Discord: &app.Discord{Status: "ready", ChannelID: "1", Channel: "gaming", Guild: "Game Night", Room: "K7PQ-2M4X", People: 3},
		Servers: &app.Servers{List: []app.Server{{URL: "wss://a/ws", Host: "a", BuiltIn: true}}, Default: "wss://a/ws", Selected: "wss://a/ws"}}
	if inCall {
		st.Session = &session.State{Room: "K7PQ-2M4X", Source: "screen", Encrypted: true, Safety: "4821 0937 5512", IsOwner: true,
			MeshComfortPeers: 2, ResolutionNote: "Your computer can't encode 4K at 60 fps (41 fps); sharing at 30 fps.",
			Members: []session.Member{
				{ID: "a", Name: "Bob", Tag: "2926", Client: "0.1.0 (83) windows", State: "connected", Layer: "high", Limit: "bandwidth", Auto: "off",
					RTTms: 152, Loss: 0.02, TxKbps: 3050, RxKbps: 12, Quality: "auto", Volume: 1, Recv: 1080, Owner: true, KeyChanged: true, WindowClosed: true},
				{ID: "c", Name: "Dave", Tag: "7301", Client: "0.1.0 (82) windows", State: "unreachable", Quality: "off", Volume: 1,
					Hint: "No connection between you yet; trying again. If it keeps failing, a firewall may block it: send logs from both sides."},
				{ID: "d", Name: "Eve", Tag: "1111", State: "untrusted", Quality: "low"},
			}}
	}
	return st
}

// TestLangWidths: no language makes the window wider than it opens
// (1100 points): labels wrap, rows don't push it wider.
func TestLangWidths(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	defer i18n.SetPreference("")
	a := test.NewApp()
	defer a.Quit()
	a.Settings().SetTheme(theme.DefaultTheme()) // the app's sizes and fonts
	for _, l := range i18n.Languages {
		if only := os.Getenv("I18N_LANGS"); only != "" && only != l.Tag {
			continue
		}
		i18n.SetPreference(l.Tag)
		for _, inCall := range []bool{false, true} {
			u := &UI{a: &fakeBackend{st: demoState(inCall)}, fa: a, win: test.NewWindow(nil)}
			u.view = container.NewStack()
			u.win.SetContent(u.view)
			u.refresh()
			w := u.view.MinSize().Width
			t.Logf("%-8s call=%-5v min width %4.0f", l.Tag, inCall, w)
			if w > 1100 {
				t.Errorf("%s (call %v): the window needs %.0f points, more than 1100", l.Tag, inCall, w)
			}
			u.win.Close()
		}
	}
}
