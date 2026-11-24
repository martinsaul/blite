package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// A Recorder keeps records in memory and appends them to its file every
// WriteEvery and on Flush: one second of readings costs a slice append, not
// a disk write. A file past FileMax is followed by a new one.
const (
	WriteEvery = 30 * time.Second
	FileMax    = 1 << 20 // ~6.5 hours
	ringMax    = 30 * 60 // records kept while the disk fails (30 min)
	keepFiles  = 24
	keepFor    = 7 * 24 * time.Hour
)

// Recorder appends records to metrics files in dir. A nil *Recorder takes
// records and does nothing.
type Recorder struct {
	dir string

	mu      sync.Mutex // pending, closed
	pending []Record
	closed  bool

	file sync.Mutex // f, size; held while writing
	f    *os.File
	size int64

	stop chan struct{}
	done chan struct{}
}

// NewRecorder starts a Recorder writing to dir.
func NewRecorder(dir string) *Recorder {
	r := &Recorder{dir: dir, stop: make(chan struct{}), done: make(chan struct{})}
	go r.run()
	return r
}

// Add keeps rec for the next write. It doesn't block on the disk.
func (r *Recorder) Add(rec Record) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if !r.closed {
		if len(r.pending) >= ringMax { // the oldest go: writes keep failing
			r.pending = append(r.pending[:0], r.pending[len(r.pending)-ringMax+1:]...)
		}
		r.pending = append(r.pending, rec)
	}
	r.mu.Unlock()
}

// Flush appends the pending records to the file now.
func (r *Recorder) Flush() error {
	if r == nil {
		return nil
	}
	r.file.Lock()
	defer r.file.Unlock()
	r.mu.Lock()
	recs := r.pending
	r.pending = nil
	r.mu.Unlock()
	if len(recs) == 0 {
		return nil
	}
	err := r.write(recs)
	if err != nil { // back in front of what came meanwhile, for the next try
		r.mu.Lock()
		r.pending = append(recs, r.pending...)
		if len(r.pending) > ringMax {
			r.pending = r.pending[len(r.pending)-ringMax:]
		}
		r.mu.Unlock()
	}
	return err
}

// Close writes what is pending and stops; later records are dropped.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	close(r.stop)
	<-r.done
	err := r.Flush()
	r.file.Lock()
	if r.f != nil {
		r.f.Close()
		r.f = nil
	}
	r.file.Unlock()
	return err
}

// Path is the file being written ("" before the first write).
func (r *Recorder) Path() string {
	if r == nil {
		return ""
	}
	r.file.Lock()
	defer r.file.Unlock()
	if r.f == nil {
		return ""
	}
	return r.f.Name()
}

func (r *Recorder) run() {
	defer close(r.done)
	t := time.NewTicker(WriteEvery)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			r.Flush()
		}
	}
}

// write appends recs, starting a file when there is none or it is full;
// r.file is held.
func (r *Recorder) write(recs []Record) error {
	b := Encode(recs)
	if r.f != nil && r.size+int64(len(b)) > FileMax {
		r.f.Close()
		r.f = nil
	}
	if r.f == nil {
		if err := os.MkdirAll(r.dir, 0o700); err != nil {
			return err
		}
		name := fmt.Sprintf("metrics-%s-%d.bin", time.Now().Format("20060102-150405"), os.Getpid())
		f, err := os.OpenFile(filepath.Join(r.dir, name), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.Write(header); err != nil {
			f.Close()
			return err
		}
		r.f, r.size = f, int64(len(header))
		Prune(r.dir, time.Now())
	}
	n, err := r.f.Write(b)
	r.size += int64(n)
	if err == nil || n == 0 {
		return err
	}
	r.f.Close() // a part record: the next write starts a new file
	r.f = nil
	return nil
}

// IsFile reports whether name is a metrics file's name.
func IsFile(name string) bool {
	return strings.HasPrefix(name, "metrics-") && strings.HasSuffix(name, ".bin")
}

// Prune deletes metrics files in dir older than a week, then the oldest
// beyond keepFiles.
func Prune(dir string, now time.Time) {
	es, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var keep []string
	for _, e := range es {
		if !IsFile(e.Name()) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > keepFor {
			os.Remove(p)
			continue
		}
		keep = append(keep, e.Name())
	}
	sort.Strings(keep) // by time in the name
	for _, n := range keep[:max(0, len(keep)-keepFiles)] {
		os.Remove(filepath.Join(dir, n))
	}
}

// Newest is the newest n metrics files in dir, oldest first.
func Newest(dir string, n int) []string {
	es, _ := os.ReadDir(dir)
	var names []string
	for _, e := range es {
		if IsFile(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names[max(0, len(names)-n):]
}
