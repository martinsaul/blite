package diag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// errNotHere: this measurement is made on Windows only.
var errNotHere = errors.New("measured on Windows only")

// Options configure a computer test (Run).
type Options struct {
	Server  string          // signaling URL, for the network step
	Quick   bool            // shorter runs
	Skip    map[string]bool // step names to leave out: system, procs, encode, capture, net
	Out     io.Writer       // progress, as it happens
	Version string          // the tool's version label, for the report
	// Window picks the window the capture step also tests, by part of its
	// title ("": the topmost window that isn't a terminal).
	Window string
	// Countdown is called before the capture runs, so the user can put
	// something moving on screen (nil: a 3 s countdown on Out).
	Countdown func(ctx context.Context)
	// Checkpoint gets the report after every step, so a crash in a driver
	// still leaves what was measured until then (nil: none).
	Checkpoint func(*Report)
}

// Report is everything a computer test found.
type Report struct {
	Tool      string        `json:"tool"`
	Version   string        `json:"version"`
	Started   time.Time     `json:"started"`
	Host      string        `json:"host"`
	Platform  string        `json:"platform"`
	Quick     bool          `json:"quick"`
	System    *System       `json:"system,omitempty"`
	Displays  []Display     `json:"displays,omitempty"`
	Processes *Processes    `json:"processes,omitempty"`
	Encode    *EncodeResult `json:"encode,omitempty"`
	Capture   *CaptureRes   `json:"capture,omitempty"`
	Net       *NetResult    `json:"net,omitempty"`
	Steps     []StepResult  `json:"steps"`
	Verdicts  []string      `json:"verdicts"`
	// Text is what the console showed (not in the JSON).
	Text string `json:"-"`
	mu   sync.Mutex
}

// StepResult is how one step went.
type StepResult struct {
	Name    string  `json:"name"`
	Secs    float64 `json:"secs"`
	Skipped bool    `json:"skipped,omitempty"`
	Error   string  `json:"error,omitempty"`
}

// printer writes progress to the console and keeps it for the report.
type printer struct {
	mu  sync.Mutex
	out io.Writer
	buf strings.Builder
}

func (p *printer) f(format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf.WriteString(s)
	if p.out != nil {
		io.WriteString(p.out, s)
	}
}

func (p *printer) text() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.String()
}

// step is one part of the test. run fills its part of the report and
// returns an error to record; a panic is recorded the same way.
type step struct {
	name    string
	title   string
	timeout time.Duration
	run     func(ctx context.Context, r *Report, p *printer) error
}

// Run tests this computer for sharing: system, displays, the programs that
// compete for the GPU's encoder, every H.264 encoder a share may use, the
// real share pipeline on the primary monitor, and the network. Each step
// has its own time limit; a failed step is recorded and the rest still run.
func Run(ctx context.Context, o Options) *Report {
	host, _ := os.Hostname()
	r := &Report{Tool: "blite-diag", Version: o.Version, Started: time.Now(), Host: host,
		Platform: runtime.GOOS + "/" + runtime.GOARCH, Quick: o.Quick}
	p := &printer{out: o.Out}
	p.f("Bellion Share computer test %s on %s, %s", o.Version, host, r.Started.Format("2006-01-02 15:04:05 MST"))
	steps := []step{
		{"system", "System", 45 * time.Second, runSystem},
		{"procs", "Programs that may compete for the GPU", 45 * time.Second, runProcs},
		{"encode", "Encoder speed (synthetic video, as fast as it goes)", 8 * time.Minute, runEncode},
		{"capture", "Screen capture with the app's share pipeline", 6 * time.Minute, func(ctx context.Context, r *Report, p *printer) error {
			return runCapture(ctx, r, p, o.Countdown, o.Window)
		}},
		{"net", "Network", 90 * time.Second, func(ctx context.Context, r *Report, p *printer) error {
			return runNet(ctx, r, p, o.Server)
		}},
	}
	for _, s := range steps {
		if ctx.Err() != nil {
			r.addStep(StepResult{Name: s.name, Skipped: true, Error: "cancelled"})
			continue
		}
		if o.Skip[s.name] {
			p.f("\n== %s: skipped", s.title)
			r.addStep(StepResult{Name: s.name, Skipped: true})
			continue
		}
		p.f("\n== %s", s.title)
		res := runStep(ctx, s, r, p)
		if res.Error != "" {
			p.f("   ERROR: %s", res.Error)
		}
		r.addStep(res)
		if o.Checkpoint != nil {
			r.with(func() { r.Text = p.text() })
			o.Checkpoint(r)
		}
	}
	var vs []string
	var failed []StepResult
	r.with(func() {
		r.Verdicts = verdicts(r)
		vs, failed = r.Verdicts, r.Steps
	})
	p.f("\n== Summary")
	for _, v := range vs {
		p.f("   %s", v)
	}
	for _, s := range failed {
		if s.Error != "" && !s.Skipped {
			p.f("   step %s failed: %s", s.Name, firstLine(s.Error))
		}
	}
	r.with(func() { r.Text = p.text() })
	return r
}

func (r *Report) addStep(s StepResult) {
	r.with(func() { r.Steps = append(r.Steps, s) })
}

// with runs f under the report's lock: steps fill the report while the
// checkpoint may be writing it out.
func (r *Report) with(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f()
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// runStep runs s with its time limit. A step that overruns is left behind
// (a GStreamer call may not return) and recorded as timed out.
func runStep(ctx context.Context, s step, r *Report, p *printer) StepResult {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if v := recover(); v != nil {
				done <- fmt.Errorf("panic: %v\n%s", v, debug.Stack())
			}
		}()
		done <- s.run(ctx, r, p)
	}()
	res := StepResult{Name: s.name}
	select {
	case err := <-done:
		if err != nil {
			res.Error = err.Error()
		}
	case <-ctx.Done():
		res.Error = fmt.Sprintf("timed out after %s", s.timeout)
		// Give the step a moment to notice and record what it has.
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
	res.Secs = time.Since(start).Seconds()
	return res
}

// JSON is the report as indented JSON.
func (r *Report) JSON() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := json.MarshalIndent(r, "", " ")
	if err != nil {
		return []byte(fmt.Sprintf("{\"error\": %q}", err.Error()))
	}
	return b
}

// Full is the console text followed by the JSON: what is saved and sent.
func (r *Report) Full() string {
	return r.Text + "\n===== json\n" + string(r.JSON()) + "\n"
}

// Save writes the report (Full) to blite-diag-<host>-<time>.txt in dir, or
// in the temporary folder when dir isn't writable. It returns the path.
func (r *Report) Save(dir string) (string, error) {
	name := fmt.Sprintf("blite-diag-%s-%s.txt", safeName(r.Host), r.Started.Format("20060102-150405"))
	b := []byte(r.Full())
	if runtime.GOOS == "windows" {
		b = []byte(strings.ReplaceAll(string(b), "\n", "\r\n")) // for Notepad
	}
	var err error
	for _, d := range []string{dir, os.TempDir()} {
		if d == "" {
			continue
		}
		path := filepath.Join(d, name)
		if err = os.WriteFile(path, b, 0o644); err == nil {
			return path, nil
		}
	}
	return "", err
}

func safeName(s string) string {
	s = strings.Map(func(c rune) rune {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			return c
		}
		return '_'
	}, s)
	if s == "" {
		return "pc"
	}
	return s
}

// sleepCtx sleeps d or until ctx ends; false if ctx ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
