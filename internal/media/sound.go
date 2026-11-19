package media

import (
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"blite/internal/gst"
)

// SoundKind identifies one of the four call notification chimes.
type SoundKind int

const (
	SoundPeerJoined     SoundKind = iota // someone joins the room
	SoundPeerSharing                     // someone else starts sharing
	SoundSelfLive                        // your own stream goes live
	SoundViewerWatching                  // someone starts watching your stream
	numSoundKinds
)

// AllSounds lists all four call notification sounds.
var AllSounds = []SoundKind{
	SoundPeerJoined,
	SoundPeerSharing,
	SoundSelfLive,
	SoundViewerWatching,
}

func (k SoundKind) String() string {
	switch k {
	case SoundPeerJoined:
		return "peer-joined"
	case SoundPeerSharing:
		return "peer-sharing"
	case SoundSelfLive:
		return "self-live"
	case SoundViewerWatching:
		return "viewer-watching"
	default:
		return "unknown"
	}
}

const (
	SoundRate     = 48000
	SoundChannels = 2
	SoundPeakDBFS = -18.0
	SoundDebounce = 2 * time.Second

	soundChunkSamples = SoundRate / 50                        // 960 frames per 20 ms
	soundChunkBytes   = soundChunkSamples * SoundChannels * 2 // 3840 bytes (S16LE stereo)
)

type chimeNote struct {
	startMs  float64
	durMs    float64
	freqHz   float64
	triangle float64 // 0 = soft sine + octave warmth, 1 = band-limited triangle
	decay    float64 // exponential decay rate (1/s)
}

var chimeRecipes = [numSoundKinds]struct {
	totalMs float64
	notes   []chimeNote
}{
	// 1. Someone joins the room: warm soft-sine ascending fourth (G4 -> C5), 220 ms.
	SoundPeerJoined: {
		totalMs: 220,
		notes: []chimeNote{
			{startMs: 0, durMs: 115, freqHz: 392.00, triangle: 0.15, decay: 11.0},
			{startMs: 92, durMs: 128, freqHz: 523.25, triangle: 0.15, decay: 10.0},
		},
	},
	// 2. Someone else starts sharing: soft triangle-sine rising major triad (C5 -> E5 -> G5), 300 ms.
	SoundPeerSharing: {
		totalMs: 300,
		notes: []chimeNote{
			{startMs: 0, durMs: 105, freqHz: 523.25, triangle: 0.65, decay: 12.0},
			{startMs: 85, durMs: 105, freqHz: 659.25, triangle: 0.65, decay: 12.0},
			{startMs: 170, durMs: 130, freqHz: 783.99, triangle: 0.55, decay: 9.5},
		},
	},
	// 3. Your own stream goes live: confident soft-sine fifth chime (D5 -> A5), 260 ms.
	SoundSelfLive: {
		totalMs: 260,
		notes: []chimeNote{
			{startMs: 0, durMs: 115, freqHz: 587.33, triangle: 0.0, decay: 10.5},
			{startMs: 95, durMs: 165, freqHz: 880.00, triangle: 0.0, decay: 8.5},
		},
	},
	// 4. Someone starts watching your stream: delicate quiet triangle grace-note bell (E5 -> B5), 180 ms.
	SoundViewerWatching: {
		totalMs: 180,
		notes: []chimeNote{
			{startMs: 0, durMs: 70, freqHz: 659.25, triangle: 0.85, decay: 15.0},
			{startMs: 52, durMs: 128, freqHz: 987.77, triangle: 0.80, decay: 13.5},
		},
	},
}

var (
	synthOnce sync.Once
	synthInt  [numSoundKinds][]int16 // interleaved stereo S16LE samples
	synthPCM  [numSoundKinds][]byte  // little-endian S16LE stereo bytes
)

