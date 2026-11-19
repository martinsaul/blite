package session

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"blite/internal/audio"
	"blite/internal/feeds"
	"blite/internal/media"
	"blite/internal/server"
)

// The tile overlay buttons toggle mute, volume, and the eye (stopping both
// video and audio completely until re-watched).
func TestVideoWindowButtons(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	cfg := func(n string) Config { return Config{Server: ws, Name: n, View: media.ViewCount, Log: quiet} }
	ctx := context.Background()
	a, err := Create(ctx, cfg("alice"), "secret-pw")
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

	b.control(alice, media.ControlMute, 0)
	if m := member(b, "alice"); !m.Muted {
		t.Fatalf("mute button: %+v", m)
	}
	b.control(alice, media.ControlVolume, 1.2)
	if m := member(b, "alice"); m.Muted || m.Volume != 1.2 {
		t.Fatalf("volume slider: %+v", m)
	}

	b.control(alice, media.ControlEye, 0)
	if m := member(b, "alice"); m.Watching || m.Eye != feeds.EyeClosed {
		t.Fatalf("eye button: %+v", m)
	}
	waitFor(t, "alice stops sending video to bob", 5*time.Second, func() bool { return member(a, "bob").Layer == "off" })
	audioBefore := member(b, "alice").Audio
	waitFor(t, "remote audio continues independently of video eye", 5*time.Second, func() bool {
		return member(b, "alice").Audio > audioBefore
	})

	if err := b.ToggleWatch(alice); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice sends video and audio again", 10*time.Second, func() bool {
		m := member(b, "alice")
		return member(a, "bob").Layer != "off" && m.Watching && m.Audio > 5
	})
}

// Nobody looking: minimized windows ask for nothing, small grid
// tiles for LOW; the viewer's own choice stays as it was and wins when
// stricter.
func TestAutoQuality(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	cfg := func(n string) Config { return Config{Server: ws, Name: n, View: media.ViewCount, Log: quiet} }
	ctx := context.Background()
	a, err := Create(ctx, cfg("alice"), "secret-pw")
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
	// Rates and diagnostics
	waitFor(t, "receive rates", 10*time.Second, func() bool { m := member(b, "alice"); return m.RxKbps > 0 && m.FPSIn > 0 })
	waitFor(t, "send rate", 10*time.Second, func() bool { return member(a, "bob").TxKbps > 0 })
	if d := a.Diagnostics(); !strings.Contains(d, "peer 1:") || strings.Contains(d, "bob") || strings.Contains(d, a.pw) {
		t.Fatalf("diagnostics:\n%s", d)
	}
	t.Log(a.Diagnostics())

	b.hiddenWins.Store(true)
	b.applyAutoQuality()
	if m := member(b, "alice"); m.Auto != "off" || m.Quality != "auto" {
		t.Fatalf("minimized: %+v", m)
	}
	waitFor(t, "alice stops sending video", 5*time.Second, func() bool { return member(a, "bob").Layer == "off" })

	b.hiddenWins.Store(false)
	b.smallTiles.Store(true)
	b.applyAutoQuality()
	waitFor(t, "alice sends LOW", 5*time.Second, func() bool { return member(a, "bob").Layer == "low" })

	b.smallTiles.Store(false)
	b.applyAutoQuality()
	if m := member(b, "alice"); m.Auto != "" {
		t.Fatalf("visible: %+v", m)
	}
	waitFor(t, "alice sends video again", 5*time.Second, func() bool { l := member(a, "bob").Layer; return l != "off" && l != "" })
}

// fakeWindow stands in for a native video window.
type fakeWindow struct {
	mu              sync.Mutex
	title           string
	closed          chan struct{}
	closeReq        chan struct{}
	closeOnce       sync.Once
	ignoreIntercept bool
	intercept       atomic.Bool
	destroyed       atomic.Bool
	minimized       atomic.Bool
	titleCalls      atomic.Int32
	interceptCalls  atomic.Int32
}

func newFakeWindow(title string) *fakeWindow {
	return &fakeWindow{
		title:    title,
		closed:   make(chan struct{}),
		closeReq: make(chan struct{}, 1),
	}
}

