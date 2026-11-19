package rtc

import (
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// The pacer holds its rate, sends LOW queued behind HIGH first, and lets
// HIGH through unpaced once it has queued maxQueue.
func TestPacer(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	pc := makePacer(1000, done) // 1 Mbit/s: 1200-byte packets about 10 ms apart
	var mu sync.Mutex
	var got []uint16
	var at []time.Time
	pc.write = func(_ *webrtc.TrackLocalStaticRTP, p *rtp.Packet) {
		mu.Lock()
		got, at = append(got, p.SequenceNumber), append(at, time.Now())
		mu.Unlock()
	}
	pkt := func(seq uint16) *rtp.Packet {
		return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq}, Payload: make([]byte, 1188)}
	}
	for i := range 20 {
		pc.send(classHigh, nil, pkt(uint16(i)))
	}
	pc.send(classLow, nil, pkt(1000))
	start := time.Now()
	go pc.run()
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 21 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 21 || got[0] != 1000 {
		t.Fatalf("order: %v", got)
	}
	// 21 packets of 1200 bytes at 0.95 Mbit/s, less the initial burst:
	// about 160 ms.
	if d := at[20].Sub(start); d < 120*time.Millisecond || d > 400*time.Millisecond {
		t.Fatalf("21 packets took %v", d)
	}
	// A full queue lets HIGH through at once.
	got = got[:0]
	pc.mu.Lock()
	pc.queued = int(pc.rate) // 1 s queued
	pc.mu.Unlock()
	mu.Unlock()
	pc.send(classHigh, nil, pkt(7))
	mu.Lock()
	if len(got) != 1 || got[0] != 7 {
		t.Fatalf("bypass: %v", got)
	}
}

func TestPacerSetRateRace(t *testing.T) {
	pkt := &rtp.Packet{Header: rtp.Header{SequenceNumber: 1}, Payload: make([]byte, 200)}
	for range 20 {
		done := make(chan struct{})
		pc := makePacer(2000, done)
		pc.write = func(*webrtc.TrackLocalStaticRTP, *rtp.Packet) {}
		go pc.run()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for k := 500; k <= 5000; k += 250 {
				pc.setRate(k)
			}
			pc.setRate(0)
		}()
		go func() {
			defer wg.Done()
			for range 30 {
				pc.send(classHigh, nil, pkt)
			}
		}()
		wg.Wait()
		close(done)
	}
}

func TestPacerSwitchToAutomaticPreservesOrder(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	pc := makePacer(1000, done) // 25 * 1200 B = 30 KB < maxQueue (59 KB)
	var mu sync.Mutex
	var got []uint16
	inWrite := make(chan struct{}, 1)
	pc.write = func(_ *webrtc.TrackLocalStaticRTP, p *rtp.Packet) {
		if p.SequenceNumber == 2 {
			select {
			case inWrite <- struct{}{}:
			default:
			}
			time.Sleep(10 * time.Millisecond)
		}
		mu.Lock()
		got = append(got, p.SequenceNumber)
		mu.Unlock()
	}
	pkt := func(seq uint16) *rtp.Packet {
		return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq}, Payload: make([]byte, 1188)}
	}
	for i := range 25 {
		pc.send(classHigh, nil, pkt(uint16(i)))
	}
	go pc.run()
	<-inWrite

	// Switch from manual rate to Automatic (0) while packets 2..24 are still in flight/queued,
	// then immediately send packets 25..49: none may overtake 2..24.
	pc.setRate(0)
	for i := 25; i < 50; i++ {
		pc.send(classHigh, nil, pkt(uint16(i)))
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 50 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 50 {
		t.Fatalf("got %d packets, want 50: %v", len(got), got)
	}
	for i, seq := range got {
		if seq != uint16(i) {
			t.Fatalf("packet %d out of order: got seq %d in %v", i, seq, got)
		}
	}
}
