package media

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"blite/internal/gst"
)

// IsHardware reports whether an encoder factory runs on a GPU or media
// engine, so encoding another rendition costs little CPU.
func IsHardware(factory string) bool { return factory != "" && factory != "x264enc" }

// Transcoder (relay side) decodes one origin's top stream once
// and encodes it again at each requested rendition height.
type Transcoder struct {
	*VideoSource
	in      *gst.Element
	Encoder string
}

// StartTranscoder starts decoding pushed access units and encoding them at
// heights (rendition heights; others are ignored). onAU gets each output
// access unit with its rendition's layer ID. Push access units from a
// keyframe on; until one arrives nothing comes out.
func StartTranscoder(heights []int, encoderForce string, onAU func(layer int, au AccessUnit)) (*Transcoder, error) {
	var layers []int
	for _, h := range heights {
		if id := RenditionLayer(h); id >= 0 {
			layers = append(layers, id)
		}
	}
	if len(layers) == 0 {
		return nil, errors.New("no rendition to transcode to")
	}
	encs := availableEncoders(encoderForce)
	if len(encs) == 0 {
		return nil, errors.New("no H.264 encoder available")
	}
	var lastErr error
	for _, enc := range encs {
		t, err := startTranscoder(layers, enc, onAU)
		if err == nil {
			t.Encoder = enc.factory
			return t, nil
		}
		lastErr = fmt.Errorf("%s: %w", enc.factory, err)
	}
	return nil, lastErr
}

func startTranscoder(ids []int, enc encoder, onAU func(layer int, au AccessUnit)) (*Transcoder, error) {
	all := LayersFor(Res2160, []int{1080, 720})
	branches := []string{"appsrc name=in is-live=true format=time do-timestamp=true " +
		"caps=video/x-h264,stream-format=byte-stream,alignment=au ! " +
		"h264parse ! decodebin ! videoconvert ! videorate ! video/x-raw,format=I420,framerate=30/1 ! tee name=t"}
	layers := make([]Layer, NumLayers)
	for _, id := range ids {
		l := all[id]
		layers[id] = l
		branches = append(branches, fmt.Sprintf("t. ! queue max-size-buffers=2 leaky=downstream ! "+
			"videoscale ! video/x-raw,width=%d,height=%d,pixel-aspect-ratio=1/1 ! %s ! "+
			"video/x-h264,profile=constrained-baseline ! "+
			"h264parse config-interval=-1 ! video/x-h264,stream-format=byte-stream,alignment=au ! "+
			"appsink name=out%d sync=false max-buffers=4 drop=true", l.Width, l.Height, enc.launch(id, l), id))
	}
	p, err := launch(strings.Join(branches, "  "))
	if err != nil {
		return nil, err
	}
	in, err := p.element("in")
	if err != nil {
		p.Stop()
		return nil, err
	}
	vs := &VideoSource{Pipeline: p, lastKey: make([]time.Time, NumLayers),
		sinks: make([]*gst.Element, NumLayers), encs: make([]*gst.Element, NumLayers)}
	for _, id := range ids {
		sink, err := p.element(fmt.Sprintf("out%d", id))
		if err != nil {
			p.Stop()
			return nil, err
		}
		e, err := p.element(fmt.Sprintf("enc%d", id))
		if err != nil {
			p.Stop()
			return nil, err
		}
		vs.sinks[id], vs.encs[id] = sink, e
		layer, frame := id, time.Second/30
		p.goLoop(func() bool {
			data, d, err := sink.Pull(100 * time.Millisecond)
			switch {
			case errors.Is(err, gst.ErrTimeout):
				return true
			case err != nil:
				return false
			}
			if d <= 0 || d > time.Second {
				d = frame
			}
			onAU(layer, AccessUnit{Data: data, Duration: d})
			return true
		})
	}
	if err := p.start(); err != nil {
		return nil, err
	}
	select {
	case err := <-p.Err:
		vs.Stop()
		return nil, err
	case <-time.After(300 * time.Millisecond):
	}
	return &Transcoder{VideoSource: vs, in: in}, nil
}

// Push hands one Annex-B access unit of the origin's top stream to the decoder.
func (t *Transcoder) Push(au []byte) {
	t.life.RLock()
	defer t.life.RUnlock()
	if t.stopped() {
		return
	}
	t.in.Push(au)
}
