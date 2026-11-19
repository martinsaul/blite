package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	keepDump   = 2
	maxDumpAge = 14 * 24 * time.Hour
)

// pruneDumps deletes crash dumps older than maxDumpAge and keeps at most the
// newest keep remaining blite-*.dmp files in dir.
func pruneDumps(dir string, now time.Time, keep int) {
	if keep < 0 {
		keep = 0
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "blite-*.dmp"))
	var rem []string
	for _, p := range matches {
		if st, err := os.Stat(p); err == nil && now.Sub(st.ModTime()) > maxDumpAge {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				fmt.Fprintln(os.Stderr, err)
			}
			continue
		}
		rem = append(rem, p)
	}
	if len(rem) <= keep {
		return
	}
	sort.Strings(rem) // names carry a sortable timestamp
	for _, p := range rem[:len(rem)-keep] {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, err)
		}
	}
}
