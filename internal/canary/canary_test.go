package canary

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"blite/internal/server"
)

func start(t *testing.T, cfg server.Config) (*server.Server, *httptest.Server) {
	t.Helper()
	cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	s := server.New(cfg)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts
}

func wsURL(ts *httptest.Server) string { return "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws" }

// The canary passes against a healthy server and leaves no room behind.
func TestCanaryOK(t *testing.T) {
	s, ts := start(t, server.Config{})
	if err := Run(context.Background(), wsURL(ts), Options{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.Rooms().Count() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("canary room still there: %d rooms", s.Rooms().Count())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// On a server that persists, the canary's room never reaches the state
// file, even while it exists.
func TestCanaryNotSaved(t *testing.T) {
	file := filepath.Join(t.TempDir(), "rooms.json")
	s, ts := start(t, server.Config{StateFile: file})
	if err := Run(context.Background(), wsURL(ts), Options{}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveState(); err != nil {
		t.Fatal(err)
	}
	s2, _ := start(t, server.Config{StateFile: file})
	if err := s2.LoadState(); err != nil || s2.Rooms().Count() != 0 {
		t.Fatalf("state after canary: %d rooms, %v", s2.Rooms().Count(), err)
	}
}

// A drained server fails the canary, fast and with a reason.
func TestCanaryDrained(t *testing.T) {
	s, ts := start(t, server.Config{})
	if _, err := s.Drain(); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	err := Run(context.Background(), wsURL(ts), Options{})
	if err == nil || !strings.Contains(err.Error(), "create") || !strings.Contains(err.Error(), "503") {
		t.Fatalf("canary on a drained server: %v", err)
	}
	if time.Since(t0) > 5*time.Second {
		t.Fatalf("took %v", time.Since(t0))
	}
}

// Against a standby, through its canary host name (dialing the server's
// address directly, as through a proxy), the canary passes: the standby
// only turns away bellion.app.
func TestCanaryStandbyCanaryHost(t *testing.T) {
	_, ts := start(t, server.Config{Standby: true, StateFile: filepath.Join(t.TempDir(), "rooms.json"), DeferState: true})
	addr := strings.TrimPrefix(ts.URL, "http://")
	if err := Run(context.Background(), "ws://blite-green.internal.example/ws", Options{Connect: addr}); err != nil {
		t.Fatal(err)
	}
	// The same through bellion.app is refused (the standby gate works).
	if err := Run(context.Background(), "ws://bellion.app/ws", Options{Connect: addr}); err == nil {
		t.Fatal("canary through bellion.app passed on a standby")
	}
}

// A server that needs a newer build than the canary claims fails it.
func TestCanaryBuild(t *testing.T) {
	_, ts := start(t, server.Config{MinCompatibleBuild: 90})
	if err := Run(context.Background(), wsURL(ts), Options{}); err == nil {
		t.Fatal("canary passed below MinCompatibleBuild")
	}
	if err := Run(context.Background(), wsURL(ts), Options{Build: 90}); err != nil {
		t.Fatal(err)
	}
}
