package logbuf

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// sink records each Write as one batch.
type sink struct {
	mu      sync.Mutex
	batches []string
	delay   time.Duration
}

func (s *sink) Write(p []byte) (int, error) {
	time.Sleep(s.delay)
	s.mu.Lock()
	s.batches = append(s.batches, string(p))
	s.mu.Unlock()
	return len(p), nil
}

func (s *sink) all() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.batches, "")
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.batches)
}

func TestOrderAndBatching(t *testing.T) {
	var s sink
	w := New(&s, 0, 0, time.Hour)
	var want strings.Builder
	for i := range 100 {
		l := fmt.Sprintf("line %d\n", i)
		w.Write([]byte(l))
		want.WriteString(l)
	}
	if s.count() != 0 {
		t.Fatalf("wrote before a flush: %d batches", s.count())
	}
	w.Flush()
	if s.count() != 1 || s.all() != want.String() {
		t.Fatalf("got %d batches: %q", s.count(), s.all())
	}
	w.Close()
}

func TestBatchSizeKicks(t *testing.T) {
	var s sink
	w := New(&s, 1<<20, 1024, time.Hour)
	defer w.Close()
	w.Write(bytes.Repeat([]byte("x"), 1100))
	deadline := time.Now().Add(2 * time.Second)
	for s.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.count() != 1 {
		t.Fatal("a full batch wasn't written")
	}
}

func TestInterval(t *testing.T) {
	var s sink
	w := New(&s, 0, 0, 20*time.Millisecond)
	defer w.Close()
	w.Write([]byte("a\n"))
	deadline := time.Now().Add(2 * time.Second)
	for s.all() != "a\n" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.all() != "a\n" {
		t.Fatal("the interval didn't write the line")
	}
}

func TestErrorFlushes(t *testing.T) {
	var s sink
	w := New(&s, 0, 0, time.Hour)
	defer w.Close()
	log := slog.New(Handler(slog.NewTextHandler(w, nil), w, slog.LevelError)).With("k", "v")
	log.Info("quiet")
	log.Warn("still quiet")
	if s.count() != 0 {
		t.Fatalf("info/warn flushed: %q", s.all())
	}
	log.WithGroup("g").Error("boom", "err", "x")
	got := s.all()
	if !strings.Contains(got, "quiet") || !strings.Contains(got, "still quiet") || !strings.Contains(got, "boom") {
		t.Fatalf("error didn't flush everything before it: %q", got)
	}
	if strings.Index(got, "still quiet") > strings.Index(got, "boom") {
		t.Fatalf("out of order: %q", got)
	}
}

func TestOverflowDrops(t *testing.T) {
	var s sink
	w := New(&s, 100, 1000, time.Hour)
	defer w.Close()
	w.Write(bytes.Repeat([]byte("a"), 60))
	w.Write(bytes.Repeat([]byte("b"), 30))
	w.Write(bytes.Repeat([]byte("c"), 20)) // 110 > 100: dropped
	w.Write(bytes.Repeat([]byte("d"), 15)) // dropped
	w.Write([]byte("e"))                   // fits again
	if d := w.Dropped(); d != 35 {
		t.Fatalf("dropped %d, want 35", d)
	}
	w.Flush()
	got := s.all()
	if strings.Contains(got, "cc") || strings.Contains(got, "dd") {
		t.Fatalf("dropped bytes written: %q", got)
	}
	i := strings.Index(got, "log: dropped 35 bytes")
	if i < 0 || !strings.HasPrefix(got, strings.Repeat("a", 60)+strings.Repeat("b", 30)+"e") {
		t.Fatalf("got %q", got)
	}
	if w.Dropped() != 0 {
		t.Fatal("the count wasn't reset once reported")
	}
	w.Write([]byte("f\n"))
	w.Flush()
	if strings.Count(s.all(), "log: dropped") != 1 {
		t.Fatalf("reported twice: %q", s.all())
	}
}

func TestSwapAndClose(t *testing.T) {
	var a, b sink
	cur := &a
	var mu sync.Mutex
	w := New(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return cur.Write(p)
	}), 0, 0, time.Hour)
	w.Write([]byte("old\n"))
	w.Swap(func() { mu.Lock(); cur = &b; mu.Unlock() })
	w.Write([]byte("new\n"))
	w.Close()
	if a.all() != "old\n" || b.all() != "new\n" {
		t.Fatalf("a %q b %q", a.all(), b.all())
	}
	w.Write([]byte("late\n"))
	w.Flush()
	if strings.Contains(b.all(), "late") {
		t.Fatal("wrote after Close")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestConcurrent: every goroutine's lines arrive whole and in its order.
func TestConcurrent(t *testing.T) {
	var s sink
	w := New(&s, 64<<20, 4096, time.Millisecond)
	const n, per = 8, 2000
	var wg sync.WaitGroup
	for g := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range per {
				fmt.Fprintf(w, "g%d %d\n", g, i)
				if i%500 == 0 {
					w.Flush()
				}
			}
		}()
	}
	wg.Wait()
	w.Close()
	next := make([]int, n)
	lines := strings.Split(strings.TrimSuffix(s.all(), "\n"), "\n")
	for _, l := range lines {
		var g, i int
		if _, err := fmt.Sscanf(l, "g%d %d", &g, &i); err != nil {
			t.Fatalf("torn line %q", l)
		}
		if i != next[g] {
			t.Fatalf("goroutine %d: got %d, want %d", g, i, next[g])
		}
		next[g]++
	}
	if len(lines) != n*per {
		t.Fatalf("%d lines, want %d", len(lines), n*per)
	}
}

// TestSlowDestination: callers don't wait for a destination that takes 50 ms
// a write.
func TestSlowDestination(t *testing.T) {
	s := sink{delay: 50 * time.Millisecond}
	w := New(&s, 0, 0, 10*time.Millisecond)
	defer w.Close()
	start := time.Now()
	for i := range 1000 {
		fmt.Fprintf(w, "line %d\n", i)
		time.Sleep(10 * time.Microsecond)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("1000 writes took %v", d)
	}
}

// Benchmarks: a slog line to a file on disk, directly and through a Writer,
// and to a destination that takes 50 ms a write.
func benchLog(b *testing.B, mk func(f *os.File) (*slog.Logger, func())) {
	f, err := os.Create(filepath.Join(b.TempDir(), "x.log"))
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	log, done := mk(f)
	defer done()
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		log.Info("share stats", "layer", 0, "n", i, "idr", false)
	}
}

func BenchmarkLogFileDirect(b *testing.B) {
	benchLog(b, func(f *os.File) (*slog.Logger, func()) {
		return slog.New(slog.NewTextHandler(f, nil)), func() {}
	})
}

func BenchmarkLogFileBuffered(b *testing.B) {
	benchLog(b, func(f *os.File) (*slog.Logger, func()) {
		w := New(f, 0, 0, 0)
		return slog.New(Handler(slog.NewTextHandler(w, nil), w, slog.LevelError)), func() { w.Close() }
	})
}

func BenchmarkLogSlowDirect(b *testing.B) {
	log := slog.New(slog.NewTextHandler(&sink{delay: 50 * time.Millisecond}, nil))
	b.ReportAllocs()
	for i := range b.N {
		log.Info("share stats", "n", i)
	}
}

func BenchmarkLogSlowBuffered(b *testing.B) {
	w := New(&sink{delay: 50 * time.Millisecond}, 0, 0, 0)
	defer w.Close()
	log := slog.New(Handler(slog.NewTextHandler(w, nil), w, slog.LevelError))
	b.ReportAllocs()
	for i := range b.N {
		log.Info("share stats", "n", i)
	}
}
