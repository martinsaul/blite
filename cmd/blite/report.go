package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"blite/internal/diag"
	"blite/internal/metrics"
	"blite/internal/proto"
	"blite/internal/redact"
)

// logInfo is what the UI shows about our logs, so a user can tell where they
// are and whether any were written.
type logInfo struct {
	Dir   string `json:"dir,omitempty"`   // where the log files are
	File  string `json:"file,omitempty"`  // ours, when we write our own
	Error string `json:"error,omitempty"` // why we can't
	// Guard (Windows): "running", or why blite-guard isn't.
	Guard  string `json:"guard,omitempty"`
	Report string `json:"report,omitempty"` // the last upload's outcome
}

var logs struct {
	sync.Mutex
	logInfo
}

func setLogs(f func(*logInfo)) {
	logs.Lock()
	f(&logs.logInfo)
	logs.Unlock()
}

func logState() logInfo {
	logs.Lock()
	defer logs.Unlock()
	l := logs.logInfo
	if runtime.GOOS == "windows" {
		l.Guard = "running"
		if unguarded != "" {
			l.Guard = unguarded
		}
	}
	return l
}

// Reports go to the signal server (POST /v1/report); an operator reads them
// there instead of asking testers for files.
const (
	reportTail   = 256 << 10 // bytes of each log sent
	reportWithin = 3 * 24 * time.Hour
	cleanExit    = "clean exit" // our last line on a normal exit
)

// crashMarks in a log mean the run crashed (Go, the guard, Xlib).
var crashMarks = []string{"fatal error:", "panic:", "guard: crash", "Exception 0x", "X Error of failed request", "SIGSEGV"}

// recentLog is the last n whole lines of the log file at path.
func recentLog(path string, n int) string {
	if path == "" {
		return ""
	}
	t, err := tail(path, 16<<10)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(t, "\n"), "\n")
	if len(lines) > 1 {
		lines = lines[1:] // the first may be cut
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n"
}

func tail(path string, n int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if st.Size() > n {
		f.Seek(-n, io.SeekEnd)
	}
	b, err := io.ReadAll(f)
	return string(b), err
}

// ownSegment matches our segments (blite-<time>-<pid>.log); the guard's are
// blite-<time>.log and blite-<time>-event.log.
var ownSegment = regexp.MustCompile(`^blite-\d{8}-\d{6}-(\d+)\.log$`)

