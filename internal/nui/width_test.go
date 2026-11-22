package nui

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"

	"blite/internal/app"
	"blite/internal/audio"
	"blite/internal/feeds"
	"blite/internal/i18n"
	"blite/internal/session"
	"blite/internal/ui"
	"blite/internal/update"
)

type shareOptCall struct {
	fps         int
	slowViewer  string
	minHeight   int
	smallerCopy bool
	uploadKbps  int
}

// fakeBackend records UI actions and serves a fixed or mutable UIState.
type fakeBackend struct {
	ui.Backend
	mu            sync.Mutex
	st            app.UIState
	watched       []string
	peerQuality   string
	peerVol       float64
	peerMuted     bool
	volunteer     *bool
	sounds        *bool
	autoWatch     *bool
	portMap       *bool
	source        string
	resolution    string
	audioMode     string
	outputDev     string
	shareCalls    []shareOptCall
	secReqPw      bool
	secPw         string
	secE2EE       bool
	secCalls      int
	probeCalls    int
	joinedLinks   []string
	joinedRecent  []string
	removedRecent []string
	createdCalls  int
	presetCalls   []string
	createdPw     string
	createdE2EE   bool
	dcEnterCalls  int
	dcGoLiveCalls int
	dcJoinCalls   int
}

func (f *fakeBackend) State() any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *fakeBackend) ToggleWatch(id string) error {
	f.mu.Lock()
	f.watched = append(f.watched, id)
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) SetPeer(_ string, q string, vol float64, muted bool) error {
	f.mu.Lock()
	f.peerQuality, f.peerVol, f.peerMuted = q, vol, muted
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) Volunteer(on bool) error {
	f.mu.Lock()
	f.volunteer = &on
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) SetVoiceInSystem(bool) error { return nil }
func (f *fakeBackend) SetHideChangelog(bool) error { return nil }

func (f *fakeBackend) SetPlaySounds(on bool) error {
	f.mu.Lock()
	f.sounds = &on
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) SetAutoWatch(on bool) error {
	f.mu.Lock()
	f.autoWatch = &on
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) SetPortMap(on bool) error {
	f.mu.Lock()
	f.portMap = &on
	f.st.Share.PortMap = on
	if f.st.Session != nil {
		f.st.Share.PortMapNextSession = !on
	}
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) SetSource(kind string) error {
	f.mu.Lock()
	f.source = kind
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) SetResolution(res string) error {
	f.mu.Lock()
	f.resolution = res
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) SetAudio(mode string, _ []string) error {
	f.mu.Lock()
	f.audioMode = mode
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) SetOutput(device string) error {
	f.mu.Lock()
	f.outputDev = device
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) SetShareOptions(fps int, slow string, minH int, smaller bool, upKbps int) error {
	f.mu.Lock()
	f.shareCalls = append(f.shareCalls, shareOptCall{
		fps:         fps,
		slowViewer:  slow,
		minHeight:   minH,
		smallerCopy: smaller,
		uploadKbps:  upKbps,
	})
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) SetRoomSecurity(reqPw bool, pw string, e2ee bool) error {
	f.mu.Lock()
	f.secReqPw, f.secPw, f.secE2EE = reqPw, pw, e2ee
	f.secCalls++
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) AddQualityPreset(res string, fps int) error {
	f.mu.Lock()
	f.presetCalls = append(f.presetCalls, "add "+res+" "+strconv.Itoa(fps))
	f.st.Share.Presets = append(f.st.Share.Presets, app.QualityPreset{Resolution: res, FPS: fps})
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) RemoveQualityPreset(res string, fps int) error {
	f.mu.Lock()
	f.presetCalls = append(f.presetCalls, "remove "+res+" "+strconv.Itoa(fps))
	f.st.Share.Presets = slices.DeleteFunc(f.st.Share.Presets, func(p app.QualityPreset) bool {
		return p == app.QualityPreset{Resolution: res, FPS: fps}
	})
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) Create(_ context.Context, pw string, e2ee bool) error {
	f.mu.Lock()
	f.createdCalls++
	f.createdPw, f.createdE2EE = pw, e2ee
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) JoinLink(_ context.Context, link string) error {
	f.mu.Lock()
	f.joinedLinks = append(f.joinedLinks, link)
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) ProbeRecentRooms() {
	f.mu.Lock()
	f.probeCalls++
	f.mu.Unlock()
}

func (f *fakeBackend) JoinRecentRoom(_ context.Context, server, code string) error {
	f.mu.Lock()
	f.joinedRecent = append(f.joinedRecent, server+"|"+code)
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) RemoveRecentRoom(server, code string) error {
	f.mu.Lock()
	f.removedRecent = append(f.removedRecent, server+"|"+code)
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) DiscordLogin()  {}
func (f *fakeBackend) DiscordUnlink() {}