func (w *fakeWindow) Handle() uintptr                 { return 0 }
func (w *fakeWindow) Closed() <-chan struct{}         { return w.closed }
func (w *fakeWindow) CloseRequested() <-chan struct{} { return w.closeReq }
func (w *fakeWindow) SetInterceptClose(on bool) {
	w.interceptCalls.Add(1)
	w.intercept.Store(on)
}
func (w *fakeWindow) PointerInside() bool { return true }
func (w *fakeWindow) Minimized() bool     { return w.minimized.Load() }
func (w *fakeWindow) Destroy()            { w.destroyed.Store(true) }
func (w *fakeWindow) SetTitle(t string) {
	w.titleCalls.Add(1)
	w.mu.Lock()
	w.title = t
	w.mu.Unlock()
}
func (w *fakeWindow) Title() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.title
}
func (w *fakeWindow) userClose() {
	if w.intercept.Load() && !w.ignoreIntercept {
		select {
		case w.closeReq <- struct{}{}:
		default:
		}
		return
	}
	w.closeOnce.Do(func() { close(w.closed) })
}

// Main streaming window, pop-out / pop-in, pin, close prompt, and auto-close
// when nothing is watched.
func TestOwnWindows(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	cfg := func(n string, v media.ViewKind) Config { return Config{Server: ws, Name: n, View: v, Log: quiet} }
	ctx := context.Background()

	var prompts atomic.Int32
	viewer, err := Create(ctx, cfg("viewer", media.ViewGrid), "")
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	viewer.OnLeavePrompt(func() { prompts.Add(1) })
	viewer.gridMu.Lock()
	viewer.gridView = media.ViewCount
	viewer.openWindow = func(title string, _, _ int) (media.Window, error) {
		return newFakeWindow(title), nil
	}
	viewer.gridMu.Unlock()

	var sharers []*Session
	for _, n := range []string{"alice", "bob"} {
		s, err := Join(ctx, cfg(n, media.ViewCount), viewer.sig.Welcome.Room, viewer.pw)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		s.SetSource(media.SourceTest)
		sharers = append(sharers, s)
	}
	if err := viewer.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}

	mainTileCount := func() int {
		viewer.gridMu.Lock()
		defer viewer.gridMu.Unlock()
		if g := viewer.grids[""]; g != nil {
			return len(g.Tiles())
		}
		return 0
	}
	winCount := func() int {
		viewer.gridMu.Lock()
		defer viewer.gridMu.Unlock()
		return len(viewer.wins)
	}
	waitFor(t, "all 3 streams in main window", 20*time.Second, func() bool {
		return winCount() == 1 && mainTileCount() == 3
	})
	aliceID := member(viewer, "alice").ID
	bobID := member(viewer, "bob").ID

	viewer.gridMu.Lock()
	gw := viewer.wins[""].(*fakeWindow)
	viewer.gridMu.Unlock()
	if gw.Title() != "Bellion Share" || !gw.intercept.Load() {
		t.Fatalf("unexpected main window: title=%q intercept=%v", gw.Title(), gw.intercept.Load())
	}

	// Closing the main window prompts "Leave the session?" without destroying it.
	gw.userClose()
	waitFor(t, "leave prompt set", 5*time.Second, func() bool {
		return viewer.State().LeavePrompt && prompts.Load() >= 1
	})
	if gw.destroyed.Load() || winCount() != 1 {
		t.Fatalf("main window destroyed on close request: destroyed=%v wins=%d", gw.destroyed.Load(), winCount())
	}
	viewer.DismissLeavePrompt()
	if viewer.State().LeavePrompt {
		t.Fatal("LeavePrompt not cleared by DismissLeavePrompt")
	}

	// Pop out alice: 2 windows open; closing alice's pop-out returns her to main.
	if err := viewer.PopOut(aliceID); err != nil {
		t.Fatal(err)
	}
	if winCount() != 2 || mainTileCount() != 2 || member(viewer, "alice").FeedState != feeds.PoppedOut {
		t.Fatalf("after PopOut(alice): wins=%d mainTiles=%d state=%s", winCount(), mainTileCount(), member(viewer, "alice").FeedState)
	}
	viewer.gridMu.Lock()
	aliceWin := viewer.wins[aliceID].(*fakeWindow)
	viewer.gridMu.Unlock()
	if aliceWin.intercept.Load() || !strings.HasPrefix(aliceWin.Title(), "Bellion Share: alice") {
		t.Fatalf("unexpected pop-out window: title=%q intercept=%v", aliceWin.Title(), aliceWin.intercept.Load())
	}
	aliceWin.userClose()
	waitFor(t, "alice returns to main window", 5*time.Second, func() bool {
		return winCount() == 1 && mainTileCount() == 3 && member(viewer, "alice").FeedState == feeds.InMain
	})

	// Pop out bob, then click eye on bob's pop-out: closes pop-out and stops watching;
	// turning eye back on puts bob in the main window (not a pop-out).
	if err := viewer.PopOut(bobID); err != nil {
		t.Fatal(err)
	}
	viewer.control(bobID, media.ControlEye, 0)
	waitFor(t, "bob stops sending video", 5*time.Second, func() bool {
		return member(sharers[1], "viewer").Layer == "off"
	})
	if winCount() != 1 || member(viewer, "bob").FeedState != feeds.NotWatched {
		t.Fatalf("after eye on pop-out: wins=%d state=%s", winCount(), member(viewer, "bob").FeedState)
	}
	if err := viewer.ToggleWatch(bobID); err != nil {
		t.Fatal(err)
	}
	if winCount() != 1 || mainTileCount() != 3 || member(viewer, "bob").FeedState != feeds.InMain {
		t.Fatalf("expected bob back in main window: wins=%d mainTiles=%d state=%s", winCount(), mainTileCount(), member(viewer, "bob").FeedState)
	}

	// Pin alice, then pin bob (single-pin invariant).
	if err := viewer.Pin(aliceID); err != nil {
		t.Fatal(err)
	}
	if member(viewer, "alice").FeedState != feeds.Pinned {
		t.Fatalf("alice not pinned: %s", member(viewer, "alice").FeedState)
	}
	if err := viewer.Pin(bobID); err != nil {
		t.Fatal(err)
	}
	if member(viewer, "bob").FeedState != feeds.Pinned || member(viewer, "alice").FeedState != feeds.InMain {
		t.Fatalf("pin switch failed: alice=%s bob=%s", member(viewer, "alice").FeedState, member(viewer, "bob").FeedState)
	}

	// Stop watching everyone (including own preview): main window closes silently
	// without LeavePrompt, and reopens automatically when watching resumes.
	for _, id := range []string{aliceID, bobID, feeds.Self} {
		if err := viewer.ToggleWatch(id); err != nil {
			t.Fatal(err)
		}
	}
	if winCount() != 0 || viewer.State().LeavePrompt {
		t.Fatalf("expected 0 windows and no leavePrompt when nothing watched: wins=%d prompt=%v", winCount(), viewer.State().LeavePrompt)
	}
	if err := viewer.ToggleWatch(aliceID); err != nil {
		t.Fatal(err)
	}
	if winCount() != 1 || mainTileCount() != 1 {
		t.Fatalf("expected main window reopened with alice: wins=%d mainTiles=%d", winCount(), mainTileCount())
	}
}

