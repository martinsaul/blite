package camera

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"blite/internal/gst"
)

func TestHashID(t *testing.T) {
	a := hashID(BackendMF, `\\?\usb#vid_046d&pid_082d&mi_00#7&1a2b&0&0000#{e5323777-f976-4f5b-9b55-b94699c46e44}\global`)
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(a) {
		t.Fatalf("id %q", a)
	}
	if a != hashID(BackendMF, `\\?\usb#vid_046d&pid_082d&mi_00#7&1a2b&0&0000#{e5323777-f976-4f5b-9b55-b94699c46e44}\global`) {
		t.Error("not deterministic")
	}
	if a == hashID(BackendDShow, `\\?\usb#vid_046d&pid_082d&mi_00#7&1a2b&0&0000#{e5323777-f976-4f5b-9b55-b94699c46e44}\global`) {
		t.Error("backend not in the hash")
	}
	if id, ok := ParseSource(Source(a)); !ok || id != a {
		t.Errorf("ParseSource(%q) = %q %v", Source(a), id, ok)
	}
	for _, s := range []string{"camera:test-mjpeg720", "camera:test-yuy2-1080p60", "camera:test-fail"} {
		if _, ok := ParseSource(s); !ok {
			t.Errorf("%s rejected", s)
		}
	}
	for _, s := range []string{"camera:", "camera:3F9A01C2B7E4", "camera:3f9a01c2b7", "camera:/dev/video0", "window:1", "camera:test-", "camera:3f9a01c2b7e4 ! fakesink"} {
		if _, ok := ParseSource(s); ok {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestV4L2Resolver(t *testing.T) {
	dir := t.TempDir()
	dev := filepath.Join(dir, "dev")
	byID := filepath.Join(dir, "by-id")
	for _, d := range []string{dev, byID} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"video0", "video1", "video4"} {
		if err := os.WriteFile(filepath.Join(dev, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(name, target string) {
		if err := os.Symlink(filepath.Join(dev, target), filepath.Join(byID, name)); err != nil {
			t.Fatal(err)
		}
	}
	link("usb-046d_HD_Pro_Webcam_C920_ABCD1234-video-index0", "video0")
	link("usb-046d_HD_Pro_Webcam_C920_ABCD1234-video-index1", "video1")
	link("usb-046d_HD_Pro_Webcam_C920_ABCD1234-video-index0-alt", "video0") // a second link: the first by name wins
	r := v4l2Resolver(byID)
	if got := r(filepath.Join(dev, "video0")); got != filepath.Join(byID, "usb-046d_HD_Pro_Webcam_C920_ABCD1234-video-index0") {
		t.Errorf("video0 -> %s", got)
	}
	if got := r(filepath.Join(dev, "video1")); !strings.HasSuffix(got, "-video-index1") {
		t.Errorf("video1 -> %s", got)
	}
	if got := r(filepath.Join(dev, "video4")); got != filepath.Join(dev, "video4") { // v4l2loopback: no by-id
		t.Errorf("video4 -> %s", got)
	}
	if got := v4l2Resolver(filepath.Join(dir, "missing"))(filepath.Join(dev, "video0")); got != filepath.Join(dev, "video0") {
		t.Errorf("no by-id dir -> %s", got)
	}
}

func TestWinInstanceKey(t *testing.T) {
	mf := `\\?\USB#VID_046D&PID_082D&MI_00#7&1A2B&0&0000#{e5323777-f976-4f5b-9b55-b94699c46e44}\GLOBAL`
	ds := `@device:pnp:\\?\usb#vid_046d&pid_082d&mi_00#7&1a2b&0&0000#{65e8773d-8f56-11d0-a3b9-00a0c9223196}\global`
	if winInstanceKey(mf) == "" || winInstanceKey(mf) != winInstanceKey(ds) {
		t.Errorf("%q != %q", winInstanceKey(mf), winInstanceKey(ds))
	}
	if k := winInstanceKey(`@device:sw:{860BB310-5D01-11D0-BD3B-00A0C911CE86}\{A3FCE0F5-3493-419F-958A-ABA1250EC20B}`); k != "" {
		t.Errorf("software device key %q", k)
	}
}

func dev(gtype, name string, props map[string]string, modes ...gst.CapsMode) gst.Device {
	return gst.Device{Name: name, Class: "Video/Source", Type: gtype, Props: props, Modes: modes}
}

var cm1080 = gst.CapsMode{Media: "image/jpeg", WMin: 1920, WMax: 1920, HMin: 1080, HMax: 1080, FPSNum: 30, FPSDen: 1}

func ident(s string) string { return s }

func listed(cs []Camera) string {
	var s []string
	for _, c := range cs {
		s = append(s, c.Backend+":"+c.Name)
	}
	return strings.Join(s, ", ")
}

func TestFromDevicesWindows(t *testing.T) {
	c920mf := `\\?\usb#vid_046d&pid_082d&mi_00#7&1a2b&0&0000#{e5323777-f976-4f5b-9b55-b94699c46e44}\global`
	devs := []gst.Device{
		dev("GstDshowDevice", "HD Pro Webcam C920", map[string]string{"gobject.device": `@device:pnp:\\?\usb#vid_046d&pid_082d&mi_00#7&1a2b&0&0000#{65e8773d-8f56-11d0-a3b9-00a0c9223196}\global`}, cm1080),
		dev("GstDshowDevice", "OBS Virtual Camera", map[string]string{"gobject.device": `@device:sw:{860BB310-5D01-11D0-BD3B-00A0C911CE86}\{A3FCE0F5-3493-419F-958A-ABA1250EC20B}`}, cm1080),
		dev("GstMFDevice", "HD Pro Webcam C920", map[string]string{"device.api": "mediafoundation", "device.path": c920mf}, cm1080),
		dev("GstMFDevice", "Integrated Camera", map[string]string{"device.path": `\\?\usb#vid_04f2&pid_b6dd&mi_00#6&2c3d&0&0000#{e5323777-f976-4f5b-9b55-b94699c46e44}\global`}, cm1080),
		// A camera whose DirectShow path doesn't line up but is PnP with the same name.
		dev("GstDshowDevice", "Integrated Camera", map[string]string{"gobject.device": `@device:pnp:\\?\usb#vid_04f2&pid_b6dd&mi_00#6&ffff&0&0000#{65e8773d-8f56-11d0-a3b9-00a0c9223196}\global`}, cm1080),
		dev("GstD3D11ScreenCaptureDevice", "Monitor", nil, cm1080), // not a camera backend
	}
	cams := fromDevices(devs, "windows", ident)
	if got, want := listed(cams), "mf:HD Pro Webcam C920, mf:Integrated Camera, dshow:OBS Virtual Camera"; got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	if cams[0].ID != hashID(BackendMF, strings.ToLower(c920mf)) || cams[0].Path != c920mf {
		t.Errorf("C920 id %s path %s", cams[0].ID, cams[0].Path)
	}
	// The path's case doesn't change the ID.
	upper := fromDevices([]gst.Device{dev("GstMFDevice", "HD Pro Webcam C920", map[string]string{"device.path": strings.ToUpper(c920mf)})}, "windows", ident)
	if upper[0].ID != cams[0].ID {
		t.Error("ID depends on path case")
	}
	if len(cams[0].Modes) != 1 || cams[0].Modes[0] != mjpg(1920, 1080, 30, 1) {
		t.Errorf("modes %v", cams[0].Modes)
	}
}

func TestFromDevicesLinux(t *testing.T) {
	resolve := func(p string) string {
		if p == "/dev/video0" {
			return "/dev/v4l/by-id/usb-046d_HD_Pro_Webcam_C920_ABCD-video-index0"
		}
		return p
	}
	devs := []gst.Device{
		dev("GstPipeWireDevice", "HD Pro Webcam C920 (V4L2)", map[string]string{"device.api": "v4l2", "api.v4l2.path": "/dev/video0", "node.name": "v4l2_input.pci-0000_00_14.0-usb-0_1_1.0", "object.serial": "71"}, cm1080),
		dev("GstPipeWireDevice", "Built-in Front Camera", map[string]string{"device.api": "libcamera", "node.name": "libcamera_input._SB_.PCI0.LNK0", "object.serial": "80"}, cm1080),
		dev("GstPipeWireDevice", "Dummy video device (0x0000)", map[string]string{"device.api": "libcamera", "node.name": "libcamera_input.usb-dummy"}, cm1080),
		dev("GstV4l2Device", "HD Pro Webcam C920", map[string]string{"device.api": "v4l2", "device.path": "/dev/video0"}, cm1080),
		dev("GstV4l2Device", "Dummy video device (0x0000)", map[string]string{"device.path": "/dev/video4"}, cm1080), // v4l2loopback
		dev("GstV4l2Device", "Dummy video device (0x0000)", map[string]string{"device.path": "/dev/video5"}, cm1080),
	}
	cams := fromDevices(devs, "linux", resolve)
	want := "v4l2:Dummy video device (0x0000), v4l2:Dummy video device (0x0000) (2), v4l2:HD Pro Webcam C920, pipewire:Built-in Front Camera"
	if got := listed(cams); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	c920 := cams[2]
	if c920.Path != resolve("/dev/video0") || c920.ID != hashID(BackendV4L2, resolve("/dev/video0")) {
		t.Errorf("C920 path %s id %s", c920.Path, c920.ID)
	}
	if pw := cams[3]; pw.Path != "libcamera_input._SB_.PCI0.LNK0" {
		t.Errorf("pipewire path %s", pw.Path)
	}
	seen := map[string]bool{}
	for _, c := range cams {
		if seen[c.ID] {
			t.Errorf("duplicate ID %s", c.ID)
		}
		seen[c.ID] = true
	}
}

func TestDuplicateKeysStayApart(t *testing.T) {
	// No path at all: the IDs come from the names and must still differ.
	cams := fromDevices([]gst.Device{dev("GstV4l2Device", "Cam", nil, cm1080), dev("GstV4l2Device", "Cam", nil, cm1080)}, "linux", ident)
	if len(cams) != 2 || cams[0].ID == cams[1].ID || cams[1].Name != "Cam (2)" {
		t.Errorf("%+v", cams)
	}
}

func TestFind(t *testing.T) {
	cams := numberNames([]Camera{
		{ID: "aaaaaaaaaaaa", Name: "HD Pro Webcam C920", Backend: BackendMF},
		{ID: "bbbbbbbbbbbb", Name: "Twin", Backend: BackendMF},
		{ID: "cccccccccccc", Name: "Twin", Backend: BackendMF},
	})
	cams = append(cams, Fakes()...)
	if c, ok := Find(cams, "bbbbbbbbbbbb", ""); !ok || c.Name != "Twin" {
		t.Errorf("by id: %v %v", c, ok)
	}
	// Moved to another USB port: new ID, same name, only one such camera.
	if c, ok := Find(cams, "dddddddddddd", "HD Pro Webcam C920"); !ok || c.ID != "aaaaaaaaaaaa" {
		t.Errorf("same-name fallback: %v %v", c, ok)
	}
	// Two cameras of that name: no guessing.
	if _, ok := Find(cams, "dddddddddddd", "Twin"); ok {
		t.Error("ambiguous name resolved")
	}
	if _, ok := Find(cams, "dddddddddddd", ""); ok {
		t.Error("unknown id without a name resolved")
	}
	if c, ok := Find(cams, "test-fail", ""); !ok || c.Backend != BackendTest {
		t.Errorf("fake: %v %v", c, ok)
	}
}

func TestListFakes(t *testing.T) {
	t.Setenv("BLITE_FAKE_CAMERAS", "1")
	Invalidate()
	defer Invalidate()
	cams, err := List()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range cams {
		if c.Backend == BackendTest {
			ids = append(ids, c.ID)
		}
	}
	if got := strings.Join(ids, ","); got != "test-mjpeg720,test-yuy2-1080p60,test-fail" {
		t.Errorf("fakes %s", got)
	}
	for _, id := range ids {
		c, err := Lookup(id)
		if err != nil || c.ID != id {
			t.Errorf("Lookup(%s) = %v %v", id, c, err)
		}
	}
	if _, err := Lookup("000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing camera: %v", err)
	}
	if f := Fit(cams[len(cams)-2], 1080, 60); f.Height != 1080 || f.FPS != 60 || f.Mode.Format != "YUY2" {
		t.Errorf("1080p60 card fit %+v", f)
	}
	if f := Fit(cams[len(cams)-3], 1080, 60); f.Height != 720 || f.FPS != 30 || !f.Mode.JPEG() {
		t.Errorf("mjpeg720 fit %+v", f)
	}
}

func TestClassify(t *testing.T) {
	be := func(code int, msg, dbg string) error {
		return &gst.BusError{Src: "camsrc", Domain: gst.DomainResource, Code: code, Msg: msg, Debug: dbg}
	}
	cases := []struct {
		err  error
		want Reason
	}{
		{nil, Other},
		{ErrNotFound, Gone},
		{fmt.Errorf("start: %w", ErrNotFound), Gone},
		// v4l2src, recorded.
		{be(gst.ResourceBusy, "Device '/dev/video0' is busy", "../sys/v4l2/gstv4l2object.c(4549): gst_v4l2_object_set_format_full (): /GstPipeline:pipeline0/GstV4l2Src:camsrc:\nCall to S_FMT failed for YUYV @ 1280x720: Device or resource busy"), Busy},
		{be(gst.ResourceOpenReadWrite, "Could not open device '/dev/video0' for reading and writing.", "../sys/v4l2/v4l2_calls.c(637): gst_v4l2_open (): /GstPipeline:pipeline0/GstV4l2Src:camsrc:\nsystem error: Permission denied"), Denied},
		{be(gst.ResourceRead, "Could not read from resource.", "../sys/v4l2/gstv4l2bufferpool.c(1338): gst_v4l2_buffer_pool_poll (): /GstPipeline:pipeline0/GstV4l2Src:camsrc:\npoll error 1: No such device (19)"), Gone},
		{be(gst.ResourceNotFound, "Cannot identify device '/dev/video9'.", "system error: No such file or directory"), Gone},
		{be(gst.ResourceNotAuthorized, "Not authorized", ""), Denied},
		// mfvideosrc (expected forms; to be confirmed on hardware).
		{be(gst.ResourceFailed, "Failed to start capture", "../sys/mediafoundation/gstmfsourcereader.cpp(560): gst_mf_source_reader_start (): hr: 0xc00d3704"), Busy},
		{be(gst.ResourceFailed, "Failed to read frame", "hr: 0xC00D3EA2"), Gone},
		{be(gst.ResourceFailed, "Failed to open device", "hr: 0x80070005"), Denied},
		{be(gst.ResourceFailed, "Failed to start capture", "hr: 0xc00dabe0"), Gone},
		{errors.New("camsrc: Internal data stream error. (streaming stopped, reason not-negotiated (-4))"), Other},
		{&gst.BusError{Domain: gst.DomainStream, Code: 1, Msg: "Internal data stream error."}, Other},
	}
	for _, c := range cases {
		if got := Classify(c.err); got != c.want {
			t.Errorf("Classify(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestReport(t *testing.T) {
	cams := numberNames([]Camera{
		{ID: "3f9a01c2b7e4", Name: "HD Pro Webcam C920", Backend: BackendMF, Path: `\\?\usb#vid_046d&pid_082d&mi_00#SERIAL1234#{guid}\global`, Modes: c920},
		{ID: "0123456789ab", Name: "OBS Virtual Camera", Backend: BackendDShow, Path: `@device:sw:{860BB310}\{A3FCE0F5}`, Modes: obs},
		{ID: "ba9876543210", Name: "Odd", Backend: BackendMF, Path: "x"},
	})
	var b strings.Builder
	report(&b, cams, Privacy{Checked: true, User: "Allow", UserDesktop: "Deny"}, false)
	out := b.String()
	for _, want := range []string{
		"3 cameras found.",
		"1. HD Pro Webcam C920", "camera:3f9a01c2b7e4", "mf (Media Foundation)",
		"MJPEG  1920x1080  30, 24, 20, 15, 10 fps",
		"1080p60  MJPEG 1920x1080 @ 30 fps  1080p30",
		"dshow (DirectShow)",
		"none we can use",
		"Windows camera privacy: BLOCKED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if !regexp.MustCompile(`Let desktop apps access your camera: +OFF`).MatchString(out) {
		t.Errorf("report lacks the desktop-apps switch:\n%s", out)
	}
	for _, leak := range []string{"SERIAL1234", "vid_046d", "A3FCE0F5", `\\?\`} {
		if strings.Contains(out, leak) {
			t.Errorf("report shows a device path (%q)", leak)
		}
	}
	b.Reset()
	report(&b, nil, Privacy{}, false)
	if !strings.HasPrefix(b.String(), "No cameras found.") || strings.Contains(b.String(), "privacy") {
		t.Errorf("empty report: %q", b.String())
	}
}

func TestWatchStops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, func() {}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Skip("no device monitor here:", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch didn't return after its context ended")
	}
}

func TestEmptyListingIsCached(t *testing.T) {
	Invalidate()
	defer Invalidate()
	if _, err := List(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	ok, at := cacheOK, cachedAt
	mu.Unlock()
	if !ok {
		t.Fatal("listing not cached")
	}
	if _, err := List(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	again := cachedAt
	mu.Unlock()
	if !again.Equal(at) {
		t.Error("second List within 5 s listed again (an empty listing wasn't cached)")
	}
	Invalidate()
	mu.Lock()
	ok = cacheOK
	mu.Unlock()
	if ok {
		t.Error("Invalidate kept the cache")
	}
}