func (f *fakeBackend) GoLiveInChannel(context.Context, string, bool) error {
	f.mu.Lock()
	f.dcGoLiveCalls++
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) EnterChannel(context.Context, string, bool) error {
	f.mu.Lock()
	f.dcEnterCalls++
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) JoinChannel(context.Context, string) error {
	f.mu.Lock()
	f.dcJoinCalls++
	f.mu.Unlock()
	return nil
}

func demoState(inCall bool) app.UIState {
	auto := true
	st := app.UIState{Version: "0.1.0 (84) linux", Name: "Alice", Tag: "4417", Autostart: &auto, AutoWatch: true, PlaySounds: true,
		Share: app.ShareConfig{
			Source:        "primary",
			Resolution:    "1080p",
			FPS:           30,
			EffectiveSlow: session.SlowFPS,
			MinHeight:     360,
			SmallerCopy:   true,
			Audio:         audio.Selection{Mode: "system"},
			E2EE:          true,
			PortMap:       true,
		},
		Update:  &update.Info{Channel: "beta", CanApply: true, Available: "0.1.0 (85)"},
		Reach:   &app.Reach{OK: false, Text: "Not reachable: your router doesn't allow port mapping (UPnP, NAT-PMP or PCP). Calls with people behind strict routers may fail."},
		Discord: &app.Discord{Status: "ready", ChannelID: "1", Channel: "gaming", Guild: "Game Night", Room: "K7PQ-2M4X", Open: true, People: 3, Sharers: []string{"Bob"}, Members: []string{"Bob", "Carol", "Dave"}},
		Servers: &app.Servers{List: []app.Server{{URL: "wss://a/ws", Host: "a", BuiltIn: true}}, Default: "wss://a/ws", Selected: "wss://a/ws"},
		Recent: []app.RecentRoom{
			{Code: "K7PQ2M4X", Display: "K7PQ-2M4X", Server: "wss://a/ws", Host: "a", BuiltIn: true, Status: "ready"},
			{Code: "MAMAC0", Display: "MAMAC0", Server: "wss://b/ws", Host: "b", HasPassword: true, Status: "checking"},
		}}
	if inCall {
		st.Session = &session.State{Room: "K7PQ-2M4X", Source: "screen", Resolution: "1080p", SelfEye: feeds.EyeOpen,
			Encrypted: true, Safety: "4821 0937 5512", IsOwner: true,
			MeshComfortPeers: 2, ResolutionNote: "Your computer can't encode 4K at 60 fps (41 fps); sharing at 30 fps.",
			ShareNotice: "Bob started sharing",
			Members: []session.Member{
				{ID: "a", Name: "Bob", Tag: "2926", Client: "0.1.0 (83) windows", State: "connected", Pair: "host/srflx", Layer: "high", Limit: "bandwidth", Auto: "off",
					RTTms: 152, Loss: 0.06, TxKbps: 3050, RxKbps: 4100, Quality: "auto", Volume: 1, Recv: 720, Sharing: true, Watching: true, Eye: feeds.EyeOpen, Owner: true, KeyChanged: true, Heard: true, Loud: true},
				{ID: "b", Name: "Carol", Tag: "0815", Client: "0.1.0 (83) linux", State: "connected", Layer: "high",
					RTTms: 35, TxKbps: 2800, RxKbps: 3500, Quality: "auto", Volume: 1, Recv: 1080, Sharing: true, Watching: false, Eye: feeds.EyeClosed, Muted: true},
				{ID: "c", Name: "Dave", Tag: "7301", Client: "0.1.0 (82) windows", State: "unreachable", Quality: "auto", Volume: 1, Eye: feeds.EyeGrey,
					Hint: "No connection between you yet; trying again. If it keeps failing, a firewall may block it: send logs from both sides."},
				{ID: "d", Name: "Eve", Tag: "1111", State: "untrusted", Quality: "low", Eye: feeds.EyeGrey},
			}}
	}
	return st
}

