package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"blite/internal/audio"
	"blite/internal/server"
	"blite/internal/session"
)

func TestQualityPresets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	a := New(session.Config{Name: "alice", View: "none", Log: quiet}, "none")
	if ps := a.State().(UIState).Share.Presets; len(ps) != 0 {
		t.Fatalf("no user presets at first, got %v", ps)
	}
	for _, bad := range []QualityPreset{{"480p", 30}, {"", 30}, {"1080p", 7}, {"1080p", 61}, {"1080p", 33}} {
		if err := a.AddQualityPreset(bad.Resolution, bad.FPS); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	for _, p := range []QualityPreset{{"1080p", 45}, {"720p", 15}, {"1080p", 45}, {"1080p", 60}} {
		if err := a.AddQualityPreset(p.Resolution, p.FPS); err != nil {
			t.Fatal(err)
		}
	}
	want := []QualityPreset{{"1080p", 45}, {"720p", 15}} // no duplicate, no copy of a built-in
	if ps := a.State().(UIState).Share.Presets; !slices.Equal(ps, want) {
		t.Fatalf("presets = %v, want %v", ps, want)
	}

	b := New(session.Config{Name: "alice", View: "none", Log: quiet}, "none")
	if ps := b.State().(UIState).Share.Presets; !slices.Equal(ps, want) {
		t.Fatalf("presets after restart = %v, want %v", ps, want)
	}
	if err := b.RemoveQualityPreset("1080p", 45); err != nil {
		t.Fatal(err)
	}
	c := New(session.Config{Name: "alice", View: "none", Log: quiet}, "none")
	if ps := c.State().(UIState).Share.Presets; !slices.Equal(ps, []QualityPreset{{"720p", 15}}) {
		t.Fatalf("presets after removing = %v", ps)
	}

	// A hand-edited file: bad, built-in and repeated entries are dropped.
	os.WriteFile(filepath.Join(dir, "blite", "settings.json"),
		[]byte(`{"presets":[{"resolution":"1080p","fps":30},{"resolution":"8K","fps":30},{"resolution":"720p","fps":25},{"resolution":"720p","fps":25}]}`), 0o600)
	if ps := loadPresets(); !slices.Equal(ps, []QualityPreset{{"720p", 25}}) {
		t.Fatalf("cleaned presets = %v", ps)
	}

	var many []QualityPreset
	for fps := 10; fps <= 55; fps += 5 {
		for _, r := range []string{"720p", "1080p"} {
			many, _ = addPreset(many, QualityPreset{r, fps})
		}
	}
	if len(many) != maxUserPresets {
		t.Fatalf("%d presets, want at most %d", len(many), maxUserPresets)
	}
}

