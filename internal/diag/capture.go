package diag

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"blite/internal/capture"
	"blite/internal/media"
)

// CaptureRes follows frames through the app's real share pipeline on the
// primary monitor, plus the capture alone, to tell where frames get lost.
type CaptureRes struct {
	Source  string         `json:"source"`  // the media source used, as a share would
	Monitor string         `json:"monitor"` // its name and size
	Window  string         `json:"window,omitempty"`
	WinSrc  string         `json:"windowSource,omitempty"`
	Probes  []CaptureProbe `json:"probes"`
	Runs    []ShareRun     `json:"runs"`
}

// CaptureProbe is the capture alone (into a fakesink), without the share's
// videorate, queues and encoders.
type CaptureProbe struct {
	Source string  `json:"source"`
	What   string  `json:"what"`
	FPS    float64 `json:"fps"`
	Caps   string  `json:"caps,omitempty"`
	Error  string  `json:"error,omitempty"`
}

// ShareRun is one target size through the whole share pipeline. The
// rates are per second over the run, at each point:
// captured -> (convert, videorate) -> offered -> HIGH's leaky queue in ->
// queue out (the rest it dropped) -> encoded.
type ShareRun struct {
	Source      string  `json:"source"`
	Target      string  `json:"target"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	BitrateKbs  int     `json:"bitrateKbs"`
	Encoder     string  `json:"encoder"`
	Chain       string  `json:"chain,omitempty"` // where the capture is scaled and converted: gpu or cpu
	Secs        float64 `json:"secs"`
	CapturedFPS float64 `json:"capturedFPS"`
	OfferedFPS  float64 `json:"offeredFPS"`
	QueueInFPS  float64 `json:"queueInFPS"`
	QueueOutFPS float64 `json:"queueOutFPS"`
	DroppedPS   float64 `json:"droppedPerSec"`
	EncodedFPS  float64 `json:"encodedFPS"`
	CaptureCaps string  `json:"captureCaps,omitempty"`
	// Gaps between captured frames (by arrival and by timestamp) and
	// between HIGH's access units: stalls an average hides.
	CaptureGaps Gaps `json:"captureGaps"`
	CapturePTS  Gaps `json:"capturePtsGaps"`
	AUGaps      Gaps `json:"auGaps"`
	// ConsumerMaxMs is the longest our access-unit callback took (it only
	// counts, like a share with nobody to send to): a slow consumer would
	// hold up the appsink and with it the encoder.
	ConsumerMaxMs float64     `json:"consumerMaxMs"`
	CPUBusyPct    float64     `json:"cpuBusyPct"` // whole machine
	GPUEngines    string      `json:"gpuEngines,omitempty"`
	GPUTop        []EngineUse `json:"gpuTop,omitempty"`
	NvidiaSMI     string      `json:"nvidiaSmi,omitempty"`
	// NoEncoder is the same capture path with a fakesink for the encoder;
	// NoEncoderOther the same on the other chains, to compare.
	NoEncoder      NoEncoderRun   `json:"noEncoder"`
	NoEncoderOther []NoEncoderRun `json:"noEncoderOther,omitempty"`
	Verdict        string         `json:"verdict"`
	Error          string         `json:"error,omitempty"`
}

// NoEncoderRun is a share's capture path into a fakesink (ProbeSharePath).
type NoEncoderRun struct {
	Chain       string  `json:"chain,omitempty"`
	CapturedFPS float64 `json:"capturedFPS"`
	OfferedFPS  float64 `json:"offeredFPS"`
	OutFPS      float64 `json:"outFPS"` // after the leaky queue
	CaptureGaps Gaps    `json:"captureGaps"`
	CaptureCaps string  `json:"captureCaps,omitempty"`
	Error       string  `json:"error,omitempty"`
}

// Gaps sums up the times between consecutive frames, in ms.
type Gaps struct {
	N      int     `json:"n"`
	P95Ms  float64 `json:"p95Ms"`
	MaxMs  float64 `json:"maxMs"`
	Over50 int     `json:"over50ms"` // gaps of more than 50 ms (3 frames at 60)
}

func gapStats(ds []time.Duration) Gaps {
	var v []float64
	for _, d := range ds {
		if d >= 0 {
			v = append(v, ms(d))
		}
	}
	if len(v) == 0 {
		return Gaps{}
	}
	sort.Float64s(v)
	g := Gaps{N: len(v), P95Ms: v[len(v)*95/100], MaxMs: v[len(v)-1]}
	for _, x := range v {
		if x > 50 {
			g.Over50++
		}
	}
	return g
}

func (g Gaps) String() string {
	if g.N == 0 {
		return "none"
	}
	return fmt.Sprintf("p95 %.0f ms, max %.0f ms, %d over 50 ms", g.P95Ms, g.MaxMs, g.Over50)
}

var monitorSize = regexp.MustCompile(`\((\d+)x(\d+)\)`)

// primaryMonitor is the source a share of the primary monitor uses and
// that monitor's size (0 if unknown).
func primaryMonitor() (src, name string, w, h int) {
	src = capture.PrimarySource()
	ts, _ := capture.List()
	var mons []capture.Target
	for _, t := range ts {
		if t.Kind == "monitor" {
			mons = append(mons, t)
		}
	}
	for _, t := range mons {
		if t.Primary || len(mons) == 1 || t.Source == src {
			name = t.Name
			if m := monitorSize.FindStringSubmatch(t.Name); m != nil {
				w, _ = strconv.Atoi(m[1])
				h, _ = strconv.Atoi(m[2])
			}
			break
		}
	}
	return src, name, w, h
}

// consoleApps are programs whose windows are terminals (this tool's own
// window among them), never the window to test.
var consoleApps = []string{"cmd", "powershell", "pwsh", "windowsterminal", "conhost", "openconsole", "blite-diag",
	"terminal", "xterm", "konsole", "alacritty", "kitty", "wezterm"}

// pickWindow is the window to test: the first whose title contains want,
// else the topmost window that isn't a terminal.
func pickWindow(want string) (capture.Target, bool) {
	ts, _ := capture.List()
	for _, t := range ts {
		if t.Kind != "window" {
			continue
		}
		if want != "" {
			if strings.Contains(strings.ToLower(t.Name), strings.ToLower(want)) {
				return t, true
			}
			continue
		}
		app := strings.ToLower(t.App)
		console := false
		for _, c := range consoleApps {
			console = console || strings.Contains(app, c)
		}
		if !console {
			return t, true
		}
	}
	return capture.Target{}, false
}

func runCapture(ctx context.Context, r *Report, p *printer, countdown func(context.Context), window string) error {
	SetDPIAware()
	media.Init()
	warm, dur, probeDur := 2*time.Second, 10*time.Second, 4*time.Second
	if r.Quick {
		warm, dur, probeDur = 1500*time.Millisecond, 4*time.Second, 2*time.Second
	}
	src, name, mw, mh := primaryMonitor()
	res := &CaptureRes{Source: src, Monitor: name}
	r.with(func() { r.Capture = res })
	p.f("   source %s (%s), as a share of the primary monitor", src, orUnknown(name))

	type target struct {
		label string
		w, h  int
	}
	var targets []target
	for _, t := range []target{{"2160p60", 3840, 2160}, {"1080p60", 1920, 1080}, {"720p60", 1280, 720}} {
		if mw > 0 && mh > 0 && mh < t.h {
			t = target{fmt.Sprintf("%dx%d@60 (native)", mw, mh), mw, mh}
		}
		if len(targets) == 0 || targets[len(targets)-1].h != t.h {
			targets = append(targets, t)
		}
	}

	p.f("   KEEP SOMETHING MOVING ON SCREEN now, at 60 fps: play a 60 fps video (YouTube 2160p60 or 1080p60) or a game, full screen if you can.")
	p.f("   The capture only gets new frames when the screen changes. Don't touch this window until the step ends.")
	if countdown != nil {
		countdown(ctx)
	} else {
		for i := 3; i > 0 && ctx.Err() == nil; i-- {
			p.f("   %d...", i)
			sleepCtx(ctx, time.Second)
		}
	}

	probe := func(source, what string, fps int) {
		rate, err := media.ProbeCapture(media.SourceKind(source), fps, time.Second, probeDur)
		pr := CaptureProbe{Source: source, What: what, FPS: rate.FPS, Caps: rate.Caps}
		if err != nil {
			pr.Error = err.Error()
		}
		r.with(func() { res.Probes = append(res.Probes, pr) })
		p.f("   capture alone, %s: %.1f fps  %s%s", what, pr.FPS, pr.Caps, errNote(err))
	}
	runs := func(source, prefix string, ts []target) error {
		probe(source, "frame rate left to the source (as the share does)", 0)
		probe(source, "asking for 60 fps", 60)
		for _, t := range ts {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			run := shareRun(ctx, source, prefix+t.label, t.w, t.h, warm, dur, p)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			for _, c := range media.ShareChains(media.SourceKind(source)) {
				if string(c) == run.Chain || run.Chain == "" {
					run.NoEncoder = noEncoderRun(source, run, c, warm, dur/2, p)
					run.Chain = string(c)
				} else {
					run.NoEncoderOther = append(run.NoEncoderOther, noEncoderRun(source, run, c, warm, dur/2, p))
				}
			}
			run.Verdict = captureVerdict(run, res.Probes)
			r.with(func() { res.Runs = append(res.Runs, run) })
			p.f("   => %s", run.Verdict)
		}
		return nil
	}
	if err := runs(src, "", targets); err != nil {
		return err
	}

	// A window: captured by its own handle, of any size (letterboxed).
	w, ok := pickWindow(window)
	if !ok {
		if window != "" {
			return fmt.Errorf("no window titled like %q", window)
		}
		p.f("   no window to test besides this console")
		return nil
	}
	r.with(func() { res.Window, res.WinSrc = w.Label(), w.Source })
	p.f("\n   window %q (%s, %s): keep it moving too", w.Label(), w.App, w.Source)
	return runs(w.Source, "window ", []target{{"1080p60", 1920, 1080}, {"720p60", 1280, 720}})
}

// shareRun runs the app's share pipeline (StartVideoSourceLayers, with
// the encoder the app would pick) at w x h, 60 fps, for dur after warm.
func shareRun(ctx context.Context, src, label string, w, h int, warm, dur time.Duration, p *printer) ShareRun {
	l := shareLayer(w, h, 60)
	run := ShareRun{Source: src, Target: label, Width: w, Height: h, BitrateKbs: l.BitrateKbs}
	res := media.Res1080
	if h > 1080 {
		res = media.Res2160
	}
	layers := media.LayersForOpts(media.LayerOpts{Resolution: res, FPS: 60, MinHeight: 360})
	layers[media.High] = l
	var (
		aus    atomic.Int64
		auMu   sync.Mutex
		auLast time.Time
		auGaps []time.Duration
	)
	var consumerMax atomic.Int64
	vs, enc, err := media.StartVideoSourceLayers(media.SourceKind(src), layers, "", "", func(layer int, _ media.AccessUnit) {
		if layer != media.High {
			return
		}
		now := time.Now()
		defer func() {
			if d := int64(time.Since(now)); d > consumerMax.Load() {
				consumerMax.Store(d)
			}
		}()
		aus.Add(1)
		auMu.Lock()
		if !auLast.IsZero() {
			auGaps = append(auGaps, now.Sub(auLast))
		}
		auLast = now
		auMu.Unlock()
	})
	if err != nil {
		run.Error = err.Error()
		run.Verdict = "the share pipeline didn't start: " + firstLine(run.Error)
		p.f("   %s: share pipeline failed: %s", label, firstLine(run.Error))
		return run
	}
	defer vs.Stop()
	run.Encoder, run.Chain = enc, string(vs.Chain())
	wait := func(d time.Duration) error {
		select {
		case err := <-vs.Err:
			if err == nil {
				err = fmt.Errorf("ended")
			}
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
			return nil
		}
	}
	if err := wait(warm); err != nil {
		run.Error = err.Error()
		return run
	}
	vs.WatchCaptureGaps()
	vs.TakeCaptureGaps()
	auMu.Lock()
	auGaps = nil
	auMu.Unlock()
	c0, a0, t0 := vs.Counts(), aus.Load(), time.Now()
	i0, tot0, cpuOK := cpuTimes()
	stopGPU, gpuErr := sampleGPU()
	nvOut := make(chan string, 1)
	go func() {
		// nvidia-smi halfway through: the encoder's sessions and load.
		if path := nvidiaSMIPath(); path != "" && sleepCtx(ctx, dur/2) {
			out, _ := runCmd(ctx, 10*time.Second, path, "--query-gpu=utilization.gpu,utilization.encoder,encoder.stats.sessionCount,encoder.stats.averageFps,clocks.gr,clocks.video,pstate", "--format=csv")
			nvOut <- out
		}
		close(nvOut)
	}()
	err = wait(dur)
	c1, a1, secs := vs.Counts(), aus.Load(), time.Since(t0).Seconds()
	arr, pts := vs.TakeCaptureGaps()
	run.CaptureGaps, run.CapturePTS = gapStats(arr), gapStats(pts)
	auMu.Lock()
	run.AUGaps = gapStats(auGaps)
	auMu.Unlock()
	run.ConsumerMaxMs = ms(time.Duration(consumerMax.Load()))
	if err != nil {
		run.Error = err.Error()
	}
	if i1, tot1, ok := cpuTimes(); ok && cpuOK && tot1 > tot0 {
		run.CPUBusyPct = 100 * (1 - float64(i1-i0)/float64(tot1-tot0))
	}
	if gpuErr == nil {
		use := stopGPU()
		nameProcs(use)
		run.GPUEngines = fmtEngines(engineTotals(use))
		if len(use) > 6 {
			use = use[:6]
		}
		run.GPUTop = use
	}
	select {
	case run.NvidiaSMI = <-nvOut:
	case <-time.After(5 * time.Second):
	}
	rate := func(a, b int) float64 {
		if a < 0 || b < 0 {
			return -1
		}
		return float64(b-a) / secs
	}
	run.Secs = secs
	run.CapturedFPS = rate(c0.Captured, c1.Captured)
	run.OfferedFPS = rate(c0.Offered, c1.Offered)
	run.QueueInFPS = rate(c0.HighIn, c1.HighIn)
	run.QueueOutFPS = rate(c0.HighOut, c1.HighOut)
	if run.QueueInFPS >= 0 && run.QueueOutFPS >= 0 {
		run.DroppedPS = max(0, run.QueueInFPS-run.QueueOutFPS)
	}
	run.EncodedFPS = float64(a1-a0) / secs
	run.CaptureCaps = media.ShortCaps(c1.CaptureCaps)
	p.f("   %s with %s (%d kbit/s, converted on the %s): captured %.1f, offered %.1f, into queue %.1f, out of queue %.1f (dropped %.1f/s), encoded %.1f fps; CPU %.0f%%",
		label, enc, l.BitrateKbs, strings.ToUpper(run.Chain), run.CapturedFPS, run.OfferedFPS, run.QueueInFPS, run.QueueOutFPS, run.DroppedPS, run.EncodedFPS, run.CPUBusyPct)
	if run.CaptureCaps != "" {
		p.f("      capture negotiated %s", run.CaptureCaps)
	}
	p.f("      gaps between captured frames: %s (by timestamp: %s); between encoded frames: %s; longest access-unit callback %.2f ms",
		run.CaptureGaps, run.CapturePTS, run.AUGaps, run.ConsumerMaxMs)
	if run.GPUEngines != "" {
		p.f("      GPU engines: %s", run.GPUEngines)
	}
	for _, l := range strings.Split(run.NvidiaSMI, "\n") {
		if l != "" {
			p.f("      nvidia-smi: %s", l)
		}
	}
	return run
}

// noEncoderRun runs run's capture path on chain with a fakesink for the
// encoder.
func noEncoderRun(src string, run ShareRun, chain media.Chain, warm, dur time.Duration, p *printer) NoEncoderRun {
	format := "NV12"
	if run.Encoder != "" {
		format = media.EncoderFormat(run.Encoder)
	}
	l := shareLayer(run.Width, run.Height, 60)
	r, err := media.ProbeSharePath(media.SourceKind(src), l, format, chain, warm, dur)
	n := NoEncoderRun{Chain: string(chain), CapturedFPS: r.Captured, OfferedFPS: r.Offered, OutFPS: r.Out, CaptureGaps: gapStats(r.Gaps), CaptureCaps: r.CaptureCaps}
	if err != nil {
		n.Error = err.Error()
	}
	p.f("   %s, encoder replaced by a fakesink, converted on the %s: captured %.1f, offered %.1f, out of queue %.1f fps; capture gaps %s%s",
		run.Target, strings.ToUpper(n.Chain), n.CapturedFPS, n.OfferedFPS, n.OutFPS, n.CaptureGaps, errNote(err))
	return n
}

// captureVerdict says where a share run lost its frames: every cause it
// sees, the most upstream first.
func captureVerdict(run ShareRun, probes []CaptureProbe) string {
	head := fmt.Sprintf("capture %s: %.0f fps encoded", run.Target, run.EncodedFPS)
	if run.CapturedFPS >= 0 {
		head += fmt.Sprintf(", %.0f captured", run.CapturedFPS)
	}
	if run.Error != "" && run.EncodedFPS <= 0 {
		return head + ", failed: " + firstLine(run.Error)
	}
	raw60 := -1.0
	for _, pr := range probes {
		if pr.Source == run.Source && pr.What == "asking for 60 fps" && pr.Error == "" {
			raw60 = pr.FPS
		}
	}
	// Near enough to 60 for a cause to lie elsewhere.
	const fine = 0.9 * okFPS
	ne := run.NoEncoder
	noEnc := ne.Error == "" && ne.CapturedFPS > 0
	var causes []string
	if run.CapturedFPS >= 0 && run.CapturedFPS < okFPS {
		switch {
		case noEnc && ne.CapturedFPS >= fine:
			causes = append(causes, fmt.Sprintf("without the encoder the same capture path does %.0f fps -> the encoder stalls the capture (shared GPU or a blocked thread)", ne.CapturedFPS))
		case raw60 >= 0 && raw60 < fine:
			causes = append(causes, fmt.Sprintf("the capture alone gives only %.0f fps (too little moving on screen? GPU busy, HAGS, overlays) -> capture is the bottleneck", raw60))
		case noEnc:
			causes = append(causes, fmt.Sprintf("the capture alone does %.0f fps but only %.0f once scaled and converted to %dx%d (no encoder) -> conversion is the bottleneck", raw60, ne.CapturedFPS, run.Width, run.Height))
		default:
			causes = append(causes, "the capture is held back")
		}
	}
	for _, o := range run.NoEncoderOther {
		if o.Error == "" && noEnc && o.CapturedFPS > ne.CapturedFPS+5 {
			causes = append(causes, fmt.Sprintf("converted on the %s instead, the capture path does %.0f fps without the encoder (%.0f on the %s)",
				strings.ToUpper(o.Chain), o.CapturedFPS, ne.CapturedFPS, strings.ToUpper(ne.Chain)))
		}
	}
	if run.CaptureGaps.MaxMs > 100 {
		causes = append(causes, fmt.Sprintf("the capture stalled: gaps up to %.0f ms (%d over 50 ms)", run.CaptureGaps.MaxMs, run.CaptureGaps.Over50))
	}
	if run.CapturedFPS > 72 {
		causes = append(causes, fmt.Sprintf("the capture delivers %.0f fps, more than the 60 sent: each extra frame is converted, then dropped by videorate (no capture frame-rate cap)", run.CapturedFPS))
	}
	if run.DroppedPS > 2 {
		causes = append(causes, fmt.Sprintf("the leaky queue dropped %.0f of %.0f frames/s -> the encoder (%s) is the bottleneck", run.DroppedPS, run.QueueInFPS, run.Encoder))
	}
	if run.OfferedFPS >= 0 && run.OfferedFPS < okFPS && run.CapturedFPS >= okFPS {
		causes = append(causes, fmt.Sprintf("captured %.0f but offered only %.0f -> lost in conversion or videorate", run.CapturedFPS, run.OfferedFPS))
	}
	if run.QueueOutFPS >= 0 && run.EncodedFPS < run.QueueOutFPS-3 {
		causes = append(causes, fmt.Sprintf("the encoder took %.0f frames/s but put out %.0f", run.QueueOutFPS, run.EncodedFPS))
	}
	if run.CPUBusyPct > 90 {
		causes = append(causes, fmt.Sprintf("the CPU was %.0f%% busy", run.CPUBusyPct))
	}
	// videorate repeats frames up to 60, so the encoder can make 60 a
	// second out of fewer captured ones.
	if run.EncodedFPS >= okFPS && (run.CapturedFPS < 0 || run.CapturedFPS >= okFPS) {
		if len(causes) == 0 {
			return head + " OK"
		}
		return head + " OK, but " + strings.Join(causes, "; ")
	}
	if len(causes) == 0 {
		causes = append(causes, fmt.Sprintf("offered %.0f, queue in %.0f out %.0f -> the encoder's output lags", run.OfferedFPS, run.QueueInFPS, run.QueueOutFPS))
	}
	return head + ": " + strings.Join(causes, "; ")
}