// TestLangWidths: no language makes the window wider than it opens
// (1100 points), including when the person side panel is open.
func TestLangWidths(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	defer i18n.SetPreference("")
	a := test.NewApp()
	defer a.Quit()
	for _, l := range i18n.Languages {
		if only := os.Getenv("I18N_LANGS"); only != "" && only != l.Tag {
			continue
		}
		i18n.SetPreference(l.Tag)
		a.Settings().SetTheme(themeFor())
		for _, inCall := range []bool{false, true} {
			u := &UI{a: &fakeBackend{st: demoState(inCall)}, fa: a, win: test.NewWindow(nil)}
			u.view = container.NewStack()
			u.win.SetContent(u.view)
			u.refresh()
			w := u.view.MinSize().Width
			t.Logf("%-8s call=%-5v min width %4.0f", l.Tag, inCall, w)
			if w > 1100 {
				t.Errorf("%s (call %v): the window needs %.0f points, more than 1100", l.Tag, inCall, w)
			}
			if !inCall {
				// Also verify the Discord Join + Share two-button banner width.
				noShare := demoState(false)
				noShare.Discord = &app.Discord{Status: "ready", ChannelID: "1", Channel: "gaming", Guild: "Game Night"}
				u.lobby.updateDiscord(u, noShare.Discord)
				w2 := u.view.MinSize().Width
				if w2 > 1100 {
					t.Errorf("%s (home Discord Join+Share): the window needs %.0f points, more than 1100", l.Tag, w2)
				}
			}
			if inCall {
				u.call.selectPeer("a")
				wp := u.view.MinSize().Width
				if wp > 1100 {
					t.Errorf("%s (call with panel): the window needs %.0f points, more than 1100", l.Tag, wp)
				}
				sh := newShareSheet(u)
				sh.update(u.st)
				sh.setAdvancedOpen(true)
				sh.upCustomBox.Show()
				sh.portMapNote.Show()
				sh.pwEntry.Show()
				ws := sh.content.MinSize().Width
				if ws > 560 {
					t.Errorf("%s (share sheet + advanced): needs %.0f points, more than 560", l.Tag, ws)
				}
			}
			u.closed.Store(true)
			u.win.Close()
		}
	}
}

