package media

import (
	"image"
	"testing"
	"time"
	"unicode/utf8"

	"blite/internal/i18n"
)

func TestGridSize(t *testing.T) {
	for _, c := range []struct{ n, rows, cols, wantR, wantC int }{
		{1, 0, 0, 1, 1},
		{2, 0, 0, 1, 2},
		{3, 0, 0, 2, 2},
		{4, 0, 0, 2, 2},
		{5, 0, 0, 2, 3},
		{3, 1, 3, 1, 3},
		{3, 3, 1, 3, 1},
		{5, 2, 2, 3, 2}, // more streams than cells: rows grow
		{4, 0, 1, 4, 1},
		{4, 1, 0, 1, 4},
		{0, 0, 0, 1, 1},
	} {
		if r, cl := GridSize(c.n, c.rows, c.cols); r != c.wantR || cl != c.wantC {
			t.Errorf("GridSize(%d, %d, %d) = %dx%d, want %dx%d", c.n, c.rows, c.cols, r, cl, c.wantR, c.wantC)
		}
	}
}

func TestLayoutRectsPinned(t *testing.T) {
	// Single pinned tile: still uses the whole canvas.
	rs, w, h := layoutRects([]Tile{{ID: "a", Pinned: true}}, 1920, 1080, 0, 0)
	if len(rs) != 1 || rs[0] != (rect{0, 0, 1920, 1080}) || w != 1920 || h != 1080 {
		t.Fatalf("1 pinned tile: %+v %dx%d", rs, w, h)
	}

	// 3 tiles, middle one pinned: primary gets 4/5 width, strip gets 1/5 width.
	rs, w, h = layoutRects([]Tile{{ID: "a"}, {ID: "b", Pinned: true}, {ID: "self", Self: true}}, 1920, 1080, 0, 0)
	if rs[1] != (rect{0, 0, 1536, 1080}) {
		t.Fatalf("pinned rect = %+v, want 0,0 1536x1080", rs[1])
	}
	if rs[0] != (rect{1536, 0, 384, 540}) || rs[2] != (rect{1536, 540, 384, 540}) {
		t.Fatalf("strip rects = %+v, %+v", rs[0], rs[2])
	}
	if w != 1920 || h != 1080 {
		t.Fatalf("canvas = %dx%d", w, h)
	}

	// 7 tiles (1 pinned + 6 in strip -> 2 strip columns), all even coordinates.
	tiles := make([]Tile, 7)
	tiles[0].Pinned = true
	rs, _, _ = layoutRects(tiles, 1920, 1080, 0, 0)
	for i, r := range rs {
		if r.x%2 != 0 || r.y%2 != 0 || r.w%2 != 0 || r.h%2 != 0 || r.w <= 0 || r.h <= 0 {
			t.Fatalf("tile %d has odd or empty rect %+v", i, r)
		}
	}
}

// Our preview as the corner picture takes no grid cell: it sits small in
// the top right corner over the others (of the pinned tile, with a strip),
// and fills the window alone.
func TestLayoutRectsPiP(t *testing.T) {
	self := Tile{ID: "self", Self: true, PiP: true}
	rs, w, h := layoutRects([]Tile{self}, 1920, 1080, 0, 0)
	if rs[0] != (rect{0, 0, 1920, 1080}) || w != 1920 || h != 1080 {
		t.Fatalf("alone: %+v %dx%d", rs, w, h)
	}
	rs, _, _ = layoutRects([]Tile{{ID: "a"}, self, {ID: "b"}}, 1920, 1080, 0, 0)
	if rs[0] != (rect{0, 0, 960, 1080}) || rs[2] != (rect{960, 0, 960, 1080}) {
		t.Fatalf("the others take the grid as if it weren't there: %+v", rs)
	}
	if rs[1] != (rect{1920 - 384 - 24, 24, 384, 216}) {
		t.Fatalf("corner picture at %+v, want 384x216 24 px in from the top right", rs[1])
	}
	rs, _, _ = layoutRects([]Tile{{ID: "a", Pinned: true}, {ID: "b"}, self}, 1920, 1080, 0, 0)
	if rs[0] != (rect{0, 0, 1536, 1080}) || rs[1] != (rect{1536, 0, 384, 1080}) {
		t.Fatalf("pinned and strip: %+v", rs)
	}
	if want := (rect{1536 - 306 - 18, 18, 306, 172}); rs[2] != want {
		t.Fatalf("corner picture at %+v, want %+v (in the pinned tile)", rs[2], want)
	}
	for _, r := range rs {
		if r.x%2 != 0 || r.y%2 != 0 || r.w%2 != 0 || r.h%2 != 0 {
			t.Fatalf("odd rect %+v", r)
		}
	}
}

