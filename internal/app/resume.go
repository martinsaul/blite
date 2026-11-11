package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"blite/internal/session"
)

// An in-call update writes the call to resume.json just before
// Blite exits for molt-swap; the build that starts next (the new one, or
// the old one after a rollback) rejoins from it. It holds the room
// password, so it is 0600, short-lived, and removed once used.

// ResumeWindow is how long a resume file stays good.
const ResumeWindow = 2 * time.Minute

type resumeFile struct {
	session.Rejoin
	Until time.Time `json:"until"`
}

// resumePath is overridden by tests.
var resumePath = func() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "blite", "resume.json"), nil
}

func writeResume(r session.Rejoin, now time.Time) error {
	f, err := resumePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(resumeFile{Rejoin: r, Until: now.Add(ResumeWindow)})
	tmp := f + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f)
}

// readResume returns a pending, unexpired resume. An expired or broken
// file is removed.
func readResume(now time.Time) (session.Rejoin, bool) {
	f, err := resumePath()
	if err != nil {
		return session.Rejoin{}, false
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return session.Rejoin{}, false
	}
	var r resumeFile
	if json.Unmarshal(b, &r) != nil || r.Room == "" || now.After(r.Until) {
		os.Remove(f)
		return session.Rejoin{}, false
	}
	return r.Rejoin, true
}

func removeResume() {
	if f, err := resumePath(); err == nil {
		os.Remove(f)
	}
}

// Resume rejoins the call an in-call update left, if there is one, and
// shares what we shared then. ok is false when there was nothing to resume.
func (a *App) Resume(ctx context.Context) (ok bool, err error) {
	r, ok := readResume(time.Now())
	if !ok {
		return false, nil
	}
	if a.busy() {
		return true, ErrInSession
	}
	a.log.Info("rejoining the call after an update", "source", r.Source, "audio", r.Audio.Mode)
	if r.Resolution != "" || r.Mode != "" {
		a.mu.Lock()
		if r.Resolution != "" {
			a.res = r.Resolution
		}
		if r.Mode != "" {
			a.mode = r.Mode
		}
		a.mu.Unlock()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	s, err := session.Join(ctx, a.config(), r.Room, r.Password)
	// Keep the file until a join worked or clearly can't: a build that
	// crashes before this point is rolled back, and the old one rejoins.
	removeResume()
	if err != nil {
		err = friendly(err)
		a.setErr("could not rejoin the call after the update: " + err.Error())
		return true, err
	}
	a.adoptAs(s, r.Source)
	var errs []error
	if r.Audio.Mode != "" && r.Audio.Mode != "none" {
		if err := s.SetAudio(r.Audio); err != nil {
			errs = append(errs, errors.New("audio: "+err.Error()))
		}
	}
	if r.Output != "" {
		s.SetOutput(r.Output)
	}
	if err := errors.Join(errs...); err != nil {
		a.setErr("rejoined, but could not restore " + err.Error())
	}
	return true, nil
}
