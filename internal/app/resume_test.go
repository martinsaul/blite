package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"blite/internal/audio"
	"blite/internal/i18n"
	"blite/internal/identity"
	"blite/internal/media"
	"blite/internal/proto"
	"blite/internal/server"
	"blite/internal/session"
)

func tempResume(t *testing.T) string {
	f := filepath.Join(t.TempDir(), "blite", "resume.json")
	old := resumePath
	resumePath = func() (string, error) { return f, nil }
	t.Cleanup(func() { resumePath = old })
	return f
}

// The resume file round-trips, is private, and goes stale after
// ResumeWindow (then it is removed).
func TestResumeFile(t *testing.T) {
	f := tempResume(t)
	now := time.Now()
	want := session.Rejoin{Room: "ABCD2345", Password: "pw-1234", Encrypted: true, Source: media.SourceKind("window:42"),
		Resolution: "1080p", Audio: audio.Selection{Mode: "apps", Apps: []string{"firefox"}}, Output: "dev1"}
	if err := writeResume(want, now); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(f); err != nil || runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("resume file mode: %v %v", st.Mode(), err)
	}
	got, ok := readResume(now.Add(ResumeWindow - time.Second))
	if !ok || got.Room != want.Room || got.Password != want.Password || !got.Encrypted || got.Source != want.Source ||
		got.Resolution != want.Resolution || got.Audio.Mode != "apps" || len(got.Audio.Apps) != 1 || got.Output != "dev1" {
		t.Fatalf("read back %+v ok=%v", got, ok)
	}
	if _, ok := readResume(now.Add(ResumeWindow + time.Second)); ok {
		t.Fatal("stale resume accepted")
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatalf("stale resume file kept: %v", err)
	}
	if _, ok := readResume(now); ok {
		t.Fatal("no file, but a resume")
	}
}

// In-call update end to end at the app level: alice shares, leaves for an
// update, and a new App with the same identity resumes from the file: it
// is back in the room, owns it, and shares the same source again, but not
// the test tone (it could have been left on).
func TestUpdateResumesCall(t *testing.T) {
	f := tempResume(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	key := identity.Ephemeral()
	mk := func(name string, k identity.Key) *App {
		return New(session.Config{Server: ws, Name: name, View: media.ViewCount, Log: quiet, Identity: k}, media.SourceKind("none"))
	}
	a, b := mk("alice", key), mk("bob", identity.Ephemeral())
	defer b.Shutdown()
	ctx := context.Background()
	if err := a.Create(ctx, "", true); err != nil {
		t.Fatal(err)
	}
	if err := a.SetSource("test"); err != nil {
		t.Fatal(err)
	}
	if err := b.JoinLink(ctx, a.State().(UIState).Session.Link); err != nil {
		t.Fatal(err)
	}
	alice := func() (frames int, owner bool) {
		for _, m := range b.State().(UIState).Session.Members {
			if m.Name == "alice" && m.State == "connected" {
				return m.Frames, m.Owner
			}
		}
		return 0, false
	}
	waitUntil := func(what string, ok func() bool) {
		t.Helper()
		for end := time.Now().Add(15 * time.Second); !ok(); time.Sleep(100 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatal("timed out: " + what)
			}
		}
	}
	waitUntil("bob sees alice's video", func() bool { n, o := alice(); return n > 10 && o })
	if err := a.SetAudio("test", nil); err != nil {
		t.Fatal(err)
	}
	waitUntil("bob decodes alice's test tone", func() bool {
		for _, m := range b.State().(UIState).Session.Members {
			if m.Name == "alice" && m.Audio > 10 {
				return true
			}
		}
		return false
	})

	if !a.leaveForUpdate() {
		t.Fatal("not in a call")
	}
	a.Shutdown()
	if _, err := os.Stat(f); err != nil {
		t.Fatalf("no resume file: %v", err)
	}

	a2 := mk("alice", key)
	defer a2.Shutdown()
	ok, err := a2.Resume(ctx)
	if !ok || err != nil {
		t.Fatalf("resume: %v %v", ok, err)
	}
	if st := a2.State().(UIState).Session; st == nil || st.Source != "test" || !st.IsOwner || st.Audio.Mode == "test" {
		t.Fatalf("resumed session %+v", st)
	}
	waitUntil("bob sees alice back, owner, video", func() bool { n, o := alice(); return n > 10 && o })
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatalf("resume file kept after use: %v", err)
	}
	if ok, _ := a2.Resume(ctx); ok {
		t.Fatal("resumed twice")
	}
}

