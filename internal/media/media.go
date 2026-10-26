// Package media wraps the GStreamer pipelines Blite needs: a video source that
// captures and encodes ONCE into H.264 access units, and a player that decodes
// received access units. Pipelines are built as gst-launch strings so they can
// be reproduced with gst-launch-1.0 when debugging.
package media

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"blite/internal/capture"
	"blite/internal/gst"
)

// Init initialises GStreamer. Safe to call more than once.
func Init() { gst.Init() }

// Layer is one encoded quality level. Every capture is encoded once per layer.
type Layer struct {
	Width, Height, FPS int
	BitrateKbs         int
}

// Resolution is the HIGH layer the sharer chose: 720p (default), 1080p or
// 4K. It's the size we encode at, whatever the source's size (letterboxed).
type Resolution string

const (
	Res720  Resolution = "720p"
	Res1080 Resolution = "1080p"
	Res2160 Resolution = "2160p" // 4K UHD
)

// highLayers is the HIGH layer per resolution; the bitrates are for screen
// content at 30 fps.
var highLayers = map[Resolution]Layer{
	Res720:  {Width: 1280, Height: 720, FPS: 30, BitrateKbs: 2500},
	Res1080: {Width: 1920, Height: 1080, FPS: 30, BitrateKbs: 5000},
	Res2160: {Width: 3840, Height: 2160, FPS: 30, BitrateKbs: 14000},
}

// ValidResolution reports whether r is one StartVideoSource accepts ("" = 720p).
func ValidResolution(r Resolution) bool {
	_, ok := highLayers[r]
	return ok || r == ""
}

// Layer IDs. HIGH is the chosen resolution and LOW (360p15) always runs;
// the renditions are in-between sizes encoded only while a viewer was
// granted one. IDs are fixed per size so a ladder change keeps
// each viewer on the size it had.
const (
	High      = 0
	Low       = 1
	Rend1080  = 2
	Rend720   = 3
	NumLayers = 4
)

var lowLayer = Layer{Width: 640, Height: 360, FPS: 15, BitrateKbs: 500}

// rendLayers are the renditions by height, with their layer IDs.
var rendLayers = map[int]struct {
	id int
	l  Layer
}{
	1080: {Rend1080, highLayers[Res1080]},
	720:  {Rend720, highLayers[Res720]},
}

// LowerResolution is one size below r ("" below 720p).
func LowerResolution(r Resolution) Resolution {
	switch r {
	case Res2160:
		return Res1080
	case Res1080:
		return Res720
	}
	return ""
}

// ResolutionLabel is how the UI names r: 720p, 1080p, 4K.
func ResolutionLabel(r Resolution) string {
	if r == Res2160 {
		return "4K"
	}
	if r == "" {
		return string(Res720)
	}
	return string(r)
}

// TopHeight is the HIGH layer's height for r.
func TopHeight(r Resolution) int {
	hi, ok := highLayers[r]
	if !ok {
		hi = highLayers[Res720]
	}
	return hi.Height
}

// RenditionsBelow lists the rendition heights a top of height top can
// offer, largest first: 4K has 1080p and 720p, 1080p has 720p.
func RenditionsBelow(top int) []int {
	var out []int
	for _, h := range []int{1080, 720} {
		if h < top {
			out = append(out, h)
		}
	}
	return out
}

// RenditionLayer is the layer ID of a rendition height (-1 if none).
func RenditionLayer(height int) int {
	if r, ok := rendLayers[height]; ok {
		return r.id
	}
	return -1
}

// LayerHeight is the height of a layer ID given the top's height (0 if unknown).
func LayerHeight(id, top int) int {
	switch id {
	case High:
		return top
	case Low:
		return lowLayer.Height
	}
	for h, r := range rendLayers {
		if r.id == id {
			return h
		}
	}
	return 0
}

// LayersFor is what we encode, indexed by layer ID: HIGH at the chosen
// resolution, LOW, and the renditions in rends (heights below the top; others
// are ignored). Absent IDs are zero Layers. Keyframes every 2 s, plus on
// demand (ForceKeyframe) when a peer switches layer or reports loss.
func LayersFor(r Resolution, rends []int) []Layer {
	hi, ok := highLayers[r]
	if !ok {
		hi = highLayers[Res720]
	}
	out := make([]Layer, NumLayers)
	out[High], out[Low] = hi, lowLayer
	for _, h := range rends {
		if rl, ok := rendLayers[h]; ok && h < hi.Height {
			out[rl.id] = rl.l
		}
	}
	return out
}

