package session

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"blite/internal/media"
	"blite/internal/metrics"
	"blite/internal/server"
)

// TestMetricsWhileSharing: a share records a reading a second, written
// when the share stops.
func TestMetricsWhileSharing(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	dir := t.TempDir()
	rec := metrics.NewRecorder(dir)
	defer rec.Close()
	s, err := Create(context.Background(), Config{Server: ws, Name: "alice", View: media.ViewCount, Metrics: rec, Log: quiet}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3500 * time.Millisecond)
	if err := s.SetSource("none"); err != nil {
		t.Fatal(err)
	}
	var rs []metrics.Record
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && len(rs) < 3; time.Sleep(100 * time.Millisecond) {
		names := metrics.Newest(dir, 1)
		if len(names) == 0 {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(dir, names[0]))
		if f, err := metrics.Decode(strings.NewReader(string(b))); err == nil {
			rs = f.Records()
		}
	}
	if len(rs) < 3 {
		t.Fatalf("%d readings written after the share stopped", len(rs))
	}
	for i, r := range rs {
		if r.Flags&metrics.FlagSharing == 0 || r.Height != 720 || r.HighFPS == 0 {
			t.Fatalf("reading %d: %+v", i, r)
		}
		if i > 0 && r.Encoded == metrics.Unknown {
			t.Fatalf("reading %d has no frame counts: %+v", i, r)
		}
	}
}
