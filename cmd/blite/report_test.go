package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCrashedLogs(t *testing.T) {
	dir := t.TempDir()
	w := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w("blite-20261026-010000-100.log", "a\n")                     // pid 100, earlier segment
	w("blite-20261026-020000-100.log", "b\nmsg=\"clean exit\"\n") // pid 100 ended cleanly
	w("blite-20261026-030000-200.log", "c\n")                     // pid 200 just stopped: unclean
	w("blite-20261026-040000-300.log", "panic: boom\n")           // crash mark
	w("blite-20261026-050000.log", "guard: started\n")            // guard, quiet
	w("blite-20261026-060000-event.log", "guard: crash: exception 0xc0000005\n")
	w("blite-20261026-070000-400.log", "current\n")
	w("sent.txt", "blite-20261026-040000-300.log\n")
	got := crashedLogs(dir, filepath.Join(dir, "blite-20261026-070000-400.log"), time.Now())
	want := []string{"blite-20261026-030000-200.log", "blite-20261026-060000-event.log"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestReportURL(t *testing.T) {
	if u, _ := reportURL("wss://blite.example.com/ws"); u != "https://blite.example.com/v1/report" {
		t.Fatal(u)
	}
}

func TestPickUserLogs(t *testing.T) {
	// Windows under the guard: the running tray's journal is the oldest;
	// two blite:// links each left a short -event journal after it.
	names := []string{
		"blite-20261103-120000-event.log",
		"blite-20261103-123000.log",
		"blite-20261103-123706-event.log",
		"blite-20261103-123810-event.log",
	}
	got := pickUserLogs(names, "")
	want := []string{"blite-20261103-123000.log", "blite-20261103-123706-event.log", "blite-20261103-123810-event.log"}
	if !slices.Equal(got, want) {
		t.Fatalf("guard: got %v, want %v", got, want)
	}
	// Our own segments: current is named.
	names = []string{"blite-20261103-100000-11.log", "blite-20261103-110000-22.log", "blite-20261103-120000-33.log", "blite-20261103-130000-44.log"}
	got = pickUserLogs(names, "blite-20261103-100000-11.log")
	want = []string{"blite-20261103-100000-11.log", "blite-20261103-120000-33.log", "blite-20261103-130000-44.log"}
	if !slices.Equal(got, want) {
		t.Fatalf("own: got %v, want %v", got, want)
	}
}

// The Send logs zip holds system.txt and the newest zipFiles logs, whole.
func TestBuildZip(t *testing.T) {
	dir := t.TempDir()
	var names []string
	for i := range 8 {
		n := fmt.Sprintf("blite-2026110%d-010000-1.log", i)
		os.WriteFile(filepath.Join(dir, n), []byte(strings.Repeat("line\n", 1000*(i+1))), 0o600)
		names = append(names, n)
	}
	b, got, err := buildZip(dir, names, "cpus: 8\n")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, names[2:]) {
		t.Fatalf("zipped %v", got)
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 1+zipFiles || zr.File[0].Name != "system.txt" {
		t.Fatalf("entries %d, first %q", len(zr.File), zr.File[0].Name)
	}
	f, _ := zr.File[len(zr.File)-1].Open()
	whole, _ := io.ReadAll(f)
	if len(whole) != 5*1000*8 {
		t.Fatalf("newest log: %d bytes, want it whole", len(whole))
	}
}
