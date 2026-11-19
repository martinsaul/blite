// Package camera lists the cameras (webcams, capture cards, virtual
// cameras) that can be shared, gives each a stable hashed ID, and picks
// the mode to open for a share setting. Device paths and serials stay in
// Camera.Path: never shown, never logged; only IDs and names are.
package camera

import (
	"context"
	"errors"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"blite/internal/gst"
)

// Camera is one camera we can share.
type Camera struct {
	ID      string // 12 hex chars (see hashID), or "test-..." for the fake cameras
	Name    string // friendly; a repeated name gets " (2)", " (3)"
	Backend string // BackendMF, BackendDShow, BackendV4L2, BackendPipeWire or BackendTest
	Path    string // what the source element opens; never shown, never logged
	Modes   []Mode
	Audio   string // the paired audio input, "" if none (not filled yet)

	base string // Name without the " (2)" suffix
}

// Source is the camera's source string, "camera:<id>".
func (c Camera) Source() string { return Source(c.ID) }

// Backends.
const (
	BackendMF       = "mf"       // Media Foundation (mfvideosrc device-path)
	BackendDShow    = "dshow"    // DirectShow (dshowvideosrc device), for virtual cameras such as OBS's
	BackendV4L2     = "v4l2"     // v4l2src device
	BackendPipeWire = "pipewire" // pipewiresrc target-object, for libcamera-only cameras
	BackendTest     = "test"     // videotestsrc (BLITE_FAKE_CAMERAS=1)
)

// BackendName is a backend for people: "Media Foundation".
func BackendName(b string) string {
	switch b {
	case BackendMF:
		return "Media Foundation"
	case BackendDShow:
		return "DirectShow"
	case BackendV4L2:
		return "Video4Linux"
	case BackendPipeWire:
		return "PipeWire"
	case BackendTest:
		return "test pattern"
	}
	return b
}

// ErrNotFound means the camera isn't connected (any more).
var ErrNotFound = errors.New("camera isn't connected")

const videoClass = "Video/Source"

// cacheFor is how long a listing is reused: listing probes every device.
const cacheFor = 5 * time.Second

var (
	mu       sync.Mutex
	cached   []Camera
	cachedAt time.Time
	cacheOK  bool                  // cached holds a listing (possibly empty)
	names    = map[string]string{} // ID -> base name of every camera seen, for Lookup
)

// List lists the cameras, real ones first by backend, then the fake ones
// when BLITE_FAKE_CAMERAS=1. A listing is reused for 5 s. It probes the
// devices, so call it off the UI thread.
func List() ([]Camera, error) {
	mu.Lock()
	defer mu.Unlock()
	if cacheOK && time.Since(cachedAt) < cacheFor {
		return clone(cached), nil
	}
	devs, err := gst.ListDevices(videoClass)
	if err != nil {
		return nil, err
	}
	cams := fromDevices(devs, runtime.GOOS, v4l2Resolver(byIDDir))
	if os.Getenv("BLITE_FAKE_CAMERAS") == "1" {
		cams = append(cams, Fakes()...)
	}
	for _, c := range cams {
		names[c.ID] = c.base
	}
	cached, cachedAt, cacheOK = cams, time.Now(), true
	return clone(cams), nil
}

func clone(cs []Camera) []Camera {
	out := make([]Camera, len(cs))
	copy(out, cs)
	return out
}

// Invalidate drops the cached listing.
func Invalidate() {
	mu.Lock()
	cached, cacheOK = nil, false
	mu.Unlock()
}

// Lookup finds a camera by ID. If the ID is gone (the camera moved to
// another USB port) and exactly one camera has the name it had, that one
// is used. ErrNotFound if neither.
func Lookup(id string) (Camera, error) {
	cams, err := List()
	if err != nil {
		return Camera{}, err
	}
	mu.Lock()
	name := names[id]
	mu.Unlock()
	if c, ok := Find(cams, id, name); ok {
		return c, nil
	}
	return Camera{}, ErrNotFound
}

