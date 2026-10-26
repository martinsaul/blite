package main

import (
	"os"
	"path/filepath"
	"reflect"
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
