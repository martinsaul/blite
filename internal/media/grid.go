package media

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"blite/internal/gst"
	"blite/internal/i18n"
)

// tileSeq numbers the intervideo channels of ViewGrid players.
var tileSeq atomic.Uint64

// Grid canvas: the window starts at this size and scales with the window.
const (
	gridW, gridH = 1920, 1080
	GridFPS      = 30 // video windows' usual frame rate
	// GridFastFPS is the frame rate for windows that show a faster stream.
	GridFastFPS = 60
)

// Tile is one stream in a video window.
type Tile struct {
	Channel string // the ViewGrid player's Channel (or self preview channel)
	Label   string // shown in the tile's top left corner
	// ID is the feed ID ("self" or peer ID); with GridConfig.OnControl the
	// tile gets overlay controls.
	ID        string
	Self      bool    // our own preview tile (no mute or volume slider)
	Muted     bool    // playback muted (remote tiles)
	Volume    float64 // playback gain 0..2 (remote tiles)
	Pinned    bool    // takes the primary area of the main window
	PoppedOut bool    // shown in its own pop-out window
	// PiP shows the tile small in a corner, over the others, instead of
	// in the grid (our own preview). A PiP tile alone fills the window.
	PiP bool
	// Stopped: they stopped sharing. The tile says so on black instead of
	// their last frame (Channel unused) and has no controls.
	Stopped bool
}

// Control is a button or slider on a tile.
type Control string

const (
	ControlEye    Control = "eye"     // stop watching (hide preview for self)
	ControlPopOut Control = "pop_out" // pop out into own window, or pop back in
	ControlPin    Control = "pin"     // pin or unpin in the main window
	ControlMute   Control = "mute"    // toggle their audio
	// ControlVolume sets their playback volume (0..2) from the slider.
	ControlVolume Control = "volume"
	// ControlExpand turns our preview from a corner picture (PiP) into a
	// tile, or back.
	ControlExpand Control = "expand"
)

// GridConfig describes a grid or pop-out window.
type GridConfig struct {
	Tiles      []Tile
	Rows, Cols int      // 0 = auto (GridSize, or pinned + strip when a tile is Pinned)
	View       ViewKind // ViewWindow, or ViewCount for headless tests
	// Width x Height is the canvas (the window's first size); 0 = 1920x1080.
	Width, Height int
	// FPS is the window's frame rate; 0 = GridFPS. Streams faster than it
	// show at its rate.
	FPS int
	// OnControl gets the controls used on a tile with an ID (v: the
	// volume for ControlVolume); nil = no controls. It runs on its own
	// goroutine.
	OnControl func(id string, c Control, v float64)
	// Window is the native window to draw into; nil: the sink
	// opens its own.
	Window Window
}

// GridTileHeight is the height of a tile in a uniform grid of n tiles on the
// default canvas.
func GridTileHeight(n, rows, cols int) int {
	r, _ := GridSize(n, rows, cols)
	return gridH / r
}

// GridSize is the rows x columns for n tiles. rows and cols of 0 mean auto
// (as square as possible, wider than tall). A fixed count grows the other
// side when there are more tiles than cells, so no stream is left out.
func GridSize(n, rows, cols int) (int, int) {
	if n < 1 {
		n = 1
	}
	switch {
	case rows > 0 && cols > 0:
		if rows*cols < n {
			rows = (n + cols - 1) / cols
		}
	case cols > 0:
		rows = (n + cols - 1) / cols
	case rows > 0:
		cols = (n + rows - 1) / rows
	default:
		cols = int(math.Ceil(math.Sqrt(float64(n))))
		rows = (n + cols - 1) / cols
	}
	return rows, cols
}

