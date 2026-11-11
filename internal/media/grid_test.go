package media

import (
	"testing"
	"time"

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

// Clicking a tile's buttons reports the tile's peer and the button; the
// buttons fade after the pointer rests.
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
	type click struct {
		id string
		c  Control
		v  float64
	}
	got := make(chan click, 8)
	g, err := StartGrid(GridConfig{
		Tiles: []Tile{{Channel: pls[0].Channel, Label: "alice", ID: "a", Volume: 1}, {Channel: pls[1].Channel, Label: "bob", ID: "b", Volume: 1}},
		View:  ViewCount, Width: 1280, Height: 720,
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
	// Where tile i's controls are: the mute and pause button centres and
	// the slider at fraction f.
	at := func(i int, what string, f float64) (float64, float64) {
		o := g.ctl.ov[i]
		u := map[string]float64{"mute": barMuteX, "pause": barPauseX, "slider": barSliderX0 + f*(barSliderX1-barSliderX0)}[what]
		return float64(o.barX) + u*float64(o.barH), float64(o.barY) + float64(o.barH)/2
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
	x, y := at(1, "mute", 0)
	sink.SendMouse(false, x, y)
	sink.SendMouse(true, x, y)
	expect(click{"b", ControlMute, 0})
	x, y = at(0, "pause", 0)
	sink.SendMouse(false, x, y)
	sink.SendMouse(true, x, y)
	expect(click{"a", ControlPause, 0})
	// drag alice's slider to 75%: 1.5
	x, y = at(0, "slider", 0.2)
	sink.SendPress(x, y)
	expect(click{"a", ControlVolume, 0.4})
	x, _ = at(0, "slider", 0.75)
	sink.SendMouse(false, x, y)
	expect(click{"a", ControlVolume, 1.5})
	sink.SendMouse(true, x, y)
	sink.SendMouse(true, 320, 200) // the picture, not a control
	select {
	case k := <-got:
		t.Fatalf("a click on the picture gave %+v", k)
	case <-time.After(300 * time.Millisecond):
	}
	sink.SendMouse(false, 320, 200)
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
	g.SetState("a", true, true, 0.5)
	g.SetWaiting("b", true)
	g.ctl.Lock()
	st, spin := g.ctl.tiles[0], g.ctl.ov[1].spinShown
	g.ctl.Unlock()
	if !st.Muted || !st.Paused || st.Volume != 0.5 || !spin {
		t.Fatalf("state not kept: %+v, spinner %v", st, spin)
	}
	if g.Frames() < 10 {
		t.Fatalf("composed %d frames", g.Frames())
	}
}

// A window with our preview in its corner and tile buttons starts and
// composes (build 78 lacked videobox on Windows and every such window failed).
func TestGridWithPreview(t *testing.T) {
	Init()
	pl, err := StartPlayer(ViewGrid)
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Stop()
	g, err := StartGrid(GridConfig{Tiles: []Tile{{Channel: pl.Channel, Label: "alice", ID: "a"}}, View: ViewCount,
		Pip: NewPreviewChannel(), Width: 1280, Height: 720, OnControl: func(string, Control, float64) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Stop()
	g.SetState("a", true, true, 1.2) // the muted and paused icons
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
	if got, want := g.ctl.labels[1].GetString("text"), i18n.L("{{.Name}} stopped sharing", i18n.D{"Name": `bob "quoted" ! name`}); got != want {
		t.Fatalf("stopped tile says %q", got)
	}
	if g.ctl.ov[1].bar != nil {
		t.Fatal("a stopped tile has controls")
	}
	g.SetState("b", true, false, 1) // no controls to update: nothing breaks
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
		{Tile{Label: "Bob", Paused: true}, "Bob  " + i18n.L("(video paused)")},
		{Tile{Label: "Bob", Muted: true, Paused: true}, "Bob  " + i18n.L("(muted, video paused)")},
		{Tile{Label: "Bob", Stopped: true, Muted: true}, i18n.L("{{.Name}} stopped sharing", i18n.D{"Name": "Bob"})},
	} {
		if got := tileText(c.t); got != c.want {
			t.Errorf("tileText(%+v) = %q, want %q", c.t, got, c.want)
		}
	}
	if got := launchQuote(`a "b" \ c`); got != `"a \"b\" \\ c"` {
		t.Error(got)
	}
}
