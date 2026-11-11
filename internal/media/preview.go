package media

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"blite/internal/gst"
)

// The self-preview is our own picture, small: the video source
// feeds it at PreviewW x PreviewH and PreviewFPS, and its window can be
// sized from previewMinH to PreviewH lines (144p to 240p).
const (
	PreviewW, PreviewH = 426, 240
	PreviewFPS         = 15
	previewMinW        = 256
	previewMinH        = 144
	// previewStartW x previewStartH is the window's first size.
	previewStartW, previewStartH = 320, 180
	previewTitle                 = "Bellion Share: your preview"
	// previewStartTimeout is how long a sink may take to draw its first
	// frames (intervideosrc sends one at once, black until we share).
	previewStartTimeout = 3 * time.Second
)

// previewSeq numbers the self-preview channels.
var previewSeq atomic.Uint64

// NewPreviewChannel names a fresh intervideo channel for a self-preview.
func NewPreviewChannel() string { return fmt.Sprintf("blite-self-%d", previewSeq.Add(1)) }

// previewWindow is a native window for the preview whose size the window
// manager keeps between 144p and 240p.
type previewWindow interface {
	Handle() uintptr
	// Closed is closed when the user closes the window.
	Closed() <-chan struct{}
	Destroy()
}

// Preview shows our own picture from a preview channel in a small window.
type Preview struct {
	*Pipeline
	win previewWindow // nil: the sink's own window (size not held) or none
	// Sink is the video sink showing the preview (for the log).
	Sink   string
	out    *gst.Counter
	mu     sync.Mutex
	closed chan struct{}
	once   sync.Once
}

// StartPreview opens a preview window showing channel. view is ViewWindow,
// or ViewCount for headless tests (no window, frames counted).
func StartPreview(channel string, view ViewKind) (*Preview, error) {
	pv := &Preview{closed: make(chan struct{})}
	if view == ViewCount {
		p, err := launch("intervideosrc channel=" + channel + " " + interHold + " ! identity name=out ! fakesink sync=false")
		if err != nil {
			return nil, err
		}
		pv.Pipeline = p
		if out, err := p.element("out"); err == nil {
			pv.out, _ = out.CountBuffers()
		}
		if err := p.start(); err != nil {
			pv.free()
			return nil, err
		}
		return pv, nil
	}
	var errs []error
	if w, err := openPreviewWindow(); err != nil {
		errs = append(errs, err)
	} else {
		for _, sink := range previewSinks {
			if !gst.HasFactory(sink) {
				continue
			}
			p, in, err := startPreviewInto(channel, sink, w)
			if err == nil {
				pv.Pipeline, pv.win, pv.Sink, pv.out = p, w, sink, in
				go pv.watch(w.Closed())
				return pv, nil
			}
			errs = append(errs, fmt.Errorf("%s: %w", sink, err))
		}
		w.Destroy()
	}
	// No window of our own (Wayland, an odd driver): let the sink open
	// its window. It can be resized freely then.
	p, err := launch("intervideosrc channel=" + channel + " " + interHold + " ! videoconvert ! autovideosink sync=false")
	if err == nil {
		err = p.start()
	}
	if err != nil {
		return nil, errors.Join(append(errs, err)...)
	}
	pv.Pipeline, pv.Sink = p, "autovideosink"
	if len(errs) > 0 {
		pv.Sink += " (" + errors.Join(errs...).Error() + ")"
	}
	return pv, nil
}

// startPreviewInto shows channel with sink in w. It returns once the sink
// drew frames: a sink can accept PLAYING and fail on its first frame (no
// Xv port, no Direct3D device). The counter counts frames into the sink.
func startPreviewInto(channel, sink string, w previewWindow) (*Pipeline, *gst.Counter, error) {
	p, err := launch(fmt.Sprintf("intervideosrc channel=%s %s ! videoconvert ! identity name=in ! %s name=sink sync=false force-aspect-ratio=true", channel, interHold, sink))
	if err != nil {
		return nil, nil, err
	}
	el, err := p.element("sink")
	if err != nil {
		p.Stop()
		return nil, nil, err
	}
	if !el.SetWindow(w.Handle()) {
		p.Stop()
		return nil, nil, errors.New("not a video overlay")
	}
	in, ok := (*gst.Counter)(nil), false
	if e, err := p.element("in"); err == nil {
		in, ok = e.CountBuffers()
	}
	fail := func(err error) (*Pipeline, *gst.Counter, error) {
		p.Stop()
		if ok {
			in.Free()
		}
		return nil, nil, err
	}
	if err := p.start(); err != nil {
		return fail(err)
	}
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(previewStartTimeout)
	for ok && in.Load() < 2 { // the 2nd frame comes once the 1st was drawn
		select {
		case err := <-p.Err:
			return fail(err)
		case <-tick.C:
		case <-deadline:
			return fail(errors.New("no frame drawn"))
		}
	}
	select {
	case err := <-p.Err:
		return fail(err)
	case <-time.After(200 * time.Millisecond):
	}
	if !ok {
		in = nil
	}
	return p, in, nil
}

func (pv *Preview) watch(closed <-chan struct{}) {
	select {
	case <-closed:
		pv.once.Do(func() { close(pv.closed) })
	case <-pv.done:
	}
}

// Closed is closed when the user closes the preview window (our own
// window; the sink's own window reports through Err instead).
func (pv *Preview) Closed() <-chan struct{} { return pv.closed }

// Frames is how many frames the preview has shown (-1 if unknown).
func (pv *Preview) Frames() int {
	pv.mu.Lock()
	defer pv.mu.Unlock()
	if pv.out == nil {
		return -1
	}
	return pv.out.Load()
}

// Stop closes the preview. The pipeline stops before its window goes.
func (pv *Preview) Stop() {
	pv.Pipeline.Stop()
	pv.free()
	if pv.win != nil {
		pv.win.Destroy()
	}
}

func (pv *Preview) free() {
	pv.mu.Lock()
	defer pv.mu.Unlock()
	if pv.out != nil {
		pv.out.Free()
		pv.out = nil
	}
}

// fitAspect returns the client size for a drag that set w x h, kept 16:9 and
// within 144p..240p. byHeight: the user drags the top or bottom edge.
func fitAspect(w, h int, byHeight bool) (int, int) {
	if byHeight {
		h = min(max(h, previewMinH), PreviewH)
		return h * 16 / 9, h
	}
	w = min(max(w, previewMinW), PreviewW)
	return w, (w*9 + 8) / 16
}