// Find returns the camera with id, or else the only camera named name
// (its name without a " (2)" suffix).
func Find(cams []Camera, id, name string) (Camera, bool) {
	for _, c := range cams {
		if c.ID == id {
			return c, true
		}
	}
	if name == "" || strings.HasPrefix(id, "test-") {
		return Camera{}, false
	}
	var hit []Camera
	for _, c := range cams {
		if c.base == name && c.Backend != BackendTest {
			hit = append(hit, c)
		}
	}
	if len(hit) == 1 {
		return hit[0], true
	}
	return Camera{}, false
}

// Watch calls changed (from its own goroutine) whenever cameras come or
// go, until ctx ends. Bursts of device messages give one call. It
// returns an error if no device monitor could be started.
func Watch(ctx context.Context, changed func()) error {
	w, err := gst.NewDeviceWatch(videoClass)
	if err != nil {
		return err
	}
	defer w.Close()
	for ctx.Err() == nil {
		if _, ok := w.Next(250 * time.Millisecond); !ok {
			continue
		}
		for ctx.Err() == nil { // coalesce a burst (one device, several nodes)
			if _, ok := w.Next(300 * time.Millisecond); !ok {
				break
			}
		}
		if ctx.Err() != nil {
			break
		}
		Invalidate()
		changed()
	}
	return nil
}

// backendOf maps a GstDevice type to our backend name ("" = not used).
func backendOf(gtype string) string {
	t := strings.ToLower(gtype)
	switch {
	case strings.Contains(t, "mfdevice"):
		return BackendMF
	case strings.Contains(t, "dshow"):
		return BackendDShow
	case strings.Contains(t, "v4l2"):
		return BackendV4L2
	case strings.Contains(t, "pipewire"):
		return BackendPipeWire
	}
	return ""
}

// order is which backends we use on goos, first-choice first.
func order(goos string) []string {
	switch goos {
	case "windows":
		return []string{BackendMF, BackendDShow}
	case "linux":
		return []string{BackendV4L2, BackendPipeWire}
	}
	return []string{BackendMF, BackendDShow, BackendV4L2, BackendPipeWire}
}

func firstProp(p map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(p[k]); v != "" {
			return v
		}
	}
	return ""
}

// fromDevices turns monitor devices into cameras: per goos, the first-
// choice backend's devices, then the other backend's that aren't the same
// camera; stable IDs; numbered duplicate names. resolve maps a v4l2 device
// node to its stable name (/dev/v4l/by-id).
func fromDevices(devs []gst.Device, goos string, resolve func(string) string) []Camera {
	byBackend := map[string][]Camera{}
	pwV4L2 := map[string]string{} // pipewire camera ID -> resolved api.v4l2.path
	for _, d := range devs {
		b := backendOf(d.Type)
		if b == "" {
			continue
		}
		c := Camera{Name: strings.TrimSpace(d.Name), Backend: b, Modes: modesOf(d.Modes)}
		if c.Name == "" {
			c.Name = "Camera"
		}
		var key string
		switch b {
		case BackendMF:
			c.Path = firstProp(d.Props, "device.path", "gobject.device-path")
			key = strings.ToLower(c.Path)
		case BackendDShow:
			c.Path = firstProp(d.Props, "gobject.device", "device.path", "gobject.device-path")
			key = strings.ToLower(c.Path)
		case BackendV4L2:
			c.Path = resolve(firstProp(d.Props, "device.path", "gobject.device-path"))
			key = c.Path
		case BackendPipeWire:
			c.Path = firstProp(d.Props, "node.name", "object.serial")
			key = c.Path
		}
		if key == "" {
			key = "name:" + c.Name
		}
		c.ID = hashID(b, key)
		if b == BackendPipeWire {
			if p := firstProp(d.Props, "api.v4l2.path"); p != "" {
				pwV4L2[c.ID] = resolve(p)
			}
		}
		byBackend[b] = append(byBackend[b], c)
	}
	var out []Camera
	seenID := map[string]bool{}
	for i, b := range order(goos) {
		cs := byBackend[b]
		sort.SliceStable(cs, func(i, j int) bool {
			if cs[i].Name != cs[j].Name {
				return cs[i].Name < cs[j].Name
			}
			return cs[i].ID < cs[j].ID
		})
		for _, c := range cs {
			if i > 0 && duplicate(c, out, pwV4L2[c.ID]) {
				continue
			}
			for n := 2; seenID[c.ID]; n++ { // same key twice (no path): keep both apart
				c.ID = hashID(c.Backend, c.Path+"\x00"+c.Name+"\x00"+strconv.Itoa(n))
			}
			seenID[c.ID] = true
			out = append(out, c)
		}
	}
	return numberNames(out)
}

