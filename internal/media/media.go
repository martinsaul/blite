// Package media wraps the GStreamer pipelines Blite needs: a video source that
// captures and encodes ONCE into H.264 access units, and a player that decodes
// received access units. Pipelines are built as gst-launch strings so they can
// be reproduced with gst-launch-1.0 when debugging.
package media

import (
	"errors"
	"fmt"
	"log/slog"
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

// ContentMode is what the share is tuned for.
type ContentMode string

const (
	// ModeDesktop (detail, the default): 30 fps; when the link or the CPU is
	// tight, lower the frame rate before the resolution, so text stays sharp.
	ModeDesktop ContentMode = "desktop"
	// ModeGame (motion): 60 fps; when tight, keep the frame rate and lower
	// the resolution.
	ModeGame ContentMode = "game"
)

// ValidMode reports whether m is a content mode ("" = desktop).
func ValidMode(m ContentMode) bool { return m == "" || m == ModeDesktop || m == ModeGame }

// HighFPS is the HIGH layer's frame rate in mode m.
func HighFPS(m ContentMode) int {
	if m == ModeGame {
		return 60
	}
	return 30
}

// atFPS is l at fps, with the bitrate scaled sublinearly for the frame rate
// (60 fps needs about 1.5x of 30, 15 fps about 0.75x: consecutive frames
// differ less).
func atFPS(l Layer, fps int) Layer {
	if fps <= 0 || fps == l.FPS {
		return l
	}
	l.BitrateKbs = max(300, l.BitrateKbs*(l.FPS+fps)/(2*l.FPS))
	l.FPS = fps
	return l
}

// ValidResolution reports whether r is one StartVideoSource accepts ("" = 720p).
func ValidResolution(r Resolution) bool {
	_, ok := highLayers[r]
	return ok || r == ""
}

// Layer IDs. HIGH is the chosen resolution and LOW (360p15 by default, or the
// configured floor) runs when a smaller copy is enabled; the renditions are
// in-between sizes encoded only while a viewer was granted one. IDs are fixed
// per size so a ladder change keeps each viewer on the size it had.
const (
	High      = 0
	Low       = 1
	Rend1080  = 2
	Rend720   = 3
	Rend540   = 4
	NumLayers = 5
)

var (
	lowLayer = Layer{Width: 640, Height: 360, FPS: 15, BitrateKbs: 500}
	rend540  = Layer{Width: 960, Height: 540, FPS: 30, BitrateKbs: 1200}
)

// LowLayerFor returns the floor LOW layer for minHeight (360, 540 or 720).
func LowLayerFor(minHeight int) Layer {
	switch {
	case minHeight >= 720:
		return Layer{Width: 1280, Height: 720, FPS: 15, BitrateKbs: 1800}
	case minHeight >= 540:
		return Layer{Width: 960, Height: 540, FPS: 15, BitrateKbs: 1000}
	default:
		return lowLayer
	}
}

// rendLayers are the renditions by height, with their layer IDs.
var rendLayers = map[int]struct {
	id int
	l  Layer
}{
	1080: {Rend1080, highLayers[Res1080]},
	720:  {Rend720, highLayers[Res720]},
	540:  {Rend540, rend540},
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
// offer, largest first: 4K has 1080p, 720p and 540p; 1080p has 720p and 540p;
// 720p has 540p.
func RenditionsBelow(top int) []int {
	var out []int
	for _, h := range []int{1080, 720, 540} {
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

// LayerOpts configures the layers a video source encodes.
type LayerOpts struct {
	Resolution  Resolution
	FPS         int   // 10..60 (0 -> 30)
	MinHeight   int   // floor height: 360 (default), 540 or 720
	SmallerCopy *bool // encode LOW / renditions below HIGH (nil -> true)
	Renditions  []int
}

// LayersForOpts returns the layers to encode for o, indexed by layer ID.
// When SmallerCopy is false or MinHeight >= HIGH's height, only HIGH runs.
func LayersForOpts(o LayerOpts) []Layer {
	hi, ok := highLayers[o.Resolution]
	if !ok {
		hi = highLayers[Res720]
	}
	fps := o.FPS
	if fps <= 0 {
		fps = 30
	}
	fps = min(max(fps, 10), 60)
	out := make([]Layer, NumLayers)
	out[High] = atFPS(hi, fps)

	smaller := o.SmallerCopy == nil || *o.SmallerCopy
	minH := o.MinHeight
	if minH <= 0 {
		minH = 360
	}
	if !smaller || minH >= hi.Height {
		return out
	}

	low := LowLayerFor(minH)
	if fps < low.FPS {
		low.FPS = fps
	}
	out[Low] = low
	for _, h := range o.Renditions {
		if rl, ok := rendLayers[h]; ok && minH < h && h < hi.Height {
			out[rl.id] = atFPS(rl.l, fps)
		}
	}
	return out
}

// LayersFor is what we encode, indexed by layer ID: HIGH at the chosen
// resolution, LOW, and the renditions in rends (heights below the top; others
// are ignored). Absent IDs are zero Layers. Keyframes every 2 s, plus on
// demand (ForceKeyframe) when a peer switches layer or reports loss.
func LayersFor(r Resolution, m ContentMode, rends []int) []Layer {
	return LayersForOpts(LayerOpts{
		Resolution: r,
		FPS:        HighFPS(m),
		MinHeight:  360,
		Renditions: rends,
	})
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
			// Windows Graphics Capture, in GPU memory (captureChain).
			return "d3d11screencapturesrc show-cursor=true capture-api=wgc", nil
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
			return fmt.Sprintf("d3d11screencapturesrc window-handle=%d show-cursor=true capture-api=wgc", id), nil
		}
		return fmt.Sprintf("ximagesrc xid=%d use-damage=false show-pointer=true", id), nil
	case "monitor":
		if win {
			h, err := capture.ParseWindow(arg) // an HMONITOR parses like an HWND
			if err != nil {
				return "", fmt.Errorf("bad monitor %q", arg)
			}
			return fmt.Sprintf("d3d11screencapturesrc monitor-handle=%d show-cursor=true capture-api=wgc", h), nil
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

// Chain is where a share scales and converts what it captures.
type Chain string

const (
	ChainCPU Chain = "cpu" // videoconvertscale, in system memory
	ChainGPU Chain = "gpu" // d3d11convert, then only HIGH's frames downloaded
)

// ForceChain makes shares use one chain where they can ("": the best).
// blite-diag compares them.
var ForceChain Chain

// gpuChainFailed: a share's GPU chain failed where the CPU one worked, so
// shares convert on the CPU from then on.
var gpuChainFailed atomic.Bool

// d3d11Source reports whether src captures into GPU memory (Windows).
func d3d11Source(src string) bool { return strings.HasPrefix(src, "d3d11screencapturesrc ") }

// chainFor is the chain a share of src uses.
func chainFor(src string) Chain {
	if d3d11Source(src) && ForceChain != ChainCPU && gst.HasFactory("d3d11convert") &&
		(ForceChain == ChainGPU || !gpuChainFailed.Load()) {
		return ChainGPU
	}
	return ChainCPU
}

// downloaded is src in system memory, at up to fps frames a second (0:
// the source's choice).
func downloaded(src string, fps int) string {
	if d3d11Source(src) {
		src += " ! d3d11download"
	}
	if fps > 0 {
		src += fmt.Sprintf(" ! video/x-raw,framerate=%d/1", fps)
	}
	return src
}

// captureChain is src at up to l.FPS, then scaled to l's size and
// converted to format (in system memory). The identity counts the frames
// captured, before any conversion.
//
// The capture sources tick at the frame rate they negotiated, so the
// framerate caps right after them are what bound their grabs: ximagesrc
// grabs on its own clock (25 fps unless asked), and d3d11screencapturesrc
// picks 30 (it only hands on a frame when the screen changed, so a still
// screen costs nothing at any rate). Pacing them after would only drop
// frames already grabbed. The scaling comes before (or with) the
// conversion: a 5K screen converted at full size to I420 and scaled after
// cost ~6x the CPU per frame of videoconvertscale doing both at once.
//
// On ChainGPU the capture is scaled and converted on the GPU and only
// HIGH's frames come down: a 4K BGRA frame downloaded and converted on the
// CPU held a capture of 54 fps to 23-46.
func captureChain(src string, l Layer, format string, chain Chain) string {
	switch {
	case chain == ChainGPU && d3d11Source(src):
		return fmt.Sprintf("%s ! video/x-raw(memory:D3D11Memory),framerate=%d/1 ! identity name=captured ! "+
			"d3d11convert add-borders=true ! video/x-raw(memory:D3D11Memory),format=%s,width=%d,height=%d,pixel-aspect-ratio=1/1 ! d3d11download",
			src, l.FPS, format, l.Width, l.Height)
	case d3d11Source(src):
		// All cores: the converter is the bottleneck here, and a hardware
		// encoder leaves the CPU free.
		return downloaded(src, l.FPS) + " ! identity name=captured ! " + convertScale() + " n-threads=0"
	case strings.HasPrefix(src, "ximagesrc "):
		src = downloaded(src, l.FPS)
	}
	return src + " ! identity name=captured ! " + convertScale()
}

// convertScale scales then converts in one element where GStreamer has it
// (1.22+: videoconvertscale, which videoconvert and videoscale are made
// of), else scales in the source's format before converting.
func convertScale() string {
	if gst.HasFactory("videoconvertscale") {
		return "videoconvertscale"
	}
	return "videoscale ! videoconvert"
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
	// CBR: the default VBR spends far below the target on a mostly static
	// screen and leaves text soft (Alex's 4K share: 1.5-7 of 21 Mbit/s).
	// spatial-aq gives edges and text more bits. Both exist in the old
	// (CUDA) and the new (D3D11) nvh264enc.
	{"nvh264enc", "bitrate=%d gop-size=%d rc-mode=cbr spatial-aq=true"},
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

// format is the raw format we feed the encoder: x264 takes I420; the
// hardware encoders (QSV, AMF, Media Foundation, NVENC, VA) want NV12, and
// on Windows refuse to link at all with I420 (seen on 75: every share fell
// back to x264).
func (e encoder) format() string {
	if e.factory == "x264enc" {
		return "I420"
	}
	return "NV12"
}

// Log receives encoder fallbacks and choices; main points it at the app log.
var Log = slog.Default()

// AvailableEncoders names the installed encoders, best first (for logs).
func AvailableEncoders() []string {
	var out []string
	for _, e := range availableEncoders("") {
		out = append(out, e.factory)
	}
	return out
}

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
	stops   []func()      // run by Stop once the loops ended
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

// atStop has Stop run f once the loops ended (f frees what they used).
func (p *Pipeline) atStop(f func()) { p.stops = append(p.stops, f) }

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
		for _, f := range p.stops {
			f()
		}
		for _, e := range p.els {
			e.Release()
		}
		p.g.Free()
	})
}

// Done returns a channel closed when Stop runs.
func (p *Pipeline) Done() <-chan struct{} {
	return p.done
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
	preview *gst.Element // the self-preview valve (nil without a preview channel)
	// previewBig is the valve of the big preview (PreviewBigChannel); the
	// preview state is previewMu's.
	previewBig   *gst.Element
	previewMu    sync.Mutex
	previewOn    bool
	previewLarge bool
	previewed    *gst.Counter // frames through the valves (nil if unknown)
	previewedBig *gst.Counter
	offered      *gst.Counter // frames the capture gave the encoders (nil if unknown)
	thin         *gst.Thinner // HIGH's frame rate step (nil if unknown)
	pacer        *pacer
	captured     *gst.Counter // frames out of the capture, before conversion (nil if unknown)
	capturedEl   *gst.Element
	dequeued     *gst.Counter // frames out of HIGH's leaky queue (nil if unknown)
	gaps         *gst.Gaps    // between captured frames, once WatchCaptureGaps ran
	chain        Chain
	life         sync.RWMutex
	lastKey      []time.Time
	keyMu        sync.Mutex
	live         sync.Once // counted out of liveSources once
	idleMu       sync.Mutex
	idle         bool // paused by SetIdle (idleMu)
}

// liveSources counts started, not yet stopped, video sources (tests).
var liveSources atomic.Int64

// LiveVideoSources reports how many video sources run (tests).
func LiveVideoSources() int { return int(liveSources.Load()) }

// StartVideoSource captures from kind once, encodes every layer of
// LayersFor(res, mode, rends), and calls onAU(layer, au) from that layer's pull goroutine. onAU must not block.
// A non-empty preview is the intervideo channel SetPreview feeds with our
// picture (PreviewW x PreviewH at PreviewFPS) for a self-preview.
func StartVideoSource(kind SourceKind, res Resolution, mode ContentMode, rends []int, encoderForce, preview string, onAU func(layer int, au AccessUnit)) (*VideoSource, string, error) {
	return StartVideoSourceOpts(kind, LayerOpts{
		Resolution: res,
		FPS:        HighFPS(mode),
		MinHeight:  360,
		Renditions: rends,
	}, encoderForce, preview, onAU)
}

// StartVideoSourceOpts captures from kind once and encodes every active layer
// of LayersForOpts(opts).
func StartVideoSourceOpts(kind SourceKind, opts LayerOpts, encoderForce, preview string, onAU func(layer int, au AccessUnit)) (*VideoSource, string, error) {
	return StartVideoSourceLayers(kind, LayersForOpts(opts), encoderForce, preview, onAU)
}

// StartVideoSourceLayers captures from kind once and encodes layers, indexed
// by layer ID (zero Layers are skipped).
func StartVideoSourceLayers(kind SourceKind, layers []Layer, encoderForce, preview string, onAU func(layer int, au AccessUnit)) (*VideoSource, string, error) {
	encs := availableEncoders(encoderForce)
	if len(encs) == 0 {
		return nil, "", errors.New("no H.264 encoder available (install gstreamer1.0-plugins-ugly for x264enc)")
	}
	var lastErr error
	for _, enc := range encs {
		vs, err := startVideoSource(kind, layers, enc, preview, onAU)
		if err == nil {
			liveSources.Add(1)
			return vs, enc.factory, nil
		}
		Log.Warn("encoder failed, trying the next", "encoder", enc.factory, "height", layers[High].Height, "err", err)
		lastErr = fmt.Errorf("%s: %w", enc.factory, err)
	}
	return nil, "", lastErr
}

// captureBranch is a share's capture up to the encoders' tee: captured,
// converted, and paced to hi by paceCapture, from the appsink fresh to the
// appsrc paced. The fresh appsink doesn't wait for a frame to preroll: the
// sinks after paced get none until it plays. The identities count what the
// capture delivers (captured) and what the encoders are offered (offered).
func captureBranch(src, format string, hi Layer, chain Chain) string {
	size := fmt.Sprintf("format=%s,width=%d,height=%d,pixel-aspect-ratio=1/1", format, hi.Width, hi.Height)
	return fmt.Sprintf("%s ! video/x-raw,%s ! "+
		"appsink name=fresh sync=false async=false max-buffers=1 drop=true enable-last-sample=false  "+
		"appsrc name=paced is-live=true format=time do-timestamp=true max-buffers=2 leaky-type=downstream ! "+
		"video/x-raw,%s,framerate=%d/1 ! identity name=offered",
		captureChain(src, hi, format, chain), size, size, hi.FPS)
}

// videorate paces LOW and the renditions to fps, filling short gaps with
// copies of the frame before but not long ones. Over maxDup the next frame
// comes with a discont at its own time instead of a burst of the copies
// missed (Windows capture, a still second: 53-178 fps that HIGH's leaky
// queue mostly dropped, which looked like a slow encoder). It holds the
// newest frame until the next: the pacer's repeats bring it out.
func videorate(fps int) string {
	return fmt.Sprintf("videorate max-duplication-time=%d", int64(maxDup(fps)))
}

// maxDup is the longest capture gap videorate fills, and how long the
// pacer repeats a frame: 100 ms, or two frames at fps if that's longer.
func maxDup(fps int) time.Duration {
	d := 100 * time.Millisecond
	if fps > 0 {
		d = max(d, 2*time.Second/time.Duration(fps))
	}
	return d
}

// startVideoSource starts a share of kind with enc. If its GPU chain
// fails, it tries the CPU's.
func startVideoSource(kind SourceKind, layers []Layer, enc encoder, preview string, onAU func(layer int, au AccessUnit)) (*VideoSource, error) {
	src, err := sourceElements(kind)
	if err != nil {
		return nil, err
	}
	chain := chainFor(src)
	vs, err := startVideoChain(src, chain, layers, enc, preview, onAU)
	if err == nil || chain != ChainGPU || ForceChain == ChainGPU {
		return vs, err
	}
	Log.Warn("converting the capture on the GPU failed, trying the CPU", "encoder", enc.factory, "err", err)
	if vs, err = startVideoChain(src, ChainCPU, layers, enc, preview, onAU); err == nil {
		gpuChainFailed.Store(true) // the encoder works, the GPU chain didn't
	}
	return vs, err
}

func startVideoChain(src string, chain Chain, layers []Layer, enc encoder, preview string, onAU func(layer int, au AccessUnit)) (*VideoSource, error) {
	hi := layers[High]
	branches := []string{captureBranch(src, enc.format(), hi, chain) + " ! tee name=t"}
	for i, l := range layers {
		if l.Height == 0 {
			continue
		}
		// HIGH: SetHighFPS drops frames at thin, before the queue, so
		// that thin counts what HIGH's encoder is offered: frames the
		// leaky queue drops because the encoder is slow count as offered.
		thin, scale := "identity name=thin ! ", ""
		if i != High {
			thin, scale = "", fmt.Sprintf("videoscale ! %s ! video/x-raw,width=%d,height=%d,framerate=%d/1 ! ", videorate(l.FPS), l.Width, l.Height, l.FPS)
		}
		// The appsink never drops: a lost access unit breaks the references
		// of every frame after it, for every viewer, until the next IDR,
		// with no packet loss to tell. A slow consumer blocks the encoder
		// instead, and the leaky queue before it drops raw frames.
		branches = append(branches, fmt.Sprintf("t. ! %squeue name=q%d max-size-buffers=2 leaky=downstream ! %s%s ! "+
			"video/x-h264,profile=constrained-baseline ! "+
			"h264parse config-interval=-1 ! video/x-h264,stream-format=byte-stream,alignment=au ! "+
			"appsink name=out%d sync=false max-buffers=4 drop=false", thin, i, scale, enc.launch(i, l), i))
	}
	if preview != "" {
		// The valves drop everything until SetPreview opens one, so a
		// preview nobody looks at costs nothing. Two sizes on two
		// channels, not one capsfilter changed while running: frames of
		// the old size still on their way then broke videoconvert's
		// mapping (gst_video_frame_map_id criticals).
		for _, big := range []bool{false, true} {
			name, ch := "preview", preview
			if big {
				name, ch = "previewbig", PreviewBigChannel(preview)
			}
			branches = append(branches, fmt.Sprintf("t. ! queue max-size-buffers=1 leaky=downstream ! valve name=%s drop=true ! "+
				"videoscale ! videorate drop-only=true ! %s ! identity name=%sed ! intervideosink channel=%s sync=false async=false",
				name, previewCaps(big, hi.Width, hi.Height, hi.FPS), name, ch))
		}
	}
	p, err := launch(strings.Join(branches, "  "))
	if err != nil {
		return nil, err
	}
	pc, err := paceCapture(p, hi.FPS)
	if err != nil {
		p.Stop()
		return nil, err
	}
	vs := &VideoSource{Pipeline: p, pacer: pc, chain: chain, lastKey: make([]time.Time, len(layers)),
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
	if e, err := p.element("offered"); err == nil {
		vs.offered, _ = e.CountBuffers()
	}
	if e, err := p.element("thin"); err == nil {
		vs.thin, _ = e.Thin(hi.FPS)
	}
	if e, err := p.element("captured"); err == nil {
		vs.captured, _ = e.CountBuffers()
		vs.capturedEl = e
	}
	if e, err := p.element("q0"); err == nil {
		vs.dequeued, _ = e.CountBuffers()
	}
	if preview != "" {
		if vs.preview, err = p.element("preview"); err != nil {
			p.Stop()
			return nil, err
		}
		if vs.previewBig, err = p.element("previewbig"); err != nil {
			p.Stop()
			return nil, err
		}
		if e, err := p.element("previewed"); err == nil {
			vs.previewed, _ = e.CountBuffers()
		}
		if e, err := p.element("previewbiged"); err == nil {
			vs.previewedBig, _ = e.CountBuffers()
		}
	}
	if err := p.start(); err != nil {
		vs.Stop()
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
		vs.pacer.repeat()
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

// SetHighFPS caps the frames HIGH encodes per second (its full rate or
// more: all). The encoders budget bits per frame, so fewer frames also
// means fewer bits, while each frame stays as sharp.
func (vs *VideoSource) SetHighFPS(fps int) {
	vs.life.RLock()
	defer vs.life.RUnlock()
	if vs.thin != nil && !vs.stopped() {
		vs.thin.SetFPS(fps)
	}
}

// SetIdle pauses (idle) or resumes the capture. Nobody watching and no
// self-preview: a paused live source grabs nothing (no XGetImage, no
// conversion, no encoding) instead of feeding valves that drop it. Resuming
// asks every encoder for a keyframe, so viewers see a picture at once; the
// running time stood still while paused, so timestamps go on without a gap.
func (vs *VideoSource) SetIdle(idle bool) {
	vs.idleMu.Lock()
	defer vs.idleMu.Unlock()
	if vs.idle == idle {
		return
	}
	vs.life.RLock()
	defer vs.life.RUnlock()
	if vs.stopped() {
		return
	}
	if idle {
		if err := vs.g.Pause(); err != nil {
			Log.Warn("capture: pause failed", "err", err)
			return
		}
		vs.idle = true
		Log.Info("capture idle: nobody watching")
		return
	}
	if err := vs.g.Play(); err != nil {
		Log.Warn("capture: resume failed", "err", err)
		return
	}
	vs.idle = false
	Log.Info("capture resumed")
	now := time.Now()
	vs.keyMu.Lock()
	for i := range vs.lastKey {
		vs.lastKey[i] = now // ForceKeyframe's throttle counts these
	}
	vs.keyMu.Unlock()
	for _, s := range vs.sinks {
		if s != nil {
			s.ForceKeyframe()
		}
	}
	vs.pacer.repeat()
}

// Idle reports whether SetIdle paused the capture.
func (vs *VideoSource) Idle() bool {
	vs.idleMu.Lock()
	defer vs.idleMu.Unlock()
	return vs.idle
}

// SetPreview starts (on) or stops feeding the preview channel.
func (vs *VideoSource) SetPreview(on bool) {
	vs.previewMu.Lock()
	defer vs.previewMu.Unlock()
	vs.previewOn = on
	vs.setPreviewValvesLocked()
}

// SetPreviewBig feeds PreviewBigChannel at PreviewBigW x PreviewBigH
// instead of the preview channel at PreviewW x PreviewH (big: our preview
// shows as a tile or in its own window, not as the corner picture). Never
// more than the size we send.
func (vs *VideoSource) SetPreviewBig(big bool) {
	vs.previewMu.Lock()
	defer vs.previewMu.Unlock()
	vs.previewLarge = big
	vs.setPreviewValvesLocked()
}

func (vs *VideoSource) setPreviewValvesLocked() {
	if vs.preview == nil {
		return
	}
	vs.life.RLock()
	defer vs.life.RUnlock()
	if !vs.stopped() {
		vs.preview.SetBool("drop", !vs.previewOn || vs.previewLarge)
		vs.previewBig.SetBool("drop", !vs.previewOn || !vs.previewLarge)
	}
}

// PreviewBigChannel is the channel a VideoSource feeds the big preview on,
// for preview channel ch.
func PreviewBigChannel(ch string) string { return ch + "-big" }

// PreviewFrames is how many frames went to the preview channels (-1 if unknown).
func (vs *VideoSource) PreviewFrames() int {
	vs.life.RLock()
	defer vs.life.RUnlock()
	if vs.previewed == nil {
		return -1
	}
	n := vs.previewed.Load()
	if vs.previewedBig != nil {
		n += vs.previewedBig.Load()
	}
	return n
}

// OfferedFrames is how many frames the capture handed to the encoders (-1
// if unknown). A still window offers few: capture only delivers changes.
func (vs *VideoSource) OfferedFrames() int {
	vs.life.RLock()
	defer vs.life.RUnlock()
	if vs.offered == nil {
		return -1
	}
	return vs.offered.Load()
}

// HighOfferedFrames is how many frames HIGH's encoder was offered, after
// SetHighFPS thinned them and before a slow encoder's queue dropped any
// (-1 if unknown).
func (vs *VideoSource) HighOfferedFrames() int {
	vs.life.RLock()
	defer vs.life.RUnlock()
	if vs.thin == nil {
		return -1
	}
	return vs.thin.Passed()
}

// Chain is where the share scales and converts what it captures.
func (vs *VideoSource) Chain() Chain { return vs.chain }

// FrameCounts follows frames through a video source (-1 where unknown).
type FrameCounts struct {
	Captured int // out of the capture, before conversion and pacing
	Offered  int // to the encoders, after pacing (with its repeats)
	HighIn   int // into HIGH's leaky queue, after SetHighFPS
	HighOut  int // out of that queue to HIGH's encoder; HighIn-HighOut it dropped
	// CaptureCaps is what the capture negotiated (size, format, frame rate).
	CaptureCaps string
}

// Counts reads the frame counters.
func (vs *VideoSource) Counts() FrameCounts {
	vs.life.RLock()
	defer vs.life.RUnlock()
	c := FrameCounts{Captured: -1, Offered: -1, HighIn: -1, HighOut: -1}
	if vs.stopped() {
		return c
	}
	load := func(n *gst.Counter) int {
		if n == nil {
			return -1
		}
		return n.Load()
	}
	c.Captured, c.Offered, c.HighOut = load(vs.captured), load(vs.offered), load(vs.dequeued)
	if vs.thin != nil {
		c.HighIn = vs.thin.Passed()
	}
	if vs.capturedEl != nil {
		c.CaptureCaps = vs.capturedEl.SrcCaps()
	}
	return c
}

// WatchCaptureGaps starts recording the time between captured frames
// (TakeCaptureGaps reads it), to catch capture stalls.
func (vs *VideoSource) WatchCaptureGaps() bool {
	vs.life.Lock()
	defer vs.life.Unlock()
	if vs.gaps == nil && vs.capturedEl != nil && !vs.stopped() {
		vs.gaps, _ = vs.capturedEl.WatchGaps(1 << 14)
	}
	return vs.gaps != nil
}

// TakeCaptureGaps returns the gaps between captured frames since the last
// take, by arrival and by timestamp (nil before WatchCaptureGaps).
func (vs *VideoSource) TakeCaptureGaps() (arrival, pts []time.Duration) {
	vs.life.RLock()
	defer vs.life.RUnlock()
	if vs.gaps == nil || vs.stopped() {
		return nil, nil
	}
	return vs.gaps.Take()
}

// Stop tears the source down; in-flight ForceKeyframe calls finish first.
func (vs *VideoSource) Stop() {
	vs.life.Lock()
	defer vs.life.Unlock()
	vs.Pipeline.Stop()
	if vs.previewed != nil {
		vs.previewed.Free()
		vs.previewed = nil
	}
	if vs.previewedBig != nil {
		vs.previewedBig.Free()
		vs.previewedBig = nil
	}
	if vs.thin != nil {
		vs.thin.Free()
		vs.thin = nil
	}
	for _, c := range []**gst.Counter{&vs.captured, &vs.dequeued} {
		if *c != nil {
			(*c).Free()
			*c = nil
		}
	}
	if vs.gaps != nil {
		vs.gaps.Free()
		vs.gaps = nil
	}
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
	sink, channel, scale := "videoconvert ! autovideosink sync=false", "", ""
	switch view {
	case ViewCount:
		sink = "videoconvert ! video/x-raw,format=I420 ! appsink name=frames sync=false max-buffers=2 drop=true"
	case ViewGrid:
		channel = fmt.Sprintf("blite-tile-%d", tileSeq.Add(1))
		sink = "intervideosink channel=" + channel + " sync=false"
		scale = playerScale()
	}
	desc := "appsrc name=in is-live=true format=time do-timestamp=true " +
		"caps=video/x-h264,stream-format=byte-stream,alignment=au ! " +
		"h264parse ! decodebin ! identity name=decoded ! " + scale + "queue max-size-buffers=2 leaky=downstream ! " + sink
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
