// Package gst is a deliberately tiny cgo shim over the stable GStreamer 1.0 C API:
// launch a pipeline, find an element by name, pull from appsink, push to appsrc,
// read errors off the bus. It replaces generated bindings, which track a single
// GStreamer release; this compiles against 1.20 through 1.26 and with MinGW.
package gst

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-app-1.0 gstreamer-video-1.0
#include <stdlib.h>
#include <string.h>
#include <gst/gst.h>
#include <gst/app/gstappsink.h>
#include <gst/app/gstappsrc.h>
#include <gst/video/video.h>

static GstElement *blite_launch(const char *desc, char **err) {
	GError *e = NULL;
	GstElement *p = gst_parse_launch(desc, &e);
	if (e != NULL) {
		*err = g_strdup(e->message);
		g_error_free(e);
		if (p != NULL) gst_object_unref(p);
		return NULL;
	}
	return p;
}

static GstElement *blite_by_name(GstElement *bin, const char *name) {
	return gst_bin_get_by_name(GST_BIN(bin), name);
}

static int blite_set_state(GstElement *e, int state) {
	return gst_element_set_state(e, (GstState)state) != GST_STATE_CHANGE_FAILURE;
}

// Pulls one sample; copies its data into *out (g_malloc'd). Returns 1 on data,
// 0 on timeout, -1 on EOS/flushing.
static int blite_pull(GstElement *sink, guint64 timeout_ns, void **out, gsize *len, guint64 *dur) {
	GstSample *s = gst_app_sink_try_pull_sample(GST_APP_SINK(sink), timeout_ns);
	if (s == NULL) {
		return gst_app_sink_is_eos(GST_APP_SINK(sink)) ? -1 : 0;
	}
	GstBuffer *b = gst_sample_get_buffer(s);
	GstMapInfo m;
	if (b == NULL || !gst_buffer_map(b, &m, GST_MAP_READ)) {
		gst_sample_unref(s);
		return 0;
	}
	*out = g_memdup2(m.data, m.size);
	*len = m.size;
	*dur = GST_BUFFER_DURATION_IS_VALID(b) ? GST_BUFFER_DURATION(b) : 0;
	gst_buffer_unmap(b, &m);
	gst_sample_unref(s);
	return 1;
}

static int blite_push(GstElement *src, const void *data, gsize len) {
	GstBuffer *b = gst_buffer_new_allocate(NULL, len, NULL);
	gst_buffer_fill(b, 0, data, len);
	return gst_app_src_push_buffer(GST_APP_SRC(src), b) == GST_FLOW_OK; // takes ownership
}

// Pops the next error/EOS message. Returns 0 none, 1 error (msg set), 2 EOS.
static int blite_bus_pop(GstElement *pipe, guint64 timeout_ns, char **msg) {
	GstBus *bus = gst_element_get_bus(pipe);
	GstMessage *m = gst_bus_timed_pop_filtered(bus, timeout_ns, GST_MESSAGE_ERROR | GST_MESSAGE_EOS);
	gst_object_unref(bus);
	if (m == NULL) return 0;
	int r = 2;
	if (GST_MESSAGE_TYPE(m) == GST_MESSAGE_ERROR) {
		GError *e = NULL;
		gchar *dbg = NULL;
		gst_message_parse_error(m, &e, &dbg);
		*msg = g_strdup_printf("%s: %s (%s)", GST_OBJECT_NAME(GST_MESSAGE_SRC(m)), e ? e->message : "?", dbg ? dbg : "");
		if (e) g_error_free(e);
		g_free(dbg);
		r = 1;
	}
	gst_message_unref(m);
	return r;
}

static int blite_has_factory(const char *name) {
	GstElementFactory *f = gst_element_factory_find(name);
	if (f == NULL) return 0;
	gst_object_unref(f);
	return 1;
}

// Asks the encoder upstream of a sink for a keyframe as soon as possible.
static int blite_force_keyframe(GstElement *sink) {
	GstEvent *ev = gst_video_event_new_upstream_force_key_unit(GST_CLOCK_TIME_NONE, TRUE, 0);
	return gst_element_send_event(sink, ev);
}

static void blite_set_double(GstElement *e, const char *name, double v) { g_object_set(e, name, v, NULL); }

static void blite_set_uint(GstElement *e, const char *name, unsigned int v) { g_object_set(e, name, v, NULL); }

