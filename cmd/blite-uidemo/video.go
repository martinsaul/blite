package main

import (
	"log"
	"os"
	"time"

	"blite/internal/media"
)

// videoScene shows the main video window as a call draws it: two people's
// streams and our own preview, in the corner (or as a tile), with working
// controls (logged). It runs until quitAfter (0: until killed).
func videoScene(tile bool, quitAfter time.Duration) {
	media.Init()
	var tiles []media.Tile
	for _, p := range []struct{ name, id, pattern string }{{"Bob#2926", "a", "smpte"}, {"Carol#0815", "b", "pinwheel"}} {
		ch := media.NewPreviewChannel()
		pp, err := media.StartPattern(ch, p.pattern, 1280, 720)
		if err != nil {
			log.Fatal(err)
		}
		defer pp.Stop()
		tiles = append(tiles, media.Tile{Channel: ch, Label: p.name, ID: p.id, Volume: 1})
	}
	self := media.NewPreviewChannel()
	src, _, err := media.StartVideoSource(media.SourceTest, media.Res720, "", nil, "", self, func(int, media.AccessUnit) {})
	if err != nil {
		log.Fatal(err)
	}
	defer src.Stop()
	src.SetPreviewBig(tile)
	src.SetPreview(true)
	ch := self
	if tile {
		ch = media.PreviewBigChannel(self)
	}
	tiles = append(tiles, media.Tile{Channel: ch, Label: "You", ID: "self", Self: true, PiP: !tile})
	win, err := media.OpenVideoWindow("Bellion Share", 1280, 720)
	if err != nil {
		log.Fatal(err)
	}
	defer win.Destroy()
	g, err := media.StartGrid(media.GridConfig{Tiles: tiles, View: media.ViewWindow, Window: win, Width: 1280, Height: 720,
		OnControl: func(id string, c media.Control, v float64) { log.Printf("control %s on %s (%.2f)", c, id, v) }})
	if err != nil {
		log.Fatal(err)
	}
	defer g.Stop()
	log.Printf("video window drawn by %s", g.Sink)
	if quitAfter <= 0 {
		quitAfter = 24 * time.Hour
	}
	select {
	case <-time.After(quitAfter):
	case <-win.Closed():
	}
	os.Stdout.Sync()
}
