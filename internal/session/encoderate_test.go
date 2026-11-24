package session

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"blite/internal/media"
	"blite/internal/server"
)

// A still window offers few frames; that must not count as a slow encoder.
func TestEncoderBehind(t *testing.T) {
	w := 10 * time.Second
	for _, c := range []struct {
		in, out int
		want    bool
	}{
		{300, 300, false}, // 30 fps, keeping up
		{300, 250, false}, // 25 fps: fast enough
		{300, 180, true},  // 18 of 30 fps: the encoder drops frames
		{100, 100, false}, // still window: 10 fps offered, all encoded
		{120, 103, false}, // still window, a few lost
		{200, 100, true},  // offered 20, encoded 10
		{-1, 180, true},   // unknown input: rate alone
		{-1, 300, false},
	} {
		if got := encoderBehind(c.in, -1, c.out, 30, w, keepUpShare); got != c.want {
			t.Errorf("in %d out %d: behind %v, want %v", c.in, c.out, got, c.want)
		}
	}
	// at 60 fps (game), 40 of 60 is behind; thinned to 15, 15 is not
	for _, c := range []struct {
		in, out, fps int
		want         bool
	}{
		{600, 400, 60, true},
		{600, 590, 60, false},
		{150, 150, 15, false},
		{150, 100, 15, true},
	} {
		if got := encoderBehind(c.in, -1, c.out, c.fps, w, keepUpShare); got != c.want {
			t.Errorf("in %d out %d at %d: behind %v, want %v", c.in, c.out, c.fps, got, c.want)
		}
	}
	// a hardware encoder keeps up at 60%: 40 of 60 fps is fine, 30 isn't
	for _, c := range []struct {
		enc     string
		in, out int
		want    bool
	}{
		{"nvh264enc", 600, 400, false},
		{"amfh264enc", 600, 300, true},
		{"x264enc", 600, 400, true},
		{"x264enc", 600, 500, false},
	} {
		if got := encoderBehind(c.in, -1, c.out, 60, w, keepUp(c.enc)); got != c.want {
			t.Errorf("%s in %d out %d: behind %v, want %v", c.enc, c.in, c.out, got, c.want)
		}
	}
}

// Only frames HIGH's leaky queue drops make the encoder slow: frames it
// passed on that never came out went missing elsewhere (in the field the
// encoder put out 2-16 of 60 fps at any size), and a lower size won't help.
func TestEncoderBehindQueue(t *testing.T) {
	w := 10 * time.Second
	for _, c := range []struct {
		in, queued, out int
		want            bool
	}{
		{600, 300, 300, true},  // the queue dropped all 300 missing
		{600, 600, 300, false}, // the queue passed all on: not the encoder
		{600, 590, 120, false}, // 10 of 480 missing dropped
		{600, 360, 120, true},  // 240 of 480: half
		{600, 370, 120, false}, // 230 of 480: less than half
		{600, -1, 300, true},   // queue unknown: as before
		{600, 600, 590, false}, // keeping up, whatever the queue did
		{600, 300, 590, false},
		{-1, 300, 120, true}, // input unknown: rate alone
	} {
		if got := encoderBehind(c.in, c.queued, c.out, 60, w, keepUpShare); got != c.want {
			t.Errorf("in %d queued %d out %d: behind %v, want %v", c.in, c.queued, c.out, got, c.want)
		}
	}
}