func ensureSynthesized() {
	synthOnce.Do(func() {
		targetPeak := math.Pow(10.0, SoundPeakDBFS/20.0)
		for k := SoundKind(0); k < numSoundKinds; k++ {
			rec := chimeRecipes[k]
			frames := int(math.Round(rec.totalMs * float64(SoundRate) / 1000.0))
			mono := make([]float64, frames)
			for _, n := range rec.notes {
				s0 := int(math.Round(n.startMs * float64(SoundRate) / 1000.0))
				nFrames := int(math.Round(n.durMs * float64(SoundRate) / 1000.0))
				attack := max(int(math.Round(0.010*float64(SoundRate))), 1)
				release := max(int(math.Round(0.018*float64(SoundRate))), 1)
				if attack+release > nFrames {
					attack = nFrames / 3
					release = nFrames / 3
				}
				for i := 0; i < nFrames && s0+i < frames; i++ {
					t := float64(i) / float64(SoundRate)
					w := 2.0 * math.Pi * n.freqHz * t

					// Soft sine with subtle octave warmth vs band-limited 3-harmonic triangle.
					sine := 0.90*math.Sin(w) + 0.10*math.Sin(2.0*w)
					tri := (8.0 / (math.Pi * math.Pi)) * (math.Sin(w) - math.Sin(3.0*w)/9.0 + math.Sin(5.0*w)/25.0)
					sample := (1.0-n.triangle)*sine + n.triangle*tri

					// Click-free raised-cosine attack & release + natural bell decay.
					env := math.Exp(-n.decay * t)
					if i < attack {
						env *= 0.5 * (1.0 - math.Cos(math.Pi*float64(i)/float64(attack)))
					}
					if rem := nFrames - 1 - i; rem < release {
						env *= 0.5 * (1.0 - math.Cos(math.Pi*float64(rem)/float64(release)))
					}
					mono[s0+i] += sample * env
				}
			}
			// Guarantee exact zero first/last frame and normalize peak to -18 dBFS.
			if frames > 0 {
				mono[0] = 0
				mono[frames-1] = 0
			}
			maxAbs := 0.0
			for _, v := range mono {
				if a := math.Abs(v); a > maxAbs {
					maxAbs = a
				}
			}
			scale := 0.0
			if maxAbs > 0 {
				scale = targetPeak / maxAbs
			}
			samples := make([]int16, frames*SoundChannels)
			pcm := make([]byte, frames*SoundChannels*2)
			for i, v := range mono {
				s := int16(math.Round(min(max(v*scale, -1.0), 1.0) * 32767.0))
				samples[i*2] = s
				samples[i*2+1] = s
				binary.LittleEndian.PutUint16(pcm[i*4:i*4+2], uint16(s))
				binary.LittleEndian.PutUint16(pcm[i*4+2:i*4+4], uint16(s))
			}
			synthInt[k] = samples
			synthPCM[k] = pcm
		}
	})
}

// SynthesizeSound returns a copy of k's interleaved stereo S16LE samples at 48 kHz.
func SynthesizeSound(k SoundKind) []int16 {
	if k < 0 || k >= numSoundKinds {
		return nil
	}
	ensureSynthesized()
	out := make([]int16, len(synthInt[k]))
	copy(out, synthInt[k])
	return out
}

// SoundPCM returns k's raw S16LE interleaved stereo PCM bytes at 48 kHz.
func SoundPCM(k SoundKind) []byte {
	if k < 0 || k >= numSoundKinds {
		return nil
	}
	ensureSynthesized()
	out := make([]byte, len(synthPCM[k]))
	copy(out, synthPCM[k])
	return out
}

// SoundWAV returns a complete 48 kHz stereo 16-bit RIFF/WAVE file for k.
func SoundWAV(k SoundKind) []byte {
	if k < 0 || k >= numSoundKinds {
		return nil
	}
	ensureSynthesized()
	pcm := synthPCM[k]
	out := make([]byte, 44+len(pcm))
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+len(pcm)))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16) // PCM fmt chunk size
	binary.LittleEndian.PutUint16(out[20:22], 1)  // PCM format
	binary.LittleEndian.PutUint16(out[22:24], SoundChannels)
	binary.LittleEndian.PutUint32(out[24:28], SoundRate)
	binary.LittleEndian.PutUint32(out[28:32], SoundRate*SoundChannels*2)
	binary.LittleEndian.PutUint16(out[32:34], SoundChannels*2)
	binary.LittleEndian.PutUint16(out[34:36], 16)
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(len(pcm)))
	copy(out[44:], pcm)
	return out
}

