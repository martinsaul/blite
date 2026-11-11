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
	gridFPS      = 30
)

// Tile is one stream in the grid window.
type Tile struct {
	Channel string // the ViewGrid player's Channel
	Label   string // shown in the tile's top left corner
	// ID is the peer the tile shows; with GridConfig.OnControl the tile
	// gets controls for it: mute, pause and volume. Muted, Paused and
	// Volume (0..2) are its state.
	ID            string
	Muted, Paused bool
	Volume        float64
	// Stopped: they stopped sharing. The tile says so on black instead of
	// their last frame (Channel unused) and has no controls.
	Stopped bool
}

// Control is a button on a tile.
type Control string

const (
	ControlMute  Control = "mute"  // toggle their audio
	ControlPause Control = "pause" // toggle their video
	// ControlVolume sets their playback volume (0..2) from the slider.
	ControlVolume Control = "volume"
)

// GridConfig describes a grid window.
type GridConfig struct {
	Tiles      []Tile
	Rows, Cols int      // 0 = auto (GridSize)
	View       ViewKind // ViewWindow, or ViewCount for headless tests
	// Pip is a preview channel (see StartVideoSource) shown small in a
	// corner; "" for none.
	Pip string
	// Width x Height is the canvas (the window's first size); 0 = 1920x1080.
	Width, Height int
	// OnControl gets the controls used on a tile with an ID (v: the
	// volume for ControlVolume); nil = no controls. It runs on its own
	// goroutine.
	OnControl func(id string, c Control, v float64)
	// Window is the native window to draw into; nil: the sink
	// opens its own.
	Window Window
}

// GridTileHeight is the height of a tile in a grid of n tiles on the
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

