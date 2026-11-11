package discord

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"
)

// dial opens Discord's named pipe (discord-ipc-0..9; the first that answers).
//
// The pipe is opened for overlapped I/O: on a plain (synchronous) handle, as
// os.OpenFile gives, Windows queues a write behind the read our reader
// goroutine has pending, so after the handshake nothing could be sent.
func dial() (io.ReadWriteCloser, error) {
	var last error
	for i := 0; i < 10; i++ {
		name, _ := windows.UTF16PtrFromString(fmt.Sprintf(`\\.\pipe\discord-ipc-%d`, i))
		h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
			windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
		if err == nil {
			return &pipe{h: h}, nil
		}
		last = err
	}
	return nil, last
}

// pipe is an overlapped pipe handle: one read and one write may be pending
// at once.
type pipe struct {
	h      windows.Handle
	mu     sync.RWMutex // held for reading by each I/O, for writing by Close
	closed atomic.Bool
}

func (p *pipe) io(b []byte, write bool) (int, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(ev)
	o := windows.Overlapped{HEvent: ev}
	var n uint32
	if write {
		err = windows.WriteFile(p.h, b, &n, &o)
	} else {
		err = windows.ReadFile(p.h, b, &n, &o)
	}
	if errors.Is(err, windows.ERROR_IO_PENDING) {
		err = windows.GetOverlappedResult(p.h, &o, &n, true)
	}
	switch {
	case err == nil:
		if n == 0 && !write && len(b) > 0 {
			return 0, io.EOF
		}
		return int(n), nil
	case p.closed.Load() || errors.Is(err, windows.ERROR_OPERATION_ABORTED):
		return int(n), io.ErrClosedPipe
	case errors.Is(err, windows.ERROR_BROKEN_PIPE), errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED):
		return int(n), io.EOF
	}
	return int(n), err
}

func (p *pipe) Read(b []byte) (int, error) { return p.io(b, false) }

func (p *pipe) Write(b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := p.io(b[n:], true)
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// Close cancels pending I/O, waits for it to end, then closes the handle.
func (p *pipe) Close() error {
	if p.closed.Swap(true) {
		return nil
	}
	// An I/O may start between our cancel and its check of closed; cancel
	// until every one has returned.
	for {
		windows.CancelIoEx(p.h, nil)
		if p.mu.TryLock() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer p.mu.Unlock()
	return windows.CloseHandle(p.h)
}