// SourceKind selects what to capture.
type SourceKind string

const (
	SourceTest   SourceKind = "test"   // moving test pattern with a clock (synthetic peers)
	SourceScreen SourceKind = "screen" // the primary screen on this OS
	SourceX11    SourceKind = "x11"    // whole X11 screen (alias of screen on Linux)
)

// Sources also take "monitor:<arg>" and "window:<id>" from capture.List.
func sourceElements(k SourceKind) (string, error) {
	if k == SourceScreen {
		if runtime.GOOS == "windows" {
			// Windows Graphics Capture, downloaded from GPU memory for the encoders.
			return "d3d11screencapturesrc show-cursor=true capture-api=wgc ! d3d11download", nil
		}
		k = SourceX11
	}
	if kind, arg, ok := strings.Cut(string(k), ":"); ok {
		return targetElements(kind, arg)
	}
	switch k {
	case SourceTest:
		src := "videotestsrc is-live=true pattern=ball"
		if gst.HasFactory("timeoverlay") { // pango plugin; not in every minimal install
			src += " ! timeoverlay font-desc=\"Sans 36\""
		}
		return src, nil
	case SourceX11:
		return "ximagesrc use-damage=false show-pointer=true", nil
	}
	return "", fmt.Errorf("unknown source %q", k)
}

// targetElements captures one monitor or window. Windows of any size are
// letterboxed into the layer sizes by videoscale (add-borders is its default).
func targetElements(kind, arg string) (string, error) {
	win := runtime.GOOS == "windows"
	switch kind {
	case "window":
		id, err := capture.ParseWindow(arg)
		if err != nil {
			return "", err
		}
		if win {
			return fmt.Sprintf("d3d11screencapturesrc window-handle=%d show-cursor=true capture-api=wgc ! d3d11download", id), nil
		}
		return fmt.Sprintf("ximagesrc xid=%d use-damage=false show-pointer=true", id), nil
	case "monitor":
		if win {
			h, err := capture.ParseWindow(arg) // an HMONITOR parses like an HWND
			if err != nil {
				return "", fmt.Errorf("bad monitor %q", arg)
			}
			return fmt.Sprintf("d3d11screencapturesrc monitor-handle=%d show-cursor=true capture-api=wgc ! d3d11download", h), nil
		}
		r, err := capture.ParseMonitor(arg)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("ximagesrc startx=%d starty=%d endx=%d endy=%d use-damage=false show-pointer=true",
			r.X, r.Y, r.X+r.W-1, r.Y+r.H-1), nil
	}
	return "", fmt.Errorf("unknown source kind %q", kind)
}

// ValidSource reports whether k is a source kind StartVideoSource accepts.
func ValidSource(k SourceKind) bool {
	switch k {
	case SourceTest, SourceScreen, SourceX11:
		return true
	}
	kind, arg, ok := strings.Cut(string(k), ":")
	if !ok {
		return false
	}
	_, err := targetElements(kind, arg)
	return err == nil
}

// encoder is one candidate H.264 encoder, best first. props gets (kbit/s, GOP
// frames). Property names are the GStreamer 1.24+ ones; hardware entries are
// verified per machine.
type encoder struct {
	factory string
	props   string
}

// Windows hardware encoders get only bitrate and gop-size: the other flags'
// names differ between plugin generations (checked in the 1.26.11 MSVC DLLs),
// and an unknown property fails the whole pipeline. StartVideoSource also
// falls back to the next encoder if one refuses to start.
var encoders = []encoder{
	{"nvh264enc", "bitrate=%d gop-size=%d"},
	{"qsvh264enc", "bitrate=%d gop-size=%d"},
	{"amfh264enc", "bitrate=%d gop-size=%d"},
	{"vah264enc", "bitrate=%d key-int-max=%d b-frames=0 rate-control=cbr"},
	{"mfh264enc", "bitrate=%d gop-size=%d"},
	{"x264enc", "bitrate=%d key-int-max=%d bframes=0 tune=zerolatency speed-preset=veryfast"},
}

