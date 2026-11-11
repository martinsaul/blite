package media

import (
	"sync/atomic"
	"testing"
	"time"
)

// Encode a live test pattern and decode it again, with nothing in between.
func TestEncodeDecodeLoopback(t *testing.T) {
	Init()
	pl, err := StartPlayer(ViewCount)
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Stop()

	aus := make(chan AccessUnit, 64)
	low := make(chan struct{}, 1)
	src, enc, err := StartVideoSource(SourceTest, "", "", nil, "", "", func(layer int, au AccessUnit) {
		if layer == Low {
			select {
			case low <- struct{}{}:
			default:
			}
			return
		}
		select {
		case aus <- au:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Stop()
	t.Logf("encoder: %s", enc)

	deadline := time.After(10 * time.Second)
	n := 0
	for pl.Frames() < 30 {
		select {
		case au := <-aus:
			n++
			pl.Push(au.Data)
		case err := <-src.Err:
			t.Fatalf("source: %v", err)
		case err := <-pl.Err:
			t.Fatalf("player: %v", err)
		case <-deadline:
			t.Fatalf("decoded %d frames from %d access units in 10 s", pl.Frames(), n)
		}
	}
	t.Logf("decoded %d frames from %d access units", pl.Frames(), n)
	select {
	case <-low:
	case <-time.After(2 * time.Second):
		t.Fatal("LOW layer produced nothing")
	}
	src.ForceKeyframe(High) // must not crash or deadlock

	// The LOW encoder's bitrate changes while PLAYING without stalling it.
	src.SetBitrate(Low, 150)
	for range 3 {
		<-low
	}
	select {
	case <-low:
	case err := <-src.Err:
		t.Fatalf("source after SetBitrate: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("LOW layer stalled after SetBitrate")
	}
}

func TestOpusLoopback(t *testing.T) {
	Init()
	pl, err := StartAudioPlayer("count")
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Stop()
	frames := make(chan []byte, 64)
	src, err := StartAudioSource(TestAudio, func(f []byte, _ time.Duration) {
		select {
		case frames <- f:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Stop()
	deadline := time.After(5 * time.Second)
	n := 0
	for pl.Buffers() < 25 {
		select {
		case f := <-frames:
			n++
			pl.Push(f)
		case err := <-src.Err:
			t.Fatalf("source: %v", err)
		case <-deadline:
			t.Fatalf("decoded %d buffers from %d frames", pl.Buffers(), n)
		}
	}
	t.Logf("decoded %d buffers from %d Opus frames", pl.Buffers(), n)
}

// SetHighFPS thins HIGH and leaves LOW alone; game mode runs HIGH at 60.
func TestHighFPS(t *testing.T) {
	Init()
	count := func(mode ContentMode, fps int) (high, low int) {
		var h, l atomic.Int64
		src, _, err := StartVideoSource(SourceTest, "", mode, nil, "x264enc", "", func(layer int, _ AccessUnit) {
			switch layer {
			case High:
				h.Add(1)
			case Low:
				l.Add(1)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		defer src.Stop()
		src.SetHighFPS(fps)
		time.Sleep(time.Second)
		h0, l0, o0 := h.Load(), l.Load(), src.HighOfferedFrames()
		time.Sleep(3 * time.Second)
		if o := src.HighOfferedFrames() - o0; o < int(h.Load()-h0)-2 {
			t.Errorf("offered %d < encoded %d", o, h.Load()-h0)
		}
		return int(h.Load()-h0) / 3, int(l.Load()-l0) / 3
	}
	if h, l := count(ModeDesktop, 10); h < 8 || h > 12 || l < 13 {
		t.Errorf("desktop at 10: HIGH %d fps, LOW %d fps", h, l)
	}
	if h, _ := count(ModeGame, 60); h < 50 {
		t.Errorf("game: HIGH %d fps", h)
	}
}
