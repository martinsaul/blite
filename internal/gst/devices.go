package gst

/*
#include <stdlib.h>
#include <string.h>
#include <gst/gst.h>

// One flattened caps structure: a media type, a format and a size and frame
// rate that are each fixed or a range. Lists in the caps are expanded into
// several modes; ranges are kept.
typedef struct {
	char media[32];
	char format[32];
	int wmin, wmax, hmin, hmax;     // equal when fixed; 0 when absent
	int fn, fd;                     // the frame rate, or the low end of a range; 0/0 when absent
	int fmaxn, fmaxd;               // the high end of a frame rate range; 0/0 when fixed
} blite_mode;

#define BLITE_MAX_MODES 4096
#define BLITE_MAX_ITEMS 64

typedef struct { int min, max; } blite_irange;
typedef struct { int n, d, maxn, maxd; } blite_frange;

static int blite_collect_ints(const GValue *v, blite_irange *out, int n, int cap) {
	if (v == NULL) {
		if (n < cap) { out[n].min = 0; out[n].max = 0; n++; }
		return n;
	}
	if (G_VALUE_HOLDS_INT(v)) {
		if (n < cap) { out[n].min = out[n].max = g_value_get_int(v); n++; }
	} else if (GST_VALUE_HOLDS_INT_RANGE(v)) {
		if (n < cap) { out[n].min = gst_value_get_int_range_min(v); out[n].max = gst_value_get_int_range_max(v); n++; }
	} else if (GST_VALUE_HOLDS_LIST(v)) {
		for (guint i = 0; i < gst_value_list_get_size(v); i++)
			n = blite_collect_ints(gst_value_list_get_value(v, i), out, n, cap);
	} else if (GST_VALUE_HOLDS_ARRAY(v)) {
		for (guint i = 0; i < gst_value_array_get_size(v); i++)
			n = blite_collect_ints(gst_value_array_get_value(v, i), out, n, cap);
	}
	return n;
}

static int blite_collect_fracs(const GValue *v, blite_frange *out, int n, int cap) {
	if (v == NULL) {
		if (n < cap) { memset(&out[n], 0, sizeof out[n]); n++; }
		return n;
	}
	if (GST_VALUE_HOLDS_FRACTION(v)) {
		if (n < cap) {
			out[n].n = gst_value_get_fraction_numerator(v);
			out[n].d = gst_value_get_fraction_denominator(v);
			out[n].maxn = out[n].maxd = 0;
			n++;
		}
	} else if (GST_VALUE_HOLDS_FRACTION_RANGE(v)) {
		if (n < cap) {
			const GValue *lo = gst_value_get_fraction_range_min(v), *hi = gst_value_get_fraction_range_max(v);
			out[n].n = gst_value_get_fraction_numerator(lo);
			out[n].d = gst_value_get_fraction_denominator(lo);
			out[n].maxn = gst_value_get_fraction_numerator(hi);
			out[n].maxd = gst_value_get_fraction_denominator(hi);
			n++;
		}
	} else if (GST_VALUE_HOLDS_LIST(v)) {
		for (guint i = 0; i < gst_value_list_get_size(v); i++)
			n = blite_collect_fracs(gst_value_list_get_value(v, i), out, n, cap);
	} else if (GST_VALUE_HOLDS_ARRAY(v)) {
		for (guint i = 0; i < gst_value_array_get_size(v); i++)
			n = blite_collect_fracs(gst_value_array_get_value(v, i), out, n, cap);
	}
	return n;
}

static int blite_collect_strs(const GValue *v, const char **out, int n, int cap) {
	if (v == NULL) {
		if (n < cap) out[n++] = "";
		return n;
	}
	if (G_VALUE_HOLDS_STRING(v)) {
		if (n < cap) out[n++] = g_value_get_string(v);
	} else if (GST_VALUE_HOLDS_LIST(v)) {
		for (guint i = 0; i < gst_value_list_get_size(v); i++)
			n = blite_collect_strs(gst_value_list_get_value(v, i), out, n, cap);
	} else if (GST_VALUE_HOLDS_ARRAY(v)) {
		for (guint i = 0; i < gst_value_array_get_size(v); i++)
			n = blite_collect_strs(gst_value_array_get_value(v, i), out, n, cap);
	}
	return n;
}

// Flattens caps into *out (g_free it). Structures with non-system-memory
// features (D3D11, DMABuf) are skipped: the pipeline reads system memory.
static int blite_caps_modes(const GstCaps *caps, blite_mode **out) {
	GArray *a = g_array_new(FALSE, TRUE, sizeof(blite_mode));
	const char *fmts[BLITE_MAX_ITEMS];
	blite_irange ws[BLITE_MAX_ITEMS], hs[BLITE_MAX_ITEMS];
	blite_frange fs[BLITE_MAX_ITEMS];
	guint size = caps ? gst_caps_get_size(caps) : 0;
	if (caps && (gst_caps_is_any(caps) || gst_caps_is_empty(caps))) size = 0;
	for (guint i = 0; i < size && a->len < BLITE_MAX_MODES; i++) {
		GstCapsFeatures *f = gst_caps_get_features(caps, i);
		if (f != NULL && !gst_caps_features_is_any(f) &&
		    !gst_caps_features_is_equal(f, GST_CAPS_FEATURES_MEMORY_SYSTEM_MEMORY)) continue;
		const GstStructure *s = gst_caps_get_structure(caps, i);
		int nf = blite_collect_strs(gst_structure_get_value(s, "format"), fmts, 0, BLITE_MAX_ITEMS);
		int nw = blite_collect_ints(gst_structure_get_value(s, "width"), ws, 0, BLITE_MAX_ITEMS);
		int nh = blite_collect_ints(gst_structure_get_value(s, "height"), hs, 0, BLITE_MAX_ITEMS);
		int nr = blite_collect_fracs(gst_structure_get_value(s, "framerate"), fs, 0, BLITE_MAX_ITEMS);
		for (int fi = 0; fi < nf; fi++)
		for (int wi = 0; wi < nw; wi++)
		for (int hi = 0; hi < nh; hi++)
		for (int ri = 0; ri < nr && a->len < BLITE_MAX_MODES; ri++) {
			blite_mode m;
			memset(&m, 0, sizeof m);
			g_strlcpy(m.media, gst_structure_get_name(s), sizeof m.media);
			g_strlcpy(m.format, fmts[fi] ? fmts[fi] : "", sizeof m.format);
			m.wmin = ws[wi].min; m.wmax = ws[wi].max;
			m.hmin = hs[hi].min; m.hmax = hs[hi].max;
			m.fn = fs[ri].n; m.fd = fs[ri].d; m.fmaxn = fs[ri].maxn; m.fmaxd = fs[ri].maxd;
			g_array_append_val(a, m);
		}
	}
	int n = a->len;
	*out = (blite_mode *)g_array_free(a, FALSE);
	return n;
}

static int blite_parse_caps_modes(const char *str, blite_mode **out) {
	GstCaps *c = gst_caps_from_string(str);
	if (c == NULL) { *out = NULL; return -1; }
	int n = blite_caps_modes(c, out);
	gst_caps_unref(c);
	return n;
}

// A device, flattened: names, its properties as strings and its modes.
typedef struct {
	char *name, *klass, *gtype;
	char **keys, **vals;
	int nprops;
	blite_mode *modes;
	int nmodes;
} blite_device;

static void blite_add_prop(GPtrArray *k, GPtrArray *v, const char *key, char *val) {
	g_ptr_array_add(k, g_strdup(key));
	g_ptr_array_add(v, val);
}

static void blite_fill_device(GstDevice *d, blite_device *o) {
	memset(o, 0, sizeof *o);
	o->name = gst_device_get_display_name(d);
	o->klass = gst_device_get_device_class(d);
	o->gtype = g_strdup(G_OBJECT_TYPE_NAME(d));
	GPtrArray *k = g_ptr_array_new(), *v = g_ptr_array_new();
	GstStructure *p = gst_device_get_properties(d);
	if (p != NULL) {
		for (int i = 0; i < gst_structure_n_fields(p); i++) {
			const char *key = gst_structure_nth_field_name(p, i);
			const GValue *val = gst_structure_get_value(p, key);
			char *s = G_VALUE_HOLDS_STRING(val) ? g_value_dup_string(val) : gst_value_serialize(val);
			blite_add_prop(k, v, key, s ? s : g_strdup(""));
		}
		gst_structure_free(p);
	}
	// Some providers (DirectShow) keep the device path in a GObject
	// property instead; those come out as "gobject.<name>".
	guint np = 0;
	GParamSpec **ps = g_object_class_list_properties(G_OBJECT_GET_CLASS(d), &np);
	for (guint i = 0; i < np; i++) {
		GParamSpec *s = ps[i];
		if (!(s->flags & G_PARAM_READABLE)) continue;
		if (s->owner_type == GST_TYPE_DEVICE || s->owner_type == GST_TYPE_OBJECT) continue;
		GType t = G_PARAM_SPEC_VALUE_TYPE(s);
		if (t != G_TYPE_STRING && t != G_TYPE_INT && t != G_TYPE_UINT) continue;
		GValue gv = G_VALUE_INIT;
		g_value_init(&gv, t);
		g_object_get_property(G_OBJECT(d), s->name, &gv);
		char *str = t == G_TYPE_STRING ? g_value_dup_string(&gv)
		          : t == G_TYPE_INT ? g_strdup_printf("%d", g_value_get_int(&gv))
		                            : g_strdup_printf("%u", g_value_get_uint(&gv));
		g_value_unset(&gv);
		char *key = g_strconcat("gobject.", s->name, NULL);
		blite_add_prop(k, v, key, str ? str : g_strdup(""));
		g_free(key);
	}
	g_free(ps);
	o->nprops = k->len;
	o->keys = (char **)g_ptr_array_free(k, FALSE);
	o->vals = (char **)g_ptr_array_free(v, FALSE);
	GstCaps *c = gst_device_get_caps(d);
	o->nmodes = blite_caps_modes(c, &o->modes);
	if (c) gst_caps_unref(c);
}

static void blite_clear_device(blite_device *o) {
	g_free(o->name); g_free(o->klass); g_free(o->gtype);
	for (int i = 0; i < o->nprops; i++) { g_free(o->keys[i]); g_free(o->vals[i]); }
	g_free(o->keys); g_free(o->vals); g_free(o->modes);
	memset(o, 0, sizeof *o);
}

static void blite_free_devices(blite_device *d, int n) {
	for (int i = 0; i < n; i++) blite_clear_device(&d[i]);
	g_free(d);
}

static GstDeviceMonitor *blite_monitor_new(const char *classes) {
	GstDeviceMonitor *m = gst_device_monitor_new();
	// PipeWire's provider hides v4l2's; we want both and drop the duplicates.
	gst_device_monitor_set_show_all_devices(m, TRUE);
	if (gst_device_monitor_add_filter(m, classes, NULL) == 0) {
		gst_object_unref(m);
		return NULL;
	}
	return m;
}

// Lists the devices of the given classes ("Video/Source") into *out.
// Returns the count; 0 when no provider handles the classes.
static int blite_list_devices(const char *classes, blite_device **out) {
	*out = NULL;
	GstDeviceMonitor *m = blite_monitor_new(classes);
	if (m == NULL) return 0;
	// Started, providers report what they track; if none starts,
	// get_devices still probes them.
	gboolean started = gst_device_monitor_start(m);
	GList *l = gst_device_monitor_get_devices(m);
	if (started) gst_device_monitor_stop(m);
	int n = g_list_length(l);
	*out = g_new0(blite_device, n > 0 ? n : 1);
	int i = 0;
	for (GList *it = l; it != NULL; it = it->next) blite_fill_device(GST_DEVICE(it->data), &(*out)[i++]);
	g_list_free_full(l, gst_object_unref);
	gst_object_unref(m);
	return n;
}

typedef struct { GstDeviceMonitor *mon; GstBus *bus; } blite_watch;

static blite_watch *blite_watch_new(const char *classes) {
	GstDeviceMonitor *m = blite_monitor_new(classes);
	if (m == NULL) return NULL;
	if (!gst_device_monitor_start(m)) { gst_object_unref(m); return NULL; }
	blite_watch *w = g_new0(blite_watch, 1);
	w->mon = m;
	w->bus = gst_device_monitor_get_bus(m);
	return w;
}

// Waits for the next hotplug message: 0 timeout, 1 added, 2 removed,
// 3 changed. On 1..3, *o is filled (blite_clear_device it).
static int blite_watch_next(blite_watch *w, guint64 timeout_ns, blite_device *o) {
	GstMessage *msg = gst_bus_timed_pop_filtered(w->bus, timeout_ns,
		GST_MESSAGE_DEVICE_ADDED | GST_MESSAGE_DEVICE_REMOVED | GST_MESSAGE_DEVICE_CHANGED);
	if (msg == NULL) return 0;
	GstDevice *d = NULL;
	int r = 0;
	switch (GST_MESSAGE_TYPE(msg)) {
	case GST_MESSAGE_DEVICE_ADDED: gst_message_parse_device_added(msg, &d); r = 1; break;
	case GST_MESSAGE_DEVICE_REMOVED: gst_message_parse_device_removed(msg, &d); r = 2; break;
	case GST_MESSAGE_DEVICE_CHANGED: gst_message_parse_device_changed(msg, &d, NULL); r = 3; break;
	default: break;
	}
	if (d != NULL) {
		blite_fill_device(d, o);
		gst_object_unref(d);
	} else {
		memset(o, 0, sizeof *o);
	}
	gst_message_unref(msg);
	return r;
}

static void blite_watch_free(blite_watch *w) {
	gst_device_monitor_stop(w->mon);
	gst_object_unref(w->bus);
	gst_object_unref(w->mon);
	g_free(w);
}

static blite_mode *blite_mode_at(blite_mode *m, int i) { return &m[i]; }
static blite_device *blite_device_at(blite_device *d, int i) { return &d[i]; }
*/
import "C"

