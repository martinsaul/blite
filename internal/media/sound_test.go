package media

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
	"time"
)

func TestSynthesizeSounds(t *testing.T) {
	if len(AllSounds) != 4 {
		t.Fatalf("AllSounds len = %d, want 4", len(AllSounds))
	}

	seenPCM := map[string]SoundKind{}
	for _, k := range AllSounds {
		samples := SynthesizeSound(k)
		if len(samples) == 0 || len(samples)%SoundChannels != 0 {
			t.Fatalf("%s: invalid sample count %d", k, len(samples))
		}
		frames := len(samples) / SoundChannels
		durMs := float64(frames) * 1000.0 / float64(SoundRate)
		if durMs < 150 || durMs > 400 {
			t.Fatalf("%s: duration = %.1f ms, want 150..400 ms", k, durMs)
		}

		// Click-free start and end.
		if samples[0] != 0 || samples[1] != 0 || samples[len(samples)-2] != 0 || samples[len(samples)-1] != 0 {
			t.Fatalf("%s: non-zero boundary samples start=(%d,%d) end=(%d,%d)",
				k, samples[0], samples[1], samples[len(samples)-2], samples[len(samples)-1])
		}

		// Peak around -18 dBFS.
		var maxAbs int16
		for _, s := range samples {
			if s < 0 {
				s = -s
			}
			if s > maxAbs {
				maxAbs = s
			}
		}
		dbfs := 20.0 * math.Log10(float64(maxAbs)/32767.0)
		if math.Abs(dbfs-SoundPeakDBFS) > 0.25 {
			t.Fatalf("%s: peak = %.2f dBFS (sample %d), want ~%.1f dBFS", k, dbfs, maxAbs, SoundPeakDBFS)
		}

		pcm := SoundPCM(k)
		if prev, dup := seenPCM[string(pcm)]; dup {
			t.Fatalf("%s has identical PCM to %s", k, prev)
		}
		seenPCM[string(pcm)] = k

		wav := SoundWAV(k)
		if len(wav) != 44+len(pcm) || !bytes.Equal(wav[0:4], []byte("RIFF")) || !bytes.Equal(wav[8:12], []byte("WAVE")) {
			t.Fatalf("%s: invalid WAV header (len=%d)", k, len(wav))
		}
		if rate := binary.LittleEndian.Uint32(wav[24:28]); rate != SoundRate {
			t.Fatalf("%s: WAV sample rate = %d, want %d", k, rate, SoundRate)
		}
	}
}

func TestSoundGateDebounce(t *testing.T) {
	var g SoundGate
	t0 := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)

	if !g.AllowAt(SoundPeerJoined, t0) {
		t.Fatal("first SoundPeerJoined should be allowed")
	}
	// Burst of same kind within 2 s plays once.
	if g.AllowAt(SoundPeerJoined, t0.Add(200*time.Millisecond)) {
		t.Fatal("duplicate SoundPeerJoined at +200ms should be suppressed")
	}
	if g.AllowAt(SoundPeerJoined, t0.Add(1999*time.Millisecond)) {
		t.Fatal("duplicate SoundPeerJoined at +1999ms should be suppressed")
	}
	// Different kind within the same 2 s window plays independently.
	if !g.AllowAt(SoundPeerSharing, t0.Add(300*time.Millisecond)) {
		t.Fatal("SoundPeerSharing should not be blocked by SoundPeerJoined")
	}
	// Same kind after 2 s plays again.
	if !g.AllowAt(SoundPeerJoined, t0.Add(2*time.Second)) {
		t.Fatal("SoundPeerJoined at +2s should be allowed again")
	}

	got := g.Played()
	want := []SoundKind{SoundPeerJoined, SoundPeerSharing, SoundPeerJoined}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("Played() = %v, want %v", got, want)
	}
}

func TestSoundPlayerGStreamer(t *testing.T) {
	before := LiveSoundPlayers()

	// 1. Fast headless "count" sink: verify all 4 chimes push expected PCM buffers.
	sp, err := StartSoundPlayer("count")
	if err != nil {
		t.Fatal(err)
	}
	if n := LiveSoundPlayers() - before; n != 1 {
		t.Fatalf("LiveSoundPlayers = %d, want 1", n)
	}
	sp.Play(SoundPeerJoined)
	sp.Play(SoundViewerWatching)

	deadline := time.Now().Add(2 * time.Second)
	for sp.Buffers() < 20 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if sp.Buffers() < 20 {
		t.Fatalf("count sink buffers = %d, want >= 20", sp.Buffers())
	}
	sp.Stop()
	if n := LiveSoundPlayers() - before; n != 0 {
		t.Fatalf("LiveSoundPlayers after Stop = %d, want 0", n)
	}

	// 2. Clock-synced sink ("fakesink sync=true"): play a chime, leave an idle
	// gap, play a second chime, and confirm GStreamer delivers both without
	// bus errors or leaks.
	clocked, err := StartSoundPlayer("fakesink sync=true")
	if err != nil {
		t.Fatal(err)
	}
	clocked.Play(SoundSelfLive)
	deadline = time.Now().Add(2 * time.Second)
	for clocked.Delivered() < 15 && time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
	}
	first := clocked.Delivered()
	if first < 15 {
		clocked.Stop()
		t.Fatalf("first chime delivered = %d, want >= 15", first)
	}

	// Idle gap with zero buffers pushed.
	time.Sleep(150 * time.Millisecond)

	clocked.Play(SoundViewerWatching)
	deadline = time.Now().Add(2 * time.Second)
	for clocked.Delivered() < first+11 && time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
	}
	second := clocked.Delivered()
	if second < first+11 {
		clocked.Stop()
		t.Fatalf("second chime after idle gap delivered = %d (first=%d)", second, first)
	}
	select {
	case berr := <-clocked.Err:
		clocked.Stop()
		t.Fatalf("unexpected GStreamer bus error: %v", berr)
	default:
	}
	clocked.Stop()
	if n := LiveSoundPlayers() - before; n != 0 {
		t.Fatalf("LiveSoundPlayers after clocked Stop = %d, want 0", n)
	}
}