// Picking a window turns sound on (its app's); picking the screen doesn't.
func TestSoundFollowsSource(t *testing.T) {
	fakeWindowPIDs(t, 4242)
	none := audio.Selection{Mode: "none"}
	system := audio.Selection{Mode: "system"}
	win := audio.Selection{Mode: "window"}
	for _, c := range []struct {
		src       string
		cur, want audio.Selection
	}{
		{"window:0x307a2", none, win},
		{"window:0x307a2", system, win},
		{"primary", none, none},
		{"primary", win, none},      // from a window back to the screen: off
		{"primary", system, system}, // the user turned it on for the screen
		{"monitor:0x10001", win, none},
		{"screen", audio.Selection{}, none},
	} {
		if got := audioForSource(c.src, c.cur); got.Mode != c.want.Mode {
			t.Errorf("%s after %q: %q, want %q", c.src, c.cur.Mode, got.Mode, c.want.Mode)
		}
	}

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	a := New(session.Config{Name: "alice", View: "none", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, "none")
	if err := a.SetSource("window:0x307a2"); err != nil {
		t.Fatal(err)
	}
	sh := a.State().(UIState).Share
	if sh.Audio.Mode != "window" || sh.SourceName == "0x307a2" || sh.SourceName == "window:0x307a2" {
		t.Fatalf("after picking a window: audio %q, name %q", sh.Audio.Mode, sh.SourceName)
	}
	if err := a.SetAudio("none", nil); err != nil { // the user can still turn it off
		t.Fatal(err)
	}
	if err := a.SetSource("primary"); err != nil {
		t.Fatal(err)
	}
	if m := a.State().(UIState).Share.Audio.Mode; m != "none" {
		t.Fatalf("entire screen: audio %q, want none", m)
	}
}

// Overlapping changes all land, on disk too.
func TestQualityPresetsConcurrent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	a := New(session.Config{Name: "alice", View: "none", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, "none")
	for fps := 15; fps <= 25; fps += 5 {
		for _, r := range []string{"720p", "1080p"} {
			if err := a.AddQualityPreset(r, fps); err != nil {
				t.Fatal(err)
			}
		}
	}
	var wg sync.WaitGroup
	for fps := 15; fps <= 25; fps += 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.RemoveQualityPreset("1080p", fps)
		}()
	}
	wg.Wait()
	want := []QualityPreset{{"720p", 15}, {"720p", 20}, {"720p", 25}}
	if ps := a.State().(UIState).Share.Presets; !slices.Equal(ps, want) {
		t.Fatalf("presets = %v, want %v", ps, want)
	}
	if ps := loadPresets(); !slices.Equal(ps, want) {
		t.Fatalf("on disk = %v, want %v", ps, want)
	}
}

// A window's title is for the UI only: the state in "Send logs" leaves it out.
func TestSourceNameNotInJSON(t *testing.T) {
	fakeWindowPIDs(t, 4242)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	a := New(session.Config{Name: "alice", View: "none", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, "none")
	a.SetSource("window:0x307a2")
	a.mu.Lock()
	a.srcInfo = sourceInfo{"window:0x307a2", "Secret plans.docx - Word", 4242}
	a.mu.Unlock()
	st := a.State().(UIState)
	if st.Share.SourceName != "Secret plans.docx - Word" {
		t.Fatalf("SourceName = %q", st.Share.SourceName)
	}
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), "Secret plans") {
		t.Fatalf("window title in the JSON state: %s", b)
	}
}

// Go live applies the home source again: that must keep the sound the user
// chose for the window (BLITE M15 review).
func TestGoLiveKeepsSoundChoice(t *testing.T) {
	fakeWindowPIDs(t, 4242)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	const win = "window:0x307a2"
	for _, c := range []struct {
		mode   string
		apps   []string
		follow bool
	}{
		{"window", nil, true}, // the default after picking a window
		{"none", nil, false},
		{"system", nil, false},
		{"apps", []string{"player#4242"}, false},
	} {
		t.Run(c.mode, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			t.Setenv("XDG_CONFIG_HOME", dir)
			a := New(session.Config{Server: ws, Name: "alice", View: "none", Log: quiet}, "none")
			defer a.Shutdown()
			if err := a.SetSource(win); err != nil {
				t.Fatal(err)
			}
			if m := a.State().(UIState).Share.Audio.Mode; m != "window" {
				t.Fatalf("picking a window: audio %q, want window", m)
			}
			if c.mode != "window" {
				if err := a.SetAudio(c.mode, c.apps); err != nil {
					t.Fatal(err)
				}
			}
			// What the lobby's "Go live and copy link" does.
			if err := a.Create(context.Background(), "", true); err != nil {
				t.Fatal(err)
			}
			a.SetSource(win) // no such window here: fails to capture, which doesn't matter
			a.mu.Lock()
			got, s := a.audio, a.sess
			a.mu.Unlock()
			if got.Mode != c.mode || !slices.Equal(got.Apps, c.apps) {
				t.Fatalf("audio after go live = %+v, want %s %v", got, c.mode, c.apps)
			}
			if s.FollowsWindowAudio() != c.follow {
				t.Fatalf("session follows the window's audio = %v, want %v", !c.follow, c.follow)
			}
		})
	}
}

