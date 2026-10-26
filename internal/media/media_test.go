package media

import (
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
	src, enc, err := StartVideoSource(SourceTest, "", nil, "", func(layer int, au AccessUnit) {
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
