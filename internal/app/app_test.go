package app

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
	"blite/internal/session"
)

// Two apps, real media, in-process signaling: A creates, B joins by link, both
// see each other's video, then A leaving is noticed by B.
func TestCreateJoinLeave(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet, ICE: nil}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	mk := func(name string) *App {
		return New(session.Config{Server: ws, Name: name, View: media.ViewCount, Log: quiet}, media.SourceTest)
	}
	a, b := mk("alice"), mk("bob")
	defer a.Shutdown()
	defer b.Shutdown()

	ctx := context.Background()
	if err := a.Create(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := a.Create(ctx, ""); err != ErrInSession {
		t.Fatalf("second create: %v", err)
	}
	link := a.State().(UIState).Session.Link
	if err := b.JoinLink(ctx, link); err != nil {
		t.Fatal(err)
	}
	if err := b.JoinLink(ctx, strings.Replace(link, "#", "#x", 1)); err != ErrInSession {
		t.Fatalf("join while in session: %v", err)
	}

	got := func(x *App, peer string) int {
		for _, m := range x.State().(UIState).Session.Members {
			if m.Name == peer && m.State == "connected" {
				return m.Frames
			}
		}
		return 0
	}
	deadline := time.Now().Add(15 * time.Second)
	for got(a, "bob") < 20 || got(b, "alice") < 20 {
		if time.Now().After(deadline) {
			t.Fatalf("frames: alice<-bob %d, bob<-alice %d", got(a, "bob"), got(b, "alice"))
		}
		time.Sleep(200 * time.Millisecond)
	}

	a.Leave()
	deadline = time.Now().Add(10 * time.Second)
	for len(b.State().(UIState).Session.Members) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("bob still sees alice after she left")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Bad password through the friendly error path.
	c := mk("carol")
	defer c.Shutdown()
	bad := link[:strings.Index(link, "#")] + "#wrong-password"
	if err := c.JoinLink(ctx, bad); err == nil || !strings.Contains(err.Error(), "session doesn't exist") && !strings.Contains(err.Error(), "wrong password") {
		t.Fatalf("bad password: %v", err)
	}
}
