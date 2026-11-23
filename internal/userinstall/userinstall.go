// Package userinstall is Bellion Share's per-user copy on Linux. The .deb
// installs the system parts and /usr/bin/blite once; updates go to a copy in
// the user's data folder, which molt-swap replaces without root (the same
// file swap and rollback as on Windows). /usr/bin/blite starts that copy when
// it is newer than itself.
package userinstall

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SystemExe is the .deb's own copy, which also starts the per-user one.
const SystemExe = "/usr/bin/blite"

// Exe is the program inside Root, relative to it.
const Exe = "bin/blite"

// buildFile in Root holds the build number of the copy there.
const buildFile = "build"

// Root is the per-user install folder: $XDG_DATA_HOME/bellion/app, by
// default ~/.local/share/bellion/app.
func Root() (string, error) {
	d := os.Getenv("XDG_DATA_HOME")
	if d == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		d = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(d, "bellion", "app"), nil
}

// Inside reports whether exe is the per-user copy.
func Inside(exe string) bool {
	root, err := Root()
	return err == nil && filepath.Clean(exe) == filepath.Join(root, Exe)
}

// Build returns the build of the copy in root, 0 if there is none or it is
// incomplete (an interrupted swap left molt-swap's journal in root.old).
func Build(root string) int64 {
	if swapping(root) {
		return 0
	}
	fi, err := os.Stat(filepath.Join(root, Exe))
	if err != nil || !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 {
		return 0
	}
	b, err := os.ReadFile(filepath.Join(root, buildFile))
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// Seed puts a copy of exe (the running build) into root before a per-user
// update, unless the copy there is at least that build. molt-swap then goes
// back to the build that is running (the one it was told about) if the new
// one fails to start.
func Seed(root, exe string, build int64) error {
	if Build(root) >= build || swapping(root) {
		return nil // this build or newer, or one molt-swap restores first
	}
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	dst := filepath.Join(root, Exe)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := copyFile(exe, dst+".tmp", 0o755); err != nil {
		return err
	}
	if err := os.Rename(dst+".tmp", dst); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, buildFile), []byte(strconv.FormatInt(build, 10)+"\n"), 0o644)
}

// swapping: molt-swap's journal says a swap of root didn't finish.
func swapping(root string) bool {
	_, err := os.Stat(filepath.Join(root+".old", ".molt-swap-in-progress"))
	return err == nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ErrNoCopy: there is no newer per-user copy to start.
var ErrNoCopy = errors.New("no newer per-user copy")
