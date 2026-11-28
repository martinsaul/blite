package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Standby mode. A fronting proxy (the Worker) sends the site's traffic to
// the primary first and this server (the warm standby) second. A client
// seated here while the primary is only restarting splits its room: the
// primary later sweeps its slot, and a restart of the standby then ends its
// call (resume_failed). So the Worker decides when the primary is down for
// good: it retries the primary for a few seconds and only then sends the
// request here, marked with FailoverHeader. In standby mode this server
// refuses every other /ws request that came through the Worker (Host is
// the site's own name) with 503, which the client retries like a dropped
// connection and the Worker sends to the primary again.
//
// ActiveFile promotes the standby by hand (touch it; remove it to demote):
// then it serves everything, as when the Worker's origin order is swapped.
// Host names not behind the Worker and the HTTP API are served as usual.
//
// LiveFile exists while this standby holds sessions of its own (a
// failover is in progress). statesync skips pulling the primary's state
// while it does: the pull would replace this server's own state file, so a
// restart here would lose those sessions, and a stale primary file adds
// nothing to rooms this server serves itself.

// FailoverHeader is set by the Worker on a request it sends to a standby
// origin after giving up on the primary: hex HMAC-SHA256(FailoverSecret,
// unix-minute) for the current or previous minute.
const FailoverHeader = "X-Bellion-Failover"

// workerHosts are the names only the Worker serves (bellion.app).
var workerHosts = []string{strings.TrimPrefix(siteURL, "https://"), "www." + strings.TrimPrefix(siteURL, "https://")}

func failoverMAC(secret string, unixMinute int64) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strconv.FormatInt(unixMinute, 10)))
	return m.Sum(nil)
}

func (s *Server) validFailover(r *http.Request) bool {
	if s.cfg.FailoverSecret == "" {
		return false
	}
	got, err := hex.DecodeString(strings.TrimSpace(r.Header.Get(FailoverHeader)))
	if err != nil || len(got) != sha256.Size {
		return false
	}
	now := time.Now
	if s.standbyNow != nil {
		now = s.standbyNow
	}
	min := now().Unix() / 60
	cur := subtle.ConstantTimeCompare(got, failoverMAC(s.cfg.FailoverSecret, min))
	prev := subtle.ConstantTimeCompare(got, failoverMAC(s.cfg.FailoverSecret, min-1))
	return (cur | prev) == 1
}

// standbyRefuses reports whether r is a WebSocket this standby must turn
// away: it came through the Worker, which hasn't given up on the primary,
// and nobody promoted this server.
func (s *Server) standbyRefuses(r *http.Request) bool {
	if !s.cfg.Standby || s.validFailover(r) {
		return false
	}
	host := strings.ToLower(r.Host)
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	worker := false
	for _, h := range workerHosts {
		worker = worker || host == h
	}
	return worker && !s.promoted()
}

// promoted: ActiveFile exists.
func (s *Server) promoted() bool {
	if s.cfg.ActiveFile == "" {
		return false
	}
	_, err := os.Stat(s.cfg.ActiveFile)
	return err == nil
}

// refuseStandby answers a refused WebSocket: 503, try again in a moment.
func (s *Server) refuseStandby(w http.ResponseWriter) {
	if now := time.Now().Unix(); s.standbyLogged.Swap(now) < now-10 {
		s.cfg.Log.Info("standby: refused a session (the primary is up or was only briefly down)")
	}
	w.Header().Set("Retry-After", "2")
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "standby: try the primary", http.StatusServiceUnavailable)
}

// syncLiveFile creates LiveFile while this server serves sessions of its
// own, and removes it when it doesn't.
func (s *Server) syncLiveFile() {
	if s.cfg.LiveFile == "" || !s.writes() {
		return // deferred or drained: the state dir is another instance's
	}
	live := s.rooms.Served() > 0
	if live == s.live.Load() {
		return
	}
	var err error
	if live {
		err = os.WriteFile(s.cfg.LiveFile, []byte("this standby serves sessions; statesync pauses\n"), 0o600)
	} else if err = os.Remove(s.cfg.LiveFile); errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if err != nil {
		s.cfg.Log.Error("standby live file", "err", err)
		return
	}
	s.live.Store(live)
	s.cfg.Log.Info("standby: serving sessions", "live", live)
}

// standbyState is Server's standby bookkeeping.
type standbyState struct {
	standbyLogged atomic.Int64 // unix time of the last refusal log line
	live          atomic.Bool  // LiveFile exists (as far as we know)
	standbyNow    func() time.Time
}