import (
	"errors"
	"time"
	"unsafe"
)

// CapsMode is one flattened caps structure. A size or frame rate is either
// fixed (Min == Max, FPSMaxNum == 0) or a range. Absent fields are 0.
type CapsMode struct {
	Media  string // "video/x-raw", "image/jpeg", ...
	Format string // "NV12", "YUY2", ... ("" for image/jpeg)

	WMin, WMax, HMin, HMax int

	FPSNum, FPSDen       int // the frame rate, or the low end of a range
	FPSMaxNum, FPSMaxDen int // the high end of a range; 0 when fixed
}

// Device is one device from a GstDeviceMonitor.
type Device struct {
	Name  string // display name
	Class string // "Video/Source", ...
	Type  string // GType name of the device: "GstMFDevice", "GstV4l2Device", ...
	// Props holds gst_device_get_properties as strings ("device.path",
	// "api.v4l2.path", "object.serial", ...) and the device object's own
	// string and integer properties as "gobject.<name>".
	Props map[string]string
	Modes []CapsMode
}

func goModes(m *C.blite_mode, n C.int) []CapsMode {
	out := make([]CapsMode, 0, int(n))
	for i := 0; i < int(n); i++ {
		c := C.blite_mode_at(m, C.int(i))
		out = append(out, CapsMode{
			Media:  C.GoString(&c.media[0]),
			Format: C.GoString(&c.format[0]),
			WMin:   int(c.wmin), WMax: int(c.wmax), HMin: int(c.hmin), HMax: int(c.hmax),
			FPSNum: int(c.fn), FPSDen: int(c.fd), FPSMaxNum: int(c.fmaxn), FPSMaxDen: int(c.fmaxd),
		})
	}
	return out
}

