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

func member(s *Session, name string) Member {
	for _, m := range s.State().Members {
		if m.Name == name {
			return m
		}
	}
	return Member{}
}

// A viewer's quality choice reaches the sender: "off" stops their video to us
// (audio keeps going), "low" caps it, "auto" lets it climb back.
func TestViewerQualityReachesSender(t *testing.T) {
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
	waitFor(t, "first frames", 15*time.Second, func() bool { return member(b, "alice").Frames > 10 })
	alice := member(b, "alice").ID

	layer := func(want string) func() bool { return func() bool { return member(a, "bob").Layer == want } }
	if err := b.SetPeer(alice, "off", 0.5, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice stops sending video to bob", 5*time.Second, layer("off"))
	time.Sleep(500 * time.Millisecond)
	f1 := member(b, "alice").Frames
	time.Sleep(time.Second)
	if f2 := member(b, "alice").Frames; f2 != f1 {
		t.Fatalf("frames still arriving with quality off: %d -> %d", f1, f2)
	}
	if m := member(b, "alice"); m.Quality != "off" || m.Volume != 0.5 {
		t.Fatalf("settings not kept: %+v", m)
	}

	if err := b.SetPeer(alice, "low", 1, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice sends LOW", 5*time.Second, layer("low"))
	if err := b.SetPeer(alice, "auto", 1, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice climbs back to HIGH", 10*time.Second, layer("high"))

	if b.SetPeer(alice, "ultra", 1, false) == nil || b.SetPeer(alice, "auto", 3, false) == nil {
		t.Fatal("accepted bad settings")
	}
}

// Overlapping source starts (a double click) leave exactly one capture
// running: the last one.
func TestOverlappingSetSource(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	a, err := Create(context.Background(), Config{Server: ws, Name: "alice", View: media.ViewCount, Log: quiet}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	before := media.LiveVideoSources()
	done := make(chan error, 2)
	for range 2 {
		go func() { done <- a.SetSource(media.SourceTest) }()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if n := media.LiveVideoSources() - before; n != 1 {
		t.Fatalf("%d captures running after two overlapping starts, want 1", n)
	}
	a.SetSource("none")
	if n := media.LiveVideoSources() - before; n != 0 {
		t.Fatalf("%d captures running after stop", n)
	}
}
