package session

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"blite/internal/media"
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
		if got := encoderBehind(c.in, c.out, 30, w); got != c.want {
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
		if got := encoderBehind(c.in, c.out, c.fps, w); got != c.want {
			t.Errorf("in %d out %d at %d: behind %v, want %v", c.in, c.out, c.fps, got, c.want)
		}
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