// A still or slow screen captures few frames: HIGH falls short of its
// frame rate and the leaky queue drops videorate's copies, but that is the
// content, not the encoder. Field numbers per second, amfh264enc at 60.
func TestEncodeCheck(t *testing.T) {
	sec := time.Second
	for _, c := range []struct {
		name                           string
		captured, in, queued, out, fps int
		share                          float64
		want                           encodeVerdict
	}{
		{"still screen", 1, 88, 2, 2, 60, keepUpShareHW, encodeStill},
		{"still screen, x264", 1, 88, 2, 2, 60, keepUpShare, encodeStill},
		{"slow screen", 22, 60, 20, 20, 60, keepUpShareHW, encodeStill},
		{"slow screen, all encoded", 22, 60, 60, 60, 60, keepUpShareHW, encodeKeptUp},
		{"moving, keeping up", 58, 60, 58, 58, 60, keepUpShareHW, encodeKeptUp},
		{"moving, keeping up, x264", 58, 60, 58, 58, 60, keepUpShare, encodeKeptUp},
		{"slow encoder", 59, 60, 30, 30, 60, keepUpShareHW, encodeBehind},
		{"slow encoder, x264", 59, 60, 30, 30, 60, keepUpShare, encodeBehind},
		{"captured at 50, all encoded", 50, 60, 50, 50, 60, keepUpShare, encodeKeptUp},
		{"queue passed all on", 59, 60, 60, 30, 60, keepUpShareHW, encodeKeptUp},
		{"queue unknown", 59, 60, -1, 30, 60, keepUpShareHW, encodeBehind},
		{"offered unknown", 59, -1, 30, 30, 60, keepUpShareHW, encodeBehind},
		{"thinned to 30", 60, 30, 30, 30, 30, keepUpShare, encodeKeptUp},
		{"thinned to 30, slow", 60, 30, 15, 15, 30, keepUpShare, encodeBehind},
		// captured unknown: as before
		{"capture unknown, slow", -1, 60, 30, 30, 60, keepUpShareHW, encodeBehind},
		{"capture unknown, burst", -1, 88, 2, 2, 60, keepUpShareHW, encodeBehind},
		{"capture unknown, keeping up", -1, 60, 60, 58, 60, keepUpShareHW, encodeKeptUp},
	} {
		if got := encodeCheck(c.captured, c.in, c.queued, c.out, c.fps, sec, c.share); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
		// the same over the check's 10 s windows
		k := int(encodeWindow / sec)
		scale := func(n int) int {
			if n < 0 {
				return n
			}
			return k * n
		}
		if got := encodeCheck(scale(c.captured), scale(c.in), scale(c.queued), scale(c.out), c.fps, encodeWindow, c.share); got != c.want {
			t.Errorf("%s over %v: %v, want %v", c.name, encodeWindow, got, c.want)
		}
	}
}

// canEncode logs its answer when it changes, else once a minute.
func TestHeadroomLog(t *testing.T) {
	var h headroomLog
	t0 := time.Now()
	for _, c := range []struct {
		ok   bool
		at   time.Duration
		want bool
	}{
		{true, 0, true},
		{true, time.Second, false},
		{true, 59 * time.Second, false},
		{false, 60 * time.Second, true},
		{false, 61 * time.Second, false},
		{true, 62 * time.Second, true},
		{true, 62*time.Second + headroomLogEvery, true},
	} {
		if got := h.changed(c.ok, t0.Add(c.at)); got != c.want {
			t.Errorf("ok %v at %v: log %v, want %v", c.ok, c.at, got, c.want)
		}
	}
}

// Readings of an unknown counter give no rate.
func TestShareStatsRates(t *testing.T) {
	if d := countDiff(10, 70); d != 60 {
		t.Errorf("countDiff: %d", d)
	}
	if countDiff(-1, 70) != -1 || countDiff(10, -1) != -1 {
		t.Error("countDiff of an unknown reading")
	}
	if r := perSec(600, 10*time.Second); r != "60.0" {
		t.Errorf("perSec: %s", r)
	}
	if r := perSec(-1, time.Second); r != "?" {
		t.Errorf("perSec unknown: %s", r)
	}
	var m atomic.Int64
	for _, v := range []int64{3, 9, 5} {
		storeMax(&m, v)
	}
	if m.Load() != 9 {
		t.Errorf("storeMax: %d", m.Load())
	}
}