// One rule before and during a call: another window turns sound on, a
// screen turns off sound that followed a window and keeps a choice made.
func TestPickSourceInCall(t *testing.T) {
	fakeWindowPIDs(t, 4242)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	a := New(session.Config{Server: "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws", Name: "alice", View: "none", Log: quiet}, "none")
	defer a.Shutdown()
	if err := a.Create(context.Background(), "", true); err != nil {
		t.Fatal(err)
	}
	mode := func() string {
		a.mu.Lock()
		defer a.mu.Unlock()
		return fmt.Sprint(a.audio.Mode, " ", a.sess.FollowsWindowAudio())
	}
	steps := []struct{ src, audio, want string }{
		{"window:0x1", "", "window true"},
		{"primary", "", "none false"},         // followed the window: off
		{"primary", "system", "system false"}, // the user turns it on
		{"window:0x2", "", "window true"},     // another window: its app
		{"window:0x2", "none", "none false"},  // turned off
		{"window:0x2", "", "none false"},      // the same window again: still off
		{"primary", "system", "system false"}, // on for the screen
		{"screen", "", "system false"},        // another screen keeps it
	}
	for i, st := range steps {
		a.SetSource(st.src)
		if st.audio != "" {
			a.SetAudio(st.audio, nil)
		}
		if got := mode(); got != st.want {
			t.Fatalf("step %d (%s, %q): %q, want %q", i, st.src, st.audio, got, st.want)
		}
	}
}

// Saves from several goroutines (the sheet's preset pick sets resolution and
// frame rate at once) all land; none reads the file empty and wipes the rest.
func TestConcurrentSettingsSaves(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := savePresets([]QualityPreset{{"720p", 15}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(4)
		go func() { defer wg.Done(); saveResolution("1080p") }()
		go func() {
			defer wg.Done()
			o := session.DefaultShareOptions()
			o.FPS = 45
			saveShareOptions(o, false, false)
		}()
		go func() { defer wg.Done(); saveE2EE(i%2 == 0) }()
		go func() { defer wg.Done(); loadSettings() }()
	}
	wg.Wait()
	st := loadSettings()
	if st.Resolution != "1080p" || st.FPS == nil || *st.FPS != 45 || st.E2EE == nil || !slices.Equal(st.Presets, []QualityPreset{{"720p", 15}}) {
		t.Fatalf("settings lost: %+v", st)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "blite", "*.tmp")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

// fakeWindowPIDs makes every "window:" source owned by process pid.
func fakeWindowPIDs(t *testing.T, pid int) {
	old := windowPID
	windowPID = func(src string) int {
		if isWindowSource(src) {
			return pid
		}
		return 0
	}
	t.Cleanup(func() { windowPID = old })
}

// A window whose process can't be found gets no sound, and the UI is told.
func TestWindowWithoutProcess(t *testing.T) {
	fakeWindowPIDs(t, 0)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	a := New(session.Config{Name: "alice", View: "none", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, "none")
	if err := a.SetSource("window:0x307a2"); err != nil {
		t.Fatal(err)
	}
	sh := a.State().(UIState).Share
	if sh.Audio.Mode != "none" || !sh.NoWindowAudio {
		t.Fatalf("audio %q, noWindowAudio %v; want none, true", sh.Audio.Mode, sh.NoWindowAudio)
	}
	a.SetSource("primary")
	if a.State().(UIState).Share.NoWindowAudio {
		t.Fatal("noWindowAudio should only be set while that window is the source")
	}
}

// A temp file a crash left mid-save goes on the next start; a fresh one
// (another save right now) stays.
func TestStaleSettingsTemp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	d := filepath.Join(dir, "blite")
	os.MkdirAll(d, 0o700)
	stale, fresh := filepath.Join(d, "settings-1.tmp"), filepath.Join(d, "settings-2.tmp")
	os.WriteFile(stale, []byte("{"), 0o600)
	os.WriteFile(fresh, []byte("{"), 0o600)
	old := time.Now().Add(-2 * time.Minute)
	os.Chtimes(stale, old, old)
	cleanTempOnce = sync.Once{}
	loadSettings()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale temp file not removed")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh temp file removed")
	}
}
