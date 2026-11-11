package session

import (
	"testing"
	"time"
)

// A still window offers few frames; that must not count as a slow encoder.
func TestEncoderBehind(t *testing.T) {
	w := 10 * time.Second
	for _, c := range []struct {
		in, out int
		want    bool
	}{
		{300, 300, false}, // 30 fps, keeping up
		{300, 250, false}, // 25 fps: fast enough
		{300, 180, true},  // 18 of 30 fps: the encoder drops frames
		{100, 100, false}, // still window: 10 fps offered, all encoded
		{120, 103, false}, // still window, a few lost
		{200, 100, true},  // offered 20, encoded 10
		{-1, 180, true},   // unknown input: rate alone
		{-1, 300, false},
	} {
		if got := encoderBehind(c.in, c.out, 30, w); got != c.want {
			t.Errorf("in %d out %d: behind %v, want %v", c.in, c.out, got, c.want)
		}
	}
	// at 60 fps (game), 40 of 60 is behind; thinned to 15, 15 is not
	for _, c := range []struct {
		in, out, fps int
		want         bool
	}{
		{600, 400, 60, true},
		{600, 590, 60, false},
		{150, 150, 15, false},
		{150, 100, 15, true},
	} {
		if got := encoderBehind(c.in, c.out, c.fps, w); got != c.want {
			t.Errorf("in %d out %d at %d: behind %v, want %v", c.in, c.out, c.fps, got, c.want)
		}
	}
}
