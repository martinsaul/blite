package diag

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"blite/internal/media"
)

// EncodeResult is how fast each share encoder runs on synthetic video.
type EncodeResult struct {
	// Mode says how the frame rates were measured.
	Mode    string      `json:"mode"`
	Sources []EncodeRun `json:"sources"` // the test source alone, per size
	Runs    []EncodeRun `json:"runs"`
	Missing []string    `json:"missing"` // share encoders not installed
}

// EncodeRun is one encoder (or the bare source) at one size.
type EncodeRun struct {
	Encoder    string  `json:"encoder"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	BitrateKbs int     `json:"bitrateKbs"`
	FPS        float64 `json:"fps"`
	Frames     int     `json:"frames"`
	Secs       float64 `json:"secs"`
	OK         bool    `json:"ok"` // reached 60 fps (57 and up)
	Error      string  `json:"error,omitempty"`
	Launch     string  `json:"launch,omitempty"`
}

// benchSizes are the sizes encoded: the app's 1080p and 4K, and 1440p.
var benchSizes = []struct{ w, h int }{{1920, 1080}, {2560, 1440}, {3840, 2160}}

// okFPS counts as keeping 60 fps (timing jitter, warm-up).
const okFPS = 57

// shareLayer is the HIGH layer a share at w x h and fps would encode: the
// app's 1080p or 4K layer, or one scaled by pixel count in between.
func shareLayer(w, h, fps int) media.Layer {
	r := media.Res1080
	if h > 1080 {
		r = media.Res2160
	}
	l := media.LayersForOpts(media.LayerOpts{Resolution: r, FPS: fps})[media.High]
	if l.Width != w || l.Height != h {
		l.BitrateKbs = int(int64(l.BitrateKbs) * int64(w*h) / int64(l.Width*l.Height))
		l.Width, l.Height = w, h
	}
	return l
}

func runEncode(ctx context.Context, r *Report, p *printer) error {
	quick := r.Quick
	warm, dur := 1500*time.Millisecond, 5*time.Second
	if quick {
		warm, dur = time.Second, 2*time.Second
	}
	res := &EncodeResult{Mode: fmt.Sprintf("source not live (as fast as the encoder takes frames), counted over %s after %s warm-up; OK = %d fps or more", dur, warm, okFPS)}
	r.with(func() { r.Encode = res })
	p.f("   %s", res.Mode)
	installed := media.AvailableEncoders()
	for _, f := range media.EncoderFactories() {
		if !slices.Contains(installed, f) {
			res.Missing = append(res.Missing, f)
		}
	}
	p.f("   not installed: %v", res.Missing)
	if len(installed) == 0 {
		return errors.New("no share encoder installed")
	}

	for _, s := range benchSizes {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		l := shareLayer(s.w, s.h, 60)
		rate, err := media.BenchSource(l, "NV12", time.Second, dur/2)
		run := EncodeRun{Encoder: "source only", Width: s.w, Height: s.h, FPS: rate.FPS, Frames: rate.Frames, Secs: rate.Secs, OK: rate.FPS >= okFPS}
		if err != nil {
			run.Error = err.Error()
		}
		r.with(func() { res.Sources = append(res.Sources, run) })
		p.f("   %-12s %4dx%-4d %6.0f fps%s", "test source", s.w, s.h, rate.FPS, errNote(err))
	}
	for _, f := range installed {
		for _, s := range benchSizes {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			l := shareLayer(s.w, s.h, 60)
			rate, err := media.BenchEncoder(f, l, warm, dur)
			run := EncodeRun{Encoder: f, Width: s.w, Height: s.h, BitrateKbs: l.BitrateKbs, FPS: rate.FPS,
				Frames: rate.Frames, Secs: rate.Secs, OK: err == nil && rate.FPS >= okFPS, Launch: rate.Launch}
			if err != nil {
				run.Error = err.Error()
			}
			r.with(func() { res.Runs = append(res.Runs, run) })
			p.f("   %-12s %4dx%-4d %6.1f fps  %s%s", f, s.w, s.h, rate.FPS, okWord(run.OK), errNote(err))
			if err != nil {
				break // a failing encoder fails the same way at the other sizes
			}
		}
	}
	return nil
}

func okWord(ok bool) string {
	if ok {
		return "OK"
	}
	return "below 60"
}

func errNote(err error) string {
	if err == nil {
		return ""
	}
	return "  ERROR " + firstLine(err.Error())
}
