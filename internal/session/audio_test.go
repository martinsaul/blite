package session

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"blite/internal/audio"
	"blite/internal/media"
	"blite/internal/server"
)

// Overlapping audio selections run one capture, not one each: two
// captures on one track made listeners hear the audio cut in and out.
func TestConcurrentSetAudioOneCapture(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	s, err := Create(context.Background(), Config{Server: ws, Name: "alice", View: media.ViewCount, Log: quiet}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	before := media.LiveAudioSources()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.SetAudio(audio.Selection{Mode: "test"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := media.LiveAudioSources() - before; n != 1 {
		t.Fatalf("%d audio captures run after 4 overlapping selections, want 1", n)
	}
	if err := s.SetAudio(audio.Selection{Mode: "none"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := media.LiveAudioSources() - before; n != 0 {
		t.Fatalf("%d audio captures still run after choosing none", n)
	}
}

// Stopping the video stops the audio shared with it, so nobody keeps
// sending sound without knowing; going live again brings that audio back.
// Audio shared on its own stays.
func TestAudioStopsWithVideo(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	s, err := Create(context.Background(), Config{Server: ws, Name: "alice", View: media.ViewCount, Log: quiet}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	before := media.LiveAudioSources()
	captures := func(want int, when string) {
		t.Helper()
		var n int
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if n = media.LiveAudioSources() - before; n == want {
				return
			}
		}
		t.Fatalf("%s: %d audio captures, want %d", when, n, want)
	}
	mode := func(want, when string) {
		t.Helper()
		if m := s.State().Audio.Mode; m != want {
			t.Fatalf("%s: audio %q, want %q", when, m, want)
		}
	}
	if err := s.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAudio(audio.Selection{Mode: "test"}); err != nil {
		t.Fatal(err)
	}
	captures(1, "live with audio")
	if err := s.SetSource("none"); err != nil {
		t.Fatal(err)
	}
	captures(0, "video stopped")
	mode("none", "video stopped")
	if err := s.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	captures(1, "live again")
	mode("test", "live again")

	// Audio only: stopping (no video to stop) leaves it on.
	if err := s.SetSource("none"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAudio(audio.Selection{Mode: "test"}); err != nil {
		t.Fatal(err)
	}
	captures(1, "audio only")
	if err := s.SetSource("none"); err != nil {
		t.Fatal(err)
	}
	captures(1, "audio only, stop with no video")
	mode("test", "audio only, stop with no video")
}