// availableEncoders returns installed encoders, best first. force names one.
func availableEncoders(force string) []encoder {
	Init()
	var out []encoder
	for _, e := range encoders {
		if force != "" && e.factory != force {
			continue
		}
		if gst.HasFactory(e.factory) {
			out = append(out, e)
		}
	}
	return out
}

// PickEncoder returns the best available encoder. force names one factory.
func PickEncoder(force string) (encoder, error) {
	if es := availableEncoders(force); len(es) > 0 {
		return es[0], nil
	}
	return encoder{}, errors.New("no H.264 encoder available (install gstreamer1.0-plugins-ugly for x264enc)")
}

// Factory is the encoder's GStreamer element name.
func (e encoder) Factory() string { return e.factory }

// launch is the encoder element for layer i, named enc<i>. Every encoder in
// the list takes its bitrate in kbit/s and allows changing it while PLAYING.
func (e encoder) launch(i int, l Layer) string {
	return fmt.Sprintf("%s name=enc%d ", e.factory, i) + fmt.Sprintf(e.props, l.BitrateKbs, 2*l.FPS)
}

// AccessUnit is one encoded H.264 frame in Annex-B byte-stream form.
type AccessUnit struct {
	Data     []byte
	Duration time.Duration
}

// Pipeline is a running GStreamer pipeline. Err receives the first bus error
// (or end of stream).
type Pipeline struct {
	g       *gst.Pipeline
	els     []*gst.Element
	loops   []func() bool // registered before start, run after Play
	Err     chan error
	done    chan struct{}
	wg      sync.WaitGroup
	once    sync.Once
	started bool
}

func launch(desc string) (*Pipeline, error) {
	g, err := gst.Launch(desc)
	if err != nil {
		return nil, fmt.Errorf("parse pipeline: %w\n  %s", err, desc)
	}
	return &Pipeline{g: g, Err: make(chan error, 1), done: make(chan struct{})}, nil
}

func (p *Pipeline) element(name string) (*gst.Element, error) {
	e, err := p.g.ByName(name)
	if err != nil {
		return nil, err
	}
	p.els = append(p.els, e)
	return e, nil
}

// goLoop runs f repeatedly until Stop or until f returns false; f should return
// promptly (Pull uses a timeout). Loops registered before start begin only once
// the pipeline is PLAYING: appsink reports EOS in any earlier state, which would
// end a pull loop before the first sample.
func (p *Pipeline) goLoop(f func() bool) {
	if !p.started {
		p.loops = append(p.loops, f)
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			select {
			case <-p.done:
				return
			default:
			}
			if !f() {
				return
			}
		}
	}()
}

func (p *Pipeline) start() error {
	if err := p.g.Play(); err != nil {
		p.Stop()
		return err
	}
	p.started = true
	for _, f := range p.loops {
		p.goLoop(f)
	}
	p.loops = nil
	p.goLoop(func() bool {
		err := p.g.BusError(200 * time.Millisecond)
		if err == nil {
			return true
		}
		select {
		case p.Err <- err:
		default:
		}
		return false
	})
	return nil
}

// Stop tears the pipeline down: stop loops, NULL the pipeline (unblocking
// pulls), wait for loops, then free references. Safe to call more than once.
func (p *Pipeline) Stop() {
	p.once.Do(func() {
		close(p.done)
		p.g.SetNull()
		p.wg.Wait()
		for _, e := range p.els {
			e.Release()
		}
		p.g.Free()
	})
}

func (p *Pipeline) stopped() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// VideoSource is a running capture with one encoder per layer.
type VideoSource struct {
	*Pipeline
	sinks   []*gst.Element
	encs    []*gst.Element
	life    sync.RWMutex
	lastKey []time.Time
	keyMu   sync.Mutex
	live    sync.Once // counted out of liveSources once
}

// liveSources counts started, not yet stopped, video sources (tests).
var liveSources atomic.Int64

// LiveVideoSources reports how many video sources run (tests).
func LiveVideoSources() int { return int(liveSources.Load()) }