// crashedLogs lists log files from earlier runs worth reporting, oldest
// first: ones with a crash mark, and the last segment of each of our own
// runs that didn't end with a clean exit (killed, or a crash the runtime
// couldn't print). Files already reported (sent.txt) are skipped.
func crashedLogs(dir, current string, now time.Time) []string {
	sent := map[string]bool{}
	if b, err := os.ReadFile(filepath.Join(dir, "sent.txt")); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			sent[l] = true
		}
	}
	es, _ := os.ReadDir(dir)
	lastOfPID := map[string]string{} // pid -> newest segment name
	var out []string
	for _, e := range es {
		name := e.Name()
		if !strings.HasSuffix(name, ".log") || filepath.Join(dir, name) == current {
			continue
		}
		if m := ownSegment.FindStringSubmatch(name); m != nil && name > lastOfPID[m[1]] {
			lastOfPID[m[1]] = name
		}
	}
	for _, e := range es {
		name := e.Name()
		p := filepath.Join(dir, name)
		if !strings.HasSuffix(name, ".log") || p == current || sent[name] {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) > reportWithin || info.Size() == 0 {
			continue
		}
		t, err := tail(p, reportTail)
		if err != nil {
			continue
		}
		crashed := false
		for _, mk := range crashMarks {
			crashed = crashed || strings.Contains(t, mk)
		}
		if m := ownSegment.FindStringSubmatch(name); m != nil && lastOfPID[m[1]] == name && !strings.Contains(t, cleanExit) {
			crashed = true
		}
		if crashed {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func markSent(dir string, names ...string) {
	f, err := os.OpenFile(filepath.Join(dir, "sent.txt"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	for _, n := range names {
		fmt.Fprintln(f, n)
	}
}

// reporter sends logs for the tray app.
type reporter struct {
	server string
	base   proto.Report // platform, version, build, OS
	name   func() string
	log    *slog.Logger
	// system is a plain-text summary of this computer and our state for
	// the zip (system.txt); nil = none.
	system func() string
}

// Send logs also uploads a zip: whole log files (up to zipFileMax each,
// the newest zipFiles of them), the newest zipMetrics metrics files and
// system.txt, at most zipMax compressed.
const (
	zipFiles   = 6
	zipFileMax = 32 << 20
	zipMetrics = 2 // up to metrics.FileMax (1 MB) each
	zipMax     = 8 << 20
)

// buildZip packs the newest of names (sorted oldest first) from dir with
// the metrics files bins and system, dropping the oldest logs until it fits
// zipMax.
func buildZip(dir string, names, bins []string, system string) ([]byte, []string, error) {
	if len(names) > zipFiles {
		names = names[len(names)-zipFiles:]
	}
	for len(names) > 0 {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		if system != "" {
			w, _ := zw.Create("system.txt")
			io.WriteString(w, redact.Text(system))
		}
		for _, n := range names {
			t, err := tail(filepath.Join(dir, n), zipFileMax)
			if err != nil {
				continue
			}
			w, err := zw.Create(n)
			if err != nil {
				return nil, nil, err
			}
			io.WriteString(w, redact.Text(t))
		}
		for _, n := range bins { // numbers only: nothing to redact
			b, err := os.ReadFile(filepath.Join(dir, n))
			if err != nil || len(b) > zipFileMax {
				continue
			}
			w, err := zw.Create(n)
			if err != nil {
				return nil, nil, err
			}
			w.Write(b)
		}
		if err := zw.Close(); err != nil {
			return nil, nil, err
		}
		if buf.Len() <= zipMax {
			return buf.Bytes(), names, nil
		}
		names = names[1:]
	}
	return nil, nil, errors.New("logs too large to zip")
}

// previous reports earlier runs' crashes, once each, in the background.
func (r *reporter) previous() {
	l := logState()
	if l.Dir == "" {
		return
	}
	for _, name := range crashedLogs(l.Dir, l.File, time.Now()) {
		t, err := tail(filepath.Join(l.Dir, name), crashRead)
		if err != nil {
			continue
		}
		rep := r.base
		// crashLog redacts what it takes from t.
		rep.Name, rep.Kind, rep.File, rep.Log, rep.Note = r.name(), "crash", name, crashLog(t, reportTail), redact.Text(l.Guard)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err = diag.SendReport(ctx, r.server, rep)
		cancel()
		if err != nil {
			r.log.Warn("crash report", "file", name, "err", err)
			return // offline: try again next start
		}
		markSent(l.Dir, name)
		r.log.Info("crash report sent", "file", name)
	}
}

// pickUserLogs chooses what "Send logs" uploads, oldest first: this run's
// log and the two newest others. This run's log is ours (current) when we
// write one, else the guard's newest journal still without "-event": the
// guard renames a journal only once its run has ended. Going by newest name
// alone sent only the stubs of short runs (each blite:// link starts one)
// and left out the session being reported.
func pickUserLogs(names []string, current string) []string {
	sort.Strings(names)
	if current == "." || !slices.Contains(names, current) {
		current = ""
		for _, n := range names {
			if !strings.HasSuffix(n, "-event.log") && !ownSegment.MatchString(n) {
				current = n
			}
		}
	}
	var out []string
	for i := len(names) - 1; i >= 0 && len(out) < 2; i-- {
		if names[i] != current {
			out = append(out, names[i])
		}
	}
	if current != "" {
		out = append(out, current)
	}
	sort.Strings(out)
	return out
}

// send uploads this run's log and the two newest others (the user's
// "Send logs").
func (r *reporter) send(ctx context.Context) error {
	flushLogs() // the report has the last lines
	l := logState()
	if l.Dir == "" {
		return errors.New("no log folder: " + l.Error)
	}
	es, _ := os.ReadDir(l.Dir)
	var names []string
	for _, e := range es {
		if strings.HasSuffix(e.Name(), ".log") {
			names = append(names, e.Name())
		}
	}
	names = pickUserLogs(names, filepath.Base(l.File))
	if len(names) == 0 {
		err := errors.New("no log files in " + l.Dir)
		setLogs(func(x *logInfo) { x.Report = err.Error() })
		return err
	}
	var b strings.Builder
	for _, n := range names {
		t, _ := tail(filepath.Join(l.Dir, n), reportTail)
		fmt.Fprintf(&b, "===== %s\n%s\n", n, redact.Text(t))
	}
	rep := r.base
	rep.Name, rep.Kind, rep.File, rep.Log, rep.Note = r.name(), "user", strings.Join(names, ", "), b.String(), redact.Text(l.Guard)
	var all []string
	for _, e := range es {
		if n := e.Name(); strings.HasSuffix(n, ".log") {
			all = append(all, n)
		}
	}
	sort.Strings(all)
	system := ""
	if r.system != nil {
		system = r.system()
	}
	metricsRec.Flush() // the readings up to now
	z, zipped, zerr := buildZip(l.Dir, all, metrics.Newest(l.Dir, zipMetrics), system)
	if zerr != nil {
		r.log.Warn("log zip", "err", zerr)
	}
	rep.Zip = z
	err := diag.SendReport(ctx, r.server, rep)
	if err != nil && len(rep.Zip) > 0 && ctx.Err() == nil {
		// A server from before zips refuses the size: send the tails alone.
		r.log.Warn("logs with zip refused, sending without", "err", err)
		rep.Zip = nil
		err = diag.SendReport(ctx, r.server, rep)
	} else if err == nil && len(z) > 0 {
		r.log.Info("log zip sent", "files", strings.Join(zipped, ","), "bytes", len(z))
	}
	res := "sent " + time.Now().Format("15:04")
	if err != nil {
		res = "could not send: " + err.Error()
	}
	setLogs(func(x *logInfo) { x.Report = res })
	r.log.Info("logs sent by the user", "result", res)
	return err
}

// feedback sends what the user wrote in the feedback dialog: an idea or a
// problem, with the diagnostics (no names, keys or links) if they agreed.
// The server keeps it as a report of kind "feedback".
func (r *reporter) feedback(ctx context.Context, kind, text, details string) error {
	rep := r.base
	rep.Name, rep.Kind, rep.Note = r.name(), "feedback", kind
	rep.Log = "feedback (" + kind + "):\n" + redact.Text(text) + "\n"
	if details != "" {
		rep.Log += "\n===== diagnostics\n" + redact.Text(details)
	}
	err := diag.SendReport(ctx, r.server, rep)
	r.log.Info("feedback sent", "kind", kind, "chars", len(text), "err", err)
	return err
}
