package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"blite/internal/identity"
	"blite/internal/media"
	"blite/internal/proto"
	"blite/internal/secret"
	"blite/internal/server"
	"blite/internal/session"
)

func tempRecent(t *testing.T) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "blite", "recent.json")
	old := recentPath
	recentPath = func() (string, error) { return f, nil }
	t.Cleanup(func() { recentPath = old })
	return f
}

func TestRecentRoomsPersistenceCapAndProbe(t *testing.T) {
	tempServers(t)
	rf := tempRecent(t)
	oldHold := recentGoneHold
	recentGoneHold = 180 * time.Millisecond
	t.Cleanup(func() { recentGoneHold = oldHold })

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(server.Config{Log: quiet})
	srv.Rooms().EmptyGrace = 0
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	// Older server stub that replies bad_request to probe (ProbeUnsupported).
	legacyTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		_, _, _ = c.Read(r.Context())
		b, _ := json.Marshal(proto.Error{T: proto.TError, Code: proto.ErrBadRequest})
		_ = c.Write(r.Context(), websocket.MessageText, b)
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	defer legacyTS.Close()
	legacyWS := "ws" + strings.TrimPrefix(legacyTS.URL, "http") + "/ws"

	ctx := context.Background()

	// Create 6 rooms: first 4 will be closed after creation, 5th stays open
	// without password, 6th stays open with a password.
	var keepers []*App
	defer func() {
		for _, k := range keepers {
			k.Shutdown()
		}
	}()

	recorder := New(session.Config{Server: ws, Name: "alice", View: "none", Log: quiet}, "none")
	defer recorder.Shutdown()

	var codes []string
	for i := 0; i < 6; i++ {
		pw := ""
		if i == 5 {
			pw = "top-secret-pw"
		}
		host := New(session.Config{Server: ws, Name: "host", View: "none", Log: quiet}, "none")
		if err := host.Create(ctx, pw, false); err != nil {
			t.Fatal(err)
		}
		link := strings.Replace(host.State().(UIState).Session.Link, "https://", "http://", 1)
		code := strings.ReplaceAll(host.State().(UIState).Session.Room, "-", "")
		codes = append(codes, code)

		if err := recorder.JoinLink(ctx, link); err != nil {
			t.Fatal(err)
		}
		recorder.Leave()

		if i >= 4 {
			keepers = append(keepers, host) // keep rooms 4 and 5 alive
		} else {
			host.Shutdown()
			waitFor(t, "closed room swept", func() bool {
				srv.Rooms().Sweep()
				return !srv.Rooms().Probe(code, pw)
			})
		}
	}

	// Cap at 5, newest first: codes[5], codes[4], codes[3], codes[2], codes[1].
	got := recorder.State().(UIState).Recent
	if len(got) != 5 {
		t.Fatalf("expected 5 recent rooms, got %d: %+v", len(got), got)
	}
	wantOrder := []string{codes[5], codes[4], codes[3], codes[2], codes[1]}
	for i, w := range wantOrder {
		if got[i].Code != w {
			t.Fatalf("recent[%d].Code = %q, want %q (all: %+v)", i, got[i].Code, w, got)
		}
	}

	// Rejoining codes[4] promotes it to index 0 without duplicating it.
	if err := recorder.JoinRecentRoom(ctx, ws, codes[4]); err != nil {
		t.Fatalf("JoinRecentRoom(%s): %v", codes[4], err)
	}
	recorder.Leave()
	got = recorder.State().(UIState).Recent
	if len(got) != 5 || got[0].Code != codes[4] || got[1].Code != codes[5] {
		t.Fatalf("rejoining did not deduplicate & promote to top: %+v", got)
	}

	// File permissions must be 0600, and UIState JSON must NEVER expose passwords.
	fi, err := os.Stat(rf)
	if err != nil {
		t.Fatalf("stat recent.json: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("recent.json perm = %04o, want 0600", perm)
	}
	stateJSON, err := json.Marshal(recorder.State())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stateJSON), "top-secret-pw") {
		t.Fatalf("UIState JSON leaked stored password: %s", string(stateJSON))
	}

	// Add an entry on the legacy server (which doesn't support probe) so we test
	// ProbeUnsupported -> "ready" alongside ProbeExists -> "ready" and ProbeGone -> "gone" -> removed.
	recorder.recordRecent(legacyWS, "ABCD2345", "", false)

	// Simulate app restart: loaded entries start as "checking".
	restarted := New(session.Config{Server: ws, Name: "alice", View: "none", Log: quiet}, "none")
	defer restarted.Shutdown()
	initial := restarted.State().(UIState).Recent
	if len(initial) != 5 {
		t.Fatalf("after restart len = %d, want 5", len(initial))
	}
	for _, r := range initial {
		if r.Status != "checking" {
			t.Fatalf("entry %s started with status %q, want checking", r.Code, r.Status)
		}
	}

	// Run ProbeRecentRooms: closed rooms (codes[3], codes[2]) must hit "gone"
	// and then disappear, while LEGACY01, codes[4], and codes[5] become "ready".
	restarted.ProbeRecentRooms()

	sawGone := false
	waitFor(t, "probe marks closed rooms gone", func() bool {
		for _, r := range restarted.State().(UIState).Recent {
			if (r.Code == codes[3] || r.Code == codes[2]) && r.Status == "gone" {
				sawGone = true
				return true
			}
		}
		return false
	})
	if !sawGone {
		t.Fatal("expected to observe 'gone' status before fade-out removal")
	}

	// Disk file should already omit gone entries even before hold finishes.
	waitFor(t, "gone rooms removed after hold and live/legacy rooms ready", func() bool {
		cur := restarted.State().(UIState).Recent
		if len(cur) != 3 {
			return false
		}
		for _, r := range cur {
			if r.Status != "ready" {
				return false
			}
		}
		return true
	})

	cur := restarted.State().(UIState).Recent
	gotCodes := []string{cur[0].Code, cur[1].Code, cur[2].Code}
	wantCodes := []string{"ABCD2345", codes[4], codes[5]}
	if !slices.Equal(gotCodes, wantCodes) {
		t.Fatalf("surviving recent rooms = %v, want %v", gotCodes, wantCodes)
	}
	if !cur[2].HasPassword {
		t.Fatalf("expected HasPassword=true on %+v", cur[2])
	}

	// JoinRecentRoom uses the stored password for codes[5].
	if err := restarted.JoinRecentRoom(ctx, ws, codes[5]); err != nil {
		t.Fatalf("JoinRecentRoom password room: %v", err)
	}
	restarted.Leave()

	// Manual removal via RemoveRecentRoom removes from memory and disk.
	for _, c := range []string{codes[5], "ABCD2345", codes[4]} {
		srv := ws
		if c == "ABCD2345" {
			srv = legacyWS
		}
		if err := restarted.RemoveRecentRoom(srv, c); err != nil {
			t.Fatal(err)
		}
	}
	if rem := restarted.State().(UIState).Recent; len(rem) != 0 {
		t.Fatalf("expected empty recent list after removing all, got %+v", rem)
	}
	if _, err := os.Stat(rf); !os.IsNotExist(err) {
		t.Fatalf("expected recent.json removed when empty, stat err=%v", err)
	}
}

