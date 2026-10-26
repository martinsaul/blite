package media

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"

	"blite/internal/gst"
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
	mu  sync.Mutex
	out *gst.Counter // frames out of the compositor (nil if unavailable)
}

// StartGrid opens a window with tiles laid out rows x cols (GridSize).
// view is ViewWindow, or ViewCount for headless tests.
func StartGrid(tiles []Tile, rows, cols int, view ViewKind) (*Grid, error) {
	rows, cols = GridSize(len(tiles), rows, cols)
	tw, th := gridW/cols&^1, gridH/rows&^1
	sink := "autovideosink sync=false"
	if view == ViewCount {
		sink = "fakesink sync=false"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "compositor name=mix background=black")
	for i := range tiles {
		fmt.Fprintf(&b, " sink_%d::xpos=%d sink_%d::ypos=%d", i, i%cols*tw, i, i/cols*th)
	}
	fmt.Fprintf(&b, " ! video/x-raw,width=%d,height=%d,framerate=%d/1 ! identity name=out ! videoconvert ! %s", tw*cols, th*rows, gridFPS, sink)
	for i, t := range tiles {
		fmt.Fprintf(&b, " intervideosrc channel=%s ! videoconvert ! videoscale add-borders=true ! "+
			"video/x-raw,width=%d,height=%d,pixel-aspect-ratio=1/1 ! "+
			"textoverlay name=label%d valignment=top halignment=left font-desc=\"Sans 14\" shaded-background=true ! mix.sink_%d",
			t.Channel, tw, th, i, i)
	}
	p, err := launch(b.String())
	if err != nil {
		return nil, err
	}
	for i, t := range tiles {
		l, err := p.element(fmt.Sprintf("label%d", i))
		if err != nil {
			p.Stop()
			return nil, err
		}
		l.SetString("text", t.Label) // a property, so no escaping in the launch line
	}
	g := &Grid{Pipeline: p}
	if out, err := p.element("out"); err == nil {
		g.out, _ = out.CountBuffers()
	}
	if err := p.start(); err != nil {
		g.free()
		return nil, err
	}
	return g, nil
}

// Frames is how many frames the grid has composed (-1 if unknown).
func (g *Grid) Frames() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.out == nil {
		return -1
	}
	return g.out.Load()
}

// Stop closes the window.
func (g *Grid) Stop() {
	g.Pipeline.Stop()
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