// When AutoWatch is off, a new remote sharer shows a closed eye and a short
// notice in State; no window opens and no video plays until clicked, while
// remote audio plays independently of video state.
func TestAutoWatchOff(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	ctx := context.Background()

	off := false
	viewer, err := Create(ctx, Config{Server: ws, Name: "viewer", View: media.ViewGrid, AutoWatch: &off, Log: quiet}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	viewer.gridMu.Lock()
	viewer.gridView = media.ViewCount
	viewer.openWindow = func(title string, _, _ int) (media.Window, error) {
		return newFakeWindow(title), nil
	}
	viewer.gridMu.Unlock()

	alice, err := Join(ctx, Config{Server: ws, Name: "alice", View: media.ViewCount, Log: quiet}, viewer.sig.Welcome.Room, viewer.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	if err := alice.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	if err := alice.SetAudio(audio.Selection{Mode: "test"}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "unwatched share notice", 10*time.Second, func() bool {
		st := viewer.State()
		m := member(viewer, "alice")
		return st.ShareNotice == "alice started sharing" && m.Eye == feeds.EyeClosed && m.FeedState == feeds.NotWatched
	})
	waitFor(t, "alice capped to off while audio plays", 15*time.Second, func() bool {
		return member(alice, "viewer").Layer == "off" && member(viewer, "alice").Audio > 5
	})
	time.Sleep(500 * time.Millisecond)
	viewer.gridMu.Lock()
	wins := len(viewer.wins)
	viewer.gridMu.Unlock()
	if m := member(viewer, "alice"); wins != 0 || m.Frames != 0 || m.Audio == 0 {
		t.Fatalf("unexpected playback with AutoWatch off: wins=%d frames=%d audio=%d", wins, m.Frames, m.Audio)
	}

	if err := viewer.ToggleWatch(member(viewer, "alice").ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "watching alice after clicking eye", 10*time.Second, func() bool {
		st := viewer.State()
		m := member(viewer, "alice")
		viewer.gridMu.Lock()
		open := len(viewer.wins) == 1
		viewer.gridMu.Unlock()
		return open && st.ShareNotice == "" && m.Eye == feeds.EyeOpen && m.Audio > 5 && member(alice, "viewer").Layer != "off"
	})
}

// A watched sharer who stops: their tile shows "NAME stopped sharing", fades
// out, and closes the main window if nothing else is watched; sharing again
// reopens the main window live. Switching source or resolution isn't stopping.
func TestStoppedSharing(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	cfg := func(n string, v media.ViewKind) Config { return Config{Server: ws, Name: n, View: v, Log: quiet} }
	ctx := context.Background()
	viewer, err := Create(ctx, cfg("viewer", media.ViewGrid), "")
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	viewer.gridMu.Lock()
	viewer.gridView = media.ViewCount
	viewer.openWindow = func(title string, _, _ int) (media.Window, error) {
		return newFakeWindow(title), nil
	}
	viewer.gridMu.Unlock()
	alice, err := Join(ctx, cfg("alice", media.ViewCount), viewer.sig.Welcome.Room, viewer.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	if err := alice.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice in the viewer", 10*time.Second, func() bool { return member(viewer, "alice").ID != "" })
	id := member(viewer, "alice").ID
	tile := func() (media.Tile, *fakeWindow) {
		viewer.gridMu.Lock()
		defer viewer.gridMu.Unlock()
		w, _ := viewer.wins[""].(*fakeWindow)
		if g := viewer.grids[""]; g != nil {
			for _, t := range g.Tiles() {
				if t.ID == id {
					return t, w
				}
			}
		}
		return media.Tile{}, w
	}
	live := func() bool { t, _ := tile(); return t.ID != "" && !t.Stopped }
	waitFor(t, "alice's tile", 20*time.Second, live)

	if err := alice.SetResolution(media.Res1080); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("DISPLAY") != "" {
		if err := alice.SetSource(media.SourceScreen); err != nil {
			t.Fatal(err)
		}
		if err := alice.SetSource(media.SourceTest); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Log("no DISPLAY: not switching to the screen")
	}
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if tl, _ := tile(); tl.Stopped {
			t.Fatal("a switch of source or size showed alice as stopped")
		}
	}
	waitFor(t, "alice's tile after the switch", 10*time.Second, live)

	if err := alice.SetSource("none"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stopped tile", 3*time.Second, func() bool { t, _ := tile(); return t.Stopped })
	_, w := tile()
	if w == nil {
		t.Fatal("no window for the stopped tile")
	}
	waitFor(t, "tile gone and window auto-closed", 8*time.Second, func() bool { t, w := tile(); return t.ID == "" && w == nil })
	if !w.destroyed.Load() || viewer.State().LeavePrompt {
		t.Fatalf("main window after last stopped tile faded: destroyed=%v leavePrompt=%v", w.destroyed.Load(), viewer.State().LeavePrompt)
	}

	if err := alice.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice live again in reopened main window", 10*time.Second, live)
}

// M12.1: starting a share, then switching to a source that fails rolls State
// back to not sharing (closing preview and notifying peers), and the very next
// successful share plays SoundSelfLive again.
func TestFailedSourceSwitchRollsBackSharingAndNextShareChimes(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	ctx := context.Background()

	s, err := Create(ctx, Config{Server: ws, Name: "alice", View: media.ViewGrid, Log: quiet}, "")
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

	bob, err := Join(ctx, Config{Server: ws, Name: "bob", View: media.ViewCount, Log: quiet}, s.sig.Welcome.Room, "")
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()

	if err := s.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice live with preview window and SoundSelfLive", 15*time.Second, func() bool {
		st := s.State()
		s.gridMu.Lock()
		wins := len(s.wins)
		s.gridMu.Unlock()
		return st.Source == string(media.SourceTest) &&
			st.SelfState == feeds.InMain &&
			st.SelfEye == feeds.EyeOpen &&
			wins == 1 &&
			member(bob, "alice").Sharing &&
			s.soundGate.Count(media.SoundSelfLive) == 1
	})

	// Switch to an invalid source while already sharing -> must fail and roll back.
	if err := s.SetSource(media.SourceKind("bogus-nonexistent-source")); err == nil {
		t.Fatal("expected SetSource with invalid kind to fail")
	}
	st := s.State()
	s.gridMu.Lock()
	winsAfterFail := len(s.wins)
	s.gridMu.Unlock()
	if st.Source != "" || st.Encoder != "" || st.SelfState != feeds.Gone || st.SelfEye != feeds.EyeGrey || winsAfterFail != 0 {
		t.Fatalf("after failed source switch: Source=%q Encoder=%q SelfState=%s SelfEye=%s wins=%d",
			st.Source, st.Encoder, st.SelfState, st.SelfEye, winsAfterFail)
	}
	waitFor(t, "bob sees alice stopped sharing after failed source switch", 5*time.Second, func() bool {
		return !member(bob, "alice").Sharing
	})

	// Immediate next successful SetSource must play SoundSelfLive again.
	if err := s.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "next SetSource plays SoundSelfLive again and reopens preview", 10*time.Second, func() bool {
		s.gridMu.Lock()
		wins := len(s.wins)
		s.gridMu.Unlock()
		return s.State().SelfState == feeds.InMain && wins == 1 &&
			s.soundGate.Count(media.SoundSelfLive) == 2
	})
}

