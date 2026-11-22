package changelog

import (
	"regexp"
	"testing"
)

func TestEntries(t *testing.T) {
	date := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	seen := map[int64]bool{}
	for i, e := range entries {
		if e.Build <= 0 || seen[e.Build] {
			t.Errorf("entry %d: bad or repeated build %d", i, e.Build)
		}
		seen[e.Build] = true
		if !date.MatchString(e.Date) {
			t.Errorf("build %d: date %q", e.Build, e.Date)
		}
		if len(e.Changes) == 0 {
			t.Errorf("build %d: no changes", e.Build)
		}
		for _, c := range e.Changes {
			if c == "" {
				t.Errorf("build %d: empty change", e.Build)
			}
		}
		if i > 0 && entries[i-1].Build < e.Build {
			t.Errorf("not newest first at build %d", e.Build)
		}
	}
}

func TestBetween(t *testing.T) {
	got := Between(208, 211)
	if len(got) != 2 || got[0].Build != 211 || got[1].Build != 210 {
		t.Fatalf("Between(208, 211) = %+v", got)
	}
	if n := len(Between(211, 211)); n != 0 {
		t.Errorf("Between(211, 211) has %d entries", n)
	}
	if all := All(0); len(all) != len(entries) {
		t.Errorf("All(0) = %d entries, want %d", len(all), len(entries))
	}
	for _, e := range All(210) {
		if e.Build > 210 {
			t.Errorf("All(210) has build %d", e.Build)
		}
	}
}