// layoutRects places tiles on a cw x ch canvas. When one of 2+ tiles is
// Pinned (and rows/cols are auto), the pinned tile gets the primary 4/5 of
// the width and the others stack in a strip on the right. A PiP tile with
// others to show sits small in the top right corner of the grid (of the
// pinned tile, with a strip) over them. All coordinates and sizes are even
// for YUV / videoscale.
func layoutRects(tiles []Tile, cw, ch, rows, cols int) (rects []rect, outW, outH int) {
	pip := slices.IndexFunc(tiles, func(t Tile) bool { return t.PiP })
	if pip < 0 || len(tiles) < 2 {
		return gridRects(tiles, cw, ch, rows, cols)
	}
	rest := slices.Delete(slices.Clone(tiles), pip, pip+1)
	rr, outW, outH := gridRects(rest, cw, ch, rows, cols)
	area := rect{0, 0, outW, outH}
	if i := slices.IndexFunc(rest, func(t Tile) bool { return t.Pinned }); i >= 0 && len(rest) >= 2 && rows == 0 && cols == 0 {
		area = rr[i]
	}
	rects = slices.Insert(rr, pip, pipRect(area))
	return rects, outW, outH
}

// pipRect is the corner picture's place in area: a fifth of its width (at
// least 240 pixels), 16:9, a little in from its top right corner.
func pipRect(area rect) rect {
	w := min(max(area.w/5, 240), area.w/2) &^ 1
	h := (w * 9 / 16) &^ 1
	if h > area.h/2 {
		h = (area.h / 2) &^ 1
		w = (h * 16 / 9) &^ 1
	}
	m := max(area.w/80, 8) &^ 1
	return rect{area.x + area.w - w - m, area.y + m, w, h}
}

// gridRects is layoutRects without a PiP tile.
func gridRects(tiles []Tile, cw, ch, rows, cols int) (rects []rect, outW, outH int) {
	rects = make([]rect, len(tiles))
	pinIdx := slices.IndexFunc(tiles, func(t Tile) bool { return t.Pinned })
	if pinIdx >= 0 && len(tiles) >= 2 && rows == 0 && cols == 0 {
		outW, outH = cw&^1, ch&^1
		mainW := (outW * 4 / 5) &^ 1
		stripW := (outW - mainW) &^ 1
		rects[pinIdx] = rect{0, 0, mainW, outH}
		others := len(tiles) - 1
		stripCols := 1
		if others > 5 {
			stripCols = 2
		}
		stripRows := (others + stripCols - 1) / stripCols
		sw, sh := (stripW/stripCols)&^1, (outH/stripRows)&^1
		k := 0
		for i := range tiles {
			if i == pinIdx {
				continue
			}
			rects[i] = rect{mainW + (k%stripCols)*sw, (k / stripCols) * sh, sw, sh}
			k++
		}
		return rects, outW, outH
	}
	r, c := GridSize(len(tiles), rows, cols)
	tw, th := (cw/c)&^1, (ch/r)&^1
	for i := range tiles {
		rects[i] = rect{(i % c) * tw, (i / c) * th, tw, th}
	}
	return rects, tw * c, th * r
}

// The corner picture's frame: pipFrame pixels of pipFrameColor (ARGB) round it.
const (
	pipFrame      = 2
	pipFrameColor = 0xff5a6378
)

// overPiP reports whether tile i is a corner picture drawn over others.
func overPiP(tiles []Tile, i int) bool { return tiles[i].PiP && len(tiles) >= 2 }

// Grid is one window that shows one or more streams.
type Grid struct {
	*Pipeline
	feed *Pipeline // pushes the drawn controls and spinners (nil: none)
	mu   sync.Mutex
	out  *gst.Counter // frames out of the compositor (nil if unavailable)
	// Sink is the video sink drawing the window (for the log).
	Sink string
	win  Window
	// dropClicks stops the window handing its clicks to the sink (nil: it doesn't).
	unclick func()
	// The controls and spinners; ctl guards them.
	ctl struct {
		sync.Mutex
		tiles  []Tile
		rects  []rect
		labels []*gst.Element
		ov     []tileOverlay
		mix    *gst.Element
		on     func(id string, c Control, v float64)
		shown  int     // tile whose controls show (-1 none)
		alpha  float64 // their opacity, 0..1
		hover  Control
		moved  time.Time
		moves  uint    // the nav move counter last seen
		drag   int     // tile whose slider is being dragged (-1 none)
		phase  float64 // spinner
		fades  []fade  // per tile: fading out (Fade)
		big    []bool  // per tile: the name shows at full size
		done   bool    // Stop began: the elements may go
	}
}