// StartVideoSource captures from kind once, encodes every layer of
// LayersFor(res, rends), and calls onAU(layer, au) from that layer's pull goroutine. onAU must not block.
func StartVideoSource(kind SourceKind, res Resolution, rends []int, encoderForce string, onAU func(layer int, au AccessUnit)) (*VideoSource, string, error) {
	encs := availableEncoders(encoderForce)
	if len(encs) == 0 {
		return nil, "", errors.New("no H.264 encoder available (install gstreamer1.0-plugins-ugly for x264enc)")
	}
	var lastErr error
	for _, enc := range encs {
		vs, err := startVideoSource(kind, LayersFor(res, rends), enc, onAU)
		if err == nil {
			liveSources.Add(1)
			return vs, enc.factory, nil
		}
		lastErr = fmt.Errorf("%s: %w", enc.factory, err)
	}
	return nil, "", lastErr
}

func startVideoSource(kind SourceKind, layers []Layer, enc encoder, onAU func(layer int, au AccessUnit)) (*VideoSource, error) {
	src, err := sourceElements(kind)
	if err != nil {
		return nil, err
	}
	hi := layers[High]
	branches := []string{fmt.Sprintf("%s ! videoconvert ! videoscale ! videorate ! "+
		"video/x-raw,format=I420,width=%d,height=%d,framerate=%d/1,pixel-aspect-ratio=1/1 ! tee name=t",
		src, hi.Width, hi.Height, hi.FPS)}
	for i, l := range layers {
		if l.Height == 0 {
			continue
		}
		scale := ""
		if i != High {
			scale = fmt.Sprintf("videoscale ! videorate ! video/x-raw,width=%d,height=%d,framerate=%d/1 ! ", l.Width, l.Height, l.FPS)
		}
		branches = append(branches, fmt.Sprintf("t. ! queue max-size-buffers=2 leaky=downstream ! %s%s ! "+
			"video/x-h264,profile=constrained-baseline ! "+
			"h264parse config-interval=-1 ! video/x-h264,stream-format=byte-stream,alignment=au ! "+
			"appsink name=out%d sync=false max-buffers=4 drop=true", scale, enc.launch(i, l), i))
	}
	p, err := launch(strings.Join(branches, "  "))
	if err != nil {
		return nil, err
	}
	vs := &VideoSource{Pipeline: p, lastKey: make([]time.Time, len(layers)),
		sinks: make([]*gst.Element, len(layers)), encs: make([]*gst.Element, len(layers))}
	for i, l := range layers {
		if l.Height == 0 {
			continue
		}
		sink, err := p.element(fmt.Sprintf("out%d", i))
		if err != nil {
			p.Stop()
			return nil, err
		}
		vs.sinks[i] = sink
		enc, err := p.element(fmt.Sprintf("enc%d", i))
		if err != nil {
			p.Stop()
			return nil, err
		}
		vs.encs[i] = enc
		layer, frame := i, time.Second/time.Duration(l.FPS)
		p.goLoop(func() bool {
			data, d, err := sink.Pull(100 * time.Millisecond)
			switch {
			case errors.Is(err, gst.ErrTimeout):
				return true
			case err != nil:
				return false
			}
			if d <= 0 || d > time.Second {
				d = frame
			}
			onAU(layer, AccessUnit{Data: data, Duration: d})
			return true
		})
	}
	if err := p.start(); err != nil {
		return nil, err
	}
	// A hardware encoder can accept PLAYING and fail a moment later (no
	// device, driver too old): give it a beat before declaring success.
	select {
	case err := <-p.Err:
		vs.Stop()
		return nil, err
	case <-time.After(300 * time.Millisecond):
	}
	return vs, nil
}

// ForceKeyframe asks a layer's encoder for a keyframe, at most once per 500 ms.
func (vs *VideoSource) ForceKeyframe(layer int) {
	if layer < 0 || layer >= len(vs.sinks) || vs.sinks[layer] == nil {
		return
	}
	vs.keyMu.Lock()
	if time.Since(vs.lastKey[layer]) < 500*time.Millisecond {
		vs.keyMu.Unlock()
		return
	}
	vs.lastKey[layer] = time.Now()
	vs.keyMu.Unlock()

	vs.life.RLock()
	defer vs.life.RUnlock()
	if !vs.stopped() {
		vs.sinks[layer].ForceKeyframe()
	}
}

