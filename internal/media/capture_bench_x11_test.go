//go:build linux

package media

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// cpuTime is the user+system CPU this process used so far.
func cpuTime() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// Measures the CPU of sharing the whole X screen for 10 s with a viewer
// (capture and encoders running) and without (SetIdle, where the source
// has it). Opt-in:
//
//	xvfb-run -a -s "-screen 0 5120x2880x24" env BLITE_CAPTURE_BENCH=1 \
//	    go test -run TestCaptureCPU -v ./internal/media
//
// BLITE_CAPTURE_BENCH_RES picks the resolution (default 1080p).
func TestCaptureCPU(t *testing.T) {
	if os.Getenv("BLITE_CAPTURE_BENCH") == "" {
		t.Skip("set BLITE_CAPTURE_BENCH=1 with DISPLAY pointing at a test X server")
	}
	Init()
	res := Resolution(os.Getenv("BLITE_CAPTURE_BENCH_RES"))
	if res == "" {
		res = Res1080
	}
	src, enc, err := StartVideoSource(SourceScreen, res, "", nil, "x264enc", "", func(int, AccessUnit) {})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Stop()
	time.Sleep(2 * time.Second) // warm up
	measure := func(label string) {
		c, o, at := cpuTime(), src.OfferedFrames(), time.Now()
		time.Sleep(10 * time.Second)
		cpu, wall := cpuTime()-c, time.Since(at)
		t.Logf("%s, %s %s: CPU %.2f s in %.1f s (%.0f%% of a core), %d frames offered",
			label, res, enc, cpu.Seconds(), wall.Seconds(), 100*cpu.Seconds()/wall.Seconds(), src.OfferedFrames()-o)
	}
	measure("viewer")
	idler, ok := any(src).(interface{ SetIdle(bool) })
	if !ok {
		measure("no viewer (no idle: same pipeline)")
		return
	}
	idler.SetIdle(true)
	measure("no viewer (idle)")
	idler.SetIdle(false)
	time.Sleep(time.Second)
	measure("viewer again")
}
