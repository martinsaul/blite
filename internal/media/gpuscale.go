package media

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"blite/internal/gst"
)

// gpuScaleChain scales decoded video down to the grid canvas (gridW x
// gridH at most, aspect kept) on the GPU before it is copied to system
// memory: a 4K frame then costs a quarter of the copy, and no CPU scaling.
// d3d11upload passes a D3D11 decoder's frames through and uploads a
// software decoder's.
var gpuScaleChain = fmt.Sprintf("d3d11upload ! d3d11convert ! video/x-raw(memory:D3D11Memory),width=[1,%d],height=[1,%d] ! d3d11download ! ", gridW, gridH)

var gpuScale struct {
	once  sync.Once
	chain string
}

// PrepareVideo checks once whether this machine can scale video on the GPU,
// so the first player doesn't wait for the check.
func PrepareVideo() { playerScale() }

// playerScale is the chain between a grid player's decoder and its
// intervideosink: gpuScaleChain where the GPU path works on this machine
// (checked once), else nothing (the grid scales on the CPU).
func playerScale() string {
	gpuScale.once.Do(func() {
		if runtime.GOOS != "windows" {
			return
		}
		Init()
		for _, f := range []string{"d3d11upload", "d3d11convert", "d3d11download"} {
			if !gst.HasFactory(f) {
				Log.Info("video: no GPU scaling", "missing", f)
				return
			}
		}
		if err := testGPUScale(); err != nil {
			Log.Warn("video: GPU scaling failed its test, scaling on the CPU", "err", err)
			return
		}
		gpuScale.chain = gpuScaleChain
		Log.Info("video: scaling decoded video on the GPU")
	})
	return gpuScale.chain
}

// testGPUScale runs a few 4K frames through gpuScaleChain and expects
// them at the canvas size.
func testGPUScale() error {
	p, err := launch(fmt.Sprintf("videotestsrc num-buffers=3 ! video/x-raw,format=NV12,width=3840,height=2160,framerate=60/1 ! %s"+
		"video/x-raw,width=%d,height=%d ! fakesink sync=false", gpuScaleChain, gridW, gridH))
	if err != nil {
		return err
	}
	defer p.Stop()
	if err := p.start(); err != nil {
		return err
	}
	select {
	case err := <-p.Err:
		if errors.Is(err, gst.ErrEOS) {
			return nil
		}
		return err
	case <-time.After(5 * time.Second):
		return errors.New("timed out")
	}
}