// duplicate reports whether c, from a fallback backend, is a camera the
// first-choice backend already listed.
func duplicate(c Camera, have []Camera, v4l2Path string) bool {
	for _, h := range have {
		switch {
		case c.Backend == BackendDShow && h.Backend == BackendMF:
			k := winInstanceKey(c.Path)
			if k != "" && k == winInstanceKey(h.Path) {
				return true
			}
			// A PnP camera whose paths don't line up: same name means the
			// same device. Software (virtual) cameras are never in MF.
			if k != "" && strings.EqualFold(c.Name, h.Name) {
				return true
			}
		case c.Backend == BackendPipeWire && h.Backend == BackendV4L2:
			if v4l2Path != "" && v4l2Path == h.Path {
				return true
			}
			// PipeWire's libcamera node of a UVC camera has no v4l2 path.
			if strings.EqualFold(c.Name, h.Name) {
				return true
			}
		}
	}
	return false
}

// numberNames gives repeated names " (2)", " (3)" in list order.
func numberNames(cs []Camera) []Camera {
	count := map[string]int{}
	for i := range cs {
		cs[i].base = cs[i].Name
		count[cs[i].Name]++
		if n := count[cs[i].Name]; n > 1 {
			cs[i].Name += " (" + strconv.Itoa(n) + ")"
		}
	}
	return cs
}

func modesOf(cms []gst.CapsMode) []Mode {
	var out []Mode
	for _, c := range cms {
		m := Mode{Media: c.Media, Format: c.Format, W: c.WMin, H: c.HMin, FPSNum: c.FPSNum, FPSDen: c.FPSDen}
		if c.WMax > c.WMin {
			m.MaxW = c.WMax
		}
		if c.HMax > c.HMin {
			m.MaxH = c.HMax
		}
		if c.FPSMaxNum > 0 {
			m.MaxFPSNum, m.MaxFPSDen = c.FPSMaxNum, c.FPSMaxDen
		}
		if usable(m) {
			out = append(out, m)
		}
	}
	return out
}

// Fakes are the test cameras BLITE_FAKE_CAMERAS=1 adds: an MJPEG 720p30
// webcam, a raw 1080p60 capture card, and one whose stream ends at once.
func Fakes() []Camera {
	return []Camera{
		{ID: "test-mjpeg720", Name: "Test camera (MJPEG 720p)", Backend: BackendTest, Path: "test-mjpeg720", base: "Test camera (MJPEG 720p)",
			Modes: []Mode{
				{Media: MediaJPEG, W: 1280, H: 720, FPSNum: 30, FPSDen: 1},
				{Media: MediaJPEG, W: 640, H: 360, FPSNum: 30, FPSDen: 1},
			}},
		{ID: "test-yuy2-1080p60", Name: "Test capture card (1080p60)", Backend: BackendTest, Path: "test-yuy2-1080p60", base: "Test capture card (1080p60)",
			Modes: []Mode{
				{Media: MediaRaw, Format: "YUY2", W: 1920, H: 1080, FPSNum: 60, FPSDen: 1},
				{Media: MediaRaw, Format: "YUY2", W: 1280, H: 720, FPSNum: 60, FPSDen: 1},
			}},
		{ID: "test-fail", Name: "Test camera (fails)", Backend: BackendTest, Path: "test-fail", base: "Test camera (fails)",
			Modes: []Mode{{Media: MediaRaw, Format: "YUY2", W: 1280, H: 720, FPSNum: 30, FPSDen: 1}}},
	}
}
