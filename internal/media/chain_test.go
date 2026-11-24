package media

import (
	"strings"
	"testing"

	"blite/internal/gst"
)

// Every capture asks for the share's frame rate right after the source;
// Windows capture is scaled and converted on the GPU on ChainGPU, and on
// the CPU's every core otherwise.
func TestCaptureChain(t *testing.T) {
	l := Layer{Width: 1920, Height: 1080, FPS: 60}
	const wgc = "d3d11screencapturesrc monitor-handle=1 show-cursor=true capture-api=wgc"
	gpu := captureChain(wgc, l, "NV12", ChainGPU)
	for _, want := range []string{
		wgc + " ! video/x-raw(memory:D3D11Memory),framerate=60/1 ! identity name=captured ! d3d11convert",
		"video/x-raw(memory:D3D11Memory),format=NV12,width=1920,height=1080,pixel-aspect-ratio=1/1 ! d3d11download",
	} {
		if !strings.Contains(gpu, want) {
			t.Errorf("GPU chain %q lacks %q", gpu, want)
		}
	}
	if cpu := captureChain(wgc, l, "I420", ChainCPU); !strings.HasPrefix(cpu, wgc+" ! d3d11download ! video/x-raw,framerate=60/1 ! identity name=captured ! ") ||
		!strings.HasSuffix(cpu, " n-threads=0") {
		t.Errorf("CPU chain %q", cpu)
	}
	x := captureChain("ximagesrc use-damage=false show-pointer=true", Layer{FPS: 30}, "I420", ChainCPU)
	if !strings.HasPrefix(x, "ximagesrc use-damage=false show-pointer=true ! video/x-raw,framerate=30/1 ! identity name=captured ! ") ||
		strings.Contains(x, "n-threads") || strings.Contains(x, "d3d11") {
		t.Errorf("X11 chain %q", x)
	}
	// ximagesrc's chain doesn't change on ChainGPU; the test source has no cap
	if x2 := captureChain("ximagesrc use-damage=false show-pointer=true", Layer{FPS: 30}, "I420", ChainGPU); x2 != x {
		t.Errorf("X11 chain on the GPU %q", x2)
	}
	if v := captureChain("videotestsrc is-live=true", l, "I420", ChainCPU); strings.Contains(v, "framerate") {
		t.Errorf("test chain %q", v)
	}
}

// Only Windows capture with d3d11convert installed converts on the GPU.
func TestChainFor(t *testing.T) {
	Init()
	if c := chainFor("ximagesrc use-damage=false"); c != ChainCPU {
		t.Errorf("ximagesrc: %s", c)
	}
	want := ChainCPU
	if gst.HasFactory("d3d11convert") {
		want = ChainGPU
	}
	if c := chainFor("d3d11screencapturesrc capture-api=wgc"); c != want {
		t.Errorf("d3d11screencapturesrc: %s, want %s", c, want)
	}
	ForceChain = ChainCPU
	defer func() { ForceChain = "" }()
	if c := chainFor("d3d11screencapturesrc capture-api=wgc"); c != ChainCPU {
		t.Errorf("forced CPU: %s", c)
	}
}