func goDevice(d *C.blite_device) Device {
	dev := Device{
		Name:  C.GoString(d.name),
		Class: C.GoString(d.klass),
		Type:  C.GoString(d.gtype),
		Props: make(map[string]string, int(d.nprops)),
		Modes: goModes(d.modes, d.nmodes),
	}
	if d.nprops > 0 {
		keys := unsafe.Slice(d.keys, int(d.nprops))
		vals := unsafe.Slice(d.vals, int(d.nprops))
		for i := range keys {
			dev.Props[C.GoString(keys[i])] = C.GoString(vals[i])
		}
	}
	return dev
}

// ParseCapsModes flattens a caps string the way device caps are flattened
// (lists expanded, ranges kept). It exists so the flattening can be tested
// without a device.
func ParseCapsModes(caps string) ([]CapsMode, error) {
	Init()
	cs := C.CString(caps)
	defer C.free(unsafe.Pointer(cs))
	var m *C.blite_mode
	n := C.blite_parse_caps_modes(cs, &m)
	if n < 0 {
		return nil, errors.New("invalid caps")
	}
	defer C.g_free(C.gpointer(m))
	return goModes(m, n), nil
}

// ListDevices lists the devices of class (e.g. "Video/Source"), with every
// provider shown (PipeWire otherwise hides v4l2). No provider for the
// class gives an empty list.
func ListDevices(class string) ([]Device, error) {
	Init()
	cs := C.CString(class)
	defer C.free(unsafe.Pointer(cs))
	var d *C.blite_device
	n := C.blite_list_devices(cs, &d)
	if d == nil {
		return nil, nil
	}
	defer C.blite_free_devices(d, n)
	out := make([]Device, 0, int(n))
	for i := 0; i < int(n); i++ {
		out = append(out, goDevice(C.blite_device_at(d, C.int(i))))
	}
	return out, nil
}

