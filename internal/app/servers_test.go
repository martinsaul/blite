package app

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"blite/internal/identity"
	"blite/internal/media"
	"blite/internal/server"
	"blite/internal/session"
)

// No test reads or writes the user's real server list.
func TestMain(m *testing.M) {
	d, err := os.MkdirTemp("", "blite-servers")
	if err != nil {
		panic(err)
	}
	serversPath = func() (string, error) { return filepath.Join(d, "blite", "servers.json"), nil }
	code := m.Run()
	os.RemoveAll(d)
	os.Exit(code)
}

func tempServers(t *testing.T) string {
	f := filepath.Join(t.TempDir(), "blite", "servers.json")
	old := serversPath
	serversPath = func() (string, error) { return f, nil }
	t.Cleanup(func() { serversPath = old })
	return f
}

const builtIn = "wss://bellion.example.com/ws"

func newTestApp() *App {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(session.Config{Server: builtIn, Name: "alice", View: media.ViewCount, Log: quiet}, media.SourceKind("none"))
}

// Added servers and the default survive a restart; the built-in server is
// always first and can't be removed.
func TestServerSettings(t *testing.T) {
	tempServers(t)
	a := newTestApp()
	if st := a.State().(UIState).Servers; st.Custom() || st.Default != builtIn || st.Selected != builtIn || !st.SelectedBuiltIn() {
		t.Fatalf("fresh: %+v", st)
	}
	if err := a.AddServer("share.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := a.AddServer("wss://share.example.com/ws"); err == nil {
		t.Fatal("added twice")
	}
	if err := a.AddServer("bellion.example.com"); err == nil {
		t.Fatal("added the built-in server")
	}
	if err := a.AddServer("not a server"); err == nil {
		t.Fatal("added garbage")
	}
	if err := a.AddServer("other.example.com:8443"); err != nil {
		t.Fatal(err)
	}
	const custom = "wss://share.example.com/ws"
	if err := a.SetDefaultServer(custom); err != nil {
		t.Fatal(err)
	}
	if err := a.RemoveServer(builtIn); err == nil {
		t.Fatal("removed the built-in server")
	}
	if err := a.SetDefaultServer("wss://unknown.example.com/ws"); err == nil {
		t.Fatal("unknown default")
	}

	b := newTestApp() // a restart
	st := b.State().(UIState).Servers
	if !st.Custom() || len(st.List) != 3 || !st.List[0].BuiltIn || st.List[0].URL != builtIn || st.List[1].URL != custom ||
		st.List[2].URL != "wss://other.example.com:8443/ws" || st.Default != custom || st.Selected != custom || st.SelectedBuiltIn() {
		t.Fatalf("after restart: %+v", st)
	}
	// The lobby's pick is for this run only.
	if err := b.SelectServer(builtIn); err != nil {
		t.Fatal(err)
	}
	if st := b.State().(UIState).Servers; st.Selected != builtIn || st.Default != custom {
		t.Fatalf("picked: %+v", st)
	}
	if st := newTestApp().State().(UIState).Servers; st.Selected != custom {
		t.Fatalf("pick kept across a restart: %+v", st)
	}
	// Removing the default makes the built-in one the default again.
	if err := b.RemoveServer(custom); err != nil {
		t.Fatal(err)
	}
	if st := newTestApp().State().(UIState).Servers; len(st.List) != 2 || st.Default != builtIn {
		t.Fatalf("after remove: %+v", st)
	}
}

