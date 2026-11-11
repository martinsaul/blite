//go:build !windows

package discord

import (
	"io"
	"net"
	"path/filepath"
	"testing"
)

func fakeListen(t *testing.T) func() (io.ReadWriteCloser, error) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	ln, err := net.Listen("unix", filepath.Join(dir, "discord-ipc-0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return func() (io.ReadWriteCloser, error) { return ln.Accept() }
}
