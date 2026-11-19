package app

import (
	"errors"
	"slices"
	"strings"

	"blite/internal/audio"
	"blite/internal/media"
)

// QualityPreset is a resolution and frame rate picked with one tap on Home.
type QualityPreset struct {
	Resolution string `json:"resolution"` // "720p", "1080p", "2160p"
	FPS        int    `json:"fps"`        // 10..60 step 5
}

// BuiltinPresets are always offered, before the user's own: each share
// resolution at 30 fps (sharp text) and 60 fps (games, video).
var BuiltinPresets = []QualityPreset{
	{"720p", 30}, {"720p", 60},
	{"1080p", 30}, {"1080p", 60},
	{"2160p", 30}, {"2160p", 60},
}

// maxUserPresets bounds the list on disk and on Home.
const maxUserPresets = 12

func validPreset(p QualityPreset) bool {
	return p.Resolution != "" && media.ValidResolution(media.Resolution(p.Resolution)) &&
		p.FPS >= 10 && p.FPS <= 60 && p.FPS%5 == 0
}

// cleanPresets drops invalid presets, built-in ones and duplicates (from a
// hand-edited settings file).
func cleanPresets(ps []QualityPreset) []QualityPreset {
	var out []QualityPreset
	for _, p := range ps {
		if validPreset(p) && !slices.Contains(BuiltinPresets, p) && !slices.Contains(out, p) && len(out) < maxUserPresets {
			out = append(out, p)
		}
	}
	return out
}

// addPreset appends p to the user's presets; one that already exists (built
// in or not) is not added twice.
func addPreset(ps []QualityPreset, p QualityPreset) ([]QualityPreset, error) {
	if !validPreset(p) {
		return ps, errors.New("unknown resolution or frame rate")
	}
	if slices.Contains(BuiltinPresets, p) || slices.Contains(ps, p) {
		return ps, nil
	}
	if len(ps) >= maxUserPresets {
		return ps, errors.New("too many presets; remove one first")
	}
	return append(slices.Clone(ps), p), nil
}

func removePreset(ps []QualityPreset, p QualityPreset) []QualityPreset {
	return slices.DeleteFunc(slices.Clone(ps), func(q QualityPreset) bool { return q == p })
}

// AddQualityPreset implements ui.Backend: a resolution and frame rate the
// user keeps on Home, kept on disk.
func (a *App) AddQualityPreset(res string, fps int) error {
	return a.changePresets(func(ps []QualityPreset) ([]QualityPreset, error) {
		return addPreset(ps, QualityPreset{res, fps})
	})
}

// RemoveQualityPreset implements ui.Backend: removes one of the user's
// presets (built-in ones stay).
func (a *App) RemoveQualityPreset(res string, fps int) error {
	return a.changePresets(func(ps []QualityPreset) ([]QualityPreset, error) {
		return removePreset(ps, QualityPreset{res, fps}), nil
	})
}

// changePresets applies change and saves the result; presetMu keeps
// overlapping changes (two quick removes) in order on disk too.
func (a *App) changePresets(change func([]QualityPreset) ([]QualityPreset, error)) error {
	a.presetMu.Lock()
	defer a.presetMu.Unlock()
	a.mu.Lock()
	ps, err := change(a.presets)
	if err == nil {
		a.presets = ps
	}
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if err := savePresets(ps); err != nil {
		a.log.Warn("save settings", "err", err)
	}
	a.bc.Notify()
	return nil
}

func isWindowSource(src string) bool { return strings.HasPrefix(src, "window:") }

// audioForSource is the audio to share after picking src: a window shares
// its app's audio (the session finds the process); anything else goes
// without sound unless the user turned sound on for it.
func audioForSource(src string, cur audio.Selection) audio.Selection {
	if isWindowSource(src) {
		return audio.Selection{Mode: "window"}
	}
	if cur.Mode == "window" || cur.Mode == "" {
		return audio.Selection{Mode: "none"}
	}
	return cur
}
