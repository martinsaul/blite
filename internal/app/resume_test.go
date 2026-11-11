package app

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"blite/internal/audio"
	"blite/internal/identity"
	"blite/internal/media"
	"blite/internal/server"
	"blite/internal/session"
)

func tempResume(t *testing.T) string {
	f := filepath.Join(t.TempDir(), "blite", "resume.json")
	old := resumePath
	resumePath = func() (string, error) { return f, nil }
	t.Cleanup(func() { resumePath = old })
	return f
}

// The resume file round-trips, is private, and goes stale after
// ResumeWindow (then it is removed).
func TestResumeFile(t *testing.T) {
	f := tempResume(t)
	now := time.Now()
	want := session.Rejoin{Room: "ABCD2345", Password: "pw-1234", Source: media.SourceKind("window:42"),
		Resolution: "1080p", Audio: audio.Selection{Mode: "apps", Apps: []string{"firefox"}}, Output: "dev1"}
	if err := writeResume(want, now); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(f); err != nil || runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("resume file mode: %v %v", st.Mode(), err)
	}
	got, ok := readResume(now.Add(ResumeWindow - time.Second))
	if !ok || got.Room != want.Room || got.Password != want.Password || got.Source != want.Source ||
		got.Resolution != want.Resolution || got.Audio.Mode != "apps" || len(got.Audio.Apps) != 1 || got.Output != "dev1" {
		t.Fatalf("read back %+v ok=%v", got, ok)
	}
	if _, ok := readResume(now.Add(ResumeWindow + time.Second)); ok {
		t.Fatal("stale resume accepted")
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatalf("stale resume file kept: %v", err)
	}
	if _, ok := readResume(now); ok {
		t.Fatal("no file, but a resume")
	}
}

// In-call update end to end at the app level: alice shares, leaves for an
// update, and a new App with the same identity resumes from the file: it
// is back in the room, owns it, and shares the same source again.
func TestUpdateResumesCall(t *testing.T) {
	f := tempResume(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	key := identity.Ephemeral()
	mk := func(name string, k identity.Key) *App {
		return New(session.Config{Server: ws, Name: name, View: media.ViewCount, Log: quiet, Identity: k}, media.SourceKind("none"))
	}
	a, b := mk("alice", key), mk("bob", identity.Ephemeral())
	defer b.Shutdown()
	ctx := context.Background()
	if err := a.Create(ctx, "", true); err != nil {
		t.Fatal(err)
	}
	if err := a.SetSource("test"); err != nil {
		t.Fatal(err)
	}
	if err := b.JoinLink(ctx, a.State().(UIState).Session.Link); err != nil {
		t.Fatal(err)
	}
	alice := func() (frames int, owner bool) {
		for _, m := range b.State().(UIState).Session.Members {
			if m.Name == "alice" && m.State == "connected" {
				return m.Frames, m.Owner
			}
		}
		return 0, false
	}
	waitUntil := func(what string, ok func() bool) {
		t.Helper()
		for end := time.Now().Add(15 * time.Second); !ok(); time.Sleep(100 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatal("timed out: " + what)
			}
		}
	}
	waitUntil("bob sees alice's video", func() bool { n, o := alice(); return n > 10 && o })

	if !a.leaveForUpdate() {
		t.Fatal("not in a call")
	}
	a.Shutdown()
	if _, err := os.Stat(f); err != nil {
		t.Fatalf("no resume file: %v", err)
	}

	a2 := mk("alice", key)
	defer a2.Shutdown()
	ok, err := a2.Resume(ctx)
	if !ok || err != nil {
		t.Fatalf("resume: %v %v", ok, err)
	}
	if st := a2.State().(UIState).Session; st == nil || st.Source != "test" || !st.IsOwner {
		t.Fatalf("resumed session %+v", st)
	}
	waitUntil("bob sees alice back, owner, video", func() bool { n, o := alice(); return n > 10 && o })
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatalf("resume file kept after use: %v", err)
	}
	if ok, _ := a2.Resume(ctx); ok {
		t.Fatal("resumed twice")
	}
}