// tileOverlay is one tile's drawn layers: the control bar and the spinner,
// each fed through its own appsrc into a compositor pad.
type tileOverlay struct {
	bar, spin       *gst.Element // appsrcs (nil: the tile has no controls)
	barPad, spinPad string
	barW, barH      int
	barX, barY      int // the bar's place on the canvas
	spinS           int
	wait            *gst.Element // "Waiting for video" text
	waiting         bool
	drawn           string // the bar state last pushed
	spinShown       bool
}

type rect struct{ x, y, w, h int }

// fade is a tile fading out: from at, over d; alpha is its opacity now.
type fade struct {
	at    time.Time
	d     time.Duration
	alpha float64
}

func (r rect) has(x, y float64) bool {
	return x >= float64(r.x) && x < float64(r.x+r.w) && y >= float64(r.y) && y < float64(r.y+r.h)
}

// interHold is how long an intervideosrc keeps showing the last frame
// before it turns black. Its default, 1 s, blanked a window share whenever
// the window sat still: Windows window capture only sends frames when the
// content changes.
const interHold = "timeout=3600000000000" // 1 h

// overlaySeq numbers the intervideo channels of drawn layers.
var overlaySeq atomic.Uint64

// gridStartTimeout is how long a sink drawing into our window may take to
// show its first frames.
const gridStartTimeout = 3 * time.Second

// StartGrid opens a window with cfg.Tiles laid out uniformly or with a pinned
// primary tile and a strip. With cfg.Window it draws into that window with
// the first sink that works there, else the sink opens a window of its own.
func StartGrid(cfg GridConfig) (*Grid, error) {
	switch {
	case cfg.View == ViewCount:
		// Headless: set g.win inside startGrid before the tick goroutine starts,
		// while skipping native window embedding on fakesink.
		return startGrid(cfg, "fakesink name=vsink sync=false", cfg.Window, false)
	case cfg.Window != nil:
		var errs []error
		for _, sink := range VideoWindowSinks() {
			if !gst.HasFactory(sink) {
				continue
			}
			g, err := startGrid(cfg, sink+" name=vsink sync=false force-aspect-ratio=true", cfg.Window, true)
			if err == nil {
				return g, nil
			}
			errs = append(errs, fmt.Errorf("%s: %w", sink, err))
		}
		g, err := startGrid(cfg, "autovideosink name=vsink sync=false", nil, false)
		if err != nil {
			return nil, errors.Join(append(errs, err)...)
		}
		g.Sink += " (" + errors.Join(errs...).Error() + ")"
		return g, nil
	}
	return startGrid(cfg, "autovideosink name=vsink sync=false", nil, false)
}

