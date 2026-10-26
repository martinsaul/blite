package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneLogs(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	touch := func(name string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, now.Add(-age), now.Add(-age))
	}
	touch("blite-old.log", 8*24*time.Hour)
	touch("other.txt", 30*24*time.Hour)
	for i := range keepMax + 3 {
		touch(fmt.Sprintf("blite-%03d.log", i), time.Duration(i)*time.Minute)
	}
	pruneLogs(dir, now)
	es, _ := os.ReadDir(dir)
	have := map[string]bool{}
	for _, e := range es {
		have[e.Name()] = true
	}
	if have["blite-old.log"] || !have["other.txt"] || len(es) != keepMax+1 {
		t.Fatalf("after prune: %v", have)
	}
	if !have["blite-000.log"] || have[fmt.Sprintf("blite-%03d.log", keepMax)] {
		t.Fatalf("kept the wrong ones: %v", have)
	}
}
