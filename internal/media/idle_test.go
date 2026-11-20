package media

import (
	"bytes"
	"sync/atomic"
	"testing"
	"time"
)

// hasIDR reports whether an Annex-B access unit holds an IDR slice.
func hasIDR(au []byte) bool {
	for {
		i := bytes.Index(au, []byte{0, 0, 1})
		if i < 0 || i+3 >= len(au) {
			return false
		}
		if au[i+3]&0x1f == 5 {
			return true
		}
		au = au[i+3:]
	}
}

// An idle source grabs and encodes nothing, and resuming brings frames
// back at once, starting with a keyframe.
func TestIdlePausesCapture(t *testing.T) {
	Init()
	var high, keys atomic.Int64
	src, _, err := StartVideoSource(SourceTest, "", "", nil, "", "", func(layer int, au AccessUnit) {
		if layer == High {
			high.Add(1)
			if hasIDR(au.Data) {
				keys.Add(1)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Stop()
	time.Sleep(time.Second)
	if src.OfferedFrames() <= 0 {
		t.Fatal("no frames before going idle")
	}

	src.SetIdle(true)
	if !src.Idle() {
		t.Fatal("not idle after SetIdle(true)")
	}
	time.Sleep(300 * time.Millisecond) // frames in flight
	o, h := src.OfferedFrames(), high.Load()
	time.Sleep(time.Second)
	if d, dh := src.OfferedFrames()-o, high.Load()-h; d != 0 || dh != 0 {
		t.Fatalf("idle source offered %d frames and encoded %d", d, dh)
	}

	time.Sleep(time.Second) // past the 2 s keyframe interval: only a forced one comes now
	k := keys.Load()
	src.SetIdle(false)
	deadline := time.Now().Add(2 * time.Second)
	for high.Load() == h && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if high.Load() == h {
		t.Fatal("no frames after resuming")
	}
	time.Sleep(200 * time.Millisecond)
	if keys.Load() == k {
		t.Fatal("no keyframe right after resuming")
	}
	select {
	case err := <-src.Err:
		t.Fatalf("pipeline error across pause/resume: %v", err)
	default:
	}
}
