package server

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Blue/green deploys. Two instances of this server ("blue",
// the live one, and "green", the new one) share one StateFile. ci-deploy
// runs:
//
//	start green, deferred -> healthcheck -> canary on green -> drain blue
//	-> adopt on green -> proxy flip -> stop blue
//
// Deferred (Config.DeferState): the server starts without loading or
// writing the StateFile, so the canary's rooms live in memory only and
// blue's file stays blue's. Adopt discards every room served so far (the
// canary's), loads the file and turns writes on.
//
// Drain (on blue): one last save, then /ws refuses new sessions with a
// retryable 503 (as a standby does) while live sockets keep relaying, and
// the StateFile is never written again, not even on shutdown: from here
// on it belongs to green.
//
// ci-deploy asks for adopt and drain on the admin listener (AdminHandler,
// loopback only) or, for adopt, with SIGHUP.

// DeferFilePrefix + color, next to the StateFile: while it exists at
// startup, a server with BLITE_COLOR=color starts deferred. ci-deploy
// creates it before starting that color and removes it after the adopt, so
// a later restart of the same container loads the file as usual.
const DeferFilePrefix = ".ci-deploy-defer-"

// DeferFile is the defer marker for color next to stateFile ("" when
// either is empty: never deferred).
func DeferFile(stateFile, color string) string {
	if stateFile == "" || color == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(stateFile), DeferFilePrefix+color)
}

// ShouldDefer reports whether a server for stateFile and color starts
// deferred: its defer marker exists now.
func ShouldDefer(stateFile, color string) bool {
	f := DeferFile(stateFile, color)
	if f == "" {
		return false
	}
	_, err := os.Stat(f)
	return err == nil
}

// Persistence modes (Server.mode).
const (
	persistNormal   int32 = iota // load and save the StateFile
	persistDeferred              // in memory only until Adopt
	persistDrained               // the StateFile belongs to another instance now
)

var (
	// ErrNotDeferred: Adopt on a server that didn't start deferred (or is
	// drained).
	ErrNotDeferred = errors.New("not deferred: started without its defer file (BLITE_COLOR + " + DeferFilePrefix + "<color>), nothing to adopt")
	// ErrDeferred: Drain on a deferred server, whose state is only its
	// canary's.
	ErrDeferred = errors.New("deferred (not adopted yet): it has no state of its own to flush")
	// ErrNotDrained: Undrain on a server that isn't drained.
	ErrNotDrained = errors.New("not drained")
)

// blueGreen is Server's blue/green bookkeeping. mode changes under saveMu.
type blueGreen struct {
	mode     atomic.Int32
	adopted  bool        // Adopt succeeded (under saveMu)
	draining atomic.Bool // /ws refuses new sessions
	// enterMu: handleWS seats a new session (create, join, resume) under
	// RLock after checking draining; Drain sets draining under Lock, so
	// once it saves, no session that passed the upgrade check before the
	// drain can still be seated after the final save (and be lost).
	enterMu     sync.RWMutex
	drainLogged atomic.Int64
}

// writes reports whether this server may write the StateFile (and the
// standby's LiveFile, which sits next to it).
func (s *Server) writes() bool { return s.mode.Load() == persistNormal }

// Deferred reports whether the server is waiting for Adopt.
func (s *Server) Deferred() bool { return s.mode.Load() == persistDeferred }

// Drained reports whether Drain has run (and no Undrain since).
func (s *Server) Drained() bool { return s.mode.Load() == persistDrained }

// Adopt ends deferred mode: it discards every room (closing their
// sockets), loads the StateFile and turns state writes on. It returns the
// number of rooms loaded, and already=true (no change) if it had already
// adopted. A StateFile that can't be loaded leaves the server deferred: the
// next save would otherwise replace it with nothing.
func (s *Server) Adopt() (rooms int, already bool, err error) {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	if s.mode.Load() != persistDeferred {
		if s.adopted {
			return 0, true, nil
		}
		return 0, false, ErrNotDeferred
	}
	discarded, outs := s.rooms.Reset()
	for _, o := range outs {
		if c, ok := o.(*conn); ok {
			c.close(websocket.StatusGoingAway, "adopting state: canary rooms discarded")
		}
	}
	n, err := s.loadState()
	if err != nil {
		s.cfg.Log.Error("adopt: load state; still deferred", "err", err, "discarded", discarded)
		return 0, false, err
	}
	s.adopted = true
	s.mode.Store(persistNormal)
	s.cfg.Log.Info("adopted state", "rooms", n, "discarded", discarded, "closed", len(outs))
	return n, false, nil
}