static void blite_set_string(GstElement *e, const char *name, const char *v) { g_object_set(e, name, v, NULL); }

static void blite_set_bool(GstElement *e, const char *name, int v) { g_object_set(e, name, (gboolean)v, NULL); }

// Makes a video sink draw into an existing native window.
static int blite_set_window(GstElement *e, guintptr h) {
	if (!GST_IS_VIDEO_OVERLAY(e)) return 0;
	gst_video_overlay_set_window_handle(GST_VIDEO_OVERLAY(e), h);
	return 1;
}

static GstPadProbeReturn blite_count_probe(GstPad *pad, GstPadProbeInfo *info, gpointer n) {
	g_atomic_int_inc((gint *)n);
	return GST_PAD_PROBE_OK;
}

// Counts buffers leaving e's src pad into *n (owned by the caller).
static int blite_count_buffers(GstElement *e, gint *n) {
	GstPad *pad = gst_element_get_static_pad(e, "src");
	if (pad == NULL) return 0;
	gst_pad_add_probe(pad, GST_PAD_PROBE_TYPE_BUFFER, blite_count_probe, n, NULL);
	gst_object_unref(pad);
	return 1;
}

static gint blite_counter_get(gint *n) { return g_atomic_int_get(n); }

// Thins the buffers entering e to keep of every base (evenly spread) and
// counts the ones it lets through.
typedef struct { gint keep, base, acc, passed; } blite_thin;

static GstPadProbeReturn blite_thin_probe(GstPad *pad, GstPadProbeInfo *info, gpointer d) {
	blite_thin *t = d;
	gint keep = g_atomic_int_get(&t->keep);
	if (keep < t->base) {
		t->acc += keep;
		if (t->acc < t->base) return GST_PAD_PROBE_DROP;
		t->acc -= t->base;
	}
	g_atomic_int_inc(&t->passed);
	return GST_PAD_PROBE_OK;
}

static int blite_thin_add(GstElement *e, blite_thin *t) {
	GstPad *pad = gst_element_get_static_pad(e, "sink");
	if (pad == NULL) return 0;
	gst_pad_add_probe(pad, GST_PAD_PROBE_TYPE_BUFFER, blite_thin_probe, t, NULL);
	gst_object_unref(pad);
	return 1;
}

static void blite_thin_set(blite_thin *t, gint keep) { g_atomic_int_set(&t->keep, keep); }
static gint blite_thin_passed(blite_thin *t) { return g_atomic_int_get(&t->passed); }

// Pointer events a video sink sends upstream (GstNavigation), collected
// for polling from Go: the last pointer position, a move counter, and
// the left clicks since the last poll.
typedef struct {
	GMutex m;
	double x, y;
	guint moves;
	int clicks;
	double cx[16], cy[16];
	int down, pressed; // left button held; pressed since the last poll at px, py
	double px, py;
} blite_nav;

static GstPadProbeReturn blite_nav_probe(GstPad *pad, GstPadProbeInfo *info, gpointer d) {
	GstEvent *ev = GST_PAD_PROBE_INFO_EVENT(info);
	if (ev == NULL || GST_EVENT_TYPE(ev) != GST_EVENT_NAVIGATION) return GST_PAD_PROBE_OK;
	blite_nav *n = d;
	double x, y;
	if (!gst_navigation_event_get_coordinates(ev, &x, &y)) return GST_PAD_PROBE_OK;
	GstNavigationEventType t = gst_navigation_event_get_type(ev);
	gint button = 0;
	g_mutex_lock(&n->m);
	if (t == GST_NAVIGATION_EVENT_MOUSE_MOVE) {
		n->x = x; n->y = y; n->moves++;
	} else if (t == GST_NAVIGATION_EVENT_MOUSE_BUTTON_PRESS &&
	           gst_navigation_event_parse_mouse_button_event(ev, &button, NULL, NULL) && button == 1) {
		n->x = x; n->y = y; n->moves++;
		n->down = 1; n->pressed = 1; n->px = x; n->py = y;
	} else if (t == GST_NAVIGATION_EVENT_MOUSE_BUTTON_RELEASE &&
	           gst_navigation_event_parse_mouse_button_event(ev, &button, NULL, NULL) && button == 1) {
		n->x = x; n->y = y; n->moves++;
		n->down = 0;
		if (n->clicks < 16) { n->cx[n->clicks] = x; n->cy[n->clicks] = y; n->clicks++; }
	}
	g_mutex_unlock(&n->m);
	return GST_PAD_PROBE_OK;
}

