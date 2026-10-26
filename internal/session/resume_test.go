package session

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"blite/internal/media"
	"blite/internal/server"
)

func frames(s *Session, name string) (int, string) {
	for _, m := range s.State().Members {
		if m.Name == name {
			return m.Frames, m.State
		}
	}
	return 0, ""
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A dropped signaling socket must not end the call: media keeps flowing, the
// slot is resumed, and signaling works again afterwards (a new peer can join
// and connect to the resumed one).
func TestSignalingDropResumes(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	cfg := func(n string) Config { return Config{Server: ws, Name: n, View: media.ViewCount, Log: quiet} }
	ctx := context.Background()

	a, err := Create(ctx, cfg("alice"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetSource(media.SourceTest)
	b, err := Join(ctx, cfg("bob"), a.sig.Welcome.Room, a.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.SetSource(media.SourceTest)
	waitFor(t, "first frames", 15*time.Second, func() bool { f, _ := frames(b, "alice"); return f > 20 })

	before, _ := frames(b, "alice")
	a.sig.Drop() // network blip on alice's signaling only
	time.Sleep(2 * time.Second)
	after, st := frames(b, "alice")
	if after <= before || st != "connected" {
		t.Fatalf("media stalled during signaling outage: %d -> %d (%s)", before, after, st)
	}
	select {
	case <-a.Done():
		t.Fatal("alice's session ended on a signaling drop")
	default:
	}

	c, err := Join(ctx, cfg("carol"), a.sig.Welcome.Room, a.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitFor(t, "carol connects to resumed alice", 15*time.Second, func() bool { _, st := frames(c, "alice"); return st == "connected" })
}