// Only servers that are neither built in, nor the user's, nor agreed to
// before need the user's OK; a yes is remembered.
func TestUnknownServerNeedsConfirmation(t *testing.T) {
	tempServers(t)
	a := newTestApp()
	if err := a.AddServer("mine.example.com"); err != nil {
		t.Fatal(err)
	}
	for server, want := range map[string]bool{
		builtIn:                            false,
		"wss://www.bellion.example.com/ws": false, // a site alias
		"wss://mine.example.com/ws":        false,
		"wss://other.example.com/ws":       true,
		"wss://other.example.com:8443/ws":  true,
	} {
		if got := a.needsConfirm(server); got != want {
			t.Errorf("%s: needs confirmation %v, want %v", server, got, want)
		}
	}
	for link, want := range map[string]string{
		"https://bellion.example.com/j/7K3QX9MD#pw":            builtIn,
		"https://www.bellion.example.com/j/7K3QX9MD#pw":        builtIn,
		"bshare://join/7K3QX9MD#pw":                            builtIn,
		"bshare://join/7K3QX9MD?server=bellion.example.com#pw": builtIn,
		"bshare://join/7K3QX9MD?server=other.example.com#pw":   "wss://other.example.com/ws",
		"https://other.example.com/j/7K3QX9MD#pw":              "wss://other.example.com/ws",
	} {
		s, _, _, err := session.ParseLinkServer(link)
		if got := a.linkServer(s); err != nil || got != want {
			t.Errorf("%s: server %q %v, want %q", link, got, err, want)
		}
	}

	// No way to ask: refused, nothing remembered.
	ctx := context.Background()
	if err := a.JoinLink(ctx, "https://other.example.com/j/7K3QX9MD#pw"); err != ErrServerNotConfirmed {
		t.Fatalf("no confirmation hook: %v", err)
	}
	asked := 0
	a.ConfirmServer = func(host string) bool {
		asked++
		if host != "other.example.com" {
			t.Errorf("asked about %q", host)
		}
		return false
	}
	if err := a.JoinLink(ctx, "https://other.example.com/j/7K3QX9MD#pw"); err != ErrServerNotConfirmed || asked != 1 {
		t.Fatalf("cancelled: %v, asked %d", err, asked)
	}
	a.ConfirmServer = func(string) bool { asked++; return true }
	if err := a.confirmServer("wss://other.example.com/ws"); err != nil || asked != 2 {
		t.Fatalf("accepted: %v, asked %d", err, asked)
	}
	if err := a.confirmServer("wss://other.example.com/ws"); err != nil || asked != 2 {
		t.Fatalf("asked again: %v, asked %d", err, asked)
	}
	if newTestApp().needsConfirm("wss://other.example.com/ws") {
		t.Fatal("the yes wasn't kept")
	}
}

// A session created on a server of the user's: its link names that server,
// someone else (built-in server only) joins it there after confirming, and
// an in-call update rejoins on it.
func TestSessionOnCustomServer(t *testing.T) {
	tempServers(t)
	f := tempResume(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ws := func() string {
		ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
		t.Cleanup(ts.Close)
		return "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	}
	def, custom := ws(), ws()
	key := identity.Ephemeral()
	mk := func(name string, k identity.Key) *App {
		return New(session.Config{Server: def, Name: name, View: media.ViewCount, Log: quiet, Identity: k}, media.SourceKind("none"))
	}
	a := mk("alice", key)
	if err := a.AddServer(custom); err != nil {
		t.Fatal(err)
	}
	if err := a.SelectServer(custom); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a.discord = Discord{Status: "ready", ChannelID: "1", Room: "7K3QX9MD"}
	if err := a.GoLiveInChannel(ctx, "", false); err != ErrDiscordServer {
		t.Fatalf("Discord go live on a custom server: %v", err)
	}
	if err := a.JoinChannel(ctx, ""); err != ErrDiscordServer {
		t.Fatalf("Discord watch with a custom server picked: %v", err)
	}
	a.discord = Discord{}
	if err := a.Create(ctx, "", false); err != nil {
		t.Fatal(err)
	}
	link := a.State().(UIState).Session.Link
	if !strings.Contains(link, "://"+session.ServerHost(custom)+"/j/") {
		t.Fatalf("link %s isn't on %s", link, custom)
	}
	if r := a.sess.Rejoin(); r.Server != custom {
		t.Fatalf("rejoin server %q", r.Server)
	}

	os.Remove(f)
	serversPath = func() (string, error) { return filepath.Join(t.TempDir(), "servers.json"), nil } // bob has no custom servers
	b := mk("bob", identity.Ephemeral())
	defer b.Shutdown()
	b.ConfirmServer = func(string) bool { return true }
	// The https link of a local test server is http://127.0.0.1:PORT.
	if err := b.JoinLink(ctx, strings.Replace(link, "https://", "http://", 1)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bob sees alice", func() bool { return len(b.State().(UIState).Session.Members) == 1 })

	if !a.leaveForUpdate() {
		t.Fatal("not in a call")
	}
	a.Shutdown()
	a2 := mk("alice", key) // the default server is the built-in one again
	defer a2.Shutdown()
	if ok, err := a2.Resume(ctx); !ok || err != nil {
		t.Fatalf("resume: %v %v", ok, err)
	}
	waitFor(t, "alice back in bob's room", func() bool {
		st := a2.State().(UIState).Session
		return st != nil && len(st.Members) == 1 && st.Members[0].Name == "bob"
	})
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(15 * time.Second); !ok(); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("timed out: " + what)
		}
	}
}