// M12.2: remote audio plays independently of video state when AutoWatch is off:
// both when a peer shares audio only and when a peer stops video while keeping
// audio, respecting mute and volume.
func TestAudioOnlyAndStoppedVideoAudioWithAutoWatchOff(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	ctx := context.Background()

	off := false
	viewer, err := Create(ctx, Config{Server: ws, Name: "viewer", View: media.ViewCount, AutoWatch: &off, Log: quiet}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()

	// Case 1: alice shares audio ONLY (no video) while viewer has AutoWatch off.
	alice, err := Join(ctx, Config{Server: ws, Name: "alice", View: media.ViewCount, Log: quiet}, viewer.sig.Welcome.Room, "")
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	if err := alice.SetAudio(audio.Selection{Mode: "test"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "viewer hears audio-only sharer alice with AutoWatch off", 10*time.Second, func() bool {
		m := member(viewer, "alice")
		return !m.Sharing && m.Eye == feeds.EyeGrey && m.Audio > 15
	})

	aliceID := member(viewer, "alice").ID
	if err := viewer.SetPeer(aliceID, "auto", 0.5, true); err != nil {
		t.Fatal(err)
	}
	if m := member(viewer, "alice"); !m.Muted || m.Volume != 0.5 {
		t.Fatalf("mute/volume not applied on audio-only sharer: %+v", m)
	}
	if err := viewer.SetPeer(aliceID, "auto", 1.4, false); err != nil {
		t.Fatal(err)
	}
	aBefore := member(viewer, "alice").Audio
	waitFor(t, "alice audio keeps playing after unmute", 5*time.Second, func() bool {
		m := member(viewer, "alice")
		return !m.Muted && m.Volume == 1.4 && m.Audio > aBefore+5
	})

	// Case 2: bob shares video + audio, viewer watches bob, then bob stops video
	// while keeping audio (with AutoWatch still off on viewer).
	bob, err := Join(ctx, Config{Server: ws, Name: "bob", View: media.ViewCount, Log: quiet}, viewer.sig.Welcome.Room, "")
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()
	if err := bob.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	if err := bob.SetAudio(audio.Selection{Mode: "test"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bob sharing video+audio", 10*time.Second, func() bool {
		return member(viewer, "bob").Sharing && member(viewer, "bob").Audio > 5
	})
	bobID := member(viewer, "bob").ID
	if err := viewer.ToggleWatch(bobID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "viewer receives bob video+audio", 10*time.Second, func() bool {
		m := member(viewer, "bob")
		return m.Frames > 5 && m.Audio > 15
	})
	if err := bob.SetSource("none"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bob video stopped", 5*time.Second, func() bool {
		return !member(viewer, "bob").Sharing
	})
	bobAudioAfterStop := member(viewer, "bob").Audio
	waitFor(t, "viewer continues hearing bob after bob stops video", 5*time.Second, func() bool {
		return member(viewer, "bob").Audio > bobAudioAfterStop+15
	})
}

// M12.3: when the main streaming window closes and the sink can't intercept
// the close, LeavePrompt is shown and changed() -> regrid() does not reopen
// the window until the user opens video again or a new share starts.
func TestNonInterceptedMainWindowCloseDoesNotReopen(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	ctx := context.Background()

	var prompts atomic.Int32
	viewer, err := Create(ctx, Config{Server: ws, Name: "viewer", View: media.ViewGrid, Log: quiet}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	viewer.OnLeavePrompt(func() { prompts.Add(1) })
	viewer.gridMu.Lock()
	viewer.gridView = media.ViewCount
	viewer.openWindow = func(title string, _, _ int) (media.Window, error) {
		w := newFakeWindow(title)
		w.ignoreIntercept = true // simulate sink/WM that cannot intercept close
		return w, nil
	}
	viewer.gridMu.Unlock()

	alice, err := Join(ctx, Config{Server: ws, Name: "alice", View: media.ViewCount, Log: quiet}, viewer.sig.Welcome.Room, "")
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	if err := alice.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}

	winCount := func() int {
		viewer.gridMu.Lock()
		defer viewer.gridMu.Unlock()
		return len(viewer.wins)
	}
	waitFor(t, "main window opens for alice", 15*time.Second, func() bool {
		return winCount() == 1
	})
	aliceID := member(viewer, "alice").ID
	viewer.gridMu.Lock()
	w1 := viewer.wins[""].(*fakeWindow)
	viewer.gridMu.Unlock()

	// User closes the non-intercepted main window: LeavePrompt is shown and
	// alice is marked NotWatched so the window stays closed.
	w1.userClose()
	waitFor(t, "main window closed, leave prompt shown, and alice marked NotWatched", 5*time.Second, func() bool {
		return winCount() == 0 &&
			member(viewer, "alice").FeedState == feeds.NotWatched &&
			viewer.State().LeavePrompt &&
			prompts.Load() == 1
	})
	// Subsequent changed() / regrid() calls must neither reopen the main window
	// nor clear LeavePrompt before the user responds.
	viewer.changed()
	viewer.regrid()
	time.Sleep(300 * time.Millisecond)
	if winCount() != 0 || member(viewer, "alice").Eye != feeds.EyeClosed || !viewer.State().LeavePrompt {
		t.Fatalf("after changed/regrid: wins=%d eye=%s leavePrompt=%v",
			winCount(), member(viewer, "alice").Eye, viewer.State().LeavePrompt)
	}
	viewer.DismissLeavePrompt()
	if viewer.State().LeavePrompt {
		t.Fatal("LeavePrompt not cleared by DismissLeavePrompt")
	}

	// Opening video again via the eye reopens the main window.
	if err := viewer.ToggleWatch(aliceID); err != nil {
		t.Fatal(err)
	}
	if winCount() != 1 || member(viewer, "alice").FeedState != feeds.InMain {
		t.Fatalf("expected main window reopened via eye: wins=%d state=%s", winCount(), member(viewer, "alice").FeedState)
	}

	// Close again: prompt fires again, then a brand-new share from bob reopens the main window.
	viewer.gridMu.Lock()
	w2 := viewer.wins[""].(*fakeWindow)
	viewer.gridMu.Unlock()
	w2.userClose()
	waitFor(t, "main window closed a second time with leave prompt", 5*time.Second, func() bool {
		return winCount() == 0 && viewer.State().LeavePrompt && prompts.Load() == 2
	})
	viewer.DismissLeavePrompt()

	bob, err := Join(ctx, Config{Server: ws, Name: "bob", View: media.ViewCount, Log: quiet}, viewer.sig.Welcome.Room, "")
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()
	if err := bob.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "new share from bob reopens main window", 15*time.Second, func() bool {
		return winCount() == 1 && member(viewer, "bob").FeedState == feeds.InMain
	})
}

