package session

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"blite/internal/audio"
	"blite/internal/feeds"
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

// A viewer's quality choice ("low" / "auto") reaches the sender; "off" is
// rejected on SetPeer because the eye toggle stops watching video (while
// remote audio plays independently, controlled by mute and volume).
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
	a.SetAudio(audio.Selection{Mode: "test"})
	b, err := Join(ctx, cfg("bob"), a.sig.Welcome.Room, a.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	waitFor(t, "first frames and audio", 15*time.Second, func() bool {
		m := member(b, "alice")
		return m.Frames > 10 && m.Audio > 10
	})
	alice := member(b, "alice").ID

	layer := func(want string) func() bool { return func() bool { return member(a, "bob").Layer == want } }
	if err := b.SetPeer(alice, "low", 0.5, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice sends LOW", 5*time.Second, layer("low"))
	if m := member(b, "alice"); m.Quality != "low" || m.Volume != 0.5 || !m.Muted {
		t.Fatalf("settings not kept: %+v", m)
	}

	if err := b.SetPeer(alice, "auto", 1, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice climbs back to HIGH", 10*time.Second, layer("high"))

	if err := b.ToggleWatch(alice); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice stops sending video to bob", 5*time.Second, layer("off"))
	time.Sleep(400 * time.Millisecond)
	m1 := member(b, "alice")
	if m1.Eye != feeds.EyeClosed || m1.Watching {
		t.Fatalf("expected closed eye and not watching: %+v", m1)
	}
	time.Sleep(800 * time.Millisecond)
	m2 := member(b, "alice")
	if m2.Frames != m1.Frames || m2.Audio != 0 {
		t.Fatalf("expected video and sound stopped while not watching: frames %d -> %d, audio %d -> %d", m1.Frames, m2.Frames, m1.Audio, m2.Audio)
	}

	if err := b.ToggleWatch(alice); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice resumes video and audio", 10*time.Second, func() bool {
		m := member(b, "alice")
		return member(a, "bob").Layer == "high" && m.Eye == feeds.EyeOpen && m.Frames > 5 && m.Audio > 5
	})

	if b.SetPeer(alice, "off", 1, false) == nil || b.SetPeer(alice, "ultra", 1, false) == nil || b.SetPeer(alice, "auto", 3, false) == nil {
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
