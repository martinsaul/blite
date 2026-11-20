package session

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"blite/internal/feeds"
	"blite/internal/media"
	"blite/internal/server"
)

// Our own preview opens as a tile in the main streaming window by default
// while we share, survives a capture restart, hides/reopens via our own eye
// (auto-closing the main window when nobody else is watched), and pops out
// and back in.
func TestSelfPreview(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	s, err := Create(context.Background(), Config{Server: ws, Name: "alice", View: media.ViewGrid, Log: quiet}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.gridMu.Lock()
	s.gridView = media.ViewCount
	s.openWindow = func(title string, _, _ int) (media.Window, error) {
		return newFakeWindow(title), nil
	}
	s.gridMu.Unlock()

	if st := s.State(); s.PreviewFrames() != -1 || st.SelfEye != feeds.EyeGrey || st.SelfState != feeds.Gone {
		t.Fatalf("preview open while not sharing: frames=%d eye=%s state=%s", s.PreviewFrames(), st.SelfEye, st.SelfState)
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
	if st := s.State(); st.SelfEye != feeds.EyeOpen || st.SelfState != feeds.InMain {
		t.Fatalf("unexpected self state while sharing: %+v", st)
	}
	s.gridMu.Lock()
	mainOpen := s.wins[""] != nil && s.grids[""] != nil
	s.gridMu.Unlock()
	if !mainOpen {
		t.Fatal("main window did not open for self-preview tile")
	}

	if err := s.SetResolution(media.Res1080); err != nil { // restarts the capture
		t.Fatal(err)
	}
	if !shows() {
		t.Fatal("no preview after the capture restarted")
	}

	// Hide our own preview via the eye: nobody else is watched, so the main
	// window closes silently without prompting to leave.
	if err := s.ToggleWatch(feeds.Self); err != nil {
		t.Fatal(err)
	}
	if st := s.State(); s.PreviewFrames() != -1 || st.SelfEye != feeds.EyeClosed || st.SelfState != feeds.NotWatched || st.LeavePrompt {
		t.Fatalf("after hiding preview: frames=%d eye=%s state=%s leavePrompt=%v", s.PreviewFrames(), st.SelfEye, st.SelfState, st.LeavePrompt)
	}
	s.gridMu.Lock()
	winCount := len(s.wins)
	s.gridMu.Unlock()
	if winCount != 0 {
		t.Fatalf("expected 0 windows open after hiding only tile, got %d", winCount)
	}

	// Turn preview back on: reopens in the main window.
	if err := s.ToggleWatch(feeds.Self); err != nil {
		t.Fatal(err)
	}
	if !shows() {
		t.Fatal("preview did not reopen after toggling eye back on")
	}
	if st := s.State(); st.SelfState != feeds.InMain || st.SelfEye != feeds.EyeOpen {
		t.Fatalf("expected InMain after re-watching preview, got %s (%s)", st.SelfState, st.SelfEye)
	}

	// Pop out our own preview, then close the pop-out: returns to main window.
	if err := s.PopOut(feeds.Self); err != nil {
		t.Fatal(err)
	}
	s.gridMu.Lock()
	popW, _ := s.wins[feeds.Self].(*fakeWindow)
	mainNil := s.wins[""] == nil
	s.gridMu.Unlock()
	if popW == nil || !mainNil {
		t.Fatalf("expected self pop-out window only: pop=%v mainNil=%v", popW, mainNil)
	}
	close(popW.closed)
	waitFor(t, "self pop-out returns to main", 5*time.Second, func() bool {
		return s.State().SelfState == feeds.InMain
	})

	if err := s.SetSource("none"); err != nil {
		t.Fatal(err)
	}
	if st := s.State(); s.PreviewFrames() != -1 || st.SelfEye != feeds.EyeGrey || st.SelfState != feeds.Gone {
		t.Fatalf("preview open after sharing stopped: frames=%d eye=%s state=%s", s.PreviewFrames(), st.SelfEye, st.SelfState)
	}
}

// Our own preview shows as a corner picture by default (filling the main
// window while alone, from the big preview), can become a tile and pop out,
// and each change is handed to Config.OnSelfView; the next session starts
// the way it was left.
func TestSelfView(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	kept := make(chan string, 8)
	start := func(view string) (*Session, *[2]int) {
		t.Helper()
		s, err := Create(context.Background(), Config{Server: ws, Name: "alice", View: media.ViewGrid, Log: quiet,
			SelfView: view, OnSelfView: func(v string) { kept <- v }}, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		var size [2]int
		s.gridMu.Lock()
		s.gridView = media.ViewCount
		s.openWindow = func(title string, w, h int) (media.Window, error) {
			size = [2]int{w, h}
			return newFakeWindow(title), nil
		}
		s.gridMu.Unlock()
		if err := s.SetSource(media.SourceTest); err != nil {
			t.Fatal(err)
		}
		return s, &size
	}
	selfTile := func(s *Session, k string) media.Tile {
		t.Helper()
		s.gridMu.Lock()
		defer s.gridMu.Unlock()
		g := s.grids[k]
		if g == nil {
			t.Fatalf("no window %q", k)
		}
		for _, tl := range g.Tiles() {
			if tl.Self {
				return tl
			}
		}
		t.Fatalf("no preview in window %q", k)
		return media.Tile{}
	}
	keep := func(want string) {
		t.Helper()
		select {
		case v := <-kept:
			if v != want {
				t.Fatalf("kept %q, want %q", v, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("%q not kept", want)
		}
	}

	s, size := start("")
	if tl := selfTile(s, ""); !tl.PiP || tl.Channel != media.PreviewBigChannel(s.selfCh) {
		t.Fatalf("default: %+v, want the corner picture, big while alone", tl)
	}
	if v := s.SelfView(); v != SelfViewCorner {
		t.Fatalf("SelfView %q", v)
	}
	s.control(feeds.Self, media.ControlExpand, 0)
	keep(SelfViewTile)
	if tl := selfTile(s, ""); tl.PiP || tl.Channel != media.PreviewBigChannel(s.selfCh) {
		t.Fatalf("expanded: %+v, want a big tile", tl)
	}
	s.control(feeds.Self, media.ControlPin, 0)
	s.control(feeds.Self, media.ControlExpand, 0) // back to the corner, unpinned
	keep(SelfViewCorner)
	if st := s.State().SelfState; st != feeds.InMain {
		t.Fatalf("back in the corner: %s", st)
	}
	s.control(feeds.Self, media.ControlPopOut, 0)
	keep(SelfViewWindow)
	if tl := selfTile(s, feeds.Self); tl.Channel != media.PreviewBigChannel(s.selfCh) || *size != [2]int{selfWindowW, selfWindowH} {
		t.Fatalf("popped out: %+v in a %v window", tl, *size)
	}
	s.control(feeds.Self, media.ControlEye, 0)
	keep(SelfViewHidden)
	s.Close()

	for _, c := range []struct {
		view string
		want feeds.State
	}{{SelfViewWindow, feeds.PoppedOut}, {SelfViewHidden, feeds.NotWatched}, {SelfViewTile, feeds.InMain}} {
		s, _ := start(c.view)
		if st := s.State().SelfState; st != c.want {
			t.Fatalf("started with %q: %s, want %s", c.view, st, c.want)
		}
		if c.view == SelfViewTile {
			if tl := selfTile(s, ""); tl.PiP {
				t.Fatalf("started as a tile: %+v", tl)
			}
		}
		select {
		case v := <-kept:
			t.Fatalf("starting kept %q", v)
		default:
		}
		s.Close()
	}
}