static void blite_nav_free(gpointer d) {
	blite_nav *n = d;
	g_mutex_clear(&n->m);
	g_free(n);
}

// Watches navigation events reaching e's src pad. The state is freed with
// the pad, so poll it only while the pipeline lives.
static blite_nav *blite_nav_watch(GstElement *e) {
	GstPad *pad = gst_element_get_static_pad(e, "src");
	if (pad == NULL) return NULL;
	blite_nav *n = g_new0(blite_nav, 1);
	g_mutex_init(&n->m);
	gst_pad_add_probe(pad, GST_PAD_PROBE_TYPE_EVENT_UPSTREAM, blite_nav_probe, n, blite_nav_free);
	gst_object_unref(pad);
	return n;
}

// Copies the state out and clears the clicks. Returns the click count.
static int blite_nav_poll(blite_nav *n, double *x, double *y, guint *moves, double *cx, double *cy,
                          int *down, int *pressed, double *px, double *py) {
	g_mutex_lock(&n->m);
	*x = n->x; *y = n->y; *moves = n->moves;
	*down = n->down; *pressed = n->pressed; *px = n->px; *py = n->py;
	n->pressed = 0;
	int c = n->clicks;
	for (int i = 0; i < c; i++) { cx[i] = n->cx[i]; cy[i] = n->cy[i]; }
	n->clicks = 0;
	g_mutex_unlock(&n->m);
	return c;
}

// Sets a property of one of e's pads (e.g. a compositor input's alpha).
static int blite_pad_set_double(GstElement *e, const char *pad, const char *name, double v) {
	GstPad *p = gst_element_get_static_pad(e, pad);
	if (p == NULL) return 0;
	g_object_set(p, name, v, NULL);
	gst_object_unref(p);
	return 1;
}

// Sends a pointer event upstream from a sink, as a window would (tests):
// kind 0 a move, 1 a left-button release (a click), 2 a press.
static int blite_send_mouse(GstElement *sink, int kind, double x, double y) {
	GstEvent *ev = kind == 1 ? gst_navigation_event_new_mouse_button_release(1, x, y, GST_NAVIGATION_MODIFIER_NONE)
	             : kind == 2 ? gst_navigation_event_new_mouse_button_press(1, x, y, GST_NAVIGATION_MODIFIER_NONE)
	                         : gst_navigation_event_new_mouse_move(x, y, GST_NAVIGATION_MODIFIER_NONE);
	GstPad *pad = gst_element_get_static_pad(sink, "sink");
	if (pad == NULL) { gst_event_unref(ev); return 0; }
	gboolean ok = gst_pad_push_event(pad, ev);
	gst_object_unref(pad);
	return ok;
}

static void blite_unref(GstElement *e) { gst_object_unref(e); }
static void blite_init(void) { gst_init(NULL, NULL); }
*/
import "C"

import (
	"errors"
	"sync"
	"time"
	"unsafe"
)

var initOnce sync.Once

// Init initialises GStreamer once.
func Init() { initOnce.Do(func() { C.blite_init(); startTrimmer(); setX11ErrorHandler() }) }

// HasFactory reports whether an element factory (plugin) is installed.
func HasFactory(name string) bool {
	Init()
	cs := C.CString(name)
	defer C.free(unsafe.Pointer(cs))
	return C.blite_has_factory(cs) == 1
}

// Element is a reference to a GstElement.
type Element struct{ p *C.GstElement }

// Pipeline is a launched pipeline. Teardown is two steps so callers can stop
// their pull loops in between: SetNull (unblocks Pull), then Free.
type Pipeline struct {
	Element
}

// Launch parses a gst-launch description.
func Launch(desc string) (*Pipeline, error) {
	Init()
	cs := C.CString(desc)
	defer C.free(unsafe.Pointer(cs))
	var cerr *C.char
	p := C.blite_launch(cs, &cerr)
	if p == nil {
		msg := "parse failed"
		if cerr != nil {
			msg = C.GoString(cerr)
			C.g_free(C.gpointer(cerr))
		}
		return nil, errors.New(msg)
	}
	return &Pipeline{Element: Element{p}}, nil
}