// Every encoder the app can pick but x264 runs on the GPU, as do the
// D3D11/D3D12/VA variants GStreamer also ships, and gets their thresholds.
func TestHardwareEncoder(t *testing.T) {
	for _, e := range []string{"nvh264enc", "nvd3d11h264enc", "nvcudah264enc", "amfh264enc", "qsvh264enc", "mfh264enc", "vah264enc", "vah264lpenc", "d3d12h264enc"} {
		if !hardwareEncoder(e) || keepUp(e) != keepUpShareHW {
			t.Errorf("%s: hardware %v, keep up at %v", e, hardwareEncoder(e), keepUp(e))
		}
	}
	for _, e := range []string{"x264enc", "openh264enc", ""} {
		if hardwareEncoder(e) || keepUp(e) != keepUpShare {
			t.Errorf("%s: hardware %v, keep up at %v", e, hardwareEncoder(e), keepUp(e))
		}
	}
	for _, e := range media.AvailableEncoders() {
		if hardwareEncoder(e) == (e == "x264enc") {
			t.Errorf("installed %s: hardware %v", e, hardwareEncoder(e))
		}
	}
}

// A step down helped if the lower size encodes 1.3x the frames or keeps
// up. In the field 2160p gave 4 fps and 1080p 5: that didn't.
func TestStepHelped(t *testing.T) {
	for _, c := range []struct {
		from, to float64
		behind   bool
		want     bool
	}{
		{4, 5, true, false},
		{16, 14, true, false},
		{10, 12.9, true, false},
		{10, 13, true, true},
		{9, 25, true, true},
		{16, 16, false, true}, // keeps up now (a still window)
	} {
		if got := stepHelped(c.from, c.to, c.behind); got != c.want {
			t.Errorf("stepHelped(%v, %v, %v) = %v, want %v", c.from, c.to, c.behind, got, c.want)
		}
	}
}

func TestStepDownBlock(t *testing.T) {
	for _, c := range []struct {
		lower                media.Resolution
		keep, noStep, helped bool
		ok                   bool
	}{
		{media.Res1080, false, false, false, true},
		{media.Res1080, true, false, false, false}, // KeepQuality
		{media.Res1080, false, true, false, false}, // a step down didn't help
		{media.Res720, false, false, false, false}, // 1080p didn't prove itself
		{media.Res720, false, false, true, true},
		{media.Res720, true, false, true, false},
	} {
		if got := stepDownBlock(c.lower, c.keep, c.noStep, c.helped); (got == "") != c.ok {
			t.Errorf("stepDownBlock(%s, %v, %v, %v) = %q", c.lower, c.keep, c.noStep, c.helped, got)
		}
	}
}

// A slow encoder steps 2160p down to 1080p on trial, with no note yet;
// the trial ends: 1080p no faster goes back to 2160p and stays there.
func TestResTrialDidntHelp(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Session{cfg: Config{Log: quiet}, res: media.Res2160, shareOpts: ShareOptions{FPS: 60, SlowViewer: SlowResolution}}
	if got := s.slowEncoder(nil, media.Res2160, 4); got != media.Res1080 {
		t.Fatalf("stepped to %q, want 1080p", got)
	}
	tr := s.resTrial
	if tr == nil || tr.from != media.Res2160 || tr.fromFPS != 4 || s.resWant != media.Res2160 || s.resNote != "" {
		t.Fatalf("trial %+v, want %q, note %q", tr, s.resWant, s.resNote)
	}
	back, ok := s.endTrial(nil, media.Res1080, tr, stepHelped(tr.fromFPS, 5, true))
	if !ok || back != media.Res2160 || s.res != media.Res2160 {
		t.Fatalf("back %q (ok %v), res %q", back, ok, s.res)
	}
	if !s.resNoStep || s.resWant != "" || s.resNote != "" || s.resTrial != nil {
		t.Fatalf("noStep %v want %q note %q trial %v", s.resNoStep, s.resWant, s.resNote, s.resTrial)
	}
	// still slow at 2160p: we keep it, and don't try again
	if got := s.slowEncoder(nil, media.Res2160, 4); got != "" || s.res != media.Res2160 {
		t.Fatalf("stepped to %q after a step that didn't help", got)
	}
	s.mu.Lock()
	s.resetResTrials()
	s.mu.Unlock()
	if s.resNoStep {
		t.Fatal("a new share should step down again")
	}
}