// Drain saves the state one last time, then refuses new sessions and never
// writes the StateFile again. already=true: it was drained before (no
// change). If the save fails it isn't drained: sessions are taken again.
func (s *Server) Drain() (already bool, err error) {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	switch s.mode.Load() {
	case persistDrained:
		return true, nil
	case persistDeferred:
		return false, ErrDeferred
	}
	s.enterMu.Lock()       // wait out sessions being seated right now
	s.draining.Store(true) // nobody new gets in, so the last save is final
	s.enterMu.Unlock()
	if err := s.saveLocked(); err != nil {
		s.draining.Store(false)
		s.cfg.Log.Error("drain: final save failed; not drained", "err", err)
		return false, err
	}
	s.mode.Store(persistDrained)
	s.cfg.Log.Info("drained: state saved for the last time; refusing new sessions", "rooms", s.rooms.Count())
	return false, nil
}

// Undrain undoes Drain (a deploy that was abandoned before anything else
// took the StateFile over): sessions and state writes resume.
func (s *Server) Undrain() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	if s.mode.Load() != persistDrained {
		return ErrNotDrained
	}
	// Reload first: if another instance wrote the file since our final
	// save, its rooms aren't overwritten by our older memory.
	if _, err := s.loadState(); err != nil {
		return err
	}
	s.mode.Store(persistNormal)
	s.draining.Store(false)
	s.cfg.Log.Info("undrained: taking sessions and saving state again")
	return nil
}

// refuseDrained answers /ws on a drained server: 503, try again (the
// Worker and clients retry; by then the new instance has the traffic).
func (s *Server) refuseDrained(w http.ResponseWriter) {
	if now := time.Now().Unix(); s.drainLogged.Swap(now) < now-10 {
		s.cfg.Log.Info("drained: refused a session")
	}
	w.Header().Set("Retry-After", "2")
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "draining: try again", http.StatusServiceUnavailable)
}

// AdminHandler serves the deploy hooks, for a loopback-only listener
// (BLITE_ADMIN_ADDR): POST /v1/admin/drain, /v1/admin/undrain,
// /v1/admin/adopt and GET /v1/admin/status. It also refuses any request
// whose peer isn't a loopback address, so it is never reachable through
// the public listener or a proxy even if mounted there by mistake.
func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/admin/drain", func(w http.ResponseWriter, _ *http.Request) {
		already, err := s.Drain()
		switch {
		case err != nil:
			http.Error(w, "drain failed: "+err.Error(), http.StatusConflict)
		case already:
			fmt.Fprintln(w, "already drained")
		default:
			fmt.Fprintf(w, "drained: state saved, rooms=%d\n", s.rooms.Count())
		}
	})
	mux.HandleFunc("POST /v1/admin/undrain", func(w http.ResponseWriter, _ *http.Request) {
		if err := s.Undrain(); err != nil {
			http.Error(w, "undrain failed: "+err.Error(), http.StatusConflict)
			return
		}
		fmt.Fprintln(w, "undrained")
	})
	mux.HandleFunc("POST /v1/admin/adopt", func(w http.ResponseWriter, _ *http.Request) {
		n, already, err := s.Adopt()
		switch {
		case err != nil:
			http.Error(w, "adopt failed: "+err.Error(), http.StatusConflict)
		case already:
			fmt.Fprintln(w, "already adopted")
		default:
			fmt.Fprintf(w, "adopted state: rooms=%d\n", n)
		}
	})
	mux.HandleFunc("GET /v1/admin/status", func(w http.ResponseWriter, _ *http.Request) {
		mode := map[int32]string{persistNormal: "normal", persistDeferred: "deferred", persistDrained: "drained"}[s.mode.Load()]
		fmt.Fprintf(w, "mode=%s rooms=%d served=%d\n", mode, s.rooms.Count(), s.rooms.Served())
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}