// SetBitrate changes a layer's encoder bitrate (kbit/s) on the fly.
func (vs *VideoSource) SetBitrate(layer, kbps int) {
	if layer < 0 || layer >= len(vs.encs) || vs.encs[layer] == nil || kbps <= 0 {
		return
	}
	vs.life.RLock()
	defer vs.life.RUnlock()
	if !vs.stopped() {
		vs.encs[layer].SetUint("bitrate", uint(kbps))
	}
}

// Stop tears the source down; in-flight ForceKeyframe calls finish first.
func (vs *VideoSource) Stop() {
	vs.life.Lock()
	defer vs.life.Unlock()
	vs.Pipeline.Stop()
	vs.live.Do(func() { liveSources.Add(-1) })
}

// ViewKind selects how received video is shown.
type ViewKind string

const (
	ViewWindow ViewKind = "window" // native window (autovideosink)
	ViewCount  ViewKind = "count"  // decode only and count frames (headless tests)
	// ViewGrid feeds a tile of the shared grid window (see Grid) instead of
	// opening a window per stream.
	ViewGrid ViewKind = "grid"
)

// Player decodes H.264 access units pushed into it.
type Player struct {
	*Pipeline
	src     *gst.Element
	decoded *gst.Counter // frames out of the decoder (nil if unavailable)
	life    sync.RWMutex // Push holds it shared so Stop can't free src mid-push
	mu      sync.Mutex
	frames  int
	pushed  int
	// Channel is the intervideo channel a ViewGrid player feeds.
	Channel string
}

// StartPlayer starts a decoder pipeline.
func StartPlayer(view ViewKind) (*Player, error) {
	sink, channel := "videoconvert ! autovideosink sync=false", ""
	switch view {
	case ViewCount:
		sink = "videoconvert ! video/x-raw,format=I420 ! appsink name=frames sync=false max-buffers=2 drop=true"
	case ViewGrid:
		channel = fmt.Sprintf("blite-tile-%d", tileSeq.Add(1))
		sink = "intervideosink channel=" + channel + " sync=false"
	}
	desc := "appsrc name=in is-live=true format=time do-timestamp=true " +
		"caps=video/x-h264,stream-format=byte-stream,alignment=au ! " +
		"h264parse ! decodebin ! identity name=decoded ! queue max-size-buffers=2 leaky=downstream ! " + sink
	p, err := launch(desc)
	if err != nil {
		return nil, err
	}
	src, err := p.element("in")
	if err != nil {
		p.Stop()
		return nil, err
	}
	pl := &Player{Pipeline: p, src: src, Channel: channel}
	if dec, err := p.element("decoded"); err == nil {
		pl.decoded, _ = dec.CountBuffers()
	}
	if view == ViewCount {
		fs, err := p.element("frames")
		if err != nil {
			p.Stop()
			return nil, err
		}
		p.goLoop(func() bool {
			_, _, err := fs.Pull(100 * time.Millisecond)
			switch {
			case errors.Is(err, gst.ErrTimeout):
				return true
			case err != nil:
				return false
			}
			pl.mu.Lock()
			pl.frames++
			pl.mu.Unlock()
			return true
		})
	}
	if err := p.start(); err != nil {
		return nil, err
	}
	return pl, nil
}

// Push hands one Annex-B access unit to the decoder.
func (pl *Player) Push(au []byte) {
	pl.life.RLock()
	defer pl.life.RUnlock()
	if pl.stopped() {
		return
	}
	pl.mu.Lock()
	pl.pushed++
	pl.mu.Unlock()
	pl.src.Push(au)
}

// Progress returns the access units pushed so far and the frames the
// decoder has produced from them (-1 if unknown). A decoder that can't keep
// up falls ever further behind (decode lag).
func (pl *Player) Progress() (pushed, decoded int) {
	pl.mu.Lock()
	pushed = pl.pushed
	pl.mu.Unlock()
	pl.life.RLock()
	defer pl.life.RUnlock()
	if pl.decoded == nil {
		return pushed, -1
	}
	return pushed, pl.decoded.Load()
}

// Stop tears the player down; concurrent Push calls finish first.
func (pl *Player) Stop() {
	pl.life.Lock()
	defer pl.life.Unlock()
	pl.Pipeline.Stop()
	if pl.decoded != nil {
		pl.decoded.Free()
		pl.decoded = nil
	}
}

// Frames returns the number of decoded frames (ViewCount only).
func (pl *Player) Frames() int {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	return pl.frames
}
