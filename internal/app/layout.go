package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"blite/internal/session"
)

// layoutFile keeps the viewing layout across restarts.
func layoutFile() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "blite", "layout.json"), nil
}

func loadLayout() session.Layout {
	def := session.Layout{Mode: session.LayoutWindows}
	f, err := layoutFile()
	if err != nil {
		return def
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return def
	}
	var l session.Layout
	if json.Unmarshal(b, &l) != nil || !l.Valid() {
		return def
	}
	return l
}

func saveLayout(l session.Layout) error {
	f, err := layoutFile()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(l)
	return os.WriteFile(f, b, 0o600)
}

// SetLayout implements ui.Backend: separate windows, or one window with the
// streams in a grid (rows x cols, 0 = auto). Kept for later sessions.
func (a *App) SetLayout(mode string, rows, cols int) error {
	l := session.Layout{Mode: mode, Rows: rows, Cols: cols}
	if !l.Valid() {
		return errors.New("unknown layout")
	}
	if err := saveLayout(l); err != nil {
		a.log.Warn("save layout", "err", err)
	}
	a.mu.Lock()
	a.layout = l
	s := a.sess
	a.mu.Unlock()
	if s != nil {
		if err := s.SetLayout(l); err != nil {
			return err
		}
	}
	a.bc.Notify()
	return nil
}
