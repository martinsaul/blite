package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"blite/internal/i18n"
	"blite/internal/media"
	"blite/internal/rtc"
	"blite/internal/server"
	"blite/internal/session"
)

// Two apps, real media, in-process signaling: A creates, B joins by link, both
// see each other's video, then A leaving is noticed by B.
func TestCreateJoinLeave(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet, ICE: nil}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	mk := func(name string) *App {
		return New(session.Config{Server: ws, Name: name, View: media.ViewCount, Log: quiet}, media.SourceTest)
	}
	a, b := mk("alice"), mk("bob")
	defer a.Shutdown()
	defer b.Shutdown()

	ctx := context.Background()
	if err := a.Create(ctx, "s3cret-pw", true); err != nil {
		t.Fatal(err)
	}
	if err := a.Create(ctx, "", true); err != ErrInSession {
		t.Fatalf("second create: %v", err)
	}
	link := a.State().(UIState).Session.Link
	if err := b.JoinLink(ctx, link); err != nil {
		t.Fatal(err)
	}
	if err := b.JoinLink(ctx, strings.Replace(link, "#", "#x", 1)); err != ErrInSession {
		t.Fatalf("join while in session: %v", err)
	}

	got := func(x *App, peer string) int {
		for _, m := range x.State().(UIState).Session.Members {
			if m.Name == peer && m.State == "connected" {
				return m.Frames
			}
		}
		return 0
	}
	deadline := time.Now().Add(15 * time.Second)
	for got(a, "bob") < 20 || got(b, "alice") < 20 {
		if time.Now().After(deadline) {
			t.Fatalf("frames: alice<-bob %d, bob<-alice %d", got(a, "bob"), got(b, "alice"))
		}
		time.Sleep(200 * time.Millisecond)
	}

	a.Leave()
	deadline = time.Now().Add(10 * time.Second)
	for len(b.State().(UIState).Session.Members) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("bob still sees alice after she left")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Bad password through the friendly error path.
	c := mk("carol")
	defer c.Shutdown()
	bad := link[:strings.Index(link, "#")] + "#wrong-password"
	if err := c.JoinLink(ctx, bad); err == nil || !strings.Contains(err.Error(), "session doesn't exist") && !strings.Contains(err.Error(), "wrong password") {
		t.Fatalf("bad password: %v", err)
	}
}

func TestDiagnosticsIncludesLocales(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("LANGUAGE", "ja_JP:pt_BR")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "pt_BR.UTF-8")
	defer i18n.SetPreference("")
	if err := i18n.SetPreference("pt-BR"); err != nil {
		t.Fatal(err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(session.Config{Name: "alice", View: "none", Log: quiet}, "none")
	diag := a.Diagnostics()
	if !strings.Contains(diag, "language: pt-BR (system: ja-JP, pt-BR, preference: pt-BR)") {
		t.Fatalf("diagnostics missing locale info:\n%s", diag)
	}
}

func TestDiscordChannelSharing(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(server.Config{Log: quiet, ICE: nil})
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	a := New(session.Config{Server: ws, Name: "alice", View: media.ViewCount, Log: quiet}, "none")
	defer a.Shutdown()
	a.setDiscord(func(d *Discord) { d.Status, d.ChannelID = "ready", "987654321" })

	ctx := context.Background()
	if err := a.GoLiveInChannel(ctx, "", false); err != nil {
		t.Fatal(err)
	}
	cr := a.lookupChannel(ctx, "987654321")
	if cr.Room == "" || !slices.Equal(cr.Members, []string{"alice"}) || len(cr.Sharers) != 0 {
		t.Fatalf("before sharing: %+v", cr)
	}

	if err := a.SetSource(string(media.SourceTest)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !slices.Equal(a.lookupChannel(ctx, "987654321").Sharers, []string{"alice"}) {
		if time.Now().After(deadline) {
			t.Fatalf("sharer not reported: %+v", a.lookupChannel(ctx, "987654321"))
		}
		time.Sleep(15 * time.Millisecond)
	}

	if err := a.SetSource("none"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for len(a.lookupChannel(ctx, "987654321").Sharers) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("sharer not cleared: %+v", a.lookupChannel(ctx, "987654321"))
		}
		time.Sleep(15 * time.Millisecond)
	}
}

func TestAutoWatchPersistence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	a := New(session.Config{Name: "alice", View: "none", Log: quiet}, "none")
	if !a.State().(UIState).AutoWatch {
		t.Fatal("AutoWatch should default to true")
	}
	if err := a.SetAutoWatch(false); err != nil {
		t.Fatal(err)
	}
	if a.State().(UIState).AutoWatch {
		t.Fatal("AutoWatch should be false after SetAutoWatch(false)")
	}

	b := New(session.Config{Name: "alice", View: "none", Log: quiet}, "none")
	if b.State().(UIState).AutoWatch {
		t.Fatal("AutoWatch=false should persist across restarts")
	}
	if !b.State().(UIState).PlaySounds {
		t.Fatal("PlaySounds should default to true")
	}
	if err := b.SetPlaySounds(false); err != nil {
		t.Fatal(err)
	}
	c := New(session.Config{Name: "alice", View: "none", Log: quiet}, "none")
	if c.State().(UIState).PlaySounds || c.State().(UIState).AutoWatch {
		t.Fatalf("settings should both persist across restarts: %+v", c.State().(UIState))
	}
}