// M12.4: a video frame already in flight after the user dismisses a
// "stopped sharing" tile must not fire ShareStart again.
func TestDismissedStoppedTileIgnoresInFlightFrame(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	ctx := context.Background()

	viewer, err := Create(ctx, Config{Server: ws, Name: "viewer", View: media.ViewGrid, Log: quiet}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	viewer.gridMu.Lock()
	viewer.gridView = media.ViewCount
	viewer.openWindow = func(title string, _, _ int) (media.Window, error) {
		return newFakeWindow(title), nil
	}
	viewer.gridMu.Unlock()

	alice, err := Join(ctx, Config{Server: ws, Name: "alice", View: media.ViewCount, Log: quiet}, viewer.sig.Welcome.Room, "")
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	if err := alice.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice live in viewer", 15*time.Second, func() bool {
		return member(viewer, "alice").FeedState == feeds.InMain
	})
	aliceID := member(viewer, "alice").ID

	if err := alice.SetSource("none"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice tile in StoppedFading", 5*time.Second, func() bool {
		return member(viewer, "alice").FeedState == feeds.StoppedFading
	})

	// User dismisses the "alice stopped sharing" tile via the eye.
	if err := viewer.ToggleWatch(aliceID); err != nil {
		t.Fatal(err)
	}
	if st := member(viewer, "alice").FeedState; st != feeds.Gone {
		t.Fatalf("expected Gone after dismissing stopped tile, got %s", st)
	}
	viewer.soundGate.Reset()

	// Deliver in-flight video frames within stoppedGrace: must NOT fire ShareStart.
	inFlightAU := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84}
	for range 3 {
		viewer.pushVideo(aliceID, inFlightAU)
	}
	time.Sleep(150 * time.Millisecond)
	viewer.gridMu.Lock()
	wins := len(viewer.wins)
	viewer.gridMu.Unlock()
	if st := member(viewer, "alice").FeedState; st != feeds.Gone || wins != 0 || len(viewer.PlayedSounds()) != 0 {
		t.Fatalf("dismissed tile came back on in-flight frame: state=%s wins=%d sounds=%v",
			st, wins, viewer.PlayedSounds())
	}
	waitFor(t, "dismissed stopped entry cleaned up after grace", 4*time.Second, func() bool {
		viewer.mu.Lock()
		defer viewer.mu.Unlock()
		_, ok := viewer.stopped[aliceID]
		return !ok
	})

	// Also verify ShareStop from NotWatched -> Gone arms cleanup of stopped[id].
	viewer.gotSending(aliceID, 720, 720, false)
	if err := viewer.ToggleWatch(aliceID); err != nil {
		t.Fatal(err)
	}
	viewer.gotSending(aliceID, 0, 0, false)
	waitFor(t, "NotWatched->Gone stopped entry cleaned up after grace", 4*time.Second, func() bool {
		viewer.mu.Lock()
		defer viewer.mu.Unlock()
		_, ok := viewer.stopped[aliceID]
		return !ok
	})
}

