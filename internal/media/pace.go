package media

import (
	"errors"
	"sync/atomic"
	"time"

	"blite/internal/gst"
)

// pacer hands the capture's frames to the encoders as they come, at most
// fps a second, and repeats the last one for a moment when nothing
// follows it.
//
// videorate did this before, but it holds the newest frame until the next
// one comes, to know whether to drop or copy it. Windows capture only
// delivers a frame when the screen changed: the next slide reached viewers
// when the mouse moved. Here a change goes out at once (or at the next
// frame slot, when the capture runs faster than fps). The repeats, a frame
// and a half apart until one went out maxDup after the change, carry it
// through the layers' videorates and encoders that hold a frame back; then
// a still screen costs nothing. The appsrc stamps every frame with the time
// it goes out, so a capture gap is a gap, with no catch-up burst after it.
type pacer struct {
	in, out *gst.Element // the appsink after the conversion, the appsrc before the tee
	fps     int
	frame   time.Duration
	window  time.Duration // how long after a change it's repeated
	last    *gst.Sample   // the newest frame
	pending bool          // last hasn't gone out yet
	fresh   time.Time     // when last came
	pushed  time.Time     // when a frame last went out
	slot    time.Time     // the earliest the next new frame goes out
	again   atomic.Bool   // repeat asked: send last again, as if it were new
}

// pacerIdle is the longest a pacer waits for a frame at once.
const pacerIdle = 100 * time.Millisecond

// paceCapture starts pacing a captureBranch of p at fps. Call it before
// p.start.
func paceCapture(p *Pipeline, fps int) (*pacer, error) {
	in, err := p.element("fresh")
	if err != nil {
		return nil, err
	}
	out, err := p.element("paced")
	if err != nil {
		return nil, err
	}
	pc := &pacer{in: in, out: out, fps: fps, frame: time.Second / time.Duration(fps), window: maxDup(fps)}
	p.goLoop(pc.step)
	p.atStop(func() { pc.last.Free() })
	return pc, nil
}

// repeat sends the newest frame again, with its repeats, as if it had just
// come. An encoder asked for a keyframe makes it from its next frame, and a
// still screen gives none: a viewer who joined or switched layer saw
// nothing until the screen changed.
func (pc *pacer) repeat() {
	if pc != nil {
		pc.again.Store(true)
	}
}

// step waits for a frame until the next one is due out, or sends it.
func (pc *pacer) step() bool {
	if pc.again.Swap(false) && pc.last != nil {
		pc.pending, pc.fresh = true, time.Now()
	}
	if d := pc.wait(time.Now()); d > 0 {
		s, err := pc.in.PullSample(min(d, pacerIdle))
		switch {
		case errors.Is(err, gst.ErrTimeout):
			return true
		case err != nil:
			return false
		}
		pc.last.Free()
		pc.last, pc.pending, pc.fresh = s, true, time.Now()
		return true
	}
	now := time.Now()
	pc.out.PushFrame(pc.last, pc.frame, pc.fps)
	pc.pending, pc.pushed = false, now
	// On the frame grid while the capture keeps up; after a gap, half a
	// frame on.
	pc.slot = pc.slot.Add(pc.frame)
	if soon := now.Add(pc.frame / 2); pc.slot.Before(soon) {
		pc.slot = soon
	}
	return true
}

// wait is how long until a frame is due out (<= 0: now), or pacerIdle
// when none is.
func (pc *pacer) wait(now time.Time) time.Duration {
	switch {
	case pc.last == nil:
		return pacerIdle
	case pc.pending:
		return pc.slot.Sub(now)
	}
	if pc.pushed.Sub(pc.fresh) >= pc.window {
		return pacerIdle
	}
	return pc.pushed.Add(pc.frame * 3 / 2).Sub(now)
}
