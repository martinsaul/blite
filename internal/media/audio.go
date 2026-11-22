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
	live  sync.Once    // counted out of liveAudio once
	loss  atomic.Int32 // expected packet loss (%) for Opus in-band FEC
	mu    sync.Mutex
	meter *gst.Meter // the encoder's input (nil if unavailable or stopped)
}

// AudioLevel is the captured audio's loudness over a while, in dBFS
// (-96 is digital silence).
type AudioLevel struct {
	PeakDB float64 `json:"peakDb"`
	RMSDB  float64 `json:"rmsDb"`
}

// Level returns the level of the audio captured since the previous call;
// ok is false when none came.
func (a *AudioSource) Level() (l AudioLevel, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.meter == nil {
		return l, false
	}
	l.PeakDB, l.RMSDB, ok = a.meter.Take()
	return l, ok
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
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.meter != nil {
		a.meter.Free()
		a.meter = nil
	}
}

// TestAudio is the "Test tone" audio (and synthetic peers' and tests'):
// a 150 ms 880 Hz beep every second at -12 dBFS, easy to tell from silence,
// a stuck tone or captured sound.
const TestAudio = "audiotestsrc is-live=true wave=ticks freq=880 sine-periods-per-tick=132 " +
	"tick-interval=1000000000 apply-tick-ramp=true volume=0.25"

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
	a.meter, _ = enc.Meter()
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
	if err := p.start(); err != nil { // stopped the pipeline
		if a.meter != nil {
			a.meter.Free()
		}
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
	decoded *gst.Counter // buffers out of the decoder (nil if unavailable or stopped)
	meter   *gst.Meter   // the decoded audio, before our volume (nil if unavailable or stopped)
}

// StartAudioPlayer plays on sink (a GStreamer sink description, e.g. from
// audio.PlaybackSink). sink == "count" decodes into an appsink (tests).
func StartAudioPlayer(sink string) (*AudioPlayer, error) {
	out := sink
	if sink == "count" {
		out = "appsink name=pcm sync=false max-buffers=4 drop=true"
	}
	desc := "appsrc name=ain is-live=true format=time do-timestamp=true " +
		"caps=audio/x-opus,rate=48000,channels=2,channel-mapping-family=0 ! " +
		"opusdec name=adec plc=true ! audioconvert name=aconv ! volume name=vol ! audioresample ! queue max-size-time=200000000 leaky=downstream ! " + out
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
	if dec, err := p.element("adec"); err == nil {
		ap.decoded, _ = dec.CountBuffers()
	}
	if conv, err := p.element("aconv"); err == nil {
		ap.meter, _ = conv.Meter() // opusdec hands it S16
	}
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
			return true
		})
	}
	if err := p.start(); err != nil { // stopped the pipeline
		if ap.decoded != nil {
			ap.decoded.Free()
		}
		if ap.meter != nil {
			ap.meter.Free()
		}
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

// Buffers returns the buffers decoded so far, whatever the sink.
func (ap *AudioPlayer) Buffers() int {
	ap.life.RLock()
	defer ap.life.RUnlock()
	if ap.decoded == nil {
		return 0
	}
	return ap.decoded.Load()
}

// Level returns the level of the audio decoded since the previous call,
// whatever our volume; ok is false when none came.
func (ap *AudioPlayer) Level() (l AudioLevel, ok bool) {
	ap.life.RLock()
	defer ap.life.RUnlock()
	if ap.meter == nil {
		return l, false
	}
	l.PeakDB, l.RMSDB, ok = ap.meter.Take()
	return l, ok
}

// Stop tears the player down; concurrent Push calls finish first.
func (ap *AudioPlayer) Stop() {
	ap.life.Lock()
	defer ap.life.Unlock()
	ap.Pipeline.Stop()
	if ap.decoded != nil {
		ap.decoded.Free()
		ap.decoded = nil
	}
	if ap.meter != nil {
		ap.meter.Free()
		ap.meter = nil
	}
}