// SoundGate coalesces bursts of the same SoundKind within SoundDebounce (2 s)
// and records played chimes for tests.
type SoundGate struct {
	mu       sync.Mutex
	debounce time.Duration
	last     [numSoundKinds]time.Time
	played   []SoundKind
}

// SetDebounce overrides SoundDebounce for g (tests; <= 0 uses SoundDebounce).
func (g *SoundGate) SetDebounce(d time.Duration) {
	g.mu.Lock()
	g.debounce = d
	g.mu.Unlock()
}

// Allow reports whether k should play now, enforcing SoundDebounce per kind.
func (g *SoundGate) Allow(k SoundKind) bool {
	return g.AllowAt(k, time.Now())
}

// AllowAt is Allow using an explicit timestamp (tests).
func (g *SoundGate) AllowAt(k SoundKind, now time.Time) bool {
	if k < 0 || k >= numSoundKinds {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	win := g.debounce
	if win <= 0 {
		win = SoundDebounce
	}
	if !g.last[k].IsZero() && now.Sub(g.last[k]) < win {
		return false
	}
	g.last[k] = now
	g.played = append(g.played, k)
	return true
}

// Played returns all sounds accepted by Allow in chronological order.
func (g *SoundGate) Played() []SoundKind {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]SoundKind, len(g.played))
	copy(out, g.played)
	return out
}

// Count returns how many times k was accepted by Allow.
func (g *SoundGate) Count(k SoundKind) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, p := range g.played {
		if p == k {
			n++
		}
	}
	return n
}

// ClearKind clears k's debounce timestamp without erasing Played history, so
// stopping sharing or failing a source switch lets the next share chime again.
func (g *SoundGate) ClearKind(k SoundKind) {
	if k < 0 || k >= numSoundKinds {
		return
	}
	g.mu.Lock()
	g.last[k] = time.Time{}
	g.mu.Unlock()
}

// Reset clears the gate's history and debounce timestamps (tests).
func (g *SoundGate) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.last = [numSoundKinds]time.Time{}
	g.played = nil
}

// liveSounds counts started, not yet stopped, sound players (tests).
var liveSounds atomic.Int64

// LiveSoundPlayers reports how many SoundPlayers are currently running (tests).
func LiveSoundPlayers() int { return int(liveSounds.Load()) }

const maxQueuedSounds = 4

// SoundPlayer plays synthesized chimes through GStreamer onto the call's
// output sink (e.g. audio.PlaybackSink).
type SoundPlayer struct {
	*Pipeline
	src       *gst.Element
	delivered *gst.Counter
	live      sync.Once
	paced     bool

	mu           sync.Mutex
	queue        []SoundKind
	wake         chan struct{}
	stoppedCount int
}

var silentChunk = make([]byte, soundChunkBytes)

