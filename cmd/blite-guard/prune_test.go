package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestPruneDumpsAgeAndCount(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

	writeDump := func(name string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("dmp"), 0o600); err != nil {
			t.Fatal(err)
		}
		mt := now.Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}

	writeDump("blite-20261025-010000.dmp", 21*24*time.Hour) // > 14 days: expired
	writeDump("blite-20261031-010000.dmp", 15*24*time.Hour) // > 14 days: expired
	writeDump("blite-20261110-010000.dmp", 5*24*time.Hour)  // fresh, 3rd newest -> pruned by count
	writeDump("blite-20261113-010000.dmp", 2*24*time.Hour)  // fresh, 2nd newest -> kept at startup
	writeDump("blite-20261114-010000.dmp", 24*time.Hour)    // fresh, newest -> kept
	writeDump("unrelated.txt", 30*24*time.Hour)             // non-dump: untouched

	list := func() []string {
		es, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range es {
			out = append(out, e.Name())
		}
		slices.Sort(out)
		return out
	}

	// Guard start: delete >14d and cap to keepDump (2).
	pruneDumps(dir, now, keepDump)
	wantStart := []string{
		"blite-20261113-010000.dmp",
		"blite-20261114-010000.dmp",
		"unrelated.txt",
	}
	if got := list(); !slices.Equal(got, wantStart) {
		t.Fatalf("after startup prune: got %v, want %v", got, wantStart)
	}

	// Before writing a new dump: cap to keepDump-1 (1) so adding one leaves 2.
	pruneDumps(dir, now, keepDump-1)
	wantBeforeDump := []string{
		"blite-20261114-010000.dmp",
		"unrelated.txt",
	}
	if got := list(); !slices.Equal(got, wantBeforeDump) {
		t.Fatalf("before new dump prune: got %v, want %v", got, wantBeforeDump)
	}

	// If all dumps are older than 14 days, startup deletes them all.
	pruneDumps(dir, now.Add(15*24*time.Hour), keepDump)
	if got := list(); !slices.Equal(got, []string{"unrelated.txt"}) {
		t.Fatalf("after all dumps expire: got %v, want [unrelated.txt]", got)
	}
}
