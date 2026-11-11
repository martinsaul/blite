package discord

import (
	"io"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// fakeListen serves discord-ipc-0 like Discord does: a byte-mode named pipe.
func fakeListen(t *testing.T) func() (io.ReadWriteCloser, error) {
	name, _ := windows.UTF16PtrFromString(`\\.\pipe\discord-ipc-0`)
	h, err := windows.CreateNamedPipe(name, windows.PIPE_ACCESS_DUPLEX,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT, 1, 4096, 4096, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(h), "discord-ipc-0")
	t.Cleanup(func() { f.Close() })
	return func() (io.ReadWriteCloser, error) {
		if err := windows.ConnectNamedPipe(h, nil); err != nil && err != windows.ERROR_PIPE_CONNECTED {
			return nil, err
		}
		return f, nil
	}
}