// StartSoundPlayer launches a GStreamer playback pipeline on sink (the same
// sink description StartAudioPlayer takes; sink == "count" captures and counts
// buffers for headless tests).
func StartSoundPlayer(sink string) (*SoundPlayer, error) {
	if sink == "" {
		return nil, errors.New("no audio sink")
	}
	ensureSynthesized()
	var desc string
	if sink == "count" {
		desc = "appsrc name=sin format=time " +
			"caps=audio/x-raw,format=S16LE,layout=interleaved,rate=48000,channels=2 ! " +
			"identity name=sdelivered ! fakesink sync=false"
	} else {
		desc = "appsrc name=sin is-live=true format=time do-timestamp=true " +
			"caps=audio/x-raw,format=S16LE,layout=interleaved,rate=48000,channels=2 ! " +
			"audioconvert ! audioresample ! queue max-size-time=500000000 leaky=downstream ! " +
			"identity name=sdelivered ! " + sink
	}
	p, err := launch(desc)
	if err != nil {
		return nil, err
	}
	src, err := p.element("sin")
	if err != nil {
		p.Stop()
		return nil, err
	}
	sp := &SoundPlayer{
		Pipeline: p,
		src:      src,
		paced:    sink != "count",
		wake:     make(chan struct{}, 1),
	}
	if del, err := p.element("sdelivered"); err == nil {
		sp.delivered, _ = del.CountBuffers()
	}
	p.goLoop(sp.feedStep)
	if err := p.start(); err != nil {
		if sp.delivered != nil {
			sp.delivered.Free()
			sp.delivered = nil
		}
		return nil, err
	}
	liveSounds.Add(1)
	return sp, nil
}

// Play enqueues k for playback on the player's output sink.
func (sp *SoundPlayer) Play(k SoundKind) {
	if k < 0 || k >= numSoundKinds || sp.stopped() {
		return
	}
	sp.mu.Lock()
	if len(sp.queue) < maxQueuedSounds {
		sp.queue = append(sp.queue, k)
	}
	sp.mu.Unlock()
	select {
	case sp.wake <- struct{}{}:
	default:
	}
}

func (sp *SoundPlayer) popQueue() (SoundKind, bool) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if len(sp.queue) == 0 {
		return 0, false
	}
	k := sp.queue[0]
	sp.queue = sp.queue[1:]
	return k, true
}

func (sp *SoundPlayer) feedStep() bool {
	k, ok := sp.popQueue()
	if !ok {
		select {
		case <-sp.done:
			return false
		case <-sp.wake:
			k, ok = sp.popQueue()
			if !ok {
				return true
			}
		}
	}

	var tick *time.Ticker
	if sp.paced {
		tick = time.NewTicker(OpusFrame)
		defer tick.Stop()
	}
	pushChunk := func(buf []byte) bool {
		if sp.stopped() {
			return false
		}
		sp.src.PushDur(buf, OpusFrame)
		if tick != nil {
			select {
			case <-sp.done:
				return false
			case <-tick.C:
			}
		}
		return true
	}

	// Prepend 2 silent 20 ms frames when waking from idle so sink startup /
	// clock lock never clips the chime's soft attack.
	if !pushChunk(silentChunk) || !pushChunk(silentChunk) {
		return false
	}

	for {
		pcm := synthPCM[k]
		for off := 0; off < len(pcm); off += soundChunkBytes {
			end := min(off+soundChunkBytes, len(pcm))
			chunk := pcm[off:end]
			if len(chunk) < soundChunkBytes {
				padded := make([]byte, soundChunkBytes)
				copy(padded, chunk)
				chunk = padded
			}
			if !pushChunk(chunk) {
				return false
			}
		}
		// 2 silent 20 ms frames after each chime: clean tail + brief breath
		// before any queued chime that follows.
		if !pushChunk(silentChunk) || !pushChunk(silentChunk) {
			return false
		}
		var more bool
		k, more = sp.popQueue()
		if !more {
			return true
		}
	}
}

// Buffers returns the number of 20 ms buffers delivered to the sink.
func (sp *SoundPlayer) Buffers() int {
	return sp.Delivered()
}

// Delivered returns the number of 20 ms buffers pushed into the output sink.
func (sp *SoundPlayer) Delivered() int {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.delivered != nil {
		return sp.delivered.Load()
	}
	return sp.stoppedCount
}

// Stop tears down the GStreamer sound pipeline cleanly.
func (sp *SoundPlayer) Stop() {
	sp.Pipeline.Stop()
	sp.mu.Lock()
	if sp.delivered != nil {
		sp.stoppedCount = sp.delivered.Load()
		sp.delivered.Free()
		sp.delivered = nil
	}
	sp.mu.Unlock()
	sp.live.Do(func() { liveSounds.Add(-1) })
}