func startGrid(cfg GridConfig, sink string, win Window, embed bool) (*Grid, error) {
	tiles := cfg.Tiles
	cw, ch := cfg.Width, cfg.Height
	if cw <= 0 || ch <= 0 {
		cw, ch = gridW, gridH
	}
	fps := cfg.FPS
	if fps <= 0 {
		fps = GridFPS
	}
	rects, outW, outH := layoutRects(tiles, cw, ch, cfg.Rows, cfg.Cols)
	controls := cfg.OnControl != nil
	ovs := make([]tileOverlay, len(tiles))
	var b, feed strings.Builder
	fmt.Fprintf(&b, "compositor name=mix background=black")
	for i, r := range rects {
		fmt.Fprintf(&b, " sink_%d::xpos=%d sink_%d::ypos=%d", i, r.x, i, r.y)
	}
	// Drawn layers: two pads per tile with controls, above everything,
	// hidden (alpha 0) until shown.
	type layer struct {
		pad, ch string
		x, y    int
		w, h    int
		appsrc  string
	}
	var layers []layer
	next := len(tiles)
	for i, t := range tiles {
		if !controls || t.ID == "" || t.Stopped {
			continue
		}
		r := rects[i]
		o := &ovs[i]
		o.barW, o.barH = barSize(r.w, r.h, t)
		o.barX, o.barY = r.x+(r.w-o.barW)/2, r.y+r.h-o.barH-o.barH/3
		o.spinS = spinnerSize(r.w, r.h)
		seq := overlaySeq.Add(1)
		for k, l := range []layer{
			{x: o.barX, y: o.barY, w: o.barW, h: o.barH},
			{x: r.x + (r.w-o.spinS)/2, y: r.y + (r.h-o.spinS)/2, w: o.spinS, h: o.spinS},
		} {
			if t.Self && k == 1 {
				// Our own preview comes from local capture, so no network spinner.
				continue
			}
			l.pad = fmt.Sprintf("sink_%d", next)
			l.ch = fmt.Sprintf("blite-ov-%d-%d", seq, k)
			l.appsrc = fmt.Sprintf("ov%d_%d", i, k)
			next++
			layers = append(layers, l)
			if k == 0 {
				o.barPad = l.pad
			} else {
				o.spinPad = l.pad
			}
		}
	}
	// A corner picture goes over the other tiles, on a frame that sets it
	// apart from them, under the controls.
	var frame string // the frame's source (none: "")
	for i, t := range tiles {
		if !overPiP(tiles, i) || t.Stopped {
			continue
		}
		r := rects[i]
		pad := fmt.Sprintf("sink_%d", next)
		next++
		fmt.Fprintf(&b, " %s::xpos=%d %s::ypos=%d %s::zorder=%d sink_%d::zorder=%d", pad, r.x-pipFrame, pad, r.y-pipFrame, pad, next, i, next+1)
		frame = fmt.Sprintf(" videotestsrc is-live=true pattern=solid-color foreground-color=%#x ! "+
			"video/x-raw,width=%d,height=%d,framerate=%d/1 ! mix.%s", pipFrameColor, r.w+2*pipFrame, r.h+2*pipFrame, fps, pad)
	}
	for _, l := range layers {
		fmt.Fprintf(&b, " %s::xpos=%d %s::ypos=%d %s::zorder=%d %s::alpha=0", l.pad, l.x, l.pad, l.y, l.pad, next+2, l.pad)
	}
	fmt.Fprintf(&b, " ! video/x-raw,width=%d,height=%d,framerate=%d/1 ! identity name=out ! videoconvert ! %s", outW, outH, fps, sink)
	// intervideosrc reports a latency of 0..0 and a live videotestsrc one
	// frame..one frame: together the compositor's latency query fails
	// (min > max), logging an error on every query, ~180 a second. A
	// queue after each intervideosrc lifts its maximum.
	inter := ""
	for _, t := range tiles {
		if t.Stopped || frame != "" {
			inter = "queue max-size-buffers=1 max-size-bytes=0 max-size-time=0 ! "
		}
	}
	for i, t := range tiles {
		r := rects[i]
		if t.Stopped { // black, the text in the middle (showStateLocked sets it)
			fmt.Fprintf(&b, " videotestsrc is-live=true pattern=black ! "+
				"video/x-raw,width=%d,height=%d,framerate=%d/1,pixel-aspect-ratio=1/1 ! "+
				"textoverlay name=label%d valignment=center halignment=center font-desc=\"Sans 28\" ! mix.sink_%d",
				r.w, r.h, fps, i, i)
			continue
		}
		wait := ""
		if ovs[i].spinPad != "" {
			wait = fmt.Sprintf("textoverlay name=wait%d silent=true text=%s valignment=position ypos=0.66 halignment=center font-desc=\"Sans 16\" shaded-background=true ! ", i, launchQuote(i18n.L("Waiting for video")))
		}
		// The framerate sets intervideosrc's, which is 30 unless asked.
		fmt.Fprintf(&b, " intervideosrc channel=%s %s ! %svideoconvert ! videoscale add-borders=true ! "+
			"video/x-raw,width=%d,height=%d,framerate=%d/1,pixel-aspect-ratio=1/1,interlace-mode=progressive ! "+
			"textoverlay name=label%d valignment=top halignment=left font-desc=\"%s\" shaded-background=true ! %smix.sink_%d",
			t.Channel, interHold, inter, r.w, r.h, fps, i, labelFont(ovs[i].barPad == ""), wait, i)
	}
	b.WriteString(frame)
	for _, l := range layers {
		// intervideosrc repeats the last frame we drew, like the tiles.
		fmt.Fprintf(&b, " intervideosrc channel=%s %s ! %svideoconvert ! mix.%s", l.ch, interHold, inter, l.pad)
		// async=false: a layer not drawn yet (the spinner until a stream
		// waits) would hold the feed short of PLAYING, and the bar's sink
		// would block on its first frame, never showing a redraw.
		fmt.Fprintf(&feed, " appsrc name=%s is-live=true format=time do-timestamp=true "+
			"caps=video/x-raw,format=RGBA,width=%d,height=%d,framerate=0/1 ! intervideosink channel=%s sync=false async=false ",
			l.appsrc, l.w, l.h, l.ch)
	}
	g := &Grid{win: win}
	if feed.Len() > 0 {
		fp, err := launch(feed.String())
		if err != nil {
			return nil, err
		}
		g.feed = fp
		for i := range ovs {
			if ovs[i].barPad == "" {
				continue
			}
			ovs[i].bar, err = fp.element(fmt.Sprintf("ov%d_0", i))
			if err == nil && ovs[i].spinPad != "" {
				ovs[i].spin, err = fp.element(fmt.Sprintf("ov%d_1", i))
			}
			if err != nil {
				fp.Stop()
				return nil, err
			}
		}
		if err := fp.start(); err != nil {
			fp.Stop()
			return nil, err
		}
	}
	fail := func(err error) (*Grid, error) {
		g.dropClicks()
		if g.Pipeline != nil {
			g.Pipeline.Stop()
		}
		if g.feed != nil {
			g.feed.Stop()
		}
		g.free()
		return nil, err
	}
	p, err := launch(b.String())
	if err != nil {
		return fail(err)
	}
	g.Pipeline = p
	g.Sink = strings.Fields(sink)[0]
	g.ctl.tiles, g.ctl.rects, g.ctl.on, g.ctl.shown, g.ctl.drag = slices.Clone(tiles), rects, cfg.OnControl, -1, -1
	g.ctl.labels, g.ctl.ov = make([]*gst.Element, len(tiles)), ovs
	g.ctl.fades = make([]fade, len(tiles))
	g.ctl.big = make([]bool, len(tiles))
	if g.ctl.mix, err = p.element("mix"); err != nil {
		return fail(err)
	}
	for i := range tiles {
		if g.ctl.labels[i], err = p.element(fmt.Sprintf("label%d", i)); err != nil {
			return fail(err)
		}
		if ovs[i].spinPad != "" {
			if g.ctl.ov[i].wait, err = p.element(fmt.Sprintf("wait%d", i)); err != nil {
				return fail(err)
			}
			g.pushSpinnerLocked(i)
		}
		if ovs[i].barPad != "" {
			g.pushBarLocked(i)
		}
		g.showStateLocked(i)
	}
	if embed && win != nil {
		vs, err := p.element("vsink")
		if err != nil {
			return fail(err)
		}
		if !vs.SetWindow(win.Handle()) {
			return fail(errors.New("not a video overlay"))
		}
		if bw, ok := win.(buttonWindow); ok && controls {
			// The window takes the clicks the sink doesn't; the sink maps
			// them to the canvas and sends them up to the nav watch.
			g.unclick = bw.SetButtonHandler(func(press bool, button int, x, y float64) { vs.SendButton(press, button, x, y) })
		}
	}
	out, err := p.element("out")
	if err == nil {
		g.out, _ = out.CountBuffers()
	}
	stopped := slices.ContainsFunc(tiles, func(t Tile) bool { return t.Stopped })
	if controls || stopped { // stopped tiles fade out in tick
		var nav *gst.Nav
		if out != nil && controls {
			nav = out.WatchNav()
		}
		p.goLoop(func() bool {
			time.Sleep(controlTick)
			g.tick(nav, time.Now())
			return true
		})
	}
	if err := p.start(); err != nil {
		return fail(err)
	}
	if embed && win != nil && g.out != nil {
		// A sink can accept PLAYING and fail on its first frame (no Xv
		// port, no Direct3D device): see frames go through first.
		deadline := time.Now().Add(gridStartTimeout)
		for g.out.Load() < 3 {
			select {
			case err := <-p.Err:
				return fail(err)
			case <-time.After(50 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				return fail(errors.New("no frame drawn"))
			}
		}
	}
	return g, nil
}

// The controls show while the pointer moves over the window, fade out
// after it rested for controlShow, and go at once when it leaves.
const (
	controlShow = 5 * time.Second
	controlFade = time.Second
	controlTick = 50 * time.Millisecond
	spinTurn    = 1200 * time.Millisecond // one spinner turn
)

// labelFont is a tile's name: full size while its controls show, a quarter
// of that otherwise so it covers little of the picture.
func labelFont(big bool) string {
	if big {
		return "Sans 14"
	}
	return "Sans 3.5"
}

// tick handles the pointer events since the last call, fades the controls
// and the tiles going away, and turns the spinners.
func (g *Grid) tick(nav *gst.Nav, now time.Time) {
	var st gst.NavState
	if nav != nil {
		st = nav.Poll()
	}
	inside := g.win == nil || g.win.PointerInside()
	g.ctl.Lock()
	defer g.ctl.Unlock()
	if g.ctl.done {
		return
	}
	if nav != nil {
		g.pointerLocked(st, now)
	}
	// opacity
	a := 1.0
	switch since := now.Sub(g.ctl.moved); {
	case g.ctl.drag >= 0:
	case !inside || g.ctl.moved.IsZero():
		a = 0
	case since > controlShow:
		a = max(0, 1-float64(since-controlShow)/float64(controlFade))
	}
	if i := g.ctl.shown; i >= 0 && g.ctl.ov[i].barPad != "" && math.Abs(a-g.ctl.alpha) > 0.01 {
		g.ctl.alpha = a
		g.ctl.mix.SetPadDouble(g.ctl.ov[i].barPad, "alpha", a)
	}
	// names: full size with the controls, small otherwise
	for i := range g.ctl.big {
		if g.ctl.ov[i].barPad == "" {
			continue
		}
		if big := i == g.ctl.shown && a > 0; big != g.ctl.big[i] {
			g.ctl.big[i] = big
			g.ctl.labels[i].SetString("font-desc", labelFont(big))
		}
	}
	// tiles going away
	for i := range g.ctl.fades {
		f := &g.ctl.fades[i]
		if f.at.IsZero() || f.alpha <= 0 {
			continue
		}
		a := 0.0
		if f.d > 0 {
			a = max(0, 1-float64(now.Sub(f.at))/float64(f.d))
		}
		if math.Abs(a-f.alpha) > 0.01 || a == 0 {
			f.alpha = a
			g.ctl.mix.SetPadDouble(fmt.Sprintf("sink_%d", i), "alpha", a)
		}
	}
	// spinners
	g.ctl.phase = math.Mod(g.ctl.phase+float64(controlTick)/float64(spinTurn), 1)
	for i := range g.ctl.ov {
		if o := &g.ctl.ov[i]; o.spinPad != "" && (o.waiting || o.spinShown) {
			g.pushSpinnerLocked(i)
		}
	}
}

func (g *Grid) pointerLocked(st gst.NavState, now time.Time) {
	hit := func(x, y float64) (int, Control, float64) {
		i := g.tileAtLocked(x, y)
		if i < 0 || g.ctl.ov[i].barPad == "" {
			return i, "", 0
		}
		o := g.ctl.ov[i]
		c, v, _ := barHit(x-float64(o.barX), y-float64(o.barY), o.barH, g.ctl.tiles[i])
		return i, c, v
	}
	i, c, _ := hit(st.At.X, st.At.Y)
	if st.Moves != g.ctl.moves {
		g.ctl.moves, g.ctl.moved = st.Moves, now
		g.showLocked(i)
		g.setHoverLocked(c)
	}
	if st.Press != nil {
		if pi, pc, v := hit(st.Press.X, st.Press.Y); pc == ControlVolume {
			g.ctl.drag = pi
			g.volumeLocked(pi, v)
		}
	}
	if g.ctl.drag >= 0 {
		if st.Down {
			o := g.ctl.ov[g.ctl.drag]
			g.volumeLocked(g.ctl.drag, sliderValue((st.At.X-float64(o.barX))/float64(o.barH)))
		} else {
			g.ctl.drag = -1
		}
	}
	for _, k := range st.Clicks {
		ki, kc, v := hit(k.X, k.Y)
		switch kc {
		case ControlEye, ControlPopOut, ControlPin, ControlMute, ControlExpand:
			id, on := g.ctl.tiles[ki].ID, g.ctl.on
			go on(id, kc, 0)
		case ControlVolume:
			if st.Press == nil && !st.Down { // a click without a press seen: set it
				g.volumeLocked(ki, v)
			}
		}
	}
}

// volumeLocked sets tile i's volume from the slider.
func (g *Grid) volumeLocked(i int, v float64) {
	t := &g.ctl.tiles[i]
	if v == t.Volume && !(t.Muted && v > 0) {
		return
	}
	t.Volume = v
	if v > 0 {
		t.Muted = false // the session unmutes too
	}
	g.pushBarLocked(i)
	id, on := t.ID, g.ctl.on
	go on(id, ControlVolume, v)
}

// tileAtLocked is the tile at x, y: a corner picture over the others first.
func (g *Grid) tileAtLocked(x, y float64) int {
	for i, r := range g.ctl.rects {
		if overPiP(g.ctl.tiles, i) && r.has(x, y) {
			return i
		}
	}
	for i, r := range g.ctl.rects {
		if r.has(x, y) {
			return i
		}
	}
	return -1
}

// showLocked moves the controls to tile i (-1: none).
func (g *Grid) showLocked(i int) {
	if i == g.ctl.shown {
		return
	}
	if old := g.ctl.shown; old >= 0 && g.ctl.ov[old].barPad != "" {
		g.ctl.mix.SetPadDouble(g.ctl.ov[old].barPad, "alpha", 0)
	}
	g.ctl.shown, g.ctl.alpha = i, -1 // -1: apply the opacity again
	g.ctl.hover = ""
}

// setHoverLocked lights the control under the pointer.
func (g *Grid) setHoverLocked(c Control) {
	if c == ControlVolume {
		c = ""
	}
	if c == g.ctl.hover {
		return
	}
	g.ctl.hover = c
	if i := g.ctl.shown; i >= 0 {
		g.pushBarLocked(i)
	}
}

// pushBarLocked draws tile i's controls if their look changed.
func (g *Grid) pushBarLocked(i int) {
	o := &g.ctl.ov[i]
	if o.bar == nil {
		return
	}
	t := g.ctl.tiles[i]
	hover := Control("")
	if i == g.ctl.shown {
		hover = g.ctl.hover
	}
	key := fmt.Sprint(t.Self, t.Muted, t.Volume, t.Pinned, t.PoppedOut, t.PiP, hover)
	if key == o.drawn {
		return
	}
	o.drawn = key
	o.bar.Push(drawBar(o.barW, o.barH, t, hover).Pix)
}

// pushSpinnerLocked shows or hides tile i's spinner, turning it.
func (g *Grid) pushSpinnerLocked(i int) {
	o := &g.ctl.ov[i]
	if o.spin == nil {
		return
	}
	if o.waiting {
		o.spin.Push(drawSpinner(o.spinS, g.ctl.phase).Pix)
	}
	if o.waiting != o.spinShown {
		o.spinShown = o.waiting
		a := 0.0
		if o.waiting {
			a = 1
		}
		g.ctl.mix.SetPadDouble(o.spinPad, "alpha", a)
		o.wait.SetBool("silent", !o.waiting)
	}
}

// SetState updates tile id's mute, volume, pinned and pop-out state on its
// label and controls.
func (g *Grid) SetState(id string, muted bool, volume float64, pinned, poppedOut bool) {
	g.ctl.Lock()
	defer g.ctl.Unlock()
	if g.ctl.done {
		return
	}
	for i, t := range g.ctl.tiles {
		if t.ID == id && (t.Muted != muted || t.Pinned != pinned || t.PoppedOut != poppedOut || (t.Volume != volume && g.ctl.drag != i)) {
			g.ctl.tiles[i].Muted = muted
			g.ctl.tiles[i].Pinned = pinned
			g.ctl.tiles[i].PoppedOut = poppedOut
			if g.ctl.drag != i { // the slider leads while dragged
				g.ctl.tiles[i].Volume = volume
			}
			g.showStateLocked(i)
		}
	}
}

// EffectiveTileHeight returns the 16:9-fitted height of tile id on this
// grid's canvas (0 if id is not shown here).
func (g *Grid) EffectiveTileHeight(id string) int {
	g.ctl.Lock()
	defer g.ctl.Unlock()
	for i, t := range g.ctl.tiles {
		if t.ID == id {
			r := g.ctl.rects[i]
			return min(r.h, r.w*9/16)
		}
	}
	return 0
}

// SetWaiting shows (or hides) a spinner on tile id: no video yet, or it
// stalled.
func (g *Grid) SetWaiting(id string, waiting bool) {
	g.ctl.Lock()
	defer g.ctl.Unlock()
	if g.ctl.done {
		return
	}
	for i, t := range g.ctl.tiles {
		if t.ID == id && g.ctl.ov[i].spin != nil && g.ctl.ov[i].waiting != waiting {
			g.ctl.ov[i].waiting = waiting
			g.pushSpinnerLocked(i)
		}
	}
}

// Fade fades tile id out over d (it is about to go). The tile must be
// Stopped: only those keep the tick running without controls.
func (g *Grid) Fade(id string, d time.Duration) {
	g.ctl.Lock()
	defer g.ctl.Unlock()
	if g.ctl.done {
		return
	}
	for i, t := range g.ctl.tiles {
		if t.ID == id && g.ctl.fades[i].at.IsZero() {
			g.ctl.fades[i] = fade{at: time.Now(), d: d, alpha: 1}
		}
	}
}

// showStateLocked writes tile i's state into its label and controls.
func (g *Grid) showStateLocked(i int) {
	t := g.ctl.tiles[i]
	label := tileText(t)
	g.ctl.labels[i].SetString("text", label) // a property, so no escaping in the launch line
	g.pushBarLocked(i)
}

// tileText is the tile's label: their name and whether playback is muted.
func tileText(t Tile) string {
	switch {
	case t.Stopped:
		return i18n.L("{{.Name}} stopped sharing", i18n.D{"Name": t.Label})
	case t.Muted && !t.Self:
		return t.Label + "  " + i18n.L("(muted)")
	}
	return t.Label
}

// launchQuote quotes s as a property value in a gst-launch line.
func launchQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// Tiles is what the grid shows.
func (g *Grid) Tiles() []Tile {
	g.ctl.Lock()
	defer g.ctl.Unlock()
	return slices.Clone(g.ctl.tiles)
}

// InWindow reports whether the grid draws into the GridConfig.Window (no
// sink could: it opened its own).
func (g *Grid) InWindow() bool { return g.win != nil }

// Frames is how many frames the grid has composed (-1 if unknown).
func (g *Grid) Frames() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.out == nil {
		return -1
	}
	return g.out.Load()
}

// Stop closes the grid. A Window of the caller's stays open.
func (g *Grid) Stop() {
	g.ctl.Lock()
	g.ctl.done = true
	g.ctl.Unlock()
	g.dropClicks()
	g.Pipeline.Stop()
	if g.feed != nil {
		g.feed.Stop()
	}
	g.free()
}

// dropClicks stops handing the window's clicks to the sink (before the
// sink goes).
func (g *Grid) dropClicks() {
	g.ctl.Lock()
	f := g.unclick
	g.unclick = nil
	g.ctl.Unlock()
	if f != nil {
		f()
	}
}

func (g *Grid) free() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.out != nil {
		g.out.Free()
		g.out = nil
	}
}
