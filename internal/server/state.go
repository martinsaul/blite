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
	if s.cfg.StateFile == "" {
		return nil
	}
	data, err := os.ReadFile(s.cfg.StateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	n, err := s.rooms.Restore(data)
	if err != nil {
		return err
	}
	s.cfg.Log.Info("state loaded", "rooms", n)
	return nil
}

// SaveState writes the rooms to Config.StateFile (owner-only, replaced
// atomically).
func (s *Server) SaveState() error {
	if s.cfg.StateFile == "" {
		return nil
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
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
