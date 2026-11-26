package media

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"blite/internal/gst"
)

// The measurements here back blite-diag (and later the app's own computer
// test). They build their pipelines from the same parts the share uses:
// the encoder list with its settings, sourceElements and convertScale.

// ErrNoFactory: the element isn't installed.
var ErrNoFactory = errors.New("not installed")

// EncoderFactories names every H.264 encoder a share may use, best first,
// installed or not.
func EncoderFactories() []string {
	var out []string
	for _, e := range encoders {
		out = append(out, e.factory)
	}
	return out
}

// EncoderLaunch is the encoder element a share would use for l.
func EncoderLaunch(factory string, l Layer) (string, error) {
	for _, e := range encoders {
		if e.factory == factory {
			return e.launch(0, l), nil
		}
	}
	return "", fmt.Errorf("%s: not a share encoder", factory)
}

// EncoderFormat is the raw format a share feeds factory.
func EncoderFormat(factory string) string { return encoder{factory: factory}.format() }

// PathRates are the frame rates along a share's capture path.
type PathRates struct {
	Captured, Offered, Out float64 // per second; Out: after the leaky queue
	Secs                   float64
	CaptureCaps            string
	Gaps, PTSGaps          []time.Duration // between captured frames
	Launch                 string
}

// ShareChains are the chains a share of kind can scale and convert on,
// best first.
func ShareChains(kind SourceKind) []Chain {
	src, err := sourceElements(kind)
	if err != nil {
		return nil
	}
	if d3d11Source(src) && gst.HasFactory("d3d11convert") {
		return []Chain{ChainGPU, ChainCPU}
	}
	return []Chain{ChainCPU}
}

// ProbeSharePath runs a share's capture path for HIGH at l (captureBranch
// on chain, then the leaky queue) into a fakesink instead of an encoder:
// what the capture and conversion manage when no encoder holds anything up.
func ProbeSharePath(kind SourceKind, l Layer, format string, chain Chain, warm, dur time.Duration) (PathRates, error) {
	src, err := sourceElements(kind)
	if err != nil {
		return PathRates{}, err
	}
	desc := captureBranch(src, format, l, chain) + " ! queue max-size-buffers=2 leaky=downstream ! identity name=out ! fakesink sync=false"
	r := PathRates{Launch: desc}
	p, err := launch(desc)
	if err != nil {
		return r, err
	}
	defer p.Stop()
	if _, err := paceCapture(p, l.FPS); err != nil {
		return r, err
	}
	var cs []*gst.Counter
	var capEl *gst.Element
	for _, n := range []string{"captured", "offered", "out"} {
		e, err := p.element(n)
		if err != nil {
			return r, err
		}
		c, ok := e.CountBuffers()
		if !ok {
			return r, errors.New("can't count buffers")
		}
		cs = append(cs, c)
		if n == "captured" {
			capEl = e
		}
	}
	gaps, ok := capEl.WatchGaps(1 << 14)
	defer func() {
		p.Stop()
		for _, c := range cs {
			c.Free()
		}
		if ok {
			gaps.Free()
		}
	}()
	if err := p.start(); err != nil {
		return r, err
	}
	wait := func(d time.Duration) error {
		select {
		case err := <-p.Err:
			if err == nil {
				err = errors.New("ended early")
			}
			return err
		case <-time.After(d):
			return nil
		}
	}
	if err := wait(warm); err != nil {
		return r, err
	}
	load := func() []int {
		var v []int
		for _, c := range cs {
			v = append(v, c.Load())
		}
		return v
	}
	if ok {
		gaps.Take()
	}
	c0, t0 := load(), time.Now()
	err = wait(dur)
	c1 := load()
	r.Secs = time.Since(t0).Seconds()
	r.Captured = float64(c1[0]-c0[0]) / r.Secs
	r.Offered = float64(c1[1]-c0[1]) / r.Secs
	r.Out = float64(c1[2]-c0[2]) / r.Secs
	r.CaptureCaps = ShortCaps(capEl.SrcCaps())
	if ok {
		r.Gaps, r.PTSGaps = gaps.Take()
	}
	return r, err
}

// Rate is a measured frame rate.
type Rate struct {
	FPS    float64
	Frames int
	Secs   float64
	Caps   string // what the counted element negotiated, when known
	Launch string // the pipeline
}

