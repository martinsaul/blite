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

	mu       sync.Mutex
	q        [2][]pacedPkt
	queued   int  // bytes waiting
	inFlight bool // a popped packet is still being written by run()
	wake     chan struct{}
	done     <-chan struct{}
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

func (pc *pacer) setRate(kbps int) {
	var rate float64
	if kbps > 0 {
		rate = float64(kbps) * 1000 / 8 * 0.95
	}
	pc.mu.Lock()
	pc.rate = rate
	pc.burst = max(rate*0.02, 6*mtu)
	pc.mu.Unlock()
	select {
	case pc.wake <- struct{}{}:
	default:
	}
}

func (pc *pacer) rateParams() (rate, burst float64) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.rate, pc.burst
}

func (pc *pacer) send(class int, t *webrtc.TrackLocalStaticRTP, p *rtp.Packet) {
	size := p.MarshalSize()
	pc.mu.Lock()
	if pc.rate <= 0 && !pc.inFlight && len(pc.q[0]) == 0 && len(pc.q[1]) == 0 {
		pc.mu.Unlock()
		pc.write(t, p)
		return
	}
	if pc.rate > 0 && class == classHigh && float64(pc.queued) > pc.rate*maxQueue.Seconds() {
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
			pc.inFlight = true
			return x, true
		}
	}
	pc.inFlight = false
	return pacedPkt{}, false
}

func (pc *pacer) run() {
	_, tokens := pc.rateParams()
	last := time.Now()
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	defer timer.Stop()
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
		var rate float64
		for {
			var burst float64
			rate, burst = pc.rateParams()
			now := time.Now()
			if rate <= 0 {
				last = now
				tokens = 0
				break
			}
			tokens = min(burst, tokens+now.Sub(last).Seconds()*rate)
			last = now
			if tokens >= size {
				break
			}
			timer.Reset(time.Duration((size - tokens) / rate * float64(time.Second)))
			select {
			case <-pc.done:
				return
			case <-pc.wake:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-timer.C:
			}
		}
		if rate > 0 {
			tokens -= size
		}
		pc.write(x.t, x.p)
	}
}