// When the server no longer has our slot after a signaling drop
// (resume_failed), the app rejoins the same room by itself, once, and
// shares the same source again.
func TestLostSlotRejoins(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(server.Config{Log: quiet})
	ts := httptest.NewUnstartedServer(srv)
	var (
		mu    sync.Mutex
		socks []net.Conn // WebSockets: hijacked, so CloseClientConnections misses them
	)
	ts.Config.ConnState = func(c net.Conn, st http.ConnState) {
		if st == http.StateHijacked {
			mu.Lock()
			socks = append(socks, c)
			mu.Unlock()
		}
	}
	ts.Start()
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	mk := func(name string) *App {
		return New(session.Config{Server: ws, Name: name, View: media.ViewCount, Log: quiet}, media.SourceKind("none"))
	}
	a, b := mk("alice"), mk("bob")
	defer a.Shutdown()
	defer b.Shutdown()
	ctx := context.Background()
	if err := a.Create(ctx, "", true); err != nil {
		t.Fatal(err)
	}
	room := a.State().(UIState).Session.Room
	if err := b.JoinLink(ctx, a.State().(UIState).Session.Link); err != nil {
		t.Fatal(err)
	}
	if err := b.SetSource("test"); err != nil {
		t.Fatal(err)
	}
	bob := func() (id string, frames int) {
		for _, m := range a.State().(UIState).Session.Members {
			if m.Name == "bob" && m.State == "connected" {
				return m.ID, m.Frames
			}
		}
		return "", 0
	}
	waitUntil := func(what string, ok func() bool) {
		t.Helper()
		for end := time.Now().Add(20 * time.Second); !ok(); time.Sleep(100 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatal("timed out: " + what)
			}
		}
	}
	waitUntil("alice sees bob's video", func() bool { _, n := bob(); return n > 10 })
	old, _ := bob()
	b.mu.Lock()
	first := b.sess
	b.mu.Unlock()

	// The server forgets bob's slot; every socket drops. Alice resumes,
	// bob's resume is refused and he rejoins.
	srv.Rooms().Leave(strings.ReplaceAll(room, "-", ""), old)
	mu.Lock()
	for _, c := range socks {
		c.Close()
	}
	mu.Unlock()
	waitUntil("bob rejoins with a new ID and video", func() bool { id, n := bob(); return id != "" && id != old && n > 10 })
	b.mu.Lock()
	again, lastErr := b.sess, b.lastErr
	b.mu.Unlock()
	if again == nil || again == first || lastErr != "" {
		t.Fatalf("bob's session after the rejoin: %p (was %p), error %q", again, first, lastErr)
	}
	if st := b.State().(UIState).Session; st == nil || st.Source != "test" {
		t.Fatalf("bob's rejoined session %+v", st)
	}
	if st := a.State().(UIState).Session; st == nil {
		t.Fatal("alice's session ended")
	}
}

