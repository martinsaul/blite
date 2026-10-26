package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"
)

// Log segments: a new one at start, every hour and past segMax. Kept a week,
// at most keepMax files.
const (
	segEvery = time.Hour
	segMax   = 20 << 20
	keepFor  = 7 * 24 * time.Hour
	keepMax  = 50
)

// logToFiles sends everything the process writes to stderr (our logs, a Go
// crash traceback, on Linux also GStreamer's warnings) to rotating files in
// logDir, so there is a log however Blite was started: from the menu, at
// sign-in or by molt-swap after an update, none of which keep stderr. Not
// when stderr already goes somewhere useful (keepStderr): a terminal, or
// blite-guard on Windows.
func logToFiles(log *slog.Logger) {
	dir, err := logDir()
	setLogs(func(l *logInfo) { l.Dir = dir })
	if err != nil {
		setLogs(func(l *logInfo) { l.Error = err.Error() })
		log.Warn("log files", "err", err)
		return
	}
	if os.Getenv("BLITE_LOG_STDERR") != "" || keepStderr() {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		setLogs(func(l *logInfo) { l.Error = err.Error() })
		log.Warn("log files", "err", err)
		return
	}
	f, err := newSegment(dir)
	if err != nil {
		setLogs(func(l *logInfo) { l.Error = err.Error() })
		log.Warn("log files", "err", err)
		return
	}
	debug.SetTraceback("all") // a crash lists every goroutine
	log.Info("logging to", "file", f.Name())
	go func() {
		start := time.Now()
		for range time.Tick(time.Minute) {
			st, err := f.Stat()
			if err != nil || (time.Since(start) < segEvery && st.Size() < segMax) {
				continue
			}
			if nf, err := newSegment(dir); err == nil {
				f.Close()
				f, start = nf, time.Now()
			}
		}
	}()
}

// newSegment opens a new log file, makes it our stderr and prunes old ones.
func newSegment(dir string) (*os.File, error) {
	name := fmt.Sprintf("blite-%s-%d.log", time.Now().Format("20060102-150405"), os.Getpid())
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err := redirectStderr(f); err != nil {
		f.Close()
		return nil, err
	}
	setLogs(func(l *logInfo) { l.File = f.Name() })
	pruneLogs(dir, time.Now())
	return f, nil
}

// pruneLogs deletes segments older than keepFor, then the oldest beyond keepMax.
func pruneLogs(dir string, now time.Time) {
	es, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type seg struct {
		path string
		mod  time.Time
	}
	var segs []seg
	for _, e := range es {
		if !strings.HasPrefix(e.Name(), "blite-") || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if now.Sub(info.ModTime()) > keepFor {
			os.Remove(p)
			continue
		}
		segs = append(segs, seg{p, info.ModTime()})
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].mod.After(segs[j].mod) })
	for _, s := range segs[min(len(segs), keepMax):] {
		os.Remove(s.path)
	}
}