// M12.6: regrid calls SetTitle and SetInterceptClose only on change, and rapid
// gotSending reports coalesce background applyAutoQuality goroutines.
func TestGotSendingCoalescesAutoQualityAndWindowCallsOnChange(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	ctx := context.Background()

	viewer, err := Create(ctx, Config{Server: ws, Name: "viewer", View: media.ViewGrid, Log: quiet}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	viewer.gridMu.Lock()
	viewer.gridView = media.ViewCount
	viewer.openWindow = func(title string, _, _ int) (media.Window, error) {
		return newFakeWindow(title), nil
	}
	viewer.gridMu.Unlock()

	alice, err := Join(ctx, Config{Server: ws, Name: "alice", View: media.ViewCount, Log: quiet}, viewer.sig.Welcome.Room, "")
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	if err := alice.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "main window open for alice", 15*time.Second, func() bool {
		viewer.gridMu.Lock()
		defer viewer.gridMu.Unlock()
		return viewer.wins[""] != nil
	})
	aliceID := member(viewer, "alice").ID

	viewer.gridMu.Lock()
	w := viewer.wins[""].(*fakeWindow)
	viewer.gridMu.Unlock()
	if gotT, gotI := w.titleCalls.Load(), w.interceptCalls.Load(); gotT != 1 || gotI != 1 {
		t.Fatalf("initial window calls: SetTitle=%d SetInterceptClose=%d, want 1/1", gotT, gotI)
	}

	for range 8 {
		viewer.regrid()
	}
	if gotT, gotI := w.titleCalls.Load(), w.interceptCalls.Load(); gotT != 1 || gotI != 1 {
		t.Fatalf("unchanged regrid repeated window calls: SetTitle=%d SetInterceptClose=%d, want 1/1", gotT, gotI)
	}

	// Pop out alice: reuses window w for pop-out -> both title and intercept change once.
	if err := viewer.PopOut(aliceID); err != nil {
		t.Fatal(err)
	}
	if gotT, gotI := w.titleCalls.Load(), w.interceptCalls.Load(); gotT != 2 || gotI != 2 {
		t.Fatalf("after reusing window as pop-out: SetTitle=%d SetInterceptClose=%d, want 2/2", gotT, gotI)
	}

	// Rename alice while popped out -> only SetTitle changes, SetInterceptClose stays 2.
	if err := alice.Rename("alicia"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pop-out window title updated on rename", 5*time.Second, func() bool {
		return strings.HasPrefix(w.Title(), "Bellion Share: alicia")
	})
	if gotT, gotI := w.titleCalls.Load(), w.interceptCalls.Load(); gotT != 3 || gotI != 2 {
		t.Fatalf("after rename: SetTitle=%d SetInterceptClose=%d, want 3/2", gotT, gotI)
	}

	// Coalescing check: hold autoQMu briefly while firing 200 gotSending reports
	// that request autoQ; goroutine count must stay bounded (coalesced to 1 worker).
	baseGoroutines := runtime.NumGoroutine()
	viewer.autoQMu.Lock()
	for range 200 {
		viewer.mu.Lock()
		if m := viewer.members[aliceID]; m != nil {
			m.asked = -1
		}
		viewer.mu.Unlock()
		viewer.gotSending(aliceID, 0, 720, false)
	}
	spawnedWhileHeld := runtime.NumGoroutine() - baseGoroutines
	viewer.autoQMu.Unlock()
	if spawnedWhileHeld > 20 {
		t.Fatalf("gotSending did not coalesce applyAutoQuality goroutines: +%d goroutines for 200 reports", spawnedWhileHeld)
	}
	waitFor(t, "coalesced autoQ worker finishes", 5*time.Second, func() bool {
		return viewer.autoQState.Load() == 0
	})
}

