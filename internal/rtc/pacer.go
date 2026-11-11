package rtc

import (
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// pacer (relay) spreads the video we forward over time at our upload rate,
// so a keyframe sent to every viewer at once doesn't hit the uplink in one
// burst, which every viewer's layerPolicy would read as congestion. LOW
// goes before HIGH; within a class packets leave in arrival order, which
// fanVideo already interleaves across viewers. Audio isn't paced.
//
// It runs only when we know our upload (Options.UploadKbps): paced at a
// guess it would add latency on a fast uplink and not help a slow one.
type pacer struct {
	rate  float64 // bytes per second
	burst float64 // bucket depth, bytes
	write func(*webrtc.TrackLocalStaticRTP, *rtp.Packet)

	mu     sync.Mutex
	q      [2][]pacedPkt
	queued int // bytes waiting
	wake   chan struct{}
	done   <-chan struct{}
}

type pacedPkt struct {
	t *webrtc.TrackLocalStaticRTP
	p *rtp.Packet
}

const (
	classLow  = 0
	classHigh = 1
	// maxQueue: HIGH beyond this much queueing goes out unpaced; holding
	// it longer would only add latency the viewers see anyway.
	maxQueue = 500 * time.Millisecond
)

func newPacer(kbps int, done <-chan struct{}) *pacer {
	pc := makePacer(kbps, done)
	go pc.run()
	return pc
}

func makePacer(kbps int, done <-chan struct{}) *pacer {
	rate := float64(kbps) * 1000 / 8 * 0.95
	pc := &pacer{rate: rate, burst: max(rate*0.02, 6*mtu), wake: make(chan struct{}, 1), done: done,
		write: func(t *webrtc.TrackLocalStaticRTP, p *rtp.Packet) { t.WriteRTP(p) }}
	return pc
}

func (pc *pacer) send(class int, t *webrtc.TrackLocalStaticRTP, p *rtp.Packet) {
	size := p.MarshalSize()
	pc.mu.Lock()
	if class == classHigh && float64(pc.queued) > pc.rate*maxQueue.Seconds() {
		pc.mu.Unlock()
		pc.write(t, p)
		return
	}
	pc.q[class] = append(pc.q[class], pacedPkt{t, p})
	pc.queued += size
	pc.mu.Unlock()
	select {
	case pc.wake <- struct{}{}:
	default:
	}
}

// next takes the next packet to send (ok false if none).
func (pc *pacer) next() (pacedPkt, bool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	for c := range pc.q {
		if len(pc.q[c]) > 0 {
			x := pc.q[c][0]
			pc.q[c][0] = pacedPkt{}
			pc.q[c] = pc.q[c][1:]
			pc.queued -= x.p.MarshalSize()
			return x, true
		}
	}
	return pacedPkt{}, false
}

func (pc *pacer) run() {
	tokens := pc.burst
	last := time.Now()
	for {
		x, ok := pc.next()
		if !ok {
			select {
			case <-pc.done:
				return
			case <-pc.wake:
				continue
			}
		}
		size := float64(x.p.MarshalSize())
		for {
			now := time.Now()
			tokens = min(pc.burst, tokens+now.Sub(last).Seconds()*pc.rate)
			last = now
			if tokens >= size {
				break
			}
			time.Sleep(time.Duration((size - tokens) / pc.rate * float64(time.Second)))
		}
		tokens -= size
		pc.write(x.t, x.p)
	}
}
