package media

import (
	"testing"
	"time"
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
	g, err := StartGrid([]Tile{{pls[0].Channel, "alice#1234"}, {pls[1].Channel, `bob "quoted" ! name`}}, 0, 0, ViewCount)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Stop()

	aus := make(chan AccessUnit, 64)
	src, _, err := StartVideoSource(SourceTest, "", nil, "", func(layer int, au AccessUnit) {
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
