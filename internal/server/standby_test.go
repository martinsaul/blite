package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"blite/internal/proto"
)

// dialHost opens /ws with a Host header and extra headers, returning the
// HTTP status of a refused upgrade (0 when it upgraded).
func dialHost(t *testing.T, url, host string, hdr http.Header) (*client, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := http.Header{}
	for k, v := range hdr {
		h[k] = v
	}
	h.Set("Host", host)
	ws, resp, err := websocket.Dial(ctx, url+"/ws", &websocket.DialOptions{HTTPHeader: h, Host: host})
	if err != nil {
		if resp == nil {
			t.Fatal(err)
		}
		return nil, resp.StatusCode
	}
	t.Cleanup(func() { ws.CloseNow() })
	return &client{t: t, ws: ws}, 0
}

// A standby refuses sessions that came through the Worker (Host
// bellion.app) with a retryable 503, unless the Worker marks the request
// as a failover or the standby is promoted with its active file. Other
// host names are served as before. While it serves sessions of its own,
// its live file exists (statesync pauses), and it goes once they end.
func TestStandby(t *testing.T) {
	dir := t.TempDir()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(Config{Standby: true, StateFile: filepath.Join(dir, "rooms.json"), Log: quiet})
	ts := httptest.NewServer(s)
	defer ts.Close()
	url := "ws" + strings.TrimPrefix(ts.URL, "http")
	active, live := filepath.Join(dir, "active"), filepath.Join(dir, "rooms.json.live")
	if s.cfg.ActiveFile != active || s.cfg.LiveFile != live {
		t.Fatalf("default files: %q %q", s.cfg.ActiveFile, s.cfg.LiveFile)
	}

	for _, host := range []string{"bellion.app", "www.bellion.app", "Bellion.app:443"} {
		if _, st := dialHost(t, url, host, nil); st != http.StatusServiceUnavailable {
			t.Fatalf("%s through the Worker, no failover: status %d, want 503", host, st)
		}
	}
	// The old names don't go through the Worker: served.
	old, st := dialHost(t, url, "bellion.zenithnetwork.com", nil)
	if st != 0 {
		t.Fatalf("old host name refused: %d", st)
	}
	old.send(proto.Create{T: proto.TCreate, Password: pw, Name: "old"})
	old.expect(proto.TWelcome)

	// The Worker gave up on the primary.
	c, st := dialHost(t, url, "bellion.app", http.Header{FailoverHeader: {"o1.bellion.app:503 x5 over 6s"}})
	if st != 0 {
		t.Fatalf("failover refused: %d", st)
	}
	c.send(proto.Create{T: proto.TCreate, Password: pw, Name: "alice"})
	w := c.expect(proto.TWelcome)
	s.syncLiveFile()
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live file while serving: %v", err)
	}

	// Promoted by hand.
	os.WriteFile(active, nil, 0o600)
	c2, st := dialHost(t, url, "bellion.app", nil)
	if st != 0 {
		t.Fatalf("promoted standby refused: %d", st)
	}
	c2.send(proto.Join{T: proto.TJoin, Room: w["room"].(string), Password: pw, Name: "bob"})
	c2.expect(proto.TWelcome)
	os.Remove(active)
	if _, st := dialHost(t, url, "bellion.app", nil); st != http.StatusServiceUnavailable {
		t.Fatalf("demoted standby: status %d, want 503", st)
	}

	// Everyone leaves: no sessions of its own, no live file.
	for _, cl := range []*client{old, c, c2} {
		cl.send(proto.Leave{T: proto.TLeave})
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.rooms.Served() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("served rooms after everyone left: %d", s.rooms.Served())
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.syncLiveFile()
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Fatalf("live file after the sessions ended: %v", err)
	}
}

// Without Standby, the Worker's names are served and no files are made.
func TestNotStandby(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{StateFile: filepath.Join(dir, "rooms.json"), ActiveFile: filepath.Join(dir, "active"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ts := httptest.NewServer(s)
	defer ts.Close()
	if _, st := dialHost(t, "ws"+strings.TrimPrefix(ts.URL, "http"), "bellion.app", nil); st != 0 {
		t.Fatalf("primary refused: %d", st)
	}
	if s.cfg.LiveFile != "" || s.cfg.ActiveFile != "" {
		t.Fatal("standby files set on a primary")
	}
}