// benchPattern moves every frame (the bars scroll) and isn't flat, so the
// encoder has real work; it's still kinder than a busy game.
const benchPattern = "videotestsrc is-live=false pattern=smpte horizontal-speed=8"

// BenchEncoder encodes a synthetic picture at l's size as fast as factory
// can (the source isn't live), with a share's settings, and measures the
// access units out per second after warm.
func BenchEncoder(factory string, l Layer, warm, dur time.Duration) (Rate, error) {
	Init()
	var enc encoder
	for _, e := range encoders {
		if e.factory == factory {
			enc = e
		}
	}
	if enc.factory == "" {
		return Rate{}, fmt.Errorf("%s: not a share encoder", factory)
	}
	if !gst.HasFactory(factory) {
		return Rate{}, ErrNoFactory
	}
	// The queue gives the encoder its own thread, as in a share, so the
	// source's drawing doesn't add to the encoder's time.
	desc := fmt.Sprintf("%s ! video/x-raw,format=%s,width=%d,height=%d,framerate=%d/1,pixel-aspect-ratio=1/1 ! queue max-size-buffers=4 ! %s ! "+
		"video/x-h264,profile=constrained-baseline ! h264parse name=counted config-interval=-1 ! "+
		"video/x-h264,stream-format=byte-stream,alignment=au ! fakesink sync=false",
		benchPattern, enc.format(), l.Width, l.Height, l.FPS, enc.launch(0, l))
	return measure(desc, warm, dur)
}

// BenchSource is BenchEncoder's source alone: whether it, not the encoder,
// bounds the encoder's number.
func BenchSource(l Layer, format string, warm, dur time.Duration) (Rate, error) {
	desc := fmt.Sprintf("%s ! video/x-raw,format=%s,width=%d,height=%d,framerate=%d/1,pixel-aspect-ratio=1/1 ! "+
		"identity name=counted ! fakesink sync=false", benchPattern, format, l.Width, l.Height, l.FPS)
	return measure(desc, warm, dur)
}

// ProbeCapture captures kind the way a share does, asking for fps frames a
// second (0: let the source pick, as a share does on Windows), and counts
// what arrives, with nothing after it.
func ProbeCapture(kind SourceKind, fps int, warm, dur time.Duration) (Rate, error) {
	src, err := sourceElements(kind)
	if err != nil {
		return Rate{}, err
	}
	return measure(downloaded(src, fps)+" ! identity name=counted ! fakesink sync=false", warm, dur)
}

// measure runs desc, waits warm, then counts the buffers out of the element
// named "counted" for dur.
func measure(desc string, warm, dur time.Duration) (Rate, error) {
	p, err := launch(desc)
	if err != nil {
		return Rate{}, err
	}
	r := Rate{Launch: desc}
	e, err := p.element("counted")
	if err != nil {
		p.Stop()
		return r, err
	}
	n, ok := e.CountBuffers()
	if !ok {
		p.Stop()
		return r, errors.New("can't count buffers")
	}
	defer func() { p.Stop(); n.Free() }()
	if err := p.start(); err != nil {
		return r, err
	}
	wait := func(d time.Duration) error {
		select {
		case err := <-p.Err:
			if err == nil {
				err = errors.New("ended early")
			}
			return err
		case <-time.After(d):
			return nil
		}
	}
	if err := wait(warm); err != nil {
		return r, err
	}
	c0, t0 := n.Load(), time.Now()
	err = wait(dur)
	r.Frames, r.Secs = n.Load()-c0, time.Since(t0).Seconds()
	r.FPS = float64(r.Frames) / r.Secs
	r.Caps = ShortCaps(e.SrcCaps())
	return r, err
}

// ShortCaps drops the caps fields nobody reads in a report.
func ShortCaps(c string) string {
	var keep []string
	for i, f := range strings.Split(c, ", ") {
		if i == 0 || strings.HasPrefix(f, "format=") || strings.HasPrefix(f, "width=") ||
			strings.HasPrefix(f, "height=") || strings.HasPrefix(f, "framerate=") {
			keep = append(keep, f)
		}
	}
	return strings.Join(keep, ", ")
}
