package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"blite/internal/proto"
	"blite/internal/room"

	"github.com/coder/websocket"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func serve(t *testing.T, s *Server) string {
	t.Helper()
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http")
}

func readFile(t *testing.T, f string) []byte {
	t.Helper()
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The defer marker counts only with BLITE_COLOR set and the color's own
// file next to the state file, and only when checked (at startup).
func TestShouldDefer(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "rooms.json")
	if ShouldDefer(state, "green") || ShouldDefer(state, "") || ShouldDefer("", "green") {
		t.Fatal("deferred without a marker")
	}
	if err := os.WriteFile(filepath.Join(dir, ".ci-deploy-defer-green"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !ShouldDefer(state, "green") {
		t.Fatal("marker for green: not deferred")
	}
	if ShouldDefer(state, "blue") || ShouldDefer(state, "") || ShouldDefer("", "green") {
		t.Fatal("another color, no color or no state file: deferred")
	}
}

// A deferred server neither loads nor writes the state file; its rooms
// (a canary's) are dropped at adopt, their sockets closed, then the file
// is loaded and saves start. Adopt is idempotent; a server that didn't
// start deferred refuses it.
func TestDeferAdopt(t *testing.T) {
	file := filepath.Join(t.TempDir(), "rooms.json")
	// Blue's state: one room with alice in it.
	blue := New(Config{StateFile: file, Log: quietLog()})
	a := dial(t, serve(t, blue))
	a.send(proto.Create{T: proto.TCreate, Password: pw, Name: "alice"})
	wa := a.expect(proto.TWelcome)
	if err := blue.SaveState(); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, file)

	green := New(Config{StateFile: file, DeferState: true, StateEvery: 10 * time.Millisecond, Log: quietLog()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go green.Run(ctx)
	url := serve(t, green)
	if !green.Deferred() {
		t.Fatal("not deferred")
	}
	c := dial(t, url) // the canary's room
	c.send(proto.Create{T: proto.TCreate, Password: pw, Name: "canary"})
	c.expect(proto.TWelcome)
	time.Sleep(50 * time.Millisecond) // a few Run save ticks
	if err := green.SaveState(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, file), before) {
		t.Fatal("deferred server wrote the state file")
	}
	// Before adopt nothing of blue's is here.
	r := dial(t, url)
	r.send(proto.Resume{T: proto.TResume, Room: wa["room"].(string), PeerID: wa["peerId"].(string), Token: wa["resume"].(string)})
	if e := r.expect(proto.TError); e["code"] != proto.ErrResumeFail {
		t.Fatalf("resume before adopt: %v", e)
	}

	if _, _, err := blue.Adopt(); !errors.Is(err, ErrNotDeferred) {
		t.Fatalf("adopt on a server that didn't defer: %v", err)
	}
	n, already, err := green.Adopt()
	if err != nil || already || n != 1 {
		t.Fatalf("adopt: %d %v %v", n, already, err)
	}
	if green.Deferred() || green.Rooms().Count() != 1 {
		t.Fatalf("after adopt: deferred %v, rooms %d", green.Deferred(), green.Rooms().Count())
	}
	// The canary's socket is closed.
	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rcancel()
	if _, _, err := c.ws.Read(rctx); err == nil || rctx.Err() != nil {
		t.Fatalf("canary socket still open after adopt: %v", err)
	}
	// Alice resumes on green.
	r = dial(t, url)
	r.send(proto.Resume{T: proto.TResume, Room: wa["room"].(string), PeerID: wa["peerId"].(string), Token: wa["resume"].(string)})
	if w := r.expect(proto.TWelcome); w["peerId"] != wa["peerId"] {
		t.Fatalf("resume after adopt: %v", w)
	}
	// Writes are on now (the save time changes on every save).
	time.Sleep(50 * time.Millisecond)
	if bytes.Equal(readFile(t, file), before) {
		t.Fatal("adopted server doesn't save")
	}
	if n, already, err := green.Adopt(); err != nil || !already || n != 0 {
		t.Fatalf("second adopt: %d %v %v", n, already, err)
	}
}

// An adopt whose state file can't be read leaves the server deferred (a
// save would replace the file with nothing); a later one can succeed.
func TestAdoptBadFileStaysDeferred(t *testing.T) {
	file := filepath.Join(t.TempDir(), "rooms.json")
	os.WriteFile(file, []byte("not json"), 0o600)
	s := New(Config{StateFile: file, DeferState: true, Log: quietLog()})
	if _, _, err := s.Adopt(); err == nil || !s.Deferred() {
		t.Fatalf("bad file: err %v, deferred %v", err, s.Deferred())
	}
	if err := s.SaveState(); err != nil || string(readFile(t, file)) != "not json" {
		t.Fatal("still deferred, yet it saved")
	}
	os.Remove(file)
	if n, _, err := s.Adopt(); err != nil || n != 0 || s.Deferred() {
		t.Fatalf("missing file: %d %v", n, err)
	}
}

// Drain saves once more, then /ws refuses new sessions (and resumes) with
// a retryable 503 while live sockets keep relaying, and the state file is
// never written again: not by Run's saves, not by SaveState (the shutdown
// path). It is idempotent; undrain undoes it.
func TestDrain(t *testing.T) {
	file := filepath.Join(t.TempDir(), "rooms.json")
	s := New(Config{StateFile: file, StateEvery: 10 * time.Millisecond, Log: quietLog()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	url := serve(t, s)
	a := dial(t, url)
	a.send(proto.Create{T: proto.TCreate, Password: pw, Name: "alice"})
	wa := a.expect(proto.TWelcome)
	b := dial(t, url)
	b.send(proto.Join{T: proto.TJoin, Room: wa["room"].(string), Password: pw, Name: "bob"})
	wb := b.expect(proto.TWelcome)
	a.expect(proto.TPeerJoined)

	already, err := s.Drain()
	if err != nil || already || !s.Drained() {
		t.Fatalf("drain: %v %v", already, err)
	}
	flushed := readFile(t, file)
	n, err := room.NewManager(time.Minute).Restore(flushed)
	if err != nil || n != 1 || !bytes.Contains(flushed, []byte(wb["peerId"].(string))) {
		t.Fatalf("flushed state: %d rooms, %v", n, err)
	}

	// Creates, joins and resumes alike are refused at the upgrade, on
	// every host name.
	for _, host := range []string{"bellion.app", "bellion.zenithnetwork.com", "blite-blue.internal.example"} {
		if _, st := dialHost(t, url, host, nil); st != http.StatusServiceUnavailable {
			t.Fatalf("new session on a drained server (%s): status %d", host, st)
		}
	}
	// Live sockets still relay.
	b.send(map[string]any{"t": "signal", "to": wa["peerId"], "data": map[string]any{"sdp": "offer"}})
	if sig := a.expect(proto.TSignal); sig["from"] != wb["peerId"] {
		t.Fatalf("signal after drain: %v", sig)
	}
	// Bob leaves; nothing is written about it.
	b.send(map[string]any{"t": "leave"})
	a.expect(proto.TPeerLeft)
	time.Sleep(50 * time.Millisecond) // Run save ticks
	if err := s.SaveState(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, file), flushed) {
		t.Fatal("state file written after drain")
	}
	if already, err := s.Drain(); err != nil || !already {
		t.Fatalf("second drain: %v %v", already, err)
	}
	if !bytes.Equal(readFile(t, file), flushed) {
		t.Fatal("second drain wrote the state file")
	}

	if err := s.Undrain(); err != nil || s.Drained() {
		t.Fatalf("undrain: %v", err)
	}
	if _, st := dialHost(t, url, "bellion.app", nil); st != 0 {
		t.Fatalf("after undrain: %d", st)
	}
	if err := s.Undrain(); !errors.Is(err, ErrNotDrained) {
		t.Fatalf("second undrain: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if bytes.Equal(readFile(t, file), flushed) {
		t.Fatal("undrained server doesn't save")
	}
}

// A deferred server has nothing to flush: drain refuses and changes
// nothing.
func TestDrainDeferred(t *testing.T) {
	s := New(Config{StateFile: filepath.Join(t.TempDir(), "rooms.json"), DeferState: true, Log: quietLog()})
	if _, err := s.Drain(); !errors.Is(err, ErrDeferred) || s.draining.Load() {
		t.Fatalf("drain while deferred: %v", err)
	}
}

// A drain whose save fails isn't a drain: sessions are taken again.
func TestDrainSaveFails(t *testing.T) {
	s := New(Config{StateFile: filepath.Join(t.TempDir(), "missing", "rooms.json"), Log: quietLog()})
	if _, err := s.Drain(); err == nil || s.Drained() || s.draining.Load() {
		t.Fatalf("failed drain: %v, drained %v", err, s.Drained())
	}
}

// The admin handler answers loopback peers only, and its endpoints map
// to Drain / Adopt / Undrain.
func TestAdminHandler(t *testing.T) {
	s := New(Config{StateFile: filepath.Join(t.TempDir(), "rooms.json"), DeferState: true, Log: quietLog()})
	h := s.AdminHandler()
	do := func(method, path, remote string) (int, string) {
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	for _, remote := range []string{"10.0.0.7:4000", "172.18.0.2:5555", "[2001:db8::1]:80", "garbage"} {
		if code, _ := do("POST", "/v1/admin/adopt", remote); code != http.StatusForbidden {
			t.Fatalf("%s: %d", remote, code)
		}
	}
	if !s.Deferred() {
		t.Fatal("a refused request adopted")
	}
	if code, body := do("POST", "/v1/admin/drain", "127.0.0.1:9"); code != http.StatusConflict {
		t.Fatalf("drain while deferred: %d %s", code, body)
	}
	if code, body := do("POST", "/v1/admin/adopt", "127.0.0.1:9"); code != 200 || !strings.Contains(body, "adopted state: rooms=0") {
		t.Fatalf("adopt: %d %s", code, body)
	}
	if code, body := do("POST", "/v1/admin/adopt", "[::1]:9"); code != 200 || !strings.Contains(body, "already") {
		t.Fatalf("adopt again: %d %s", code, body)
	}
	if code, body := do("POST", "/v1/admin/drain", "127.0.0.1:9"); code != 200 || !strings.Contains(body, "drained") {
		t.Fatalf("drain: %d %s", code, body)
	}
	if code, body := do("POST", "/v1/admin/drain", "127.0.0.1:9"); code != 200 || !strings.Contains(body, "already drained") {
		t.Fatalf("drain again: %d %s", code, body)
	}
	if code, body := do("GET", "/v1/admin/status", "127.0.0.1:9"); code != 200 || !strings.Contains(body, "mode=drained") {
		t.Fatalf("status: %d %s", code, body)
	}
	if code, _ := do("POST", "/v1/admin/undrain", "127.0.0.1:9"); code != 200 {
		t.Fatalf("undrain: %d", code)
	}
	// The public handler has no admin routes.
	req := httptest.NewRequest("POST", "/v1/admin/drain", nil)
	req.RemoteAddr = "127.0.0.1:9"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code == 200 || s.Drained() {
		t.Fatalf("public handler drained: %d", w.Code)
	}
}

// Deferred and drained servers leave the standby's live file alone: the
// state dir is shared with the other color.
func TestLiveFileOnlyWhenWriting(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{Standby: true, StateFile: filepath.Join(dir, "rooms.json"), DeferState: true, Log: quietLog()})
	c, st := dialHost(t, serve(t, s), "blite-green.internal.example", nil)
	if st != 0 {
		t.Fatalf("canary host refused on a standby: %d", st)
	}
	c.send(proto.Create{T: proto.TCreate, Password: pw, Name: "x"})
	c.expect(proto.TWelcome)
	s.syncLiveFile()
	if _, err := os.Stat(filepath.Join(dir, "rooms.json.live")); err == nil {
		t.Fatal("deferred server wrote the live file")
	}
}

// A socket upgraded before the drain whose first message comes after it
// is refused (closed, try again), not seated after the final save, where
// the new instance would never know about it.
func TestDrainRefusesUpgradedSocket(t *testing.T) {
	file := filepath.Join(t.TempDir(), "rooms.json")
	s := New(Config{StateFile: file, Log: quietLog()})
	url := serve(t, s)
	a := dial(t, url) // upgraded, not seated yet
	if _, err := s.Drain(); err != nil {
		t.Fatal(err)
	}
	a.send(proto.Create{T: proto.TCreate, Password: pw, Name: "alice"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := a.ws.Read(ctx)
	if err == nil {
		t.Fatalf("seated after the drain: %s", b)
	}
	if st := websocket.CloseStatus(err); st != websocket.StatusTryAgainLater {
		t.Fatalf("close status %v (%v), want try again later", st, err)
	}
	if n := s.rooms.Count(); n != 0 {
		t.Fatalf("%d rooms after a refused create", n)
	}
}
