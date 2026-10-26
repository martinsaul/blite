package media

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"blite/internal/gst"
)

// Memory-growth isolation: loops pre-encoded 1080p access units into one player
// variant for BLITE_LEAK_MINUTES and prints RSS every 30 s. Run one
// variant per process: BLITE_LEAK_VARIANT=full|fakesink|nodec|avdec.
func TestLeakLoop(t *testing.T) {
	variant := os.Getenv("BLITE_LEAK_VARIANT")
	if variant == "" {
		t.Skip("BLITE_LEAK_VARIANT not set")
	}
	mins := 10
	fmt.Sscan(os.Getenv("BLITE_LEAK_MINUTES"), &mins)
	Init()
	// 300 access units from a keyframe on (5 GOPs at 30 fps).
	aus := make(chan []byte, 512)
	src, _, err := StartVideoSource(SourceTest, Res1080, nil, "x264enc", func(layer int, au AccessUnit) {
		if layer == High {
			select {
			case aus <- au.Data:
			default:
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	var clip [][]byte
	for len(clip) < 300 {
		au := <-aus
		if len(clip) == 0 && !annexBKey(au) {
			continue
		}
		clip = append(clip, au)
	}
	src.Stop()

	const in = "appsrc name=in is-live=true format=time do-timestamp=true caps=video/x-h264,stream-format=byte-stream,alignment=au ! h264parse ! "
	var push func([]byte)
	var stop func()
	switch variant {
	case "full":
		pl, err := StartPlayer(ViewCount)
		if err != nil {
			t.Fatal(err)
		}
		push, stop = pl.Push, pl.Stop
	default:
		desc := map[string]string{
			"fakesink": in + "decodebin ! identity name=decoded ! queue max-size-buffers=2 leaky=downstream ! videoconvert ! video/x-raw,format=I420 ! fakesink sync=false",
			"nodec":    in + "fakesink sync=false",
			"avdec":    in + "avdec_h264 ! queue max-size-buffers=2 leaky=downstream ! videoconvert ! video/x-raw,format=I420 ! appsink name=frames sync=false max-buffers=2 drop=true",
		}[variant]
		p, err := launch(desc)
		if err != nil {
			t.Fatal(err)
		}
		e, err := p.element("in")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(desc, "name=frames") {
			fs, _ := p.element("frames")
			p.goLoop(func() bool {
				_, _, err := fs.Pull(100 * time.Millisecond)
				return err == nil || errors.Is(err, gst.ErrTimeout)
			})
		}
		if err := p.start(); err != nil {
			t.Fatal(err)
		}
		push, stop = func(b []byte) { e.Push(b) }, p.Stop
	}
	defer stop()
	tick := time.NewTicker(time.Second / 30)
	defer tick.Stop()
	end := time.Now().Add(time.Duration(mins) * time.Minute)
	next := time.Now()
	for i := 0; time.Now().Before(end); i++ {
		<-tick.C
		push(clip[i%len(clip)])
		if time.Now().After(next) {
			fmt.Printf("%s %s rss=%s\n", variant, time.Now().Format("15:04:05"), rss())
			next = next.Add(30 * time.Second)
		}
	}
}

func annexBKey(au []byte) bool {
	for i := 0; i+4 < len(au); i++ {
		if au[i] == 0 && au[i+1] == 0 && au[i+2] == 1 {
			if t := au[i+3] & 0x1f; t == 5 || t == 7 {
				return true
			}
		}
	}
	return false
}

func rss() string {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			return strings.TrimSpace(strings.TrimPrefix(l, "VmRSS:"))
		}
	}
	return "?"
}
