package session

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"blite/internal/media"
	"blite/internal/server"
)

// The video window buttons toggle the same settings as the main
// window: mute their audio, and pause their video (alice stops sending).
func TestVideoWindowButtons(t *testing.T) {
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

	b.control(alice, media.ControlMute, 0)
	if m := member(b, "alice"); !m.Muted {
		t.Fatalf("mute button: %+v", m)
	}
	b.control(alice, media.ControlPause, 0)
	if m := member(b, "alice"); m.Quality != "off" || !m.Muted {
		t.Fatalf("pause button: %+v", m)
	}
	waitFor(t, "alice stops sending video to bob", 5*time.Second, func() bool { return member(a, "bob").Layer == "off" })
	b.control(alice, media.ControlPause, 0)
	b.control(alice, media.ControlMute, 0)
	if m := member(b, "alice"); m.Quality != "auto" || m.Muted {
		t.Fatalf("resume and unmute: %+v", m)
	}
	waitFor(t, "alice sends video again", 5*time.Second, func() bool { return member(a, "bob").Layer != "off" })
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
	title     string
	closed    chan struct{}
	destroyed bool
	minimized bool
}

func (w *fakeWindow) Handle() uintptr         { return 0 }
func (w *fakeWindow) Closed() <-chan struct{} { return w.closed }
func (w *fakeWindow) SetTitle(t string)       { w.title = t }
func (w *fakeWindow) PointerInside() bool     { return true }
func (w *fakeWindow) Minimized() bool         { return w.minimized }
func (w *fakeWindow) Destroy()                { w.destroyed = true }

// Video windows are ours: a layout switch keeps them open, a
// window the user closed stays closed (and its video off) until shown again.
func TestOwnWindows(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	cfg := func(n string, v media.ViewKind) Config { return Config{Server: ws, Name: n, View: v, Log: quiet} }
	ctx := context.Background()
	var opened []*fakeWindow
	var omu sync.Mutex
	viewer, err := Create(ctx, cfg("viewer", media.ViewGrid), "")
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	viewer.gridMu.Lock()
	viewer.gridView = media.ViewCount
	viewer.gridMu.Unlock()
	viewer.openWindow = func(title string, _, _ int) (media.Window, error) {
		omu.Lock()
		defer omu.Unlock()
		w := &fakeWindow{title: title, closed: make(chan struct{})}
		opened = append(opened, w)
		return w, nil
	}
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
	count := func() (open int) {
		viewer.gridMu.Lock()
		defer viewer.gridMu.Unlock()
		return len(viewer.wins)
	}
	waitFor(t, "a window each", 20*time.Second, func() bool { return count() == 2 })
	if err := viewer.SetLayout(Layout{Mode: LayoutGrid}); err != nil {
		t.Fatal(err)
	}
	omu.Lock()
	n := len(opened)
	omu.Unlock()
	if count() != 1 || n != 2 {
		t.Fatalf("grid: %d windows open, %d opened in all; want 1 kept of the 2", count(), n)
	}
	viewer.gridMu.Lock()
	gw := viewer.wins[""].(*fakeWindow)
	viewer.gridMu.Unlock()
	if gw.destroyed || gw.title != "Bellion Share" {
		t.Fatalf("grid window %+v", gw)
	}
	close(gw.closed) // the user closes it
	waitFor(t, "closed noted", 5*time.Second, func() bool { return viewer.State().GridClosed })
	waitFor(t, "alice stops sending", 10*time.Second, func() bool { return member(sharers[0], "viewer").Layer == "off" })
	if err := viewer.ShowWindow(""); err != nil {
		t.Fatal(err)
	}
	if count() != 1 || viewer.State().GridClosed {
		t.Fatalf("shown again: %d windows, closed %v", count(), viewer.State().GridClosed)
	}
	waitFor(t, "alice sends again", 10*time.Second, func() bool { l := member(sharers[0], "viewer").Layer; return l != "off" && l != "" })

	// The grid and our preview closed: "*" reopens both, "self" the preview.
	if err := viewer.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	if err := viewer.SetLayout(Layout{Mode: LayoutGrid, Preview: PreviewWindow}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "preview", 5*time.Second, func() bool { return viewer.PreviewFrames() >= 0 })
	closePreview := func() {
		t.Helper()
		viewer.gridMu.Lock()
		viewer.pvClosed = true // as when the user closes it
		viewer.gridMu.Unlock()
		viewer.regrid(false)
		if st := viewer.State(); !st.PreviewClosed || viewer.PreviewFrames() != -1 {
			t.Fatalf("preview closed: noted %v, %d frames", st.PreviewClosed, viewer.PreviewFrames())
		}
	}
	closePreview()
	viewer.gridMu.Lock()
	gw = viewer.wins[""].(*fakeWindow)
	viewer.gridMu.Unlock()
	close(gw.closed)
	waitFor(t, "grid closed noted", 5*time.Second, func() bool { return viewer.State().GridClosed })
	if err := viewer.ShowWindow("*"); err != nil {
		t.Fatal(err)
	}
	if st := viewer.State(); count() != 1 || st.GridClosed || st.PreviewClosed || viewer.PreviewFrames() < 0 {
		t.Fatalf("all shown again: %d windows, grid closed %v, preview closed %v, %d preview frames", count(), st.GridClosed, st.PreviewClosed, viewer.PreviewFrames())
	}
	closePreview()
	if err := viewer.ShowWindow("self"); err != nil {
		t.Fatal(err)
	}
	if viewer.State().PreviewClosed || viewer.PreviewFrames() < 0 {
		t.Fatal("preview not shown again")
	}
}

// A sharer who stops: their tile says so, then goes; they come back live
// when they share again, and a window the user closed on them reopens.
// Switching what they share, or its size, isn't stopping.
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
	viewer.gridMu.Unlock()
	viewer.openWindow = func(title string, _, _ int) (media.Window, error) {
		return &fakeWindow{title: title, closed: make(chan struct{})}, nil
	}
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
		w, _ := viewer.wins[id].(*fakeWindow)
		if g := viewer.grids[id]; g != nil {
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
	waitFor(t, "tile gone", 8*time.Second, func() bool { t, w := tile(); return t.ID == "" && w == nil })
	if !w.destroyed {
		t.Fatal("alice's window stayed open without a tile")
	}

	if err := alice.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice live again", 10*time.Second, live)

	// The user closes her window; her next share opens it again.
	_, w = tile()
	close(w.closed)
	waitFor(t, "closed noted", 5*time.Second, func() bool { return member(viewer, "alice").WindowClosed })
	if err := alice.SetSource("none"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if err := alice.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "her window reopened", 10*time.Second, func() bool { return !member(viewer, "alice").WindowClosed && live() })
}