// JoinLink, rejoinLost and Resume all refuse an E2EE downgrade when the link
// or saved session state says the room was encrypted.
func TestE2EEDowngradeEnforcedAcrossJoinRejoinAndResume(t *testing.T) {
	tempServers(t)
	tempRecent(t)
	tempResume(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv := server.New(server.Config{Log: quiet})
	upTS := httptest.NewServer(srv)
	defer upTS.Close()
	upWS := "ws" + strings.TrimPrefix(upTS.URL, "http") + "/ws"

	var downgrade atomic.Bool
	var mu sync.Mutex
	var downSocks []net.Conn
	proxyTS := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		down, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer down.CloseNow()
		up, _, err := websocket.Dial(r.Context(), upWS, nil)
		if err != nil {
			return
		}
		defer up.CloseNow()
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		go func() {
			defer cancel()
			for {
				typ, b, err := down.Read(ctx)
				if err != nil {
					return
				}
				if up.Write(ctx, typ, b) != nil {
					return
				}
			}
		}()
		for {
			typ, b, err := up.Read(ctx)
			if err != nil {
				return
			}
			if downgrade.Load() {
				var env proto.Envelope
				if json.Unmarshal(b, &env) == nil && env.T == proto.TWelcome {
					var wl proto.Welcome
					if json.Unmarshal(b, &wl) == nil {
						wl.E2EE = false
						b, _ = json.Marshal(wl)
					}
				}
			}
			if down.Write(ctx, typ, b) != nil {
				return
			}
		}
	}))
	proxyTS.Config.ConnState = func(c net.Conn, st http.ConnState) {
		if st == http.StateHijacked {
			mu.Lock()
			downSocks = append(downSocks, c)
			mu.Unlock()
		}
	}
	proxyTS.Start()
	defer proxyTS.Close()
	proxyWS := "ws" + strings.TrimPrefix(proxyTS.URL, "http") + "/ws"

	cm := &proto.ClientMeta{AppVersion: "0.1.0", Build: proto.E2EEBuild}
	mk := func(serverURL, name string, k identity.Key) *App {
		a := New(session.Config{Server: serverURL, Name: name, View: media.ViewCount, Client: cm, Identity: k, Log: quiet}, media.SourceTest)
		a.ConfirmServer = func(string) bool { return true }
		return a
	}

	ctx := context.Background()
	aliceKey := identity.Ephemeral()
	a := mk(proxyWS, "alice", aliceKey)
	defer a.Shutdown()
	if err := a.Create(ctx, "pw-e2ee", true); err != nil {
		t.Fatal(err)
	}
	stA := a.State().(UIState).Session
	if stA == nil || !stA.Encrypted {
		t.Fatalf("expected encrypted session, got %+v", stA)
	}
	encLink := strings.Replace(stA.Link, "https://", "http://", 1)
	code := strings.ReplaceAll(stA.Room, "-", "")

	// 1. JoinLink with marker fails when proxy downgrades Welcome.E2EE to false.
	downgrade.Store(true)
	b := mk(proxyWS, "bob", identity.Ephemeral())
	defer b.Shutdown()
	if err := b.JoinLink(ctx, encLink); !errors.Is(err, session.ErrE2EEDowngrade) {
		t.Fatalf("JoinLink with marker on downgraded room: %v, want ErrE2EEDowngrade", err)
	}
	if b.State().(UIState).Session != nil {
		t.Fatal("bob has an active session after E2EE downgrade")
	}
	if tr := i18n.Err(session.ErrE2EEDowngrade); tr == nil {
		t.Fatal("nil translated error")
	}

	// 2. When Welcome.E2EE is true, bob joins; then if bob's slot is lost and
	// the server downgrades on rejoinLost, bob refuses to rejoin unencrypted.
	downgrade.Store(false)
	if err := b.JoinLink(ctx, encLink); err != nil {
		t.Fatalf("JoinLink with marker on honest room: %v", err)
	}
	var bobID string
	for end := time.Now().Add(10 * time.Second); bobID == ""; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("timed out waiting for alice to see bob")
		}
		for _, m := range a.State().(UIState).Session.Members {
			if m.Name == "bob" {
				bobID = m.ID
			}
		}
	}

	downgrade.Store(true)
	srv.Rooms().Leave(code, bobID)
	mu.Lock()
	conns := append([]net.Conn(nil), downSocks...)
	mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
	for end := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		ui := b.State().(UIState)
		if ui.Session == nil && strings.Contains(ui.Error, session.ErrE2EEDowngrade.Error()) {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("rejoinLost did not fail with ErrE2EEDowngrade: session=%+v err=%q", ui.Session, ui.Error)
		}
	}

	// 3. Resume from resume.json enforces E2EE too.
	if !a.leaveForUpdate() {
		t.Fatal("alice not in call for update")
	}
	a2 := mk(proxyWS, "alice", aliceKey)
	defer a2.Shutdown()
	ok, err := a2.Resume(ctx)
	if !ok || !errors.Is(err, session.ErrE2EEDowngrade) {
		t.Fatalf("Resume on downgraded server: ok=%v err=%v, want true, ErrE2EEDowngrade", ok, err)
	}
	if a2.State().(UIState).Session != nil {
		t.Fatal("alice resumed into downgraded session")
	}
}