func TestShareOptionsPersistenceAndPreCall(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	a := New(session.Config{Name: "alice", View: "none", Log: quiet}, "none")
	defer a.Shutdown()
	if err := a.SetResolution("1080p"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetShareOptions(45, "fps", 540, false, true, 7500); err != nil {
		t.Fatal(err)
	}
	if err := a.SetRoomSecurity(true, "pw1234", false); err != nil {
		t.Fatal(err)
	}
	st := a.State().(UIState)
	if !st.Share.RequirePassword || st.Share.RoomPassword != "pw1234" || !st.Share.HasPassword || st.Share.E2EE {
		t.Fatalf("pre-call room security mismatch: %+v", st.Share)
	}
	if jb, err := json.Marshal(st); err != nil || strings.Contains(string(jb), "pw1234") {
		t.Fatalf("JSON state leaked RoomPassword: %s (err=%v)", string(jb), err)
	}
	if sb, err := os.ReadFile(filepath.Join(dir, "blite", "settings.json")); err != nil || strings.Contains(string(sb), "pw1234") {
		t.Fatalf("settings.json missing or leaked RoomPassword: %s (err=%v)", string(sb), err)
	}

	ts := httptest.NewServer(server.New(server.Config{Log: quiet, ICE: nil}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	b := New(session.Config{Server: ws, Name: "alice", View: "none", Log: quiet}, media.SourceTest)
	defer b.Shutdown()
	sh := b.State().(UIState).Share
	if sh.Resolution != "1080p" || sh.FPS != 45 || sh.SlowViewer != "fps" || sh.EffectiveSlow != "fps" ||
		sh.MinHeight != 540 || sh.SmallerCopy || !sh.KeepQuality || sh.UploadKbps != 7500 || sh.E2EE {
		t.Fatalf("persisted share settings mismatch: %+v", sh)
	}
	if sh.RequirePassword || sh.RoomPassword != "" || sh.HasPassword {
		t.Fatalf("transient room password should not persist across restarts: %+v", sh)
	}

	if err := b.Create(context.Background(), "", sh.E2EE); err != nil {
		t.Fatal(err)
	}
	live := b.State().(UIState).Session
	if live == nil {
		t.Fatal("expected live session")
	}
	if live.Resolution != "1080p" || live.ShareOptions.FPS != 45 || live.ShareOptions.SlowViewer != "fps" ||
		live.EffectiveSlow != "fps" || live.ShareOptions.MinHeight != 540 || live.ShareOptions.SmallerCopy || !live.ShareOptions.KeepQuality ||
		live.UploadLimitKbps != 7500 || live.Encrypted {
		t.Fatalf("live session did not inherit pre-call share options: %+v", live)
	}
}

func waitApp(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for app condition")
		}
		time.Sleep(15 * time.Millisecond)
	}
}

func TestAppPlaySoundsLiveSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	ts := httptest.NewServer(server.New(server.Config{Log: quiet, ICE: nil}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	alice := New(session.Config{Server: ws, Name: "alice", View: media.ViewCount, Log: quiet}, media.SourceTest)
	defer alice.Shutdown()
	if err := alice.Create(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
	alice.mu.Lock()
	s := alice.sess
	alice.mu.Unlock()
	if s == nil || !s.PlaySounds() {
		t.Fatal("live session should start with PlaySounds=true")
	}
	waitApp(t, 5*time.Second, func() bool {
		return len(s.PlayedSounds()) == 1 && s.PlayedSounds()[0] == media.SoundSelfLive && s.SoundBuffers() >= 10
	})

	// Toggling PlaySounds off on App propagates immediately to the live session.
	if err := alice.SetPlaySounds(false); err != nil {
		t.Fatal(err)
	}
	if s.PlaySounds() {
		t.Fatal("live session PlaySounds should be false after App.SetPlaySounds(false)")
	}
	bob := New(session.Config{Server: ws, Name: "bob", View: media.ViewCount, Log: quiet}, "none")
	defer bob.Shutdown()
	if err := bob.SetPlaySounds(false); err != nil {
		t.Fatal(err)
	}
	if err := bob.JoinLink(context.Background(), alice.State().(UIState).Session.Link); err != nil {
		t.Fatal(err)
	}
	bob.mu.Lock()
	bs := bob.sess
	bob.mu.Unlock()
	if bs == nil || bs.PlaySounds() {
		t.Fatal("bob session should inherit PlaySounds=false")
	}
	waitApp(t, 5*time.Second, func() bool {
		st := bob.State().(UIState).Session
		return st != nil && len(st.Members) == 1 && st.Members[0].Sharing
	})
	if got := s.PlayedSounds(); len(got) != 1 {
		t.Fatalf("alice PlayedSounds after muted join = %v, want only initial [self-live]", got)
	}
	if got := bs.PlayedSounds(); len(got) != 0 {
		t.Fatalf("bob PlayedSounds while PlaySounds=false = %v, want empty", got)
	}
}

func TestDiagnosticsRedactsSecrets(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(session.Config{Name: "alice", View: "none", Log: quiet}, "none")
	a.RecentLog = func() string {
		return strings.Join([]string{
			`guard: started blite.exe pid 10 args ["bshare://join/K7PQ2M4X#pwGuard"]`,
			`level=INFO msg="open" link=https://share.bellion.io/j/7K3QX9MD#pwLink password="pw Quoted" token=tok99`,
			`level=WARN msg="failed https://share.bellion.io/j/MAMACO#pwUrl by alice#7F2A"`,
			"",
		}, "\n")
	}
	diag := a.Diagnostics()
	for _, secret := range []string{"pwGuard", "pwLink", "pw Quoted", "tok99", "pwUrl"} {
		if strings.Contains(diag, secret) {
			t.Fatalf("Diagnostics leaked %q:\n%s", secret, diag)
		}
	}
	for _, keep := range []string{"K7PQ2M4X#[cut]", "MAMACO#[cut]", "alice#7F2A", "link=https://share.bellion.io/j/7K3QX9MD#[cut]", "password=[cut]"} {
		if !strings.Contains(diag, keep) {
			t.Fatalf("Diagnostics lost %q:\n%s", keep, diag)
		}
	}
}

func TestPortMapSettingNextSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	if !LoadPortMap() {
		t.Fatal("LoadPortMap should default to true")
	}

	port := rtc.NewPortOpts(33471, false, quiet)
	defer port.Close()

	// Disable before starting App so New() does not trigger live LAN UPnP probes.
	if err := savePortMap(false); err != nil {
		t.Fatal(err)
	}
	if LoadPortMap() {
		t.Fatal("LoadPortMap should be false after savePortMap(false)")
	}

	ts := httptest.NewServer(server.New(server.Config{Log: quiet, ICE: nil}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	a := New(session.Config{Server: ws, Name: "alice", View: "none", Port: port, Log: quiet}, "none")
	defer a.Shutdown()

	st := a.State().(UIState)
	if st.Share.PortMap || st.Share.PortMapNextSession || st.Reach != nil {
		t.Fatalf("initial state with PortMap=false: Share=%+v Reach=%+v", st.Share, st.Reach)
	}
	if port.Mapping() {
		t.Fatal("port.Mapping() should be false")
	}

	// Start a call while PortMap=false; peer session still works using the fixed local UDP port.
	if err := a.Create(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
	if a.State().(UIState).Share.PortMapNextSession {
		t.Fatal("PortMapNextSession should be false before changing during call")
	}

	// Change setting to true during the call: current call keeps Mapping()=false,
	// and UI state reports PortMapNextSession=true.
	if err := a.SetPortMap(true); err != nil {
		t.Fatal(err)
	}
	inCall := a.State().(UIState)
	if !inCall.Share.PortMap || !inCall.Share.PortMapNextSession {
		t.Fatalf("expected PortMap=true & PortMapNextSession=true during call, got %+v", inCall.Share)
	}
	if port.Mapping() {
		t.Fatal("port.Mapping() must not change mid-call")
	}

	// Toggle back to false during the same call: matches active session again.
	if err := a.SetPortMap(false); err != nil {
		t.Fatal(err)
	}
	if a.State().(UIState).Share.PortMapNextSession {
		t.Fatal("PortMapNextSession should clear when reverted to active call's value")
	}
	a.Leave()
}

func TestUploadFlagWinsOverSavedSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 1. Start without the flag and change FPS: saving other options must NOT
	// write a default 0 for uploadKbps into settings.json.
	a0 := New(session.Config{Name: "alice", View: "none", Log: quiet}, "none")
	if err := a0.SetShareOptions(60, session.SlowResolution, 360, true, false, 0); err != nil {
		t.Fatal(err)
	}
	a0.Shutdown()

	sf, err := settingsFile()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(sf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "uploadKbps") {
		t.Fatalf("saving unrelated share options wrote uploadKbps to disk: %s", string(raw))
	}

	// 2. Start with --relay-upload-kbps flag (4200), change FPS, and restart with
	// the flag: the flag's limit must win both before and after restart.
	a1 := New(session.Config{Name: "alice", View: "none", UploadKbps: 4200, Log: quiet}, "none")
	if got := a1.State().(UIState).Share.UploadKbps; got != 4200 {
		t.Fatalf("initial UploadKbps with flag = %d, want 4200", got)
	}
	if err := a1.SetMode("desktop"); err != nil {
		t.Fatal(err)
	}
	// Even if a caller passes 0 when changing FPS during this run, restarting
	// with the flag must still apply the flag's limit.
	if err := a1.SetShareOptions(45, session.SlowFPS, 540, true, false, 0); err != nil {
		t.Fatal(err)
	}
	a1.Shutdown()

	a2 := New(session.Config{Name: "alice", View: "none", UploadKbps: 4200, Log: quiet}, "none")
	defer a2.Shutdown()
	st2 := a2.State().(UIState).Share
	if st2.FPS != 45 || st2.MinHeight != 540 {
		t.Fatalf("restarted share options = %+v, want FPS=45 MinHeight=540", st2)
	}
	if st2.UploadKbps != 4200 {
		t.Fatalf("restarted UploadKbps with flag = %d, want 4200", st2.UploadKbps)
	}

	// 3. Even when settings.json has a saved non-zero uploadKbps (7500), the CLI
	// flag wins on startup without overwriting the saved setting on FPS change.
	aNoFlag := New(session.Config{Name: "alice", View: "none", Log: quiet}, "none")
	if err := aNoFlag.SetShareOptions(30, "", 360, true, false, 7500); err != nil {
		t.Fatal(err)
	}
	aNoFlag.Shutdown()

	aWithFlag := New(session.Config{Name: "alice", View: "none", UploadKbps: 4200, Log: quiet}, "none")
	if got := aWithFlag.State().(UIState).Share.UploadKbps; got != 4200 {
		t.Fatalf("UploadKbps with flag over saved 7500 = %d, want 4200", got)
	}
	if err := aWithFlag.SetMode("game"); err != nil {
		t.Fatal(err)
	}
	// Choosing Automatic (0) in the UI while --upload is set applies to the
	// current run without deleting the previously saved 7500 limit on disk.
	if err := aWithFlag.SetShareOptions(60, session.SlowResolution, 360, true, false, 0); err != nil {
		t.Fatal(err)
	}
	if got := aWithFlag.State().(UIState).Share.UploadKbps; got != 0 {
		t.Fatalf("in-run UploadKbps after choosing Automatic = %d, want 0", got)
	}
	aWithFlag.Shutdown()

	aAfter := New(session.Config{Name: "alice", View: "none", Log: quiet}, "none")
	defer aAfter.Shutdown()
	if got := aAfter.State().(UIState).Share.UploadKbps; got != 7500 {
		t.Fatalf("saved UploadKbps without flag = %d, want 7500", got)
	}
}

func TestAnonymizeLogsDefaultAndExplicitOff(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Never chose (no settings.json): defaults to on.
	if !AnonymizeLogs() {
		t.Fatal("AnonymizeLogs should default to true when unset")
	}
	a := New(session.Config{Name: "alice", View: "none", Log: quiet}, "none")
	defer a.Shutdown()
	if !a.State().(UIState).AnonymizeLogs {
		t.Fatal("UIState.AnonymizeLogs should default to true")
	}

	// Never chose, even after other settings are written to settings.json: stays on.
	if err := a.SetResolution("1080p"); err != nil {
		t.Fatal(err)
	}
	if !AnonymizeLogs() || !a.State().(UIState).AnonymizeLogs {
		t.Fatal("AnonymizeLogs should stay true when unrelated settings are saved")
	}

	// Explicitly unticked in Send logs: becomes false and stays false across restarts.
	var sentAnon []bool
	a.SendLogs = func(_ context.Context, anon bool) error {
		sentAnon = append(sentAnon, anon)
		return nil
	}
	if err := a.SendLogsNow(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(sentAnon) != 1 || sentAnon[0] {
		t.Fatalf("SendLogsNow(false) passed %v", sentAnon)
	}
	if AnonymizeLogs() || a.State().(UIState).AnonymizeLogs {
		t.Fatal("AnonymizeLogs should be false after user explicitly unticked it")
	}
	if err := a.SetResolution("720p"); err != nil {
		t.Fatal(err)
	}
	b := New(session.Config{Name: "alice", View: "none", Log: quiet}, "none")
	defer b.Shutdown()
	if AnonymizeLogs() || b.State().(UIState).AnonymizeLogs {
		t.Fatal("explicit AnonymizeLogs=false must persist across unrelated saves and restarts")
	}

	// Re-ticking in Send logs turns it back on.
	b.SendLogs = a.SendLogs
	if err := b.SendLogsNow(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if !AnonymizeLogs() || !b.State().(UIState).AnonymizeLogs {
		t.Fatal("AnonymizeLogs should return to true after SendLogsNow(true)")
	}
}

func TestCrashReportsSettingAndAnswer(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	if got := CrashReports(); got != CrashReportsAsk {
		t.Fatalf("default CrashReports() = %q, want %q", got, CrashReportsAsk)
	}
	a := New(session.Config{Name: "alice", View: "none", Log: quiet}, "none")
	defer a.Shutdown()
	st := a.State().(UIState)
	if st.CrashReports != CrashReportsAsk || st.CrashReport {
		t.Fatalf("initial UIState crash fields = (%q, %v)", st.CrashReports, st.CrashReport)
	}

	for _, mode := range []string{CrashReportsAlways, CrashReportsNever, CrashReportsAsk} {
		if err := a.SetCrashReports(mode); err != nil {
			t.Fatalf("SetCrashReports(%q): %v", mode, err)
		}
		if got := CrashReports(); got != mode {
			t.Fatalf("CrashReports() = %q, want %q", got, mode)
		}
		if got := a.State().(UIState).CrashReports; got != mode {
			t.Fatalf("UIState.CrashReports = %q, want %q", got, mode)
		}
	}
	if err := a.SetCrashReports("bogus"); err == nil {
		t.Fatal("SetCrashReports accepted bogus value")
	}

	// Pending crash prompt: Send with auto=true -> uploads and switches to always.
	var answered []bool
	a.OfferCrashReport(func(_ context.Context, send bool) error {
		answered = append(answered, send)
		return nil
	})
	if !a.State().(UIState).CrashReport {
		t.Fatal("CrashReport should be true after OfferCrashReport")
	}
	if err := a.AnswerCrashReport(context.Background(), true, true); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(answered, []bool{true}) || a.State().(UIState).CrashReport || CrashReports() != CrashReportsAlways {
		t.Fatalf("after Send+auto: answered=%v pending=%v mode=%q", answered, a.State().(UIState).CrashReport, CrashReports())
	}

	// Don't send with auto=true -> switches to never.
	a.OfferCrashReport(func(_ context.Context, send bool) error {
		answered = append(answered, send)
		return nil
	})
	if err := a.AnswerCrashReport(context.Background(), false, true); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(answered, []bool{true, false}) || CrashReports() != CrashReportsNever {
		t.Fatalf("after DontSend+auto: answered=%v mode=%q", answered, CrashReports())
	}
}