// ByName finds a named element in the pipeline. The caller owns the reference
// and must Release it before Free.
func (p *Pipeline) ByName(name string) (*Element, error) {
	cs := C.CString(name)
	defer C.free(unsafe.Pointer(cs))
	e := C.blite_by_name(p.p, cs)
	if e == nil {
		return nil, errors.New("no element named " + name)
	}
	return &Element{e}, nil
}

// Release drops an element reference from ByName.
func (e *Element) Release() {
	if e != nil && e.p != nil {
		C.blite_unref(e.p)
		e.p = nil
	}
}

const (
	stateNull    = 1
	statePlaying = 4
)

// Play starts the pipeline.
func (p *Pipeline) Play() error {
	if C.blite_set_state(p.p, statePlaying) == 0 {
		return errors.New("pipeline failed to start")
	}
	return nil
}

// SetNull stops the pipeline; blocked Pull calls return.
func (p *Pipeline) SetNull() { C.blite_set_state(p.p, stateNull) }

// Free drops the pipeline reference. Call after SetNull and after every
// element from ByName has been Released.
func (p *Pipeline) Free() { p.Release() }

// ErrTimeout is returned by Pull when no sample arrived in time.
var ErrTimeout = errors.New("timeout")

// ErrEOS is returned by Pull once the appsink is at end of stream or flushing.
var ErrEOS = errors.New("end of stream")

// Pull takes one sample from an appsink, copying its data.
func (e *Element) Pull(timeout time.Duration) ([]byte, time.Duration, error) {
	var (
		data unsafe.Pointer
		n    C.gsize
		dur  C.guint64
	)
	switch C.blite_pull(e.p, C.guint64(timeout), &data, &n, &dur) {
	case 1:
		b := C.GoBytes(data, C.int(n))
		C.g_free(C.gpointer(data))
		return b, time.Duration(dur), nil
	case 0:
		return nil, 0, ErrTimeout
	}
	return nil, 0, ErrEOS
}

// Push hands a buffer to an appsrc.
func (e *Element) Push(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	return C.blite_push(e.p, unsafe.Pointer(&b[0]), C.gsize(len(b))) == 1
}

// ForceKeyframe sends an upstream force-key-unit event from this (sink) element.
func (e *Element) ForceKeyframe() bool { return C.blite_force_keyframe(e.p) == 1 }

// SetDouble sets a gdouble property (e.g. volume's "volume").
func (e *Element) SetDouble(name string, v float64) {
	cs := C.CString(name)
	defer C.free(unsafe.Pointer(cs))
	C.blite_set_double(e.p, cs, C.double(v))
}

// SetUint sets a guint property (e.g. an encoder's "bitrate").
func (e *Element) SetUint(name string, v uint) {
	cs := C.CString(name)
	defer C.free(unsafe.Pointer(cs))
	C.blite_set_uint(e.p, cs, C.uint(v))
}

// SetBool sets a boolean property (e.g. valve's "drop").
func (e *Element) SetBool(name string, v bool) {
	cs := C.CString(name)
	defer C.free(unsafe.Pointer(cs))
	b := C.int(0)
	if v {
		b = 1
	}
	C.blite_set_bool(e.p, cs, b)
}

// SetWindow makes a video sink draw into the native window h (an X11
// window id or a Win32 HWND). It reports false if e isn't a video sink.
func (e *Element) SetWindow(h uintptr) bool { return C.blite_set_window(e.p, C.guintptr(h)) == 1 }

// SetString sets a string property (e.g. textoverlay's "text").
func (e *Element) SetString(name, v string) {
	cs, cv := C.CString(name), C.CString(v)
	defer C.free(unsafe.Pointer(cs))
	defer C.free(unsafe.Pointer(cv))
	C.blite_set_string(e.p, cs, cv)
}

// BusError waits up to timeout for an error or EOS. It returns nil if neither came.
func (p *Pipeline) BusError(timeout time.Duration) error {
	var msg *C.char
	switch C.blite_bus_pop(p.p, C.guint64(timeout), &msg) {
	case 1:
		s := C.GoString(msg)
		C.g_free(C.gpointer(msg))
		return errors.New(s)
	case 2:
		return ErrEOS
	}
	return nil
}

// Counter counts the buffers leaving an element's src pad.
type Counter struct{ n *C.gint }

