package session

import (
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"blite/internal/media"
	"blite/internal/metrics"
)

// shareMeter is what the per-second readings take from the running share.
type shareMeter struct {
	src     *media.VideoSource
	encoded *atomic.Int64 // HIGH access units
	maxSend *atomic.Int64 // the slowest send since the last reading
}

// recordMetrics adds a reading to cfg.Metrics every second while we share
// or watch, and has the recorder write when either stops or we leave.
func (s *Session) recordMetrics() {
	rec := s.cfg.Metrics
	t := time.NewTicker(time.Second)
	defer t.Stop()
	defer rec.Flush()
	var (
		meter   *shareMeter
		last    media.FrameCounts
		lastEnc int64
		lastTx  = map[string]uint64{}
		lastAt  time.Time
		active  bool
	)
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		now := time.Now()
		dt := now.Sub(lastAt).Seconds()
		lastAt = now
		s.mu.Lock()
		src, res, preview, fps := s.src, orRes(s.res), s.previewOn, s.highFPSLocked()
		s.mu.Unlock()
		m := s.meter.Load()
		if m != nil && m.src != src {
			m = nil
		}
		windows := int(s.nGrids.Load())
		if m == nil && windows == 0 {
			if active {
				go rec.Flush()
			}
			active, meter = false, nil
			continue
		}
		active = true
		r := metrics.Record{TimeMs: now.UnixMilli(), Captured: metrics.Unknown, Offered: metrics.Unknown,
			QueueIn: metrics.Unknown, QueueOut: metrics.Unknown, Encoded: metrics.Unknown,
			VideoWindows: uint8(min(windows, 255)), LowKbps: metrics.Clamp16(s.mesh.LowKbps())}
		r.CPUBusy = uint8(min(100, 100*cpuMeter.Busy()+0.5))
		if m != nil {
			c, enc := m.src.Counts(), m.encoded.Load()
			if m == meter { // a second since the last reading of this share
				d := func(a, b int) uint16 { return metrics.Clamp16(countDiff(a, b)) }
				r.Captured, r.Offered = d(last.Captured, c.Captured), d(last.Offered, c.Offered)
				r.QueueIn, r.QueueOut = d(last.HighIn, c.HighIn), d(last.HighOut, c.HighOut)
				r.Encoded = d(int(lastEnc), int(enc))
			}
			meter, last, lastEnc = m, c, enc
			r.MaxSendUs = uint32(min(m.maxSend.Swap(0)/int64(time.Microsecond), 1<<32-1))
			r.HighFPS = uint8(min(fps, 255))
			h, _ := strconv.Atoi(strings.TrimSuffix(string(res), "p"))
			r.Height = metrics.Clamp16(h)
			r.Flags |= metrics.FlagSharing
			if preview {
				r.Flags |= metrics.FlagPreview
			}
			if m.src.Idle() {
				r.Flags |= metrics.FlagIdle
			}
		} else if meter != nil {
			go rec.Flush() // the share stopped
			meter = nil
		}
		stats := s.mesh.Stats()
		ids := make([]string, 0, len(stats))
		for id := range stats {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		up, n := 0, 0
		tx := make(map[string]uint64, len(ids))
		for _, id := range ids {
			b := stats[id].TxBytes
			tx[id] = b
			p, ok := lastTx[id]
			if !ok || b < p || dt <= 0 {
				continue
			}
			kbps := int(float64(b-p) * 8 / 1000 / dt)
			up += kbps
			r.SetPeer(n, kbps)
			n++
		}
		lastTx = tx
		r.UpKbps, r.Peers = uint32(up), uint8(min(len(ids), 255))
		rec.Add(r)
	}
}
