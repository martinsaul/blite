package nui

import (
	"slices"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"

	"blite/internal/app"
	"blite/internal/i18n"
)

func TestPresetEntries(t *testing.T) {
	sh := app.ShareConfig{Resolution: "1080p", FPS: 60, Presets: []app.QualityPreset{{Resolution: "720p", FPS: 15}}}
	es, active := presetEntries(sh)
	if len(es) != len(app.BuiltinPresets)+1 || es[active].p != (app.QualityPreset{Resolution: "1080p", FPS: 60}) || es[active].removable {
		t.Fatalf("entries %+v, active %d", es, active)
	}
	if last := es[len(es)-1]; !last.removable || last.p.FPS != 15 {
		t.Fatalf("the user's preset should come last, removable: %+v", last)
	}
	sh.Resolution, sh.FPS = "720p", 15
	if es, active := presetEntries(sh); active != len(es)-1 {
		t.Fatalf("user preset not selected: %d", active)
	}
	// A frame rate no preset has (Advanced's slider) shows first, selected.
	sh.FPS = 45
	es, active = presetEntries(sh)
	if active != 0 || es[0].removable || presetLabel(es[0].p) != "720p, 45 fps" || len(es) != len(app.BuiltinPresets)+2 {
		t.Fatalf("custom: %+v active %d", es[0], active)
	}
}

func TestHomePresets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	i18n.SetPreference("en")
	defer i18n.SetPreference("")

	a := test.NewApp()
	defer a.Quit()
	a.Settings().SetTheme(themeFor())
	st := demoState(false)
	st.Share.Presets = []app.QualityPreset{{Resolution: "1080p", FPS: 45}}
	be := &fakeBackend{st: st}
	u := &UI{a: be, fa: a, win: test.NewWindow(nil)}
	defer func() {
		u.closed.Store(true)
		u.win.Close()
	}()
	u.view = container.NewStack()
	u.win.SetContent(u.view)
	u.refresh()

	bar := u.lobby.presets
	if !bar.chips[2].active || bar.chips[2].text != "1080p, 30 fps" {
		t.Fatalf("selected chip %q (%v)", bar.chips[2].text, bar.chips[2].active)
	}
	user := bar.chips[len(bar.chips)-1]
	if user.text != "1080p, 45 fps" || user.onRemove == nil || bar.chips[0].onRemove != nil {
		t.Fatal("only the user's preset can be removed")
	}

	test.Tap(bar.chips[5]) // 4K, 60 fps
	wait(t, func() bool {
		be.mu.Lock()
		defer be.mu.Unlock()
		return be.resolution == "2160p" && len(be.shareCalls) == 1 && be.shareCalls[0].fps == 60
	})
	if !bar.chips[5].active || bar.chips[2].active {
		t.Fatal("the tapped preset should show selected right away")
	}

	user.Resize(user.MinSize())
	test.TapAt(user, fyne.NewPos(user.Size().Width-5, 5)) // its x
	wait(t, func() bool {
		be.mu.Lock()
		defer be.mu.Unlock()
		return slices.Equal(be.presetCalls, []string{"remove 1080p 45"})
	})
	u.refresh()
	if len(bar.chips) != len(app.BuiltinPresets) {
		t.Fatalf("%d chips after removing", len(bar.chips))
	}
}

func wait(t *testing.T, cond func() bool) {
	t.Helper()
	for range 400 {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out")
}

// All screens, test pattern and watch only live in Advanced; Home shows
// them and its cards switch away from them.
func TestOtherSources(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	i18n.SetPreference("en")
	defer i18n.SetPreference("")

	a := test.NewApp()
	defer a.Quit()
	a.Settings().SetTheme(themeFor())
	st := demoState(false)
	st.Share.Source = "test"
	be := &fakeBackend{st: st}
	u := &UI{a: be, fa: a, win: test.NewWindow(nil)}
	defer func() {
		u.closed.Store(true)
		u.win.Close()
	}()
	u.view = container.NewStack()
	u.win.SetContent(u.view)
	u.refresh()

	v := u.lobby
	if !v.chipSource.Visible() || v.chipSource.text != "Test pattern" || v.tileScreen.active || v.tileWindow.active {
		t.Fatalf("home: chip %q (%v), tiles %v/%v", v.chipSource.text, v.chipSource.Visible(), v.tileScreen.active, v.tileWindow.active)
	}
	test.Tap(v.chipSource)
	if u.sheet == nil || !u.sheet.advOpen || u.sheet.otherSrcSeg.selected != "test" {
		t.Fatal("the source chip should open Advanced with the source selected")
	}
	test.Tap(u.sheet.otherSrcSeg.items[0])
	wait(t, func() bool {
		be.mu.Lock()
		defer be.mu.Unlock()
		return be.source == "screen"
	})

	st.Share.Source = "none"
	be.mu.Lock()
	be.st = st
	be.mu.Unlock()
	u.refresh()
	if v.chipSource.text != "Nothing (watch only)" || u.sheet.otherSrcSeg.selected != "none" {
		t.Fatalf("watch only: chip %q, sheet %q", v.chipSource.text, u.sheet.otherSrcSeg.selected)
	}
	test.Tap(v.tileScreen)
	wait(t, func() bool {
		be.mu.Lock()
		defer be.mu.Unlock()
		return be.source == "primary"
	})
	st.Share.Source = "primary"
	be.mu.Lock()
	be.st = st
	be.mu.Unlock()
	u.refresh()
	if v.chipSource.Visible() || !v.tileScreen.active || u.sheet.otherSrcSeg.selected != "" {
		t.Fatal("Entire screen should replace the other source")
	}
}

// A window whose process is unknown: "No audio", and a note saying why.
func TestNoWindowAudioNote(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	i18n.SetPreference("en")
	defer i18n.SetPreference("")

	a := test.NewApp()
	defer a.Quit()
	a.Settings().SetTheme(themeFor())
	st := demoState(false)
	st.Share.Source, st.Share.SourceName = "window:0x307a2", "Game"
	st.Share.Audio.Mode = "none"
	st.Share.NoWindowAudio = true
	be := &fakeBackend{st: st}
	u := &UI{a: be, fa: a, win: test.NewWindow(nil)}
	defer func() {
		u.closed.Store(true)
		u.win.Close()
	}()
	u.view = container.NewStack()
	u.win.SetContent(u.view)
	u.refresh()
	if u.lobby.chipAudio.text != "No audio" || !u.lobby.noWinAudio.Visible() {
		t.Fatalf("home: chip %q, note %v", u.lobby.chipAudio.text, u.lobby.noWinAudio.Visible())
	}
	sh := newShareSheet(u)
	sh.update(u.st)
	if !sh.noWinAudioNote.Visible() || len(sh.audioSeg.opts) != 3 || sh.audioSeg.selected != "none" {
		t.Fatalf("sheet: note %v, options %+v", sh.noWinAudioNote.Visible(), sh.audioSeg.opts)
	}

	st.Share.NoWindowAudio = false
	st.Share.Audio.Mode = "window"
	be.mu.Lock()
	be.st = st
	be.mu.Unlock()
	u.refresh()
	sh.update(u.st)
	if u.lobby.chipAudio.text != "With sound" || u.lobby.noWinAudio.Visible() || sh.noWinAudioNote.Visible() || sh.audioSeg.selected != "window" {
		t.Fatal("a window with a known process shares its app's audio, without the note")
	}
}
