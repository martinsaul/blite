// Package logbuf decouples log callers from the disk: Write only appends to
// memory, and one goroutine writes what piled up in batches. A slow write (a
// virus scanner holding the file, a busy disk) then stalls that goroutine
// instead of the encoder or packet loop that logged.
package logbuf

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// Defaults: what a stalled disk may pile up before lines are dropped, the
// pending size that starts a write early, and the longest a line waits.
// Every bounds what a native crash (one the Go runtime can't see, like a
// GStreamer DLL fault) or a panic outside the main goroutine loses: the
// lines of the last interval at most.
const (
	DefaultMax   = 2 << 20
	DefaultBatch = 64 << 10
	DefaultEvery = 250 * time.Millisecond
)

// Writer is an io.Writer that never blocks on its destination. When Max
// bytes are pending it drops whole writes, counts them, and writes a
// "log: dropped N bytes" line with the next batch.
type Writer struct {
	mu      sync.Mutex // buf, spare, dropped, closed
	buf     []byte
	spare   []byte
	dropped int64
	closed  bool
	max     int
	batch   int

	out  sync.Mutex // held while writing to dst; batches go out in order
	dst  io.Writer
	kick chan struct{}
	stop chan struct{}
	done chan struct{}
}

// New starts a Writer to dst. max, batch and every <= 0 take the defaults.
func New(dst io.Writer, max, batch int, every time.Duration) *Writer {
	if max <= 0 {
		max = DefaultMax
	}
	if batch <= 0 {
		batch = DefaultBatch
	}
	if every <= 0 {
		every = DefaultEvery
	}
	w := &Writer{max: max, batch: batch, dst: dst, kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	go w.run(every)
	return w
}

// Write appends p to the pending batch. It never fails: after Close, or with
// Max pending, p is dropped (and counted).
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.closed || len(w.buf)+len(p) > w.max {
		w.dropped += int64(len(p))
		w.mu.Unlock()
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	full := len(w.buf) >= w.batch
	w.mu.Unlock()
	if full {
		select {
		case w.kick <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

// Flush writes everything pending now and returns once it is written.
func (w *Writer) Flush() error {
	w.out.Lock()
	defer w.out.Unlock()
	return w.writeOut()
}

// Swap writes everything pending, then runs fn while no batch is being
// written: for changing what dst writes to (a new log file).
func (w *Writer) Swap(fn func()) {
	w.out.Lock()
	defer w.out.Unlock()
	w.writeOut()
	fn()
}

// Close flushes and stops the writing goroutine; later writes are dropped.
func (w *Writer) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()
	close(w.stop)
	<-w.done
	return w.Flush()
}

// Dropped is how many bytes were dropped and not yet reported in the log.
func (w *Writer) Dropped() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

func (w *Writer) run(every time.Duration) {
	defer close(w.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
		case <-w.kick:
		}
		w.Flush()
	}
}

// writeOut writes the pending batch to dst; w.out is held. The buffers
// trade places, so callers append to the other one meanwhile.
func (w *Writer) writeOut() error {
	w.mu.Lock()
	b, dropped := w.buf, w.dropped
	w.buf, w.spare, w.dropped = w.spare[:0], nil, 0
	w.mu.Unlock()
	if dropped > 0 { // at the end of the batch the lines were dropped from
		b = fmt.Appendf(b, "time=%s level=WARN msg=\"log: dropped %d bytes\"\n", time.Now().Format(time.RFC3339Nano), dropped)
	}
	var err error
	if len(b) > 0 {
		_, err = w.dst.Write(b)
	}
	if cap(b) > 4*w.batch && cap(b) > w.max/2 {
		b = nil // a stall's big buffer isn't kept around
	}
	w.mu.Lock()
	w.spare = b[:0]
	w.mu.Unlock()
	return err
}

// Handler wraps h so a record at level or above is written out before the
// log call returns: an error (and what led to it) is on disk even if the
// process dies right after.
func Handler(h slog.Handler, w *Writer, level slog.Level) slog.Handler {
	return &handler{h, w, level}
}

type handler struct {
	slog.Handler
	w     *Writer
	level slog.Level
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	err := h.Handler.Handle(ctx, r)
	if r.Level >= h.level {
		h.w.Flush()
	}
	return err
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	return &handler{h.Handler.WithAttrs(as), h.w, h.level}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{h.Handler.WithGroup(name), h.w, h.level}
}
