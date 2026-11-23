package userinstall

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRootAndInside(t *testing.T) {
	d := t.TempDir()
	t.Setenv("XDG_DATA_HOME", d)
	root, err := Root()
	if err != nil || root != filepath.Join(d, "bellion", "app") {
		t.Fatalf("Root = %q, %v", root, err)
	}
	if !Inside(filepath.Join(root, "bin", "blite")) || Inside(SystemExe) {
		t.Fatal("Inside")
	}
}

// Seed copies the running build into an empty folder (so molt-swap has
// something to go back to); Build reads it back, and an interrupted swap or
// a missing program reads as no copy.
func TestSeedAndBuild(t *testing.T) {
	root := filepath.Join(t.TempDir(), "app")
	exe := filepath.Join(t.TempDir(), "blite")
	os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755)
	if Build(root) != 0 {
		t.Fatal("empty root has a build")
	}
	if err := Seed(root, exe, 216); err != nil {
		t.Fatal(err)
	}
	if b := Build(root); b != 216 {
		t.Fatalf("Build = %d", b)
	}
	if fi, err := os.Stat(filepath.Join(root, Exe)); err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("seeded program: %v %v", fi, err)
	}
	// A newer copy stays; an older one (a newer .deb runs) is replaced.
	os.WriteFile(filepath.Join(root, buildFile), []byte("220\n"), 0o644)
	if err := Seed(root, exe, 216); err != nil || Build(root) != 220 {
		t.Fatalf("Seed replaced a newer copy: %v %d", err, Build(root))
	}
	if err := Seed(root, exe, 230); err != nil || Build(root) != 230 {
		t.Fatalf("Seed kept an older copy: %v %d", err, Build(root))
	}
	os.WriteFile(filepath.Join(root, buildFile), []byte("220\n"), 0o644)
	os.MkdirAll(root+".old", 0o755)
	os.WriteFile(filepath.Join(root+".old", ".molt-swap-in-progress"), nil, 0o644)
	if Build(root) != 0 {
		t.Fatal("a copy mid-swap counts")
	}
	os.RemoveAll(root + ".old")
	os.Chmod(filepath.Join(root, Exe), 0o644)
	if Build(root) != 0 {
		t.Fatal("a copy that can't run counts")
	}
	// A broken folder (no build file) is replaced by Seed.
	os.Remove(filepath.Join(root, buildFile))
	if err := Seed(root, exe, 216); err != nil || Build(root) != 216 {
		t.Fatalf("Seed over a broken copy: %v %d", err, Build(root))
	}
}