// DeviceEventKind says what happened to a device.
type DeviceEventKind int

// Device events.
const (
	DeviceAdded DeviceEventKind = iota + 1
	DeviceRemoved
	DeviceChanged
)

func (k DeviceEventKind) String() string {
	switch k {
	case DeviceAdded:
		return "added"
	case DeviceRemoved:
		return "removed"
	case DeviceChanged:
		return "changed"
	}
	return "?"
}

// DeviceEvent is one hotplug message.
type DeviceEvent struct {
	Kind   DeviceEventKind
	Device Device
}

// DeviceWatch reports devices coming and going. It pops the monitor's bus
// directly, so no GLib main loop is needed. Next and Close must be called
// from one goroutine.
type DeviceWatch struct{ w *C.blite_watch }

// NewDeviceWatch starts watching devices of class.
func NewDeviceWatch(class string) (*DeviceWatch, error) {
	Init()
	cs := C.CString(class)
	defer C.free(unsafe.Pointer(cs))
	w := C.blite_watch_new(cs)
	if w == nil {
		return nil, errors.New("no device provider could be started for " + class)
	}
	return &DeviceWatch{w: w}, nil
}

// Next waits up to timeout for a device event; false if none came.
func (w *DeviceWatch) Next(timeout time.Duration) (DeviceEvent, bool) {
	if w == nil || w.w == nil {
		return DeviceEvent{}, false
	}
	var d C.blite_device
	r := C.blite_watch_next(w.w, C.guint64(max(timeout, 0)), &d)
	if r == 0 {
		return DeviceEvent{}, false
	}
	ev := DeviceEvent{Kind: DeviceEventKind(r), Device: goDevice(&d)}
	C.blite_clear_device(&d)
	return ev, true
}

// Close stops the monitor. Safe to call more than once.
func (w *DeviceWatch) Close() {
	if w != nil && w.w != nil {
		C.blite_watch_free(w.w)
		w.w = nil
	}
}
