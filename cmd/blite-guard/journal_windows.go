package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Retention: a segment where nothing happened expires after keepQuiet; one
// with an event (an error line, a crash, an injected DLL) is kept for
// keepEvent. Segments roll every segmentLen, so a run that lasts days still
// only keeps its last 24 h of quiet logging.
const (
	segmentLen = time.Hour
	keepQuiet  = 24 * time.Hour
	keepEvent  = 7 * 24 * time.Hour
)

// journal is the guard's rolling log: Blite's output and the guard's own
// lines, in hourly segments named blite-<start>.log, renamed to
// blite-<start>-event.log once something worth keeping happened in them.
type journal struct {
	dir string

	mu      sync.Mutex
	f       *os.File
	path    string
	started time.Time
	event   bool
	partial []byte // Blite output after its last newline
}

func openJournal(dir string) (*journal, error) {
	j := &journal{dir: dir}
	j.prune()
	return j, j.rotate(time.Now())
}

// rotate closes the current segment (keeping its event mark) and opens a new
// one. Callers hold mu, except openJournal.
func (j *journal) rotate(now time.Time) error {
	j.closeSegment()
	j.path = filepath.Join(j.dir, "blite-"+now.Format("20060102-150405")+".log")
	f, err := os.Create(j.path)
	if err != nil {
		return err
	}
	j.f, j.started, j.event = f, now, false
	return nil
}

func (j *journal) closeSegment() {
	if j.f == nil {
		return
	}
	j.f.Close()
	j.f = nil
	if j.event {
		os.Rename(j.path, strings.TrimSuffix(j.path, ".log")+"-event.log")
	}
}

// prune deletes quiet segments older than keepQuiet and event segments older
// than keepEvent. Old-style per-run logs count as quiet.
func (j *journal) prune() {
	m, _ := filepath.Glob(filepath.Join(j.dir, "*.log"))
	now := time.Now()
	for _, p := range m {
		if p == j.path {
			continue
		}
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		keep := keepQuiet
		if strings.HasSuffix(p, "-event.log") {
			keep = keepEvent
		}
		if now.Sub(st.ModTime()) > keep {
			os.Remove(p)
		}
	}
}

func (j *journal) writeLine(line []byte) {
	now := time.Now()
	if now.Sub(j.started) >= segmentLen {
		if err := j.rotate(now); err != nil {
			return
		}
		go j.prune()
	}
	if j.f != nil {
		j.f.Write(line)
	}
}

// Guardf writes one guard line. event marks the segment as worth keeping.
func (j *journal) Guardf(event bool, format string, args ...any) {
	line := fmt.Sprintf("%s guard: %s\n", time.Now().Format("2006-01-02T15:04:05.000Z07:00"), fmt.Sprintf(format, args...))
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.partial) > 0 { // keep Blite's unterminated output in order
		j.writeLine(append(j.partial, '\n'))
		j.partial = j.partial[:0]
	}
	j.writeLine([]byte(line))
	j.event = j.event || event
}

// Write takes Blite's stdout and stderr. Lines that look like trouble mark
// the segment as an event.
func (j *journal) Write(b []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	data := append(j.partial, b...)
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		line := data[:i+1]
		j.writeLine(line) // may roll the segment; the mark goes to the new one
		if eventful(line) {
			j.event = true
		}
		data = data[i+1:]
	}
	j.partial = append(j.partial[:0], data...)
	return len(b), nil
}

func eventful(line []byte) bool {
	for _, k := range [][]byte{[]byte("level=ERROR"), []byte("fatal error"), []byte("panic:"), []byte("Exception 0x")} {
		if bytes.Contains(line, k) {
			return true
		}
	}
	return false
}

func (j *journal) Close() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.partial) > 0 {
		j.writeLine(append(j.partial, '\n'))
		j.partial = nil
	}
	j.closeSegment()
}
