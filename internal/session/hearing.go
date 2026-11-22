package session

import (
	"time"

	"blite/internal/media"
)

// What the People list shows of each peer's audio: whether it reaches us
// and whether it is sound rather than silence.
const (
	hearEvery = 200 * time.Millisecond
	// heardFor is how long audio counts as reaching us after its last frame.
	heardFor = 1500 * time.Millisecond
	// loudDB is the decoded peak (dBFS) above which they make sound: well
	// above Opus's comfort noise and a quiet room.
	loudDB = -45.0
)

type hearing struct {
	buffers int       // decoded buffers at the last look
	lastAt  time.Time // when the count last grew
	heard   bool
	loud    bool
}

// watchHearing samples each peer's audio player and pushes the UI when
// someone's audio starts, stops, or goes loud or quiet. Only the UI hears
// of it: tiles don't change.
func (s *Session) watchHearing() {
	t := time.NewTicker(hearEvery)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case now := <-t.C:
			if s.sampleHearing(now) {
				s.mu.Lock()
				f := s.onChange
				s.mu.Unlock()
				if f != nil {
					f()
				}
			}
		}
	}
}

// sampleHearing updates s.hear from the players; it reports a change.
func (s *Session) sampleHearing(now time.Time) bool {
	s.mu.Lock()
	players := make(map[string]*media.AudioPlayer, len(s.aplayers))
	for id, ap := range s.aplayers {
		players[id] = ap
	}
	prev := s.hear
	s.mu.Unlock()

	next := make(map[string]hearing, len(players))
	for id, ap := range players {
		h := prev[id] // only this goroutine writes s.hear
		if n := ap.Buffers(); n != h.buffers {
			h.buffers, h.lastAt = n, now
		}
		h.heard = !h.lastAt.IsZero() && now.Sub(h.lastAt) < heardFor
		l, ok := ap.Level()
		h.loud = h.heard && ok && l.PeakDB > loudDB
		next[id] = h
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	changed := len(next) != len(s.hear)
	for id, h := range next {
		if old, ok := s.hear[id]; !ok || old.heard != h.heard || old.loud != h.loud {
			changed = true
		}
		if _, ok := s.aplayers[id]; !ok { // stopped meanwhile
			delete(next, id)
			changed = true
		}
	}
	s.hear = next
	return changed
}