func TestJoinByBareCodeAndHostedRoom(t *testing.T) {
	tempServers(t)
	tempRecent(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	defTS := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer defTS.Close()
	defWS := "ws" + strings.TrimPrefix(defTS.URL, "http") + "/ws"

	customTS := httptest.NewServer(server.New(server.Config{Log: quiet, HostKey: "hk-123"}))
	defer customTS.Close()
	customWS := "ws" + strings.TrimPrefix(customTS.URL, "http") + "/ws"

	ctx := context.Background()

	// 1. Standard 8-char room with password on default server, joined by bare code.
	alice := New(session.Config{Server: defWS, Name: "alice", View: "none", Log: quiet}, "none")
	defer alice.Shutdown()
	if err := alice.Create(ctx, "secret99", false); err != nil {
		t.Fatal(err)
	}
	disp := alice.State().(UIState).Session.Room // e.g. "K7PQ-2M4X"

	bob := New(session.Config{Server: defWS, Name: "bob", View: "none", Log: quiet}, "none")
	defer bob.Shutdown()
	if err := bob.JoinLink(ctx, strings.ToLower(disp)); err == nil || !strings.Contains(err.Error(), "wrong password") {
		t.Fatalf("expected wrong password error for bare code without #pw, got %v", err)
	}
	if err := bob.JoinLink(ctx, strings.ToLower(disp)+"#secret99"); err != nil {
		t.Fatalf("join bare code with #pw: %v", err)
	}
	bob.Leave()

	// 2. Hosted 6-char room ("MAMAC0", typed as "MAMACO") on a custom selected server.
	hostedSess, err := session.Create(ctx, session.Config{
		Server:   customWS,
		Name:     "relay",
		View:     "none",
		Log:      quiet,
		HostRoom: "MAMAC0",
		HostKey:  "hk-123",
	}, "")
	if err != nil {
		t.Fatalf("create hosted room: %v", err)
	}
	defer hostedSess.Close()

	if err := bob.AddServer(customWS); err != nil {
		t.Fatal(err)
	}
	if err := bob.SelectServer(customWS); err != nil {
		t.Fatal(err)
	}
	if err := bob.JoinLink(ctx, "MAMACO"); err != nil {
		t.Fatalf("join hosted room by bare code MAMACO: %v", err)
	}
	if gotRoom := bob.State().(UIState).Session.Room; gotRoom != "MAMAC0" {
		t.Fatalf("joined room = %q, want MAMAC0", gotRoom)
	}
}

func TestDiscordEnterAndShareChannel(t *testing.T) {
	tempServers(t)
	tempRecent(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(server.Config{Log: quiet})
	srv.Rooms().EmptyGrace = 0
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	// Even when DefaultSource is SourceTest, EnterChannel joins/creates WITHOUT sharing.
	alice := New(session.Config{Server: ws, Name: "alice", View: media.ViewCount, Log: quiet}, media.SourceTest)
	defer alice.Shutdown()
	alice.setDiscord(func(d *Discord) { d.Status, d.ChannelID = "ready", "555111" })

	ctx := context.Background()
	if err := alice.EnterChannel(ctx, "", false); err != nil {
		t.Fatalf("alice EnterChannel: %v", err)
	}
	cr := alice.lookupChannel(ctx, "555111")
	if cr.Room == "" || !slices.Equal(cr.Members, []string{"alice"}) || len(cr.Sharers) != 0 {
		t.Fatalf("after EnterChannel expected alice in room without sharing, got %+v", cr)
	}

	// Bob clicks "Share" (GoLiveInChannel) before his Discord poll even knows cr.Room:
	// ErrChannelBusy fallback looks up cr.Room and joins Alice's room, sharing SourceTest!
	bob := New(session.Config{Server: ws, Name: "bob", View: media.ViewCount, Log: quiet}, media.SourceTest)
	defer bob.Shutdown()
	bob.setDiscord(func(d *Discord) { d.Status, d.ChannelID = "ready", "555111" })
	if err := bob.GoLiveInChannel(ctx, "", false); err != nil {
		t.Fatalf("bob GoLiveInChannel into existing channel room: %v", err)
	}

	waitFor(t, "bob sharing in alice's channel room", func() bool {
		c := alice.lookupChannel(ctx, "555111")
		return len(c.Members) == 2 && slices.Equal(c.Sharers, []string{"bob"})
	})
}

func TestRecentPasswordsSealedAndResealedOnWindows(t *testing.T) {
	tempServers(t)
	rf := tempRecent(t)

	oldEnabled, oldProtect, oldUnprotect := secret.Enabled, secret.Protect, secret.Unprotect
	t.Cleanup(func() {
		secret.Enabled = oldEnabled
		secret.Protect = oldProtect
		secret.Unprotect = oldUnprotect
	})
	secret.Enabled = true
	secret.Protect = func(b []byte) ([]byte, error) {
		out := make([]byte, len(b))
		for i, v := range b {
			out[i] = v ^ 0x33
		}
		return append([]byte("win:"), out...), nil
	}
	secret.Unprotect = func(b []byte) ([]byte, error) {
		rest, ok := strings.CutPrefix(string(b), "win:")
		if !ok {
			return nil, os.ErrPermission
		}
		out := make([]byte, len(rest))
		for i := range len(rest) {
			out[i] = rest[i] ^ 0x33
		}
		return out, nil
	}

	// Seed recent.json with an old plaintext entry.
	const ws = "ws://127.0.0.1:18080/ws"
	legacy := []recentDiskEntry{
		{Server: ws, Code: "ABCD2345", Password: "legacy-plain-password"},
	}
	if err := os.MkdirAll(filepath.Dir(rf), 0o700); err != nil {
		t.Fatal(err)
	}
	lb, _ := json.Marshal(legacy)
	if err := os.WriteFile(rf, lb, 0o600); err != nil {
		t.Fatal(err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(session.Config{Server: ws, Name: "alice", View: "none", Log: quiet}, "none")
	defer a.Shutdown()

	// Loaded old entry works in memory.
	if len(a.recent) != 1 || a.recent[0].password != "legacy-plain-password" {
		t.Fatalf("loaded legacy recent = %+v", a.recent)
	}

	// Recording a new room writes recent.json and re-seals all saved passwords.
	a.recordRecent(ws, "K7PQ2M4X", "new-secret-pw", false)
	a.recordRecent(ws, "NOPASS23", "", false)

	raw, err := os.ReadFile(rf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "legacy-plain-password") || strings.Contains(string(raw), "new-secret-pw") {
		t.Fatalf("recent.json leaked plaintext password: %s", raw)
	}
	var disk []recentDiskEntry
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	if len(disk) != 3 {
		t.Fatalf("disk len = %d, want 3", len(disk))
	}
	if disk[0].Password != "" {
		t.Fatalf("empty password should stay empty, got %q", disk[0].Password)
	}
	if !secret.Sealed(disk[1].Password) || !secret.Sealed(disk[2].Password) {
		t.Fatalf("passwords not dpapi-sealed on disk: %+v", disk)
	}

	// Reloading opens both sealed passwords back to plaintext in memory.
	loaded := loadRecent(ws)
	if len(loaded) != 3 || loaded[0].password != "" || loaded[1].password != "new-secret-pw" || loaded[2].password != "legacy-plain-password" {
		t.Fatalf("reloaded recent = %+v", loaded)
	}

	// If profile is copied to another machine where Unprotect fails, dpapi: blob is not kept as password.
	secret.Unprotect = func([]byte) ([]byte, error) { return nil, os.ErrPermission }
	foreign := loadRecent(ws)
	if len(foreign) != 3 || foreign[1].password != "" || foreign[2].password != "" {
		t.Fatalf("foreign machine loadRecent kept unreadable password: %+v", foreign)
	}
}

func TestIdentityErrorShownAndBlocksSession(t *testing.T) {
	tempServers(t)
	tempRecent(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(session.Config{Server: "ws://127.0.0.1:18080/ws", Name: "alice", View: "none", Log: quiet}, "none")
	defer a.Shutdown()

	a.SetIdentityError(identity.ErrWrongAccount)
	st := a.State().(UIState)
	if st.Error != identity.ErrWrongAccount.Error() {
		t.Fatalf("UIState.Error = %q, want %q", st.Error, identity.ErrWrongAccount.Error())
	}
	if err := a.Create(context.Background(), "", true); err != identity.ErrWrongAccount {
		t.Fatalf("Create with identity error = %v, want ErrWrongAccount", err)
	}
	if err := a.JoinLink(context.Background(), "ABCD2345"); err != identity.ErrWrongAccount {
		t.Fatalf("JoinLink with identity error = %v, want ErrWrongAccount", err)
	}
	if _, err := a.BackupIdentity("correct horse"); err != identity.ErrWrongAccount {
		t.Fatalf("BackupIdentity with identity error = %v, want ErrWrongAccount", err)
	}
}
