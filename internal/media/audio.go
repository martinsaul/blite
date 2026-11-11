package media

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"blite/internal/gst"
)

// Opus at 48 kHz stereo, 20 ms frames: the WebRTC default.
const (
	OpusBitrate = 128000
	OpusFrame   = 20 * time.Millisecond
)

// AudioSource encodes a GStreamer audio source description to Opus frames.
type AudioSource struct {
	*Pipeline
	live sync.Once    // counted out of liveAudio once
	loss atomic.Int32 // expected packet loss (%) for Opus in-band FEC
}

// SetPacketLoss tells the encoder the loss (%) to protect against with
// in-band FEC; the capture loop applies it.
func (a *AudioSource) SetPacketLoss(pct int) { a.loss.Store(int32(min(max(pct, 0), 100))) }

// liveAudio counts started, not yet stopped, audio sources (tests).
var liveAudio atomic.Int64

// LiveAudioSources reports how many audio sources run (tests).
func LiveAudioSources() int { return int(liveAudio.Load()) }

// Stop tears the source down.
func (a *AudioSource) Stop() {
	a.Pipeline.Stop()
	a.live.Do(func() { liveAudio.Add(-1) })
}

// TestAudio is a tone generator for synthetic peers and tests.
const TestAudio = "audiotestsrc is-live=true wave=sine freq=660 volume=0.3"

// StartAudioSource captures src (e.g. audio.Sharer.Capture()) and calls onFrame
// with each 20 ms Opus frame. onFrame must not block.
func StartAudioSource(src string, onFrame func(frame []byte, d time.Duration)) (*AudioSource, error) {
	if src == "" {
		return nil, errors.New("no audio source")
	}
	desc := fmt.Sprintf("%s ! queue max-size-time=100000000 leaky=downstream ! audioconvert ! audioresample ! "+
		"audio/x-raw,rate=48000,channels=2 ! opusenc name=aenc bitrate=%d frame-size=%d audio-type=generic inband-fec=true ! "+
		"appsink name=aout sync=false max-buffers=10 drop=true", src, OpusBitrate, OpusFrame.Milliseconds())
	p, err := launch(desc)
	if err != nil {
		return nil, err
	}
	sink, err := p.element("aout")
	if err != nil {
		p.Stop()
		return nil, err
	}
	enc, err := p.element("aenc")
	if err != nil {
		p.Stop()
		return nil, err
	}
	a := &AudioSource{Pipeline: p}
	applied := int32(0)
	p.goLoop(func() bool {
		if l := a.loss.Load(); l != applied {
			enc.SetUint("packet-loss-percentage", uint(l)) // a gint; same varargs
			applied = l
		}
		data, d, err := sink.Pull(100 * time.Millisecond)
		switch {
		case errors.Is(err, gst.ErrTimeout):
			return true
		case err != nil:
			return false
		}
		if d <= 0 || d > time.Second {
			d = OpusFrame
		}
		onFrame(data, d)
		return true
	})
	if err := p.start(); err != nil {
		return nil, err
	}
	liveAudio.Add(1)
	return a, nil
}

// AudioPlayer decodes one peer's Opus frames and plays them on a device.
type AudioPlayer struct {
	*Pipeline
	src     *gst.Element
	vol     *gst.Element
	life    sync.RWMutex
	mu      sync.Mutex
	samples int
}

// StartAudioPlayer plays on sink (a GStreamer sink description, e.g. from
// audio.PlaybackSink). sink == "count" decodes and counts frames (tests).
func StartAudioPlayer(sink string) (*AudioPlayer, error) {
	out := sink
	if sink == "count" {
		out = "appsink name=pcm sync=false max-buffers=4 drop=true"
	}
	desc := "appsrc name=ain is-live=true format=time do-timestamp=true " +
		"caps=audio/x-opus,rate=48000,channels=2,channel-mapping-family=0 ! " +
		"opusdec plc=true ! audioconvert ! volume name=vol ! audioresample ! queue max-size-time=200000000 leaky=downstream ! " + out
	p, err := launch(desc)
	if err != nil {
		return nil, err
	}
	src, err := p.element("ain")
	if err != nil {
		p.Stop()
		return nil, err
	}
	vol, err := p.element("vol")
	if err != nil {
		p.Stop()
		return nil, err
	}
	ap := &AudioPlayer{Pipeline: p, src: src, vol: vol}
	if sink == "count" {
		pcm, err := p.element("pcm")
		if err != nil {
			p.Stop()
			return nil, err
		}
		p.goLoop(func() bool {
			_, _, err := pcm.Pull(100 * time.Millisecond)
			switch {
			case errors.Is(err, gst.ErrTimeout):
				return true
			case err != nil:
				return false
			}
			ap.mu.Lock()
			ap.samples++
			ap.mu.Unlock()
			return true
		})
	}
	if err := p.start(); err != nil {
		return nil, err
	}
	return ap, nil
}

// Push hands one Opus frame to the decoder.
func (ap *AudioPlayer) Push(frame []byte) {
	ap.life.RLock()
	defer ap.life.RUnlock()
	if !ap.stopped() {
		ap.src.Push(frame)
	}
}

// SetVolume sets the playback gain (0 = mute, 1 = as sent).
func (ap *AudioPlayer) SetVolume(v float64) {
	ap.life.RLock()
	defer ap.life.RUnlock()
	if !ap.stopped() {
		ap.vol.SetDouble("volume", v)
	}
}

// Buffers returns decoded buffers so far (count sink only).
func (ap *AudioPlayer) Buffers() int {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	return ap.samples
}

// Stop tears the player down; concurrent Push calls finish first.
func (ap *AudioPlayer) Stop() {
	ap.life.Lock()
	defer ap.life.Unlock()
	ap.Pipeline.Stop()
}
