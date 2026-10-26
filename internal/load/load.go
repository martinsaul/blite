// Package load measures how busy this process keeps the machine, so a
// provider can tell whether it has room for another encoder.
package load

import (
	"runtime"
	"sync"
	"time"
)

// Meter samples the process's CPU time. Busy is the share of all cores the
// process used over the last window (0..1).
type Meter struct {
	mu      sync.Mutex
	samples []sample
	window  time.Duration
}

type sample struct {
	at  time.Time
	cpu time.Duration
}

// NewMeter samples every second, keeping one window of history.
func NewMeter(window time.Duration) *Meter {
	m := &Meter{window: window}
	m.tick()
	go func() {
		for range time.Tick(time.Second) {
			m.tick()
		}
	}()
	return m
}

func (m *Meter) tick() {
	cpu, ok := processCPU()
	if !ok {
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.samples = append(m.samples, sample{now, cpu})
	for len(m.samples) > 2 && now.Sub(m.samples[1].at) >= m.window {
		m.samples = m.samples[1:]
	}
}

// Busy is the fraction of all cores used over the window; -1 if unknown.
func (m *Meter) Busy() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.samples) < 2 {
		return -1
	}
	a, b := m.samples[0], m.samples[len(m.samples)-1]
	wall := b.at.Sub(a.at)
	if wall <= 0 {
		return -1
	}
	return float64(b.cpu-a.cpu) / float64(wall) / float64(runtime.NumCPU())
}
