// Package changelog is what changed in each build, embedded in the app so an
// update can show everything since the build it replaced (skipped builds
// too). Entries are in English; newest first.
package changelog

import (
	_ "embed"
	"encoding/json"
	"sort"
)

// Entry is one build's changes.
type Entry struct {
	Build   int64    `json:"build"`
	Date    string   `json:"date"` // YYYY-MM-DD
	Changes []string `json:"changes"`
}

//go:embed changelog.json
var raw []byte

var entries = func() []Entry {
	var e []Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		panic("changelog.json: " + err.Error())
	}
	sort.SliceStable(e, func(i, j int) bool { return e[i].Build > e[j].Build })
	return e
}()

// All returns every entry up to build (0 = no limit), newest first.
func All(build int64) []Entry { return Between(0, build) }

// Between returns the entries after from, up to and including to (0 = no
// limit), newest first.
func Between(from, to int64) []Entry {
	var out []Entry
	for _, e := range entries {
		if e.Build > from && (to <= 0 || e.Build <= to) {
			out = append(out, e)
		}
	}
	return out
}