// 2160p -> 1080p helped (the note tells why), so 1080p -> 720p may be
// tried; it didn't help, so we go back to 1080p with the 4K note, and
// don't try 4K again.
func TestResTrialHelped(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Session{cfg: Config{Log: quiet}, res: media.Res2160, shareOpts: ShareOptions{FPS: 60, SlowViewer: SlowResolution}}
	s.slowEncoder(nil, media.Res2160, 10)
	if back, ok := s.endTrial(nil, media.Res1080, s.resTrial, stepHelped(10, 20, true)); !ok || back != "" {
		t.Fatalf("helped: back %q ok %v", back, ok)
	}
	note := s.resNote
	if !s.resHelped || note == "" || s.res != media.Res1080 || s.resWant != media.Res2160 {
		t.Fatalf("helped %v note %q res %q want %q", s.resHelped, note, s.res, s.resWant)
	}
	if got := s.slowEncoder(nil, media.Res1080, 20); got != media.Res720 {
		t.Fatalf("stepped to %q, want 720p", got)
	}
	if back, ok := s.endTrial(nil, media.Res720, s.resTrial, stepHelped(20, 22, true)); !ok || back != media.Res1080 {
		t.Fatalf("didn't help: back %q ok %v", back, ok)
	}
	if s.res != media.Res1080 || s.resNote != note || s.resWant != media.Res2160 || !s.resNoStep {
		t.Fatalf("res %q note %q want %q noStep %v", s.res, s.resNote, s.resWant, s.resNoStep)
	}
}

// Below 1080p only once 1080p proved to help: a share chosen at 1080p
// stays there.
func TestResNotBelow1080(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Session{cfg: Config{Log: quiet}, res: media.Res1080, shareOpts: ShareOptions{FPS: 60, SlowViewer: SlowResolution}}
	if got := s.slowEncoder(nil, media.Res1080, 8); got != "" || s.res != media.Res1080 || s.resTrial != nil {
		t.Fatalf("stepped to %q", got)
	}
}

// KeepQuality: a slow encoder never lowers the resolution.
func TestKeepQuality(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Session{cfg: Config{Log: quiet}, res: media.Res2160, shareOpts: ShareOptions{FPS: 60, SlowViewer: SlowResolution, KeepQuality: true}}
	if got := s.slowEncoder(nil, media.Res2160, 4); got != "" || s.res != media.Res2160 || s.resTrial != nil || s.resNote != "" {
		t.Fatalf("stepped to %q, res %q", got, s.res)
	}
	if !NormalizeShareOptions(ShareOptions{KeepQuality: true}).KeepQuality || DefaultShareOptions().KeepQuality {
		t.Fatal("KeepQuality: kept by Normalize, off by default")
	}
}

