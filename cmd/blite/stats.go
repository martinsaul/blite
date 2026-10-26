package main

import (
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"blite/internal/gst"
	"blite/internal/session"
)

// sampler turns cumulative per-member counters into per-interval rates for the
// headless stats log (one record per peer per tick, easy to aggregate).
type sampler struct {
	last    map[string]session.Member
	freezes map[string]int // intervals with no decoded frames after the first one
	at      time.Time
	cpu     time.Duration
}

func newSampler() *sampler {
	return &sampler{last: map[string]session.Member{}, freezes: map[string]int{}, at: time.Now(), cpu: cpuTime()}
}

func (s *sampler) log(log *slog.Logger, st session.State) {
	now, cpu := time.Now(), cpuTime()
	dt := now.Sub(s.at).Seconds()
	if dt <= 0 {
		return
	}
	cIn, cFree := gst.MallocStats()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	log.Info("self", "cpu", round1(100*(cpu-s.cpu).Seconds()/dt), "encoder", st.Encoder, "lowKbps", st.LowKbps, "peers", len(st.Members),
		"rssMB", rssMB(), "goHeapMB", round1(float64(ms.HeapAlloc)/1e6), "goSysMB", round1(float64(ms.Sys)/1e6),
		"goroutines", runtime.NumGoroutine(), "cInuseMB", round1(float64(cIn)/1e6), "cFreeMB", round1(float64(cFree)/1e6))
	for _, m := range st.Members {
		p, seen := s.last[m.ID]
		fps := float64(m.Frames-p.Frames) / dt
		if seen && p.Frames > 0 && m.Frames == p.Frames {
			s.freezes[m.ID]++
		}
		log.Info("peer", "peer", m.Name, "state", m.State, "pair", m.Pair, "rttMs", m.RTTms,
			"upKbps", kbps(m.Tx-p.Tx, dt), "downKbps", kbps(m.Rx-p.Rx, dt), "fps", round1(fps),
			"audio", m.Audio-p.Audio, "sendLayer", m.Layer, "loss", round1(100*m.Loss), "freezes", s.freezes[m.ID], "recv", m.Recv)
		s.last[m.ID] = m
	}
	s.at, s.cpu = now, cpu
}

func kbps(b uint64, dt float64) float64 {
	if int64(b) < 0 { // counters reset (ICE restart / reconnect)
		return 0
	}
	return round1(float64(b) * 8 / 1000 / dt)
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// rssMB is our resident memory (Linux; 0 elsewhere), Go and C alike.
func rssMB() float64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0
	}
	pages, _ := strconv.ParseFloat(f[1], 64)
	return round1(pages * float64(os.Getpagesize()) / 1e6)
}
