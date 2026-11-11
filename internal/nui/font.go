package nui

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"unicode"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
	"github.com/go-text/typesetting/font"
	"github.com/go-text/typesetting/fontscan"

	"blite/internal/i18n"
)

// Fyne's bundled font has Latin, Greek and Cyrillic only. Other scripts
// come from system fonts it finds per character, which is fine for CJK but
// breaks scripts whose letters combine (Indic vowel signs, Thai marks,
// Arabic joining): a font that has the consonant but not the sign (a
// Grantha font for Tamil, say) splits the cluster and the sign shows on a
// dotted circle. For those languages the theme's text font is a system font
// that covers every letter the translation uses; Latin text the font lacks
// still comes from Fyne's own.

// scriptFonts are the families to try per language, Windows' first.
var scriptFonts = map[string]struct {
	script   *unicode.RangeTable
	families []string
}{
	"hi": {unicode.Devanagari, []string{"Nirmala UI", "Noto Sans Devanagari", "Noto Sans Devanagari UI", "Lohit Devanagari", "Mangal"}},
	"mr": {unicode.Devanagari, []string{"Nirmala UI", "Noto Sans Devanagari", "Noto Sans Devanagari UI", "Lohit Marathi", "Lohit Devanagari", "Mangal"}},
	"bn": {unicode.Bengali, []string{"Nirmala UI", "Noto Sans Bengali", "Noto Sans Bengali UI", "Lohit Bengali", "Vrinda"}},
	"ta": {unicode.Tamil, []string{"Nirmala UI", "Noto Sans Tamil", "Noto Sans Tamil UI", "Lohit Tamil", "Latha"}},
	"te": {unicode.Telugu, []string{"Nirmala UI", "Noto Sans Telugu", "Noto Sans Telugu UI", "Lohit Telugu", "Gautami"}},
	"th": {unicode.Thai, []string{"Leelawadee UI", "Noto Sans Thai", "Noto Sans Thai UI", "Tahoma", "Loma", "Garuda"}},
	"ar": {unicode.Arabic, []string{"Segoe UI", "Noto Sans Arabic", "Noto Sans Arabic UI", "Noto Naskh Arabic", "DejaVu Sans"}},
	"fa": {unicode.Arabic, []string{"Segoe UI", "Noto Sans Arabic", "Noto Sans Arabic UI", "Noto Naskh Arabic", "Vazirmatn", "DejaVu Sans"}},
	"ur": {unicode.Arabic, []string{"Segoe UI", "Noto Sans Arabic", "Noto Sans Arabic UI", "Noto Naskh Arabic", "DejaVu Sans"}},
}

// The others (CJK, Latin, Cyrillic...) do fine with Fyne's per-character
// fallback.

// textTheme is the default theme with the language's text fonts.
type textTheme struct {
	fyne.Theme
	regular, bold fyne.Resource
}

func (t *textTheme) Font(s fyne.TextStyle) fyne.Resource {
	switch {
	case s.Monospace || s.Symbol:
	case s.Bold && t.bold != nil:
		return t.bold
	case t.regular != nil:
		return t.regular
	}
	return t.Theme.Font(s)
}

var (
	fontsMu  sync.Mutex
	fontMap  *fontscan.FontMap
	langFont = map[string]*textTheme{} // by language; nil: Fyne's fonts
)

// themeFor is the theme for the current language.
func themeFor() fyne.Theme {
	tag := i18n.Current()
	sf, ok := scriptFonts[tag]
	if !ok {
		return theme.DefaultTheme()
	}
	fontsMu.Lock()
	defer fontsMu.Unlock()
	t, done := langFont[tag]
	if !done {
		t = findFonts(sf.families, letters(sf.script))
		langFont[tag] = t
	}
	if t == nil {
		return theme.DefaultTheme()
	}
	return t
}

// letters are the runes of script that the translation uses.
func letters(script *unicode.RangeTable) []rune {
	seen := map[rune]bool{}
	var out []rune
	for _, k := range i18n.Keys() {
		for _, r := range i18n.L(k) {
			if unicode.Is(script, r) && !seen[r] {
				seen[r] = true
				out = append(out, r)
			}
		}
	}
	return out
}

// findFonts picks the first family with a regular face that has every one
// of need; its bold face too if there is one. nil if none.
func findFonts(families []string, need []rune) *textTheme {
	if fontMap == nil {
		fontMap = fontscan.NewFontMap(quietLog{})
		if err := fontMap.UseSystemFonts(""); err != nil { // Fyne's index: shared with its fallback
			return nil
		}
	}
	for _, fam := range families {
		t := &textTheme{Theme: theme.DefaultTheme()}
		for _, loc := range fontMap.FindSystemFonts(fam) {
			if loc.Index != 0 || loc.Instance != 0 {
				continue // in a collection, or a variable font's instance: Fyne loads a file's first
			}
			b, err := os.ReadFile(loc.File)
			if err != nil {
				continue
			}
			f, err := font.ParseTTF(bytes.NewReader(b))
			if err != nil || !covers(f, need) {
				continue
			}
			a := f.Describe().Aspect
			if a.Style != font.StyleNormal {
				continue
			}
			res := fyne.NewStaticResource(filepath.Base(loc.File), b)
			switch {
			case a.Weight >= 600 && t.bold == nil:
				t.bold = res
			case a.Weight >= 380 && a.Weight < 500 && t.regular == nil:
				t.regular = res
			}
		}
		if t.regular != nil {
			return t
		}
	}
	return nil
}

func covers(f *font.Face, need []rune) bool {
	for _, r := range need {
		if _, ok := f.NominalGlyph(r); !ok {
			return false
		}
	}
	return true
}

type quietLog struct{}

func (quietLog) Printf(string, ...any) {}