// Rebuilding a non-embedded grid (openWindow == nil, !g.InWindow()) N times
// must not leak watchGridWindow goroutines.
func TestNonEmbeddedGridRebuildNoGoroutineLeak(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	ctx := context.Background()

	playOff := false
	viewer, err := Create(ctx, Config{Server: ws, Name: "viewer", View: media.ViewGrid, PlaySounds: &playOff, Log: quiet}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	viewer.gridMu.Lock()
	viewer.gridView = media.ViewCount
	viewer.openWindow = nil
	viewer.gridMu.Unlock()

	if err := viewer.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "non-embedded main grid running", 10*time.Second, func() bool {
		viewer.gridMu.Lock()
		defer viewer.gridMu.Unlock()
		g := viewer.grids[""]
		return g != nil && !g.InWindow()
	})

	// Warm up two rebuilds and let background goroutines settle.
	for range 2 {
		if err := viewer.Pin(feeds.Self); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "regrid settled", 3*time.Second, func() bool {
		return viewer.regridState.Load() == 0 && viewer.autoQState.Load() == 0
	})
	time.Sleep(150 * time.Millisecond)
	base := runtime.NumGoroutine()

	const rebuilds = 15
	for range rebuilds {
		if err := viewer.Pin(feeds.Self); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "watchGridWindow goroutines exit on Stop", 5*time.Second, func() bool {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		watchers := strings.Count(string(buf[:n]), "watchGridWindow(")
		return watchers == 1 && runtime.NumGoroutine() <= base+3
	})
}