func TestShareOptionsSlowViewerPolicy(t *testing.T) {
	def := DefaultShareOptions()
	if def.FPS != 30 || def.EffectiveSlowViewer() != SlowFPS || def.MinHeight != 360 || !def.SmallerCopy {
		t.Fatalf("unexpected defaults: %+v (effective=%q)", def, def.EffectiveSlowViewer())
	}

	// Automatic slow-viewer policy follows FPS unless overridden explicitly.
	if got := NormalizeShareOptions(ShareOptions{FPS: 60}).EffectiveSlowViewer(); got != SlowResolution {
		t.Fatalf("60 fps auto policy = %q, want %q", got, SlowResolution)
	}
	if got := NormalizeShareOptions(ShareOptions{FPS: 45}).EffectiveSlowViewer(); got != SlowResolution {
		t.Fatalf("45 fps auto policy = %q, want %q", got, SlowResolution)
	}
	if got := NormalizeShareOptions(ShareOptions{FPS: 40}).EffectiveSlowViewer(); got != SlowFPS {
		t.Fatalf("40 fps auto policy = %q, want %q", got, SlowFPS)
	}
	if got := NormalizeShareOptions(ShareOptions{FPS: 60, SlowViewer: SlowFPS}).EffectiveSlowViewer(); got != SlowFPS {
		t.Fatalf("60 fps explicit fps override = %q, want %q", got, SlowFPS)
	}
	if got := NormalizeShareOptions(ShareOptions{FPS: 30, SlowViewer: SlowResolution}).EffectiveSlowViewer(); got != SlowResolution {
		t.Fatalf("30 fps explicit resolution override = %q, want %q", got, SlowResolution)
	}

	// cpuStep steps down FPS for SlowFPS (45 -> 30 -> 20 -> 15 -> false),
	// and refuses to step FPS for SlowResolution.
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Session{cfg: Config{Log: quiet}, shareOpts: ShareOptions{FPS: 45, SlowViewer: SlowFPS, MinHeight: 360, SmallerCopy: true}}
	for _, want := range []int{30, 20, 15} {
		if !s.cpuStep(media.Res1080, nil, 24) || s.cpuFPS != want {
			t.Fatalf("cpuStep want %d, got cpuFPS=%d", want, s.cpuFPS)
		}
	}
	if s.cpuStep(media.Res1080, nil, 10) {
		t.Fatal("cpuStep should stop after 15 fps")
	}

	sRes := &Session{cfg: Config{Log: quiet}, shareOpts: ShareOptions{FPS: 60, SlowViewer: SlowResolution, MinHeight: 360, SmallerCopy: true}}
	if sRes.cpuStep(media.Res1080, nil, 35) {
		t.Fatal("SlowResolution must lower resolution first, not cpuFPS")
	}
}

// A hardware encoder on a mostly idle CPU needs more slow windows in a row
// before we step down: such windows are more likely a stall at the start.
func TestStrikesNeeded(t *testing.T) {
	for _, c := range []struct {
		enc  string
		busy float64
		want int
	}{
		{"nvh264enc", 0.01, encodeStrikesIdle},
		{"amfh264enc", 0.3, encodeStrikesIdle},
		{"nvh264enc", 0.9, encodeStrikes},
		{"nvh264enc", -1, encodeStrikes}, // not measured yet
		{"x264enc", 0.01, encodeStrikes},
		{"openh264enc", 0.01, encodeStrikes},
		{"", 0.01, encodeStrikes},
	} {
		if got := strikesNeeded(c.enc, c.busy); got != c.want {
			t.Errorf("strikesNeeded(%q, %.2f) = %d, want %d", c.enc, c.busy, got, c.want)
		}
	}
}

// The chosen size is tried again after 2 minutes, then 4, 8, and at most
// every 15.
func TestResRetryAfter(t *testing.T) {
	want := []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for tries, w := range want {
		if got := resRetryAfter(tries); got != w {
			t.Errorf("resRetryAfter(%d) = %v, want %v", tries, got, w)
		}
	}
}

// Turning KeepQuality on after a slow encoder stepped us down goes back to
// the chosen size.
func TestKeepQualityGoesBack(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	s, err := Create(context.Background(), Config{Server: "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws", Name: "alice", View: media.ViewCount, Log: quiet}, "secret-pw")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.mu.Lock()
	s.res, s.resWant, s.resNote = media.Res1080, media.Res2160, "Your computer can't encode 4K fast enough (9 fps); sharing at 1080p."
	s.mu.Unlock()
	o := s.ShareOptions()
	o.KeepQuality = true
	if err := s.SetShareOptions(o); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.res != media.Res2160 || s.resWant != "" || s.resNote != "" || !s.shareOpts.KeepQuality {
		t.Fatalf("res %q want %q note %q opts %+v", s.res, s.resWant, s.resNote, s.shareOpts)
	}
}
