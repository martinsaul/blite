package server

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// LoadState loads the rooms saved in Config.StateFile, if there is one.
// Rooms this server has served since its last load are kept as they are
// (see room.Manager.Restore). A missing file is not an error.
func (s *Server) LoadState() error {
	_, err := s.loadState()
	return err
}

// loadState is LoadState, returning the number of rooms loaded.
func (s *Server) loadState() (int, error) {
	if s.cfg.StateFile == "" {
		return 0, nil
	}
	data, err := os.ReadFile(s.cfg.StateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, problems, err := s.rooms.RestoreReport(data)
	if err != nil {
		return 0, err
	}
	for _, p := range problems {
		s.cfg.Log.Warn("state: bad room", "problem", p)
	}
	s.cfg.Log.Info("state loaded", "rooms", n, "problems", len(problems))
	return n, nil
}

// SaveState writes the rooms to Config.StateFile (owner-only, replaced
// atomically). While deferred (until Adopt) or drained it writes nothing
// (bluegreen.go).
func (s *Server) SaveState() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	if !s.writes() {
		return nil
	}
	return s.saveLocked()
}

// saveLocked is SaveState under saveMu, whatever the mode.
func (s *Server) saveLocked() error {
	if s.cfg.StateFile == "" {
		return nil
	}
	data, err := s.rooms.Snapshot()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.cfg.StateFile), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.cfg.StateFile)
}