func TestControlWindowInteractions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	i18n.SetPreference("en")
	defer i18n.SetPreference("")

	a := test.NewApp()
	defer a.Quit()
	a.Settings().SetTheme(themeFor())

	be := &fakeBackend{st: demoState(true)}
	u := &UI{a: be, fa: a, win: test.NewWindow(nil)}
	defer u.win.Close()
	u.view = container.NewStack()
	u.win.SetContent(u.view)
	u.refresh()

	if u.win.MainMenu() != nil {
		t.Fatal("control window should have no menu bar")
	}
	if !u.call.gear.dot {
		t.Fatal("gear button should show update dot when update is available")
	}

	// Verify Gear and "?" menu items cover all required settings and actions.
	var gearLabels []string
	for _, it := range u.gearMenuItems() {
		if !it.IsSeparator {
			gearLabels = append(gearLabels, it.Label)
		}
	}
	for _, want := range []string{
		"Your name...", "Your identity key...", "Servers...", "Language",
		"Open at sign-in", "Check for updates",
	} {
		if !slices.Contains(gearLabels, want) {
			t.Errorf("gear menu missing %q (got %v)", want, gearLabels)
		}
	}
	for _, moved := range []string{ // now in the call footer and Share options
		"Play sounds", "Start watching new streams automatically",
		"Include voice chat apps (Discord, TeamSpeak...) in whole-system audio",
	} {
		if slices.Contains(gearLabels, moved) {
			t.Errorf("gear menu still has %q", moved)
		}
	}

	// Footer toggles: sounds and auto-watch, both on in the demo, turn off.
	test.Tap(u.call.sounds)
	test.Tap(u.call.autoWatch)
	// The speaker mutes Bob locally, keeping his quality and volume.
	test.Tap(u.call.people["a"].speaker)
	for i := 0; i < 400; i++ {
		be.mu.Lock()
		done := be.sounds != nil && be.autoWatch != nil && be.peerMuted
		be.mu.Unlock()
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	be.mu.Lock()
	if be.sounds == nil || *be.sounds || be.autoWatch == nil || *be.autoWatch {
		t.Errorf("footer toggles: sounds %v, auto-watch %v; want both off", be.sounds, be.autoWatch)
	}
	if !be.peerMuted || be.peerVol != 1 || be.peerQuality != "auto" {
		t.Errorf("speaker: muted %v vol %v quality %q; want muted at 1, auto", be.peerMuted, be.peerVol, be.peerQuality)
	}
	be.mu.Unlock()

	var helpLabels []string
	for _, it := range u.helpMenuItems() {
		if !it.IsSeparator {
			helpLabels = append(helpLabels, it.Label)
		}
	}
	for _, want := range []string{
		"Send feedback...", "Send logs to the developers",
		"Open log folder", "Copy diagnostics", "About Bellion Share",
	} {
		if !slices.Contains(helpLabels, want) {
			t.Errorf("help menu missing %q (got %v)", want, helpLabels)
		}
	}

	// Eye toggles: own row (open) and Bob (open) trigger ToggleWatch; Dave (grey) does not.
	test.Tap(u.call.self.eye)
	test.Tap(u.call.people["a"].eye)
	test.Tap(u.call.people["c"].eye)
	// Wait briefly for u.do goroutines.
	for i := 0; i < 400; i++ {
		be.mu.Lock()
		n := len(be.watched)
		be.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	be.mu.Lock()
	gotWatched := slices.Clone(be.watched)
	be.mu.Unlock()
	if !slices.Contains(gotWatched, feeds.Self) || !slices.Contains(gotWatched, "a") || slices.Contains(gotWatched, "c") {
		t.Fatalf("eye toggles watched = %v, want [self, a] without c", gotWatched)
	}

	// Clicking a row opens the side panel; segmented Best/Low updates peer quality;
	// clicking the same row again closes the side panel.
	if u.call.panel.root.Visible() {
		t.Fatal("side panel should start hidden")
	}
	u.call.selectPeer("a")
	if !u.call.panel.root.Visible() || u.call.panel.id != "a" {
		t.Fatal("side panel should open for peer a")
	}
	if len(u.call.panel.quality.items) != 2 {
		t.Fatalf("quality options = %d, want 2 (Best / Low)", len(u.call.panel.quality.items))
	}
	test.Tap(u.call.panel.quality.items[1]) // Low (saves data)

	u.call.selectPeer("a")
	if u.call.panel.root.Visible() {
		t.Fatal("clicking selected row again should close side panel")
	}

	// Relay switch toggles Volunteer(true).
	test.Tap(u.call.relaySw)
	for i := 0; i < 400; i++ {
		be.mu.Lock()
		v := be.volunteer
		q := be.peerQuality
		be.mu.Unlock()
		if v != nil && q == "low" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if be.peerQuality != "low" {
		t.Errorf("peer quality = %q, want low", be.peerQuality)
	}
	if be.volunteer == nil || !*be.volunteer {
		t.Errorf("volunteer = %v, want true", be.volunteer)
	}
}

func TestShareSheetAndAdvanced(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	i18n.SetPreference("en")
	defer i18n.SetPreference("")

	a := test.NewApp()
	defer a.Quit()
	a.Settings().SetTheme(themeFor())

	st := demoState(false)
	st.Share.Audio = audio.Selection{Mode: "window"}
	be := &fakeBackend{st: st}
	u := &UI{
		a:       be,
		fa:      a,
		win:     test.NewWindow(nil),
		devices: []audio.Device{{ID: "headset-1", Name: "USB Headset"}},
	}
	defer u.win.Close()
	u.view = container.NewStack()
	u.win.SetContent(u.view)
	u.refresh()

	sh := newShareSheet(u)
	sh.update(u.st)

	if sh.audioSeg.selected != "window" || len(sh.audioSeg.opts) != 5 {
		t.Fatalf("audio = %q of %+v, want \"This window's app\" offered and selected", sh.audioSeg.selected, sh.audioSeg.opts)
	}
	if sh.advBox.Visible() {
		t.Fatal("Advanced section should start collapsed")
	}

	waitShareCalls := func(want int) shareOptCall {
		t.Helper()
		for i := 0; i < 400; i++ {
			be.mu.Lock()
			n := len(be.shareCalls)
			var last shareOptCall
			if n > 0 {
				last = be.shareCalls[n-1]
			}
			be.mu.Unlock()
			if n >= want {
				return last
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %d SetShareOptions calls", want)
		return shareOptCall{}
	}

	// Default FPS is 30 -> slow viewer defaults to SlowFPS ("Lower the frame rate first").
	if sh.slowSeg.selected != session.SlowFPS {
		t.Fatalf("initial slowSeg = %q, want %q", sh.slowSeg.selected, session.SlowFPS)
	}

	// The sheet has quality presets, no source choice (Home's cards pick it).
	if es, active := presetEntries(sh.cur); es[active].p != (app.QualityPreset{Resolution: "1080p", FPS: 30}) || !sh.presets.chips[active].active {
		t.Fatalf("selected preset = %+v", es[active].p)
	}
	// Tap 1080p, 60 fps -> default slow viewer follows FPS to SlowResolution ("Lower the resolution first").
	test.Tap(sh.presets.chips[3])
	got := waitShareCalls(1)
	if got.fps != 60 || got.slowViewer != session.SlowAuto || sh.slowSeg.selected != session.SlowResolution {
		t.Fatalf("after 60 fps: call=%+v slowSeg=%q", got, sh.slowSeg.selected)
	}

	// Expand Advanced and explicitly pick "Lower the frame rate first": overrides 60 fps default.
	test.Tap(sh.advBtn)
	if !sh.advBox.Visible() {
		t.Fatal("Advanced section should expand when tapped")
	}
	test.Tap(sh.slowSeg.items[1])
	got = waitShareCalls(2)
	if got.fps != 60 || got.slowViewer != session.SlowFPS {
		t.Fatalf("explicit slow-viewer override: got %+v", got)
	}

	// Move Advanced FPS slider to 45 -> the presets show "1080p, 45 fps" first, selected.
	sh.fpsSlider.OnChanged(45)
	sh.fpsSlider.OnChangeEnded(45)
	got = waitShareCalls(3)
	if got.fps != 45 || len(sh.presets.chips) != len(app.BuiltinPresets)+1 || sh.presets.chips[0].text != "1080p, 45 fps" || !sh.presets.chips[0].active {
		t.Fatalf("custom 45 fps: call=%+v first=%q", got, sh.presets.chips[0].text)
	}

	// Lowest resolution 540p + disable smaller copy + custom 4.5 Mbit/s upload limit.
	test.Tap(sh.minResSeg.items[1])
	got = waitShareCalls(4)
	if got.minHeight != 540 {
		t.Fatalf("minHeight = %d, want 540", got.minHeight)
	}
	test.Tap(sh.smallerSw)
	got = waitShareCalls(5)
	if got.smallerCopy {
		t.Fatal("smallerCopy should be false after toggling off")
	}
	test.Tap(sh.upModeSeg.items[1]) // Custom (defaults to 10 Mbit/s = 10000 kbps)
	got = waitShareCalls(6)
	if got.uploadKbps != 10000 || !sh.upCustomBox.Visible() {
		t.Fatalf("custom upload default: got %+v visible=%v", got, sh.upCustomBox.Visible())
	}
	sh.upEntry.SetText("4.5")
	test.Tap(sh.upApply)
	got = waitShareCalls(7)
	if got.uploadKbps != 4500 {
		t.Fatalf("custom upload 4.5 Mbit/s = %d kbps, want 4500", got.uploadKbps)
	}
	test.Tap(sh.upModeSeg.items[0]) // Back to Automatic
	got = waitShareCalls(8)
	if got.uploadKbps != 0 || sh.upCustomBox.Visible() {
		t.Fatalf("automatic upload: got %+v visible=%v", got, sh.upCustomBox.Visible())
	}

	// Room security toggles in Advanced: Require a password + E2EE off.
	test.Tap(sh.pwSw)
	sh.pwEntry.SetText("s3cret")
	test.Tap(sh.e2eeSw)
	for i := 0; i < 400; i++ {
		be.mu.Lock()
		n, req, pw, e2ee := be.secCalls, be.secReqPw, be.secPw, be.secE2EE
		be.mu.Unlock()
		if n >= 3 && req && pw == "s3cret" && !e2ee {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	be.mu.Lock()
	if !be.secReqPw || be.secPw != "s3cret" || be.secE2EE {
		t.Fatalf("security = req=%v pw=%q e2ee=%v", be.secReqPw, be.secPw, be.secE2EE)
	}
	be.mu.Unlock()

	// Router port-mapping toggle: outside a call, note stays hidden;
	// during a call, toggling shows "Applies to your next session.".
	if !sh.portMapSw.checked || sh.portMapNote.Visible() {
		t.Fatalf("initial portMap = checked:%v noteVis:%v", sh.portMapSw.checked, sh.portMapNote.Visible())
	}
	test.Tap(sh.portMapSw)
	for i := 0; i < 200; i++ {
		be.mu.Lock()
		pm := be.portMap
		be.mu.Unlock()
		if pm != nil && !*pm {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if sh.portMapNote.Visible() {
		t.Fatal("portMapNote should remain hidden outside a call")
	}

	// Simulate an active call starting with PortMap=true, then toggling it off.
	inCallSt := demoState(true)
	sh.update(inCallSt)
	if !sh.portMapSw.checked || sh.portMapNote.Visible() {
		t.Fatalf("in-call initial portMap = checked:%v noteVis:%v", sh.portMapSw.checked, sh.portMapNote.Visible())
	}
	test.Tap(sh.portMapSw)
	if !sh.portMapNote.Visible() || sh.portMapNote.Text != "Applies to your next session." {
		t.Fatalf("portMapNote in call = %q (vis=%v), want 'Applies to your next session.'", sh.portMapNote.Text, sh.portMapNote.Visible())
	}

	// Verify upload limit appears in personPanel Connection details.
	callSt := demoState(true)
	callSt.Session.UploadLimitKbps = 6000
	callSt.Session.TxKbps = 5200
	callSt.Session.Members[1].Layer = "720p"
	callSt.Session.Members[1].Limit = "upload"
	be.mu.Lock()
	be.st = callSt
	be.mu.Unlock()
	u.refresh()
	u.call.selectPeer("b")
	if !strings.Contains(u.call.panel.sendLine.Text, "upload limit") {
		t.Fatalf("sendLine = %q, want 'upload limit'", u.call.panel.sendLine.Text)
	}
	if !u.call.panel.uploadLine.Visible() || !strings.Contains(u.call.panel.uploadLine.Text, "5.2 Mbit/s / 6.0 Mbit/s") {
		t.Fatalf("uploadLine = %q (visible=%v)", u.call.panel.uploadLine.Text, u.call.panel.uploadLine.Visible())
	}
}

func TestHomeCardsDiscordAndRecent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	i18n.SetPreference("en")
	defer i18n.SetPreference("")

	a := test.NewApp()
	defer a.Quit()
	a.Settings().SetTheme(themeFor())

	st := demoState(false)
	st.Share.FPS = 60
	st.Share.EffectiveSlow = session.SlowResolution
	// Start with nobody sharing in Discord channel so Join + Share buttons show.
	st.Discord = &app.Discord{Status: "ready", ChannelID: "1", Channel: "gaming", Guild: "Game Night"}
	st.Recent = []app.RecentRoom{
		{Code: "K7PQ2M4X", Display: "K7PQ-2M4X", Server: "wss://a/ws", Host: "a", BuiltIn: true, Status: "checking"},
		{Code: "MAMAC0", Display: "MAMAC0", Server: "wss://b/ws", Host: "b", HasPassword: true, Status: "ready"},
		{Code: "DEAD0001", Display: "DEAD-0001", Server: "wss://a/ws", Host: "a", BuiltIn: true, Status: "gone"},
	}

	be := &fakeBackend{st: st}
	u := &UI{a: be, fa: a, win: test.NewWindow(nil)}
	defer func() {
		u.closed.Store(true)
		u.win.Close()
	}()
	u.view = container.NewStack()
	u.win.SetContent(u.view)
	u.refresh()

	// 1. Showing Home triggers ProbeRecentRooms once.
	for i := 0; i < 50; i++ {
		be.mu.Lock()
		pc := be.probeCalls
		be.mu.Unlock()
		if pc >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	be.mu.Lock()
	if be.probeCalls != 1 {
		t.Fatalf("probeCalls = %d, want 1", be.probeCalls)
	}
	be.mu.Unlock()

	// 2. Share card: tiles + summary chips + clicking chip opens Share options sheet + Go live.
	if !u.lobby.tileScreen.active || u.lobby.tileWindow.active {
		t.Fatalf("expected Entire screen active, A window inactive")
	}
	if es, active := presetEntries(normalizeShareState(u.st)); presetLabel(es[active].p) != "1080p, 60 fps" || u.lobby.chipAudio.text != "With sound" {
		t.Fatalf("preset / chip = %q / %q, want '1080p, 60 fps' / 'With sound'", presetLabel(es[active].p), u.lobby.chipAudio.text)
	}
	test.Tap(u.lobby.chipAudio)
	if u.sheet == nil {
		t.Fatal("clicking a settings chip should open the Share options sheet")
	}

	// Switch to window source and No audio + password + unencrypted -> chips reflect state.
	st.Share.Source = "window:0x307a2"
	st.Share.SourceName = "Firefox"
	st.Share.Audio.Mode = "none"
	st.Share.RequirePassword = true
	st.Share.RoomPassword = "cat1"
	st.Share.E2EE = false
	be.mu.Lock()
	be.st = st
	be.mu.Unlock()
	u.refresh()
	if u.lobby.tileScreen.active || !u.lobby.tileWindow.active || u.lobby.tileWindow.sub != "Firefox" {
		t.Fatalf("window tile mismatch: screen=%v win=%v sub=%q", u.lobby.tileScreen.active, u.lobby.tileWindow.active, u.lobby.tileWindow.sub)
	}
	if u.lobby.chipAudio.text != "No audio" || !u.lobby.chipSec.Visible() || u.lobby.chipSec.text != "Unencrypted" {
		t.Fatalf("audio/sec chips = %q / %q (vis=%v)", u.lobby.chipAudio.text, u.lobby.chipSec.text, u.lobby.chipSec.Visible())
	}

	// Tapping Entire screen tile resets source to "primary".
	test.Tap(u.lobby.tileScreen)
	for i := 0; i < 50; i++ {
		be.mu.Lock()
		src := be.source
		be.mu.Unlock()
		if src == "primary" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	be.mu.Lock()
	if be.source != "primary" {
		t.Fatalf("tileScreen did not set source=primary, got %q", be.source)
	}
	be.mu.Unlock()

	// Primary "Go live and copy link" button calls Create with configured password & E2EE.
	test.Tap(u.lobby.live)
	for i := 0; i < 50; i++ {
		be.mu.Lock()
		cc := be.createdCalls
		be.mu.Unlock()
		if cc >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	be.mu.Lock()
	if be.createdCalls != 1 || be.createdPw != "cat1" || be.createdE2EE {
		t.Fatalf("Go live = calls:%d pw:%q e2ee:%v", be.createdCalls, be.createdPw, be.createdE2EE)
	}
	be.mu.Unlock()
	u.bg.Wait() // its refresh must not run into the next steps (or tests)

	// 3. Join card: bare code Watch calls JoinLink.
	u.lobby.link.SetText("MAMACO")
	test.Tap(u.lobby.join)
	for i := 0; i < 50; i++ {
		be.mu.Lock()
		jl := slices.Clone(be.joinedLinks)
		be.mu.Unlock()
		if len(jl) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	be.mu.Lock()
	if !slices.Equal(be.joinedLinks, []string{"MAMACO"}) {
		t.Fatalf("joinedLinks = %v, want [MAMACO]", be.joinedLinks)
	}
	be.mu.Unlock()
	u.bg.Wait()

	// 4. Discord banner: Join (without sharing) + Share when nobody is sharing;
	// "Bob is sharing their screen" + avatars + Watch when someone is sharing.
	if u.lobby.dcPrimary.Text != "Join" || !u.lobby.dcSecondary.Visible() || u.lobby.dcSecondary.Text != "Share" {
		t.Fatalf("Discord buttons = %q / %q (secVis=%v)", u.lobby.dcPrimary.Text, u.lobby.dcSecondary.Text, u.lobby.dcSecondary.Visible())
	}
	be.mu.Lock()
	be.source = ""
	be.mu.Unlock()
	test.Tap(u.lobby.dcPrimary) // Join without sharing
	for i := 0; i < 50; i++ {
		be.mu.Lock()
		ec := be.dcEnterCalls
		be.mu.Unlock()
		if ec >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	be.mu.Lock()
	if be.dcEnterCalls != 1 || be.source != "" {
		t.Fatalf("Discord Join should call EnterChannel without SetSource: enter=%d src=%q", be.dcEnterCalls, be.source)
	}
	be.mu.Unlock()
	u.bg.Wait()

	test.Tap(u.lobby.dcSecondary) // Share
	for i := 0; i < 50; i++ {
		be.mu.Lock()
		gl := be.dcGoLiveCalls
		be.mu.Unlock()
		if gl >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	be.mu.Lock()
	if be.dcGoLiveCalls != 1 {
		t.Fatalf("Discord Share did not call GoLiveInChannel: %d", be.dcGoLiveCalls)
	}
	be.mu.Unlock()
	u.bg.Wait()

	// Someone sharing in Discord: "Bob is sharing their screen", avatars, and Watch.
	st.Discord = &app.Discord{
		Status:    "ready",
		ChannelID: "1",
		Channel:   "gaming",
		Guild:     "Game Night",
		Room:      "K7PQ-2M4X",
		Open:      true,
		People:    2,
		Sharers:   []string{"Bob"},
		Members:   []string{"Bob", "Carol"},
	}
	be.mu.Lock()
	be.st = st
	be.mu.Unlock()
	u.refresh()
	if u.lobby.dcTitle.Text != "Bob is sharing their screen" || u.lobby.dcPrimary.Text != "Watch" || u.lobby.dcSecondary.Visible() {
		t.Fatalf("Discord sharing state = title:%q pri:%q secVis:%v", u.lobby.dcTitle.Text, u.lobby.dcPrimary.Text, u.lobby.dcSecondary.Visible())
	}
	if !u.lobby.dcAvatars.Visible() || len(u.lobby.dcAvatars.Objects) != 2 {
		t.Fatalf("Discord avatars = vis:%v len:%d", u.lobby.dcAvatars.Visible(), len(u.lobby.dcAvatars.Objects))
	}
	test.Tap(u.lobby.dcPrimary)
	for i := 0; i < 50; i++ {
		be.mu.Lock()
		jc := be.dcJoinCalls
		be.mu.Unlock()
		if jc >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	be.mu.Lock()
	if be.dcJoinCalls != 1 {
		t.Fatalf("Discord Watch did not call JoinChannel: %d", be.dcJoinCalls)
	}
	be.mu.Unlock()
	u.bg.Wait()

	// 5. Recent rooms: checking & gone rows are NOT clickable; ready row joins; x removes; empty hides card.
	if !u.lobby.recent.root.Visible() {
		t.Fatal("Recent rooms card should be visible when non-empty")
	}
	rowChecking := u.lobby.recent.rows["wss://a/ws|K7PQ2M4X"]
	rowReady := u.lobby.recent.rows["wss://b/ws|MAMAC0"]
	rowGone := u.lobby.recent.rows["wss://a/ws|DEAD0001"]
	if rowChecking == nil || rowReady == nil || rowGone == nil {
		t.Fatalf("missing recent rows: %+v", u.lobby.recent.rows)
	}
	test.Tap(rowChecking)
	test.Tap(rowGone)
	time.Sleep(25 * time.Millisecond)
	be.mu.Lock()
	if len(be.joinedRecent) != 0 {
		t.Fatalf("checking/gone rows must not be clickable, got %v", be.joinedRecent)
	}
	be.mu.Unlock()

	test.Tap(rowReady)
	for i := 0; i < 50; i++ {
		be.mu.Lock()
		jr := slices.Clone(be.joinedRecent)
		be.mu.Unlock()
		if len(jr) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	be.mu.Lock()
	if !slices.Equal(be.joinedRecent, []string{"wss://b/ws|MAMAC0"}) {
		t.Fatalf("joinedRecent = %v, want [wss://b/ws|MAMAC0]", be.joinedRecent)
	}
	be.mu.Unlock()

	test.Tap(rowReady.removeBtn)
	for i := 0; i < 50; i++ {
		be.mu.Lock()
		rr := slices.Clone(be.removedRecent)
		be.mu.Unlock()
		if len(rr) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	be.mu.Lock()
	if !slices.Equal(be.removedRecent, []string{"wss://b/ws|MAMAC0"}) {
		t.Fatalf("removedRecent = %v, want [wss://b/ws|MAMAC0]", be.removedRecent)
	}
	be.mu.Unlock()

	st.Recent = nil
	be.mu.Lock()
	be.st = st
	be.mu.Unlock()
	u.refresh()
	if u.lobby.recent.root.Visible() {
		t.Fatal("Recent rooms card must be hidden when list is empty")
	}
}

func TestApplyScenes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	i18n.SetPreference("en")
	defer i18n.SetPreference("")

	a := test.NewApp()
	defer a.Quit()
	a.Settings().SetTheme(themeFor())

	// Lobby scenes: sharesheet, advanced, gear, help.
	for _, sc := range []string{"sharesheet", "advanced", "gear", "help"} {
		u := &UI{a: &fakeBackend{st: demoState(false)}, fa: a, win: test.NewWindow(nil)}
		u.view = container.NewStack()
		u.win.SetContent(u.view)
		u.refresh()
		u.applyScene(sc)
		switch sc {
		case "sharesheet":
			if u.sheet == nil || u.sheet.advOpen {
				t.Fatalf("sharesheet scene: sheet=%v advOpen=%v", u.sheet, u.sheet != nil && u.sheet.advOpen)
			}
		case "advanced":
			if u.sheet == nil || !u.sheet.advOpen || !u.sheet.advBox.Visible() {
				t.Fatalf("advanced scene: sheet=%v advOpen=%v", u.sheet, u.sheet != nil && u.sheet.advOpen)
			}
		default:
			if len(u.win.Canvas().Overlays().List()) == 0 {
				t.Fatalf("%s scene: expected popup menu overlay", sc)
			}
		}
		u.closed.Store(true)
		u.win.Close()
	}

	// Call scenes: panel, gear, help.
	for _, sc := range []string{"panel", "gear", "help"} {
		u := &UI{a: &fakeBackend{st: demoState(true)}, fa: a, win: test.NewWindow(nil)}
		u.view = container.NewStack()
		u.win.SetContent(u.view)
		u.refresh()
		u.applyScene(sc)
		if sc == "panel" {
			if !u.call.panel.root.Visible() || u.call.panel.id != "a" {
				t.Fatalf("panel scene: vis=%v id=%q", u.call.panel.root.Visible(), u.call.panel.id)
			}
		} else if len(u.win.Canvas().Overlays().List()) == 0 {
			t.Fatalf("call %s scene: expected popup menu overlay", sc)
		}
		u.closed.Store(true)
		u.win.Close()
	}
}