// CountBuffers starts counting e's output buffers. Free the counter only
// once the pipeline is in NULL (no more buffers).
func (e *Element) CountBuffers() (*Counter, bool) {
	n := (*C.gint)(C.calloc(1, C.size_t(unsafe.Sizeof(C.gint(0)))))
	if C.blite_count_buffers(e.p, n) != 1 {
		C.free(unsafe.Pointer(n))
		return nil, false
	}
	return &Counter{n: n}, true
}

// Load returns the count so far.
func (c *Counter) Load() int { return int(C.blite_counter_get(c.n)) }

// Free releases the counter.
func (c *Counter) Free() { C.free(unsafe.Pointer(c.n)) }

// Thinner lets through only some of the buffers entering an element:
// fps of every base, spread evenly. The frames keep their timestamps, so
// the caps (and the encoder's idea of the frame rate) stay as they are.
type Thinner struct {
	t    *C.blite_thin
	base int
}

// Thin starts thinning e's input at base frames per second (nothing is
// dropped until SetFPS). Free it only once the pipeline is in NULL.
func (e *Element) Thin(base int) (*Thinner, bool) {
	t := (*C.blite_thin)(C.calloc(1, C.size_t(unsafe.Sizeof(C.blite_thin{}))))
	t.keep, t.base = C.gint(base), C.gint(base)
	if C.blite_thin_add(e.p, t) != 1 {
		C.free(unsafe.Pointer(t))
		return nil, false
	}
	return &Thinner{t: t, base: base}, true
}

// SetFPS keeps fps frames per second (base or more: all of them).
func (t *Thinner) SetFPS(fps int) { C.blite_thin_set(t.t, C.gint(min(max(fps, 1), t.base))) }

// Passed is how many buffers it let through.
func (t *Thinner) Passed() int { return int(C.blite_thin_passed(t.t)) }

// Free releases it.
func (t *Thinner) Free() { C.free(unsafe.Pointer(t.t)) }

// Nav is the pointer state of a video window (see WatchNav).
type Nav struct{ n *C.blite_nav }

// Pointer is a position in the video's pixels.
type Pointer struct{ X, Y float64 }

// WatchNav collects the pointer events a video sink sends upstream through
// e (an element whose src pad leads to the sink). It is valid while the
// pipeline lives; nil if e has no src pad.
func (e *Element) WatchNav() *Nav {
	n := C.blite_nav_watch(e.p)
	if n == nil {
		return nil
	}
	return &Nav{n}
}

// NavState is what Poll saw.
type NavState struct {
	At     Pointer   // the last pointer position
	Moves  uint      // grows with every move
	Clicks []Pointer // left-button releases since the previous Poll
	Down   bool      // the left button is held
	Press  *Pointer  // where it went down, if since the previous Poll
}

// Poll returns the pointer state and the clicks since the previous Poll.
func (n *Nav) Poll() NavState {
	var x, y, px, py C.double
	var mv C.guint
	var down, pressed C.int
	var cx, cy [16]C.double
	c := int(C.blite_nav_poll(n.n, &x, &y, &mv, &cx[0], &cy[0], &down, &pressed, &px, &py))
	st := NavState{At: Pointer{float64(x), float64(y)}, Moves: uint(mv), Down: down != 0}
	for i := 0; i < c; i++ {
		st.Clicks = append(st.Clicks, Pointer{float64(cx[i]), float64(cy[i])})
	}
	if pressed != 0 {
		st.Press = &Pointer{float64(px), float64(py)}
	}
	return st
}

// SetPadDouble sets a double property of e's pad (e.g. "sink_2" "alpha").
func (e *Element) SetPadDouble(pad, name string, v float64) bool {
	cp, cn := C.CString(pad), C.CString(name)
	defer C.free(unsafe.Pointer(cp))
	defer C.free(unsafe.Pointer(cn))
	return C.blite_pad_set_double(e.p, cp, cn, C.double(v)) == 1
}

// SendMouse makes sink send a pointer move (or a left-button release, a
// click) upstream, as its window would (tests).
func (e *Element) SendMouse(click bool, x, y float64) bool {
	r := C.int(0)
	if click {
		r = 1
	}
	return C.blite_send_mouse(e.p, r, C.double(x), C.double(y)) == 1
}

// SendPress makes sink send a left-button press upstream (tests).
func (e *Element) SendPress(x, y float64) bool {
	return C.blite_send_mouse(e.p, 2, C.double(x), C.double(y)) == 1
}