// The corner picture takes the pointer over the tile under it, and its bar
// has hide, pop out and expand.
func TestGridPiPControls(t *testing.T) {
	Init()
	pl, err := StartPlayer(ViewGrid)
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Stop()
	got := make(chan Control, 8)
	g, err := StartGrid(GridConfig{
		Tiles: []Tile{
			{Channel: pl.Channel, Label: "alice", ID: "a", Volume: 1},
			{Channel: NewPreviewChannel(), Label: "You", ID: "self", Self: true, PiP: true},
		},
		View: ViewCount,
		OnControl: func(id string, c Control, _ float64) {
			if id == "self" {
				got <- c
			} else {
				got <- "alice:" + c
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Stop()
	sink, err := g.element("vsink")
	if err != nil {
		t.Fatal(err)
	}
	g.ctl.Lock()
	o, r := g.ctl.ov[1], g.ctl.rects[1]
	g.ctl.Unlock()
	sink.SendMouse(false, float64(r.x+r.w/2), float64(r.y+r.h/4))
	time.Sleep(3 * controlTick)
	g.ctl.Lock()
	shown := g.ctl.shown
	g.ctl.Unlock()
	if shown != 1 {
		t.Fatalf("the pointer over the corner picture shows tile %d's controls", shown)
	}
	for k, want := range []Control{ControlEye, ControlPopOut, ControlExpand} {
		x, y := float64(o.barX)+barButtonX(k)*float64(o.barH), float64(o.barY)+float64(o.barH)/2
		if !r.has(x, y) {
			t.Fatalf("%s at %.0f,%.0f is outside the corner picture %+v", want, x, y, r)
		}
		sink.SendMouse(false, x, y)
		sink.SendMouse(true, x, y)
		select {
		case c := <-got:
			if c != want {
				t.Fatalf("clicked %s, got %s", want, c)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("clicked %s: nothing", want)
		}
	}
}

// Two decoders feed one grid; it keeps composing, also after one of them stops.
func TestGridComposesTwoPlayers(t *testing.T) {
	Init()
	var pls []*Player
	for range 2 {
		pl, err := StartPlayer(ViewGrid)
		if err != nil {
			t.Fatal(err)
		}
		defer pl.Stop()
		pls = append(pls, pl)
	}
	if pls[0].Channel == "" || pls[0].Channel == pls[1].Channel {
		t.Fatalf("channels %q %q", pls[0].Channel, pls[1].Channel)
	}
	g, err := StartGrid(GridConfig{Tiles: []Tile{{Channel: pls[0].Channel, Label: "alice#1234"}, {Channel: pls[1].Channel, Label: `bob "quoted" ! name`}}, View: ViewCount})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Stop()

	aus := make(chan AccessUnit, 64)
	src, _, err := StartVideoSource(SourceTest, "", "", nil, "", "", func(layer int, au AccessUnit) {
		if layer != High {
			return
		}
		select {
		case aus <- au:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Stop()

	wait := func(frames int, feed []*Player) {
		t.Helper()
		start := g.Frames()
		deadline := time.After(10 * time.Second)
		for g.Frames()-start < frames {
			select {
			case au := <-aus:
				for _, pl := range feed {
					pl.Push(au.Data)
				}
			case err := <-g.Err:
				t.Fatalf("grid: %v", err)
			case <-deadline:
				t.Fatalf("grid composed %d frames in 10 s", g.Frames()-start)
			}
		}
	}
	wait(30, pls)
	for _, pl := range pls {
		if _, d := pl.Progress(); d == 0 {
			t.Fatal("a player decoded nothing")
		}
	}
	pls[1].Stop()
	wait(15, pls[:1])
}

// Clicking a tile's buttons reports the tile's feed ID and the button (eye,
// pop-out, pin, mute, volume; compact 3-button bar on self); the buttons fade
// after the pointer rests.
func TestGridControls(t *testing.T) {
	Init()
	var pls []*Player
	for range 2 {
		pl, err := StartPlayer(ViewGrid)
		if err != nil {
			t.Fatal(err)
		}
		defer pl.Stop()
		pls = append(pls, pl)
	}
	selfCh := NewPreviewChannel()
	type click struct {
		id string
		c  Control
		v  float64
	}
	got := make(chan click, 16)
	g, err := StartGrid(GridConfig{
		Tiles: []Tile{
			{Channel: pls[0].Channel, Label: "alice", ID: "a", Volume: 1},
			{Channel: pls[1].Channel, Label: "bob", ID: "b", Volume: 1, PoppedOut: true},
			{Channel: selfCh, Label: "You", ID: "self", Self: true},
		},
		View:  ViewCount,
		Width: 1280, Height: 720,
		OnControl: func(id string, c Control, v float64) { got <- click{id, c, v} },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Stop()
	sink, err := g.element("vsink")
	if err != nil {
		t.Fatal(err)
	}
	at := func(i int, what string, f float64) (float64, float64) {
		o := g.ctl.ov[i]
		scale := float64(o.barH)
		u := map[string]float64{
			"eye":    barEyeX,
			"pop":    barPopX,
			"pin":    barPinX,
			"mute":   barMuteX,
			"slider": barSliderX0 + f*(barSliderX1-barSliderX0),
		}[what]
		return float64(o.barX) + u*scale, float64(o.barY) + float64(o.barH)/2
	}
	expect := func(want click) {
		t.Helper()
		select {
		case k := <-got:
			if k != want {
				t.Fatalf("got %+v, want %+v", k, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no %+v", want)
		}
	}

	for _, tc := range []struct {
		tile int
		what string
		want click
	}{
		{0, "eye", click{"a", ControlEye, 0}},
		{0, "pop", click{"a", ControlPopOut, 0}},
		{0, "pin", click{"a", ControlPin, 0}},
		{1, "mute", click{"b", ControlMute, 0}},
		{2, "eye", click{"self", ControlEye, 0}},
		{2, "pop", click{"self", ControlPopOut, 0}},
		{2, "pin", click{"self", ControlPin, 0}},
	} {
		x, y := at(tc.tile, tc.what, 0)
		sink.SendMouse(false, x, y)
		sink.SendMouse(true, x, y)
		expect(tc.want)
	}

	// Drag alice's slider to 75%: 1.5.
	x, y := at(0, "slider", 0.2)
	sink.SendPress(x, y)
	expect(click{"a", ControlVolume, 0.4})
	x, _ = at(0, "slider", 0.75)
	sink.SendMouse(false, x, y)
	expect(click{"a", ControlVolume, 1.5})
	sink.SendMouse(true, x, y)
	sink.SendMouse(true, 100, 100) // the picture, not a control
	select {
	case k := <-got:
		t.Fatalf("a click on the picture gave %+v", k)
	case <-time.After(300 * time.Millisecond):
	}
	sink.SendMouse(false, 100, 100)
	time.Sleep(3 * controlTick)
	g.ctl.Lock()
	shown, alpha := g.ctl.shown, g.ctl.alpha
	g.ctl.Unlock()
	if shown != 0 || alpha < 0.99 {
		t.Fatalf("controls on tile %d at opacity %.2f after moving over alice, want tile 0 opaque", shown, alpha)
	}
	time.Sleep(controlShow + controlFade + 300*time.Millisecond)
	g.ctl.Lock()
	alpha = g.ctl.alpha
	g.ctl.Unlock()
	if alpha > 0.01 {
		t.Fatalf("controls at opacity %.2f after the pointer rested, want faded out", alpha)
	}
	g.SetState("a", true, 0.5, true, false)
	g.SetWaiting("b", true)
	g.ctl.Lock()
	st, spin := g.ctl.tiles[0], g.ctl.ov[1].spinShown
	g.ctl.Unlock()
	if !st.Muted || !st.Pinned || st.Volume != 0.5 || !spin {
		t.Fatalf("state not kept: %+v, spinner %v", st, spin)
	}
	if g.Frames() < 10 {
		t.Fatalf("composed %d frames", g.Frames())
	}
}

// A pinned stream composes alongside strip tiles (including our own preview).
func TestGridPinnedWithPreviewTile(t *testing.T) {
	Init()
	pl, err := StartPlayer(ViewGrid)
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Stop()
	g, err := StartGrid(GridConfig{
		Tiles: []Tile{
			{Channel: pl.Channel, Label: "alice", ID: "a", Pinned: true, Volume: 1},
			{Channel: NewPreviewChannel(), Label: "You", ID: "self", Self: true},
		},
		View:  ViewCount,
		Width: 1280, Height: 720,
		OnControl: func(string, Control, float64) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Stop()
	if h := g.EffectiveTileHeight("a"); h <= 360 {
		t.Fatalf("pinned tile effective height = %d, want > 360", h)
	}
	if h := g.EffectiveTileHeight("self"); h == 0 || h > 360 {
		t.Fatalf("strip tile effective height = %d, want 1..360", h)
	}
	deadline := time.Now().Add(10 * time.Second)
	for g.Frames() < 10 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if n := g.Frames(); n < 10 {
		t.Fatalf("composed %d frames", n)
	}
}

// A tile whose sharer stopped says so on black next to a live one; frames
// keep coming, and Fade takes it out.
func TestGridStoppedTile(t *testing.T) {
	Init()
	fakeText(t)
	pl, err := StartPlayer(ViewGrid)
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Stop()
	g, err := StartGrid(GridConfig{Tiles: []Tile{{Channel: pl.Channel, Label: "alice", ID: "a", Volume: 1},
		{Label: `bob "quoted" ! name`, ID: "b", Stopped: true}}, View: ViewCount, Width: 1280, Height: 720,
		OnControl: func(string, Control, float64) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Stop()
	frames := func(n int) {
		t.Helper()
		start := g.Frames()
		deadline := time.Now().Add(10 * time.Second)
		for g.Frames()-start < n && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if d := g.Frames() - start; d < n {
			t.Fatalf("composed %d frames", d)
		}
	}
	frames(10)
	// In the language the tests run in (BLITE_LANG, the locale).
	g.ctl.Lock()
	label := g.ctl.ov[1].label
	g.ctl.Unlock()
	if got, want := label.text, i18n.L("{{.Name}} stopped sharing", i18n.D{"Name": `bob "quoted" ! name`}); got != want {
		t.Fatalf("stopped tile says %q", got)
	}
	if g.ctl.ov[1].bar != nil {
		t.Fatal("a stopped tile has controls")
	}
	g.SetState("b", true, 1, false, false) // no controls to update: nothing breaks
	g.Fade("b", 300*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	g.ctl.Lock()
	mid := g.ctl.fades[1].alpha
	g.ctl.Unlock()
	time.Sleep(400 * time.Millisecond)
	g.ctl.Lock()
	end, live := g.ctl.fades[1].alpha, g.ctl.fades[0]
	g.ctl.Unlock()
	if mid <= 0 || mid >= 1 || end != 0 || !live.at.IsZero() {
		t.Fatalf("fade: %.2f then %.2f, live tile %+v", mid, end, live)
	}
	frames(10)
}

func TestGridLabelText(t *testing.T) {
	for _, c := range []struct {
		t    Tile
		want string
	}{
		{Tile{Label: "Bob"}, "Bob"},
		{Tile{Label: "Bob", Muted: true}, "Bob  " + i18n.L("(muted)")},
		{Tile{Label: "You", Self: true, Muted: true}, "You"},
		{Tile{Label: "Bob", Stopped: true, Muted: true}, i18n.L("{{.Name}} stopped sharing", i18n.D{"Name": "Bob"})},
	} {
		if got := tileText(c.t); got != c.want {
			t.Errorf("tileText(%+v) = %q, want %q", c.t, got, c.want)
		}
	}
}

// fakeText stands in for the UI's DrawText: an opaque box px high and half
// that wide per character.
func fakeText(t *testing.T) {
	old := DrawText
	DrawText = func(s string, px float64) *image.NRGBA {
		img := image.NewNRGBA(image.Rect(0, 0, max(int(px/2)*utf8.RuneCountInString(s), 1), max(int(px*1.4), 1)))
		for i := range img.Pix {
			img.Pix[i] = 255
		}
		return img
	}
	t.Cleanup(func() { DrawText = old })
}

// A text too wide for its tile is drawn smaller; the box is even and on a
// panel.
func TestTextBox(t *testing.T) {
	if textBox("Bob", 20, 600, true) != nil {
		t.Fatal("a box without DrawText")
	}
	fakeText(t)
	b := textBox("Bob", 20, 600, true)
	if w, h := b.Rect.Dx(), b.Rect.Dy(); w != 46 || h != 32 || w%2 != 0 || h%2 != 0 {
		t.Fatalf("box %dx%d", w, h)
	}
	if b.NRGBAAt(1, b.Rect.Dy()/2).A == 0 {
		t.Fatal("no panel at the edge")
	}
	if b := textBox("a long name, longer than its tile", 20, 120, true); b.Rect.Dx() > 120 || b.Rect.Dy() >= 32 {
		t.Fatalf("too wide a text: %v", b.Rect)
	}
	if p := placed(100, 40, b, false); p.Rect.Dx() != 100 || p.NRGBAAt(99, 39).A != 0 || p.NRGBAAt(2, 2).A == 0 {
		t.Fatal("placed wrong")
	}
}

// The names are drawn layers: pushed again only when the text or size
// changes, and none without DrawText.
func TestGridLabels(t *testing.T) {
	Init()
	pl, err := StartPlayer(ViewGrid)
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Stop()
	cfg := GridConfig{Tiles: []Tile{{Channel: pl.Channel, Label: "alice", ID: "a", Volume: 1}}, View: ViewCount, Width: 1280, Height: 720,
		OnControl: func(string, Control, float64) {}}
	g, err := StartGrid(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if g.ctl.ov[0].label.src != nil || g.ctl.ov[0].wait.src != nil {
		t.Fatal("texts without DrawText")
	}
	g.Stop()

	fakeText(t)
	g, err = StartGrid(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Stop()
	g.ctl.Lock()
	l, w := g.ctl.ov[0].label, g.ctl.ov[0].wait
	g.ctl.Unlock()
	if l.src == nil || l.text != "alice" || l.drawn != "false alice" || w.src == nil {
		t.Fatalf("label %+v, wait %+v", l, w)
	}
	// wide enough for "alice  (muted)" at full size, inside the tile
	if want := textBox(tileText(Tile{Label: "alice", Muted: true}), labelPx(true, 1280), 1280, true).Rect.Dx(); l.w != want || l.x != 24 || l.y != 24 {
		t.Fatalf("label at %d,%d %dx%d, want %d wide", l.x, l.y, l.w, l.h, want)
	}
	g.SetState("a", true, 1, false, false)
	g.ctl.Lock()
	g.ctl.big[0] = true
	g.pushLabelLocked(0)
	got := g.ctl.ov[0].label.drawn
	g.ctl.Unlock()
	if want := "true " + tileText(Tile{Label: "alice", Muted: true}); got != want {
		t.Fatalf("label drawn %q, want %q", got, want)
	}
}

type noopWindow struct{ closed chan struct{} }

func (w *noopWindow) Handle() uintptr         { return 0 }
func (w *noopWindow) Closed() <-chan struct{} { return w.closed }
func (w *noopWindow) SetTitle(string)         {}
func (w *noopWindow) PointerInside() bool     { return true }
func (w *noopWindow) Minimized() bool         { return false }
func (w *noopWindow) Destroy()                {}

// TestStartGridViewCountWindowNoRace verifies g.win is initialized before the
// tick goroutine starts in ViewCount mode (M12.7).
func TestStartGridViewCountWindowNoRace(t *testing.T) {
	Init()
	origTick := controlTick
	_ = origTick
	w := &noopWindow{closed: make(chan struct{})}
	for range 6 {
		g, err := StartGrid(GridConfig{
			Tiles:     []Tile{{Channel: NewPreviewChannel(), Label: "You", ID: "self", Self: true}},
			View:      ViewCount,
			Window:    w,
			Width:     640,
			Height:    360,
			OnControl: func(string, Control, float64) {},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !g.InWindow() {
			g.Stop()
			t.Fatal("expected InWindow() == true in ViewCount with cfg.Window")
		}
		select {
		case <-g.Done():
			g.Stop()
			t.Fatal("g.Done() closed before Stop")
		default:
		}
		time.Sleep(70 * time.Millisecond)
		g.Stop()
		select {
		case <-g.Done():
		default:
			t.Fatal("g.Done() not closed after Stop")
		}
	}
}

// Clicking a tile's volume slider redraws the slider in the composed
// picture, not just the volume.
func TestGridSliderRedraws(t *testing.T) {
	Init()
	got := make(chan float64, 16)
	g, err := startGrid(GridConfig{
		Tiles: []Tile{{Channel: NewPreviewChannel(), Label: "alice", ID: "a", Volume: 0.2}},
		View:  ViewCount,
		Width: 640, Height: 360,
		OnControl: func(id string, c Control, v float64) {
			if c == ControlVolume {
				got <- v
			}
		},
	}, "video/x-raw,format=RGBA ! appsink name=vsink sync=false max-buffers=1 drop=true", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Stop()
	sink, err := g.element("vsink")
	if err != nil {
		t.Fatal(err)
	}
	o := g.ctl.ov[0]
	at := func(f float64) (int, int) {
		return o.barX + int((barSliderX0+f*(barSliderX1-barSliderX0))*float64(o.barH)), o.barY + o.barH/2
	}
	luma := func(f float64) int {
		t.Helper()
		x, y := at(f)
		var data []byte
		for range 5 { // the newest frame
			if d, _, err := sink.Pull(time.Second); err == nil {
				data = d
			}
		}
		if data == nil {
			t.Fatal("no frame")
		}
		p := (y*640 + x) * 4
		return int(data[p]) + int(data[p+1]) + int(data[p+2])
	}
	x, y := at(0.5)
	sink.SendMouse(false, float64(x), float64(y)) // show the controls
	time.Sleep(300 * time.Millisecond)
	before := luma(0.7)
	x, y = at(0.9)
	sink.SendPress(float64(x), float64(y))
	sink.SendMouse(true, float64(x), float64(y))
	select {
	case v := <-got:
		t.Logf("volume %.2f", v)
	case <-time.After(2 * time.Second):
		t.Fatal("no volume change")
	}
	time.Sleep(300 * time.Millisecond)
	after := luma(0.7)
	t.Logf("slider at 70%%: luma %d before, %d after", before, after)
	if after <= before+60 {
		t.Fatalf("the slider didn't redraw: luma %d before, %d after", before, after)
	}
}