// Grid is one window that shows several streams tiled.
type Grid struct {
	*Pipeline
	feed *Pipeline // pushes the drawn controls and spinners (nil: none)
	mu   sync.Mutex
	out  *gst.Counter // frames out of the compositor (nil if unavailable)
	// Sink is the video sink drawing the window (for the log).
	Sink string
	win  Window
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

// The self-preview in the grid sits in the bottom right corner, gridPipH
// lines of the gridH canvas: 144p to 240p on screen for a grid window
// 640 to 1080 lines tall.
const (
	gridPipW, gridPipH = 384, 216
	gridPipMargin      = 16
	gridPipFrame       = 3 // white border, so the preview stands out from dark content
)

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

// StartGrid opens a window with the tiles laid out rows x cols (GridSize).
// With cfg.Window it draws into that window with the first sink that
// works there, else the sink opens a window of its own.
func StartGrid(cfg GridConfig) (*Grid, error) {
	switch {
	case cfg.View == ViewCount:
		g, err := startGrid(cfg, "fakesink name=vsink sync=false", nil)
		if g != nil {
			g.win = cfg.Window // headless: take it as drawn into
		}
		return g, err
	case cfg.Window != nil:
		var errs []error
		for _, sink := range VideoWindowSinks() {
			if !gst.HasFactory(sink) {
				continue
			}
			g, err := startGrid(cfg, sink+" name=vsink sync=false force-aspect-ratio=true", cfg.Window)
			if err == nil {
				return g, nil
			}
			errs = append(errs, fmt.Errorf("%s: %w", sink, err))
		}
		g, err := startGrid(cfg, "autovideosink name=vsink sync=false", nil)
		if err != nil {
			return nil, errors.Join(append(errs, err)...)
		}
		g.Sink += " (" + errors.Join(errs...).Error() + ")"
		return g, nil
	}
	return startGrid(cfg, "autovideosink name=vsink sync=false", nil)
}

func startGrid(cfg GridConfig, sink string, win Window) (*Grid, error) {
	tiles := cfg.Tiles
	rows, cols := GridSize(len(tiles), cfg.Rows, cfg.Cols)
	cw, ch := cfg.Width, cfg.Height
	if cw <= 0 || ch <= 0 {
		cw, ch = gridW, gridH
	}
	tw, th := cw/cols&^1, ch/rows&^1
	pipW, pipH, pipMargin := gridPipW*cw/gridW&^1, gridPipH*cw/gridW&^1, gridPipMargin*cw/gridW
	controls := cfg.OnControl != nil
	ovs := make([]tileOverlay, len(tiles))
	var b, feed strings.Builder
	fmt.Fprintf(&b, "compositor name=mix background=black")
	rects := make([]rect, len(tiles))
	for i := range tiles {
		rects[i] = rect{i % cols * tw, i / cols * th, tw, th}
		fmt.Fprintf(&b, " sink_%d::xpos=%d sink_%d::ypos=%d", i, rects[i].x, i, rects[i].y)
	}
	pipPad := len(tiles)
	if cfg.Pip != "" { // the pad's position includes the frame videobox adds
		fmt.Fprintf(&b, " sink_%d::xpos=%d sink_%d::ypos=%d sink_%d::zorder=%d", pipPad,
			tw*cols-pipW-pipMargin-gridPipFrame, pipPad, th*rows-pipH-pipMargin-gridPipFrame, pipPad, pipPad+1)
	}
	// Drawn layers: two pads per tile with controls, above everything,
	// hidden (alpha 0) until shown.
	type layer struct {
		pad, ch     string
		x, y, w, h  int
		appsrc, out string
	}
	var layers []layer
	next := pipPad + 1
	for i, t := range tiles {
		if !controls || t.ID == "" || t.Stopped {
			continue
		}
		o := &ovs[i]
		o.barW, o.barH = barSize(th)
		o.barX, o.barY = rects[i].x+(tw-o.barW)/2, rects[i].y+th-o.barH-o.barH/3
		o.spinS = spinnerSize(th)
		seq := overlaySeq.Add(1)
		for k, l := range []layer{
			{x: o.barX, y: o.barY, w: o.barW, h: o.barH},
			{x: rects[i].x + (tw-o.spinS)/2, y: rects[i].y + (th-o.spinS)/2, w: o.spinS, h: o.spinS},
		} {
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
	for _, l := range layers {
		fmt.Fprintf(&b, " %s::xpos=%d %s::ypos=%d %s::zorder=%d %s::alpha=0", l.pad, l.x, l.pad, l.y, l.pad, next+1, l.pad)
	}
	fmt.Fprintf(&b, " ! video/x-raw,width=%d,height=%d,framerate=%d/1 ! identity name=out ! videoconvert ! %s", tw*cols, th*rows, gridFPS, sink)
	for i, t := range tiles {
		if t.Stopped { // black, the text in the middle (showStateLocked sets it)
			fmt.Fprintf(&b, " videotestsrc is-live=true pattern=black ! "+
				"video/x-raw,width=%d,height=%d,framerate=%d/1,pixel-aspect-ratio=1/1 ! "+
				"textoverlay name=label%d valignment=center halignment=center font-desc=\"Sans 28\" ! mix.sink_%d",
				tw, th, gridFPS, i, i)
			continue
		}
		wait := ""
		if ovs[i].barPad != "" {
			wait = fmt.Sprintf("textoverlay name=wait%d silent=true text=%s valignment=position ypos=0.66 halignment=center font-desc=\"Sans 16\" shaded-background=true ! ", i, launchQuote(i18n.L("Waiting for video")))
		}
		fmt.Fprintf(&b, " intervideosrc channel=%s %s ! videoconvert ! videoscale add-borders=true ! "+
			"video/x-raw,width=%d,height=%d,pixel-aspect-ratio=1/1,interlace-mode=progressive ! "+
			"textoverlay name=label%d valignment=top halignment=left font-desc=\"%s\" shaded-background=true ! %smix.sink_%d",
			t.Channel, interHold, tw, th, i, labelFont(ovs[i].barPad == ""), wait, i)
	}
	if cfg.Pip != "" {
		// videobox draws the frame: a separate live source for it broke
		// the compositor's latency query and flooded the logs with errors.
		fmt.Fprintf(&b, " intervideosrc channel=%s %s ! videoconvert ! videoscale add-borders=true ! "+
			"video/x-raw,width=%d,height=%d,pixel-aspect-ratio=1/1,interlace-mode=progressive ! "+
			"textoverlay text=%s valignment=bottom halignment=left font-desc=\"Sans 24\" shaded-background=true ! "+
			"videobox left=-%d right=-%d top=-%d bottom=-%d fill=white ! mix.sink_%d",
			cfg.Pip, interHold, pipW, pipH, launchQuote(i18n.L("You")), gridPipFrame, gridPipFrame, gridPipFrame, gridPipFrame, pipPad)
	}
	for _, l := range layers {
		// intervideosrc repeats the last frame we drew, like the tiles.
		fmt.Fprintf(&b, " intervideosrc channel=%s %s ! videoconvert ! mix.%s", l.ch, interHold, l.pad)
		fmt.Fprintf(&feed, " appsrc name=%s is-live=true format=time do-timestamp=true "+
			"caps=video/x-raw,format=RGBA,width=%d,height=%d,framerate=0/1 ! intervideosink channel=%s sync=false ",
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
			if ovs[i].bar, err = fp.element(fmt.Sprintf("ov%d_0", i)); err == nil {
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
		if ovs[i].barPad != "" {
			if g.ctl.ov[i].wait, err = p.element(fmt.Sprintf("wait%d", i)); err != nil {
				return fail(err)
			}
			g.pushBarLocked(i)
			g.pushSpinnerLocked(i)
		}
		g.showStateLocked(i)
	}
	if win != nil {
		vs, err := p.element("vsink")
		if err != nil {
			return fail(err)
		}
		if !vs.SetWindow(win.Handle()) {
			return fail(errors.New("not a video overlay"))
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
	if win != nil && g.out != nil {
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
		c, v, _ := barHit(x-float64(o.barX), y-float64(o.barY), o.barH)
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
		case ControlMute, ControlPause:
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

func (g *Grid) tileAtLocked(x, y float64) int {
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
	key := fmt.Sprint(t.Muted, t.Paused, t.Volume, hover)
	if key == o.drawn {
		return
	}
	o.drawn = key
	o.bar.Push(drawBar(o.barH, t.Muted, t.Paused, t.Volume, hover).Pix)
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

// SetState updates tile id's mute, pause and volume on its label and
// controls.
func (g *Grid) SetState(id string, muted, paused bool, volume float64) {
	g.ctl.Lock()
	defer g.ctl.Unlock()
	if g.ctl.done {
		return
	}
	for i, t := range g.ctl.tiles {
		if t.ID == id && (t.Muted != muted || t.Paused != paused || (t.Volume != volume && g.ctl.drag != i)) {
			g.ctl.tiles[i].Muted, g.ctl.tiles[i].Paused = muted, paused
			if g.ctl.drag != i { // the slider leads while dragged
				g.ctl.tiles[i].Volume = volume
			}
			g.showStateLocked(i)
		}
	}
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

// tileText is the tile's label: their name and what's off.
func tileText(t Tile) string {
	switch {
	case t.Stopped:
		return i18n.L("{{.Name}} stopped sharing", i18n.D{"Name": t.Label})
	case t.Muted && t.Paused:
		return t.Label + "  " + i18n.L("(muted, video paused)")
	case t.Muted:
		return t.Label + "  " + i18n.L("(muted)")
	case t.Paused:
		return t.Label + "  " + i18n.L("(video paused)")
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
	g.Pipeline.Stop()
	if g.feed != nil {
		g.feed.Stop()
	}
	g.free()
}

func (g *Grid) free() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.out != nil {
		g.out.Free()
		g.out = nil
	}
}
