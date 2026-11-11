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

// The self-preview shows only while we share and the layout
// asks for it, and survives a restart of the capture.
func TestSelfPreview(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	s, err := Create(context.Background(), Config{Server: ws, Name: "alice", View: media.ViewCount, Log: quiet}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if (Layout{Mode: LayoutWindows, Preview: "corner"}).Valid() {
		t.Fatal("unknown preview accepted")
	}
	if err := s.SetLayout(Layout{Mode: LayoutWindows, Preview: PreviewWindow}); err != nil {
		t.Fatal(err)
	}
	if n := s.PreviewFrames(); n != -1 {
		t.Fatalf("preview open (%d frames) while not sharing", n)
	}
	if err := s.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	shows := func() bool {
		t.Helper()
		a := s.PreviewFrames()
		time.Sleep(500 * time.Millisecond)
		return a >= 0 && s.PreviewFrames() > a
	}
	if !shows() {
		t.Fatal("no preview while sharing")
	}
	if err := s.SetResolution(media.Res1080); err != nil { // restarts the capture
		t.Fatal(err)
	}
	if !shows() {
		t.Fatal("no preview after the capture restarted")
	}
	s.mu.Lock()
	on := s.previewOn
	s.mu.Unlock()
	if !on {
		t.Fatal("preview channel closed after the restart")
	}
	if err := s.SetLayout(Layout{Mode: LayoutWindows}); err != nil {
		t.Fatal(err)
	}
	if n := s.PreviewFrames(); n != -1 {
		t.Fatalf("preview still open (%d frames) after turning it off", n)
	}
	if err := s.SetLayout(Layout{Mode: LayoutWindows, Preview: PreviewGrid}); err != nil {
		t.Fatal(err)
	}
	if !shows() { // no grid window yet: it floats
		t.Fatal("grid preview didn't float without a grid window")
	}
	if err := s.SetSource("none"); err != nil {
		t.Fatal(err)
	}
	if n := s.PreviewFrames(); n != -1 {
		t.Fatalf("preview open (%d frames) after sharing stopped", n)
	}
}
