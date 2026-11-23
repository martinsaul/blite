package userinstall

import (
	"os"
	"path/filepath"
	"syscall"
)

// Handover runs the per-user copy in place of this process when this is
// the .deb's copy and the per-user one is a newer build. It returns only
// when there is none (or it can't start), and this copy carries on.
// BLITE_SYSTEM_COPY=1 skips it (to start the .deb's copy by hand).
func Handover(build int64) error {
	if os.Getenv("BLITE_SYSTEM_COPY") == "1" {
		return ErrNoCopy
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, _ = filepath.EvalSymlinks(exe); exe != SystemExe {
		return ErrNoCopy
	}
	root, err := Root()
	if err != nil {
		return err
	}
	if Build(root) <= build {
		return ErrNoCopy
	}
	p := filepath.Join(root, Exe)
	return syscall.Exec(p, append([]string{p}, os.Args[1:]...), os.Environ())
}
