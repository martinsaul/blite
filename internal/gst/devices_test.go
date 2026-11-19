package gst

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseCapsModes(t *testing.T) {
	cases := []struct {
		name, caps string
		want       []CapsMode
	}{
		{"fixed", "image/jpeg, width=(int)1920, height=(int)1080, framerate=(fraction)30/1",
			[]CapsMode{{Media: "image/jpeg", WMin: 1920, WMax: 1920, HMin: 1080, HMax: 1080, FPSNum: 30, FPSDen: 1}}},
		{"lists", "video/x-raw, format=(string){ YUY2, NV12 }, width=(int)1280, height=(int)720, framerate=(fraction){ 30/1, 30000/1001 }",
			[]CapsMode{
				{Media: "video/x-raw", Format: "YUY2", WMin: 1280, WMax: 1280, HMin: 720, HMax: 720, FPSNum: 30, FPSDen: 1},
				{Media: "video/x-raw", Format: "YUY2", WMin: 1280, WMax: 1280, HMin: 720, HMax: 720, FPSNum: 30000, FPSDen: 1001},
				{Media: "video/x-raw", Format: "NV12", WMin: 1280, WMax: 1280, HMin: 720, HMax: 720, FPSNum: 30, FPSDen: 1},
				{Media: "video/x-raw", Format: "NV12", WMin: 1280, WMax: 1280, HMin: 720, HMax: 720, FPSNum: 30000, FPSDen: 1001},
			}},
		{"ranges", "video/x-raw, format=(string)YUY2, width=(int)[ 2, 8192 ], height=(int)[ 2, 8192 ], framerate=(fraction)[ 1/1, 60/1 ]",
			[]CapsMode{{Media: "video/x-raw", Format: "YUY2", WMin: 2, WMax: 8192, HMin: 2, HMax: 8192, FPSNum: 1, FPSDen: 1, FPSMaxNum: 60, FPSMaxDen: 1}}},
		{"several structures and no rate", "image/jpeg, width=(int)640, height=(int)480; video/x-raw, format=(string)NV12, width=(int)1920, height=(int)1080, framerate=(fraction){ 60/1, 30/1 }",
			[]CapsMode{
				{Media: "image/jpeg", WMin: 640, WMax: 640, HMin: 480, HMax: 480},
				{Media: "video/x-raw", Format: "NV12", WMin: 1920, WMax: 1920, HMin: 1080, HMax: 1080, FPSNum: 60, FPSDen: 1},
				{Media: "video/x-raw", Format: "NV12", WMin: 1920, WMax: 1920, HMin: 1080, HMax: 1080, FPSNum: 30, FPSDen: 1},
			}},
		{"non-system memory is skipped", "video/x-raw(memory:DMABuf), format=(string)NV12, width=(int)1280, height=(int)720; video/x-raw, format=(string)NV12, width=(int)640, height=(int)360, framerate=(fraction)30/1",
			[]CapsMode{{Media: "video/x-raw", Format: "NV12", WMin: 640, WMax: 640, HMin: 360, HMax: 360, FPSNum: 30, FPSDen: 1}}},
		{"empty", "EMPTY", []CapsMode{}},
		{"any", "ANY", []CapsMode{}},
	}
	for _, c := range cases {
		got, err := ParseCapsModes(c.caps)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
	if _, err := ParseCapsModes("video/x-raw, width=(int)[ broken"); err == nil {
		t.Error("broken caps parsed")
	}
}

// The host may have no cameras; listing must still work and be well formed.
func TestListDevices(t *testing.T) {
	ds, err := ListDevices("Video/Source")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if d.Type == "" || d.Props == nil {
			t.Errorf("device %q: type %q props %v", d.Name, d.Type, d.Props)
		}
	}
	if ds, err := ListDevices("No/Such/Class"); err != nil || len(ds) != 0 {
		t.Errorf("unknown class: %v %v", ds, err)
	}
}

func TestDeviceWatch(t *testing.T) {
	w, err := NewDeviceWatch("Video/Source")
	if err != nil {
		t.Skip("no provider starts here:", err)
	}
	defer w.Close()
	start := time.Now()
	for i := 0; i < 3; i++ {
		w.Next(20 * time.Millisecond) // no hotplug expected; must not block past the timeout
	}
	if time.Since(start) > 2*time.Second {
		t.Error("Next blocked past its timeout")
	}
	w.Close()
	if _, ok := w.Next(time.Millisecond); ok {
		t.Error("event after Close")
	}
}

func TestBusErrorTyped(t *testing.T) {
	p, err := Launch("videotestsrc num-buffers=1 ! video/x-raw,width=64,height=64 ! filesink location=/nonexistent/dir/x")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Free()
	defer p.SetNull()
	_ = p.Play()
	var be *BusError
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e := p.BusError(200 * time.Millisecond)
		if e == nil {
			continue
		}
		if !errors.As(e, &be) {
			t.Fatalf("not a *BusError: %T %v", e, e)
		}
		break
	}
	if be == nil {
		t.Fatal("no bus error")
	}
	if be.Domain != DomainResource || be.Code != ResourceOpenWrite {
		t.Errorf("domain %q code %d, want %q %d", be.Domain, be.Code, DomainResource, ResourceOpenWrite)
	}
	if !strings.HasPrefix(be.Error(), be.Src+": "+be.Msg+" (") {
		t.Errorf("Error() = %q", be.Error())
	}
}
