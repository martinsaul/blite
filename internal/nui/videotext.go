package nui

import (
	"bytes"
	"image"
	"image/color"
	"math"
	"slices"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
	"github.com/go-text/render"
	"github.com/go-text/typesetting/di"
	"github.com/go-text/typesetting/font"
	"github.com/go-text/typesetting/fontscan"
	"github.com/go-text/typesetting/language"
	"github.com/go-text/typesetting/shaping"
	"golang.org/x/image/math/fixed"

	"blite/internal/media"
)

// The video windows' texts (names on the tiles, "Waiting for video") are
// drawn here in Go, in the theme's fonts with the system's for the
// characters they lack, and composed over the video by the grid: Pango
// in GStreamer's textoverlay crashed on Windows when several overlays
// came up at once.
//
// Fyne's own text drawing shares its fonts and caches with the UI thread
// (a software canvas's Capture also cleans the UI's caches), so this
// shapes and draws with go-text directly, on faces of its own: one lock
// covers them, and any goroutine may draw.

// SetVideoText has the video windows draw their texts (media.DrawText).
func SetVideoText() { media.DrawText = drawVideoText }

var vtext struct {
	sync.Mutex
	shaper shaping.HarfbuzzShaper
	faces  map[fyne.Resource]*font.Face // parsed theme fonts
	sys    *fontscan.FontMap            // system fonts for the rest (nil: none)
	sysErr bool
}

// drawVideoText draws text white on transparent, px pixels high: as wide as
// it advances and as high as the text font's line (nil: no text).
func drawVideoText(text string, px float64) *image.NRGBA {
	if text == "" || px <= 0 {
		return nil
	}
	th := themeFor()
	vtext.Lock()
	defer vtext.Unlock()
	fm := &videoFonts{}
	for _, res := range []fyne.Resource{th.Font(fyne.TextStyle{}), theme.DefaultTextFont(), theme.DefaultEmojiFont(), theme.DefaultSymbolFont()} {
		if f := themeFace(res); f != nil && !slices.Contains(fm.faces, f) {
			fm.faces = append(fm.faces, f)
		}
	}
	if len(fm.faces) == 0 {
		return nil
	}

	size := fixed.Int26_6(math.Round(px * 64))
	runes := []rune(text)
	in := shaping.Input{Text: runes, RunEnd: len(runes), Direction: di.DirectionLTR, Face: fm.faces[0], Size: size}
	var seg shaping.Segmenter
	var runs []shaping.Output
	w := fixed.Int26_6(0)
	for _, part := range seg.Split(in, fm) {
		out := vtext.shaper.Shape(part)
		runs = append(runs, out)
		w += out.Advance
	}
	// Right-to-left runs side by side (an Arabic name in several fonts)
	// read from the right.
	for i := 0; i < len(runs); {
		j := i
		for j < len(runs) && runs[j].Direction.Progression() == di.TowardTopLeft {
			j++
		}
		slices.Reverse(runs[i:j])
		i = max(j, i+1)
	}

	ext, _ := fm.faces[0].FontHExtents()
	scale := px / float64(fm.faces[0].Upem())
	ascent := math.Ceil(float64(ext.Ascender) * scale)
	h := int(ascent + math.Ceil(-float64(ext.Descender)*scale))
	iw := int(math.Ceil(float64(w) / 64))
	if iw <= 0 || h <= 0 {
		return nil
	}
	img := image.NewRGBA(image.Rect(0, 0, iw, h))
	r := render.Renderer{FontSize: float32(px), PixScale: 1, Color: color.White}
	x := 0
	for _, run := range runs {
		r.DrawShapedRunAt(run, img, x, int(ascent))
		x += int(math.Round(float64(run.Advance) / 64))
	}
	out := image.NewNRGBA(img.Rect)
	for i := 0; i < len(img.Pix); i += 4 {
		p, q := img.Pix[i:i+4], out.Pix[i:i+4]
		if a := uint32(p[3]); a > 0 {
			for k := range 3 {
				q[k] = uint8(min(uint32(p[k])*255/a, 255))
			}
			q[3] = p[3]
		}
	}
	return out
}

// themeFace is res parsed (nil: none, or not a font). vtext is locked.
func themeFace(res fyne.Resource) *font.Face {
	if res == nil {
		return nil
	}
	if f, ok := vtext.faces[res]; ok {
		return f
	}
	f, err := font.ParseTTF(bytes.NewReader(res.Content()))
	if err != nil {
		f = nil
	}
	if vtext.faces == nil {
		vtext.faces = map[fyne.Resource]*font.Face{}
	}
	vtext.faces[res] = f
	return f
}

// videoFonts resolves a character to the first theme font that has it,
// else a system font for its script, else the text font. vtext is locked.
type videoFonts struct {
	faces  []*font.Face
	script language.Script
}

func (m *videoFonts) SetScript(s language.Script) { m.script = s }

func (m *videoFonts) ResolveFace(r rune) *font.Face {
	for _, f := range m.faces {
		if _, ok := f.NominalGlyph(r); ok {
			return f
		}
	}
	if vtext.sys == nil && !vtext.sysErr { // only when a theme font lacks a character
		vtext.sys = fontscan.NewFontMap(quietLog{})
		if err := vtext.sys.UseSystemFonts(""); err != nil { // Fyne's index
			vtext.sys, vtext.sysErr = nil, true
		}
	}
	if sys := vtext.sys; sys != nil {
		sys.SetQuery(fontscan.Query{Families: []string{fontscan.SansSerif}})
		sys.SetScript(m.script)
		if f := sys.ResolveFace(r); f != nil {
			if _, ok := f.NominalGlyph(r); ok {
				return f
			}
		}
	}
	return m.faces[0]
}