// An async rollback in setRenditions must not overwrite mesh top with 0 when a
// concurrent SetSource succeeds in the gap before SetTop(0) runs.
func TestSetRenditionsRollbackDoesNotClobberConcurrentSetSource(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	ctx := context.Background()

	alice, err := Create(ctx, Config{Server: ws, Name: "alice", View: media.ViewCount, Log: quiet}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	if err := alice.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}

	bob, err := Join(ctx, Config{Server: ws, Name: "bob", View: media.ViewCount, Log: quiet}, alice.sig.Welcome.Room, "")
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()
	waitFor(t, "bob sees alice at 720p", 15*time.Second, func() bool {
		return member(bob, "alice").Recv == 720 && member(bob, "alice").FeedState == feeds.InMain
	})

	ready := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	alice.mu.Lock()
	alice.srcKind = "window:0xdeadbeef" // force setRenditions' startSource to fail
	alice.rendRollbackHook = func(run func()) {
		close(ready)
		<-release
		run()
		close(done)
	}
	alice.mu.Unlock()

	if err := alice.setRenditions([]int{360}); err == nil {
		t.Fatal("expected setRenditions with invalid source to fail")
	}
	<-ready

	alice.mu.Lock()
	alice.rendRollbackHook = nil
	alice.mu.Unlock()

	// Succeeding SetSource runs in the gap before the async rollback calls SetTop(0).
	if err := alice.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done

	// Give any erroneously sent Sending{Top: 0} time to arrive, then verify
	// alice's mesh top stayed 720 and frames continue flowing to bob.
	time.Sleep(250 * time.Millisecond)
	if st := alice.State(); st.Source != string(media.SourceTest) {
		t.Fatalf("alice Source = %q, want %q", st.Source, media.SourceTest)
	}
	if m := member(bob, "alice"); !m.Sharing || m.Recv != 720 || m.FeedState != feeds.InMain {
		t.Fatalf("stale rollback clobbered alice's share: sharing=%v recv=%d feedState=%s",
			m.Sharing, m.Recv, m.FeedState)
	}
	framesGrow(t, "bob still receives alice frames after stale rollback", bob, "alice")
}
