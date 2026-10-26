// Package server is blite-signal's HTTP surface: the /ws signaling socket, the
// /j/<code> join landing page and /healthz.
package server

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/time/rate"

	"blite/internal/proto"
	"blite/internal/ratelimit"
	"blite/internal/room"
	"blite/internal/turn"
)

const (
	maxMessage   = 64 << 10
	sendBuffer   = 64
	pingInterval = 30 * time.Second // some proxies drop idle WebSockets after ~100 s
	writeTimeout = 10 * time.Second
)

// Config controls a Server.
type Config struct {
	ResumeGrace time.Duration
	// TrustProxy makes the server take the client IP from CF-Connecting-IP /
	// X-Forwarded-For. Only enable it behind a reverse proxy that sets them.
	TrustProxy bool
	ICE        turn.Provider
	// MinCompatibleBuild refuses create/join from clients with a lower build
	// number (update_required). IMPORTANT: bump it ONLY for a breaking protocol
	// change; it locks out everyone below it. For anything else bump
	// MinRecommendedBuild: those clients still get in, with an update nudge.
	// 0 = accept any.
	MinCompatibleBuild  int
	MinRecommendedBuild int
	// HostKey lets a hosted relay create rooms with a fixed code
	// (proto.Create.Room). Empty = nobody can.
	HostKey string
	// ReportDir stores client log reports (POST /v1/report). Empty = the
	// endpoint is off.
	ReportDir string
	Log       *slog.Logger
}

// Server serves Blite signaling.
type Server struct {
	cfg      Config
	rooms    *room.Manager
	failIP   *ratelimit.Window
	failRoom *ratelimit.Window
	createIP *ratelimit.Window
	reportIP *ratelimit.Window
	mux      *http.ServeMux
}

// New returns a Server. Call Run to start background sweeps.
func New(cfg Config) *Server {
	if cfg.ResumeGrace == 0 {
		cfg.ResumeGrace = 60 * time.Second
	}
	if cfg.ICE == nil {
		cfg.ICE = turn.DefaultSTUN
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	s := &Server{
		cfg:      cfg,
		rooms:    room.NewManager(cfg.ResumeGrace),
		failIP:   ratelimit.New(5, 10*time.Minute),
		failRoom: ratelimit.New(20, 10*time.Minute),
		createIP: ratelimit.New(10, 10*time.Minute),
		reportIP: ratelimit.New(20, time.Hour),
		mux:      http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /ws", s.handleWS)
	s.mux.HandleFunc("GET /j/{code}", s.handleJoinPage)
	s.mux.HandleFunc("GET /static/join.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Write([]byte(joinJS))
	})
	if cfg.ReportDir != "" {
		s.mux.HandleFunc("POST /v1/report", s.handleReport)
	}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	s.mux.ServeHTTP(w, r)
}

// Run sweeps expired resume slots and rate-limit state until ctx ends.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	if s.cfg.ReportDir != "" {
		go func() {
			for {
				s.pruneReports()
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Hour):
				}
			}
		}()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.rooms.Sweep()
			s.failIP.Sweep()
			s.failRoom.Sweep()
			s.createIP.Sweep()
			s.reportIP.Sweep()
		}
	}
}

// Rooms exposes the room manager (tests).
func (s *Server) Rooms() *room.Manager { return s.rooms }

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
			return ip
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// conn is one WebSocket client. It implements room.Outbox.
type conn struct {
	ws     *websocket.Conn
	out    chan []byte
	closed chan struct{}
	once   sync.Once
}

func (c *conn) Send(msg any) bool {
	b, err := json.Marshal(msg)
	if err != nil {
		return false
	}
	select {
	case c.out <- b:
		return true
	case <-c.closed:
		return false
	default:
		// A peer that can't keep up with a few KB of signaling is gone; drop it.
		c.close(websocket.StatusPolicyViolation, "slow consumer")
		return false
	}
}

// CloseSoon closes the connection after a moment, so messages already
// queued (a kick notice) get out first.
func (c *conn) CloseSoon() {
	time.AfterFunc(500*time.Millisecond, func() { c.close(websocket.StatusPolicyViolation, "kicked") })
}

func (c *conn) close(code websocket.StatusCode, reason string) {
	c.once.Do(func() {
		close(c.closed)
		go c.ws.Close(code, reason)
	})
}

func (c *conn) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case b := <-c.out:
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.ws.Write(wctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				c.close(websocket.StatusGoingAway, "write failed")
				return
			}
		}
	}
}

func (c *conn) pingLoop(ctx context.Context) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.ws.Ping(pctx)
			cancel()
			if err != nil {
				c.close(websocket.StatusGoingAway, "ping timeout")
				return
			}
		}
	}
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil) // default Origin check: same host or no Origin (native client)
	if err != nil {
		return
	}
	ws.SetReadLimit(maxMessage)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	c := &conn{ws: ws, out: make(chan []byte, sendBuffer), closed: make(chan struct{})}
	go c.writeLoop(ctx)
	go c.pingLoop(ctx)

	ip := s.clientIP(r)
	var seat room.Seat
	left := false
	lim := rate.NewLimiter(50, 100)

	defer func() {
		if seat.PeerID != "" {
			if left {
				s.rooms.Leave(seat.Room, seat.PeerID)
			} else {
				s.rooms.Disconnected(seat.Room, seat.PeerID, c)
			}
		}
		c.close(websocket.StatusNormalClosure, "")
	}()

	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if !lim.Allow() {
			c.close(websocket.StatusPolicyViolation, "too many messages")
			return
		}
		var env proto.Envelope
		if json.Unmarshal(data, &env) != nil {
			c.Send(proto.Error{T: proto.TError, Code: proto.ErrBadRequest})
			continue
		}
		if seat.PeerID == "" {
			seat = s.enter(env.T, data, ip, c)
			continue
		}
		switch env.T {
		case proto.TSignal:
			var m proto.Signal
			if json.Unmarshal(data, &m) != nil || m.To == "" || len(m.Data) == 0 {
				c.Send(proto.Error{T: proto.TError, Code: proto.ErrBadRequest})
				continue
			}
			fwd := proto.Signal{T: proto.TSignal, From: seat.PeerID, Data: m.Data}
			if !s.rooms.Relay(seat.Room, seat.PeerID, m.To, fwd) {
				c.Send(proto.Error{T: proto.TError, Code: proto.ErrNoPeer})
			}
		case proto.TRelayVolunteer:
			var m proto.RelayVolunteer
			if json.Unmarshal(data, &m) != nil {
				c.Send(proto.Error{T: proto.TError, Code: proto.ErrBadRequest})
				continue
			}
			if err := s.rooms.Volunteer(seat.Room, seat.PeerID, m.On); err != nil {
				c.Send(proto.Error{T: proto.TError, Code: err.Error()})
			}
		case proto.TRename:
			var m proto.Rename
			if json.Unmarshal(data, &m) != nil {
				c.Send(proto.Error{T: proto.TError, Code: proto.ErrBadRequest})
				continue
			}
			if err := s.rooms.Rename(seat.Room, seat.PeerID, m.Name); err != nil {
				c.Send(proto.Error{T: proto.TError, Code: err.Error()})
			}
		case proto.TTransfer:
			var m proto.Transfer
			if json.Unmarshal(data, &m) != nil {
				c.Send(proto.Error{T: proto.TError, Code: proto.ErrBadRequest})
				continue
			}
			if err := s.rooms.Transfer(seat.Room, seat.PeerID, m.PeerID); err != nil {
				c.Send(proto.Error{T: proto.TError, Code: err.Error()})
			}
		case proto.TKick:
			var m proto.Kick
			if json.Unmarshal(data, &m) != nil {
				c.Send(proto.Error{T: proto.TError, Code: proto.ErrBadRequest})
				continue
			}
			if err := s.rooms.Kick(seat.Room, seat.PeerID, m.PeerID, m.Ban); err != nil {
				c.Send(proto.Error{T: proto.TError, Code: err.Error()})
			} else {
				s.cfg.Log.Info("kicked", "ban", m.Ban)
			}
		case proto.TLeave:
			left = true
			return
		default:
			c.Send(proto.Error{T: proto.TError, Code: proto.ErrBadRequest})
		}
	}
}

func build(c *proto.ClientMeta) int {
	if c == nil {
		return 0 // clients from before version reporting
	}
	return c.Build
}

func osOf(c *proto.ClientMeta) string {
	if c == nil {
		return ""
	}
	return c.OSVersion
}

// enter handles the first message on a connection: create, join or resume.
// It returns the seat, or a zero Seat after sending an error.
func (s *Server) enter(t string, data []byte, ip string, c *conn) room.Seat {
	fail := func(code string) room.Seat {
		c.Send(proto.Error{T: proto.TError, Code: code})
		return room.Seat{}
	}
	var (
		seat room.Seat
		err  error
		cm   *proto.ClientMeta
	)
	switch t {
	case proto.TCreate:
		var m proto.Create
		if json.Unmarshal(data, &m) != nil {
			return fail(proto.ErrBadRequest)
		}
		if s.createIP.Blocked(ip) {
			return fail(proto.ErrRateLimited)
		}
		if cm = m.Client; build(cm) < s.cfg.MinCompatibleBuild {
			return fail(proto.ErrUpdateRequired)
		}
		s.createIP.Add(ip)
		if m.Room == "" {
			seat, err = s.rooms.Create(m.Password, m.Name, m.IDKey, cm.Label(), build(cm), c)
			break
		}
		if s.cfg.HostKey == "" || subtle.ConstantTimeCompare([]byte(m.HostKey), []byte(s.cfg.HostKey)) != 1 {
			s.failIP.Add(ip)
			return fail(proto.ErrBadRequest)
		}
		seat, err = s.rooms.CreateFixed(room.NormalizeCode(m.Room), m.Password, m.Name, m.IDKey, cm.Label(), build(cm), m.Owners, c)
	case proto.TJoin:
		var m proto.Join
		if json.Unmarshal(data, &m) != nil {
			return fail(proto.ErrBadRequest)
		}
		if cm = m.Client; build(cm) < s.cfg.MinCompatibleBuild {
			return fail(proto.ErrUpdateRequired)
		}
		code := room.NormalizeCode(m.Room)
		if code == "" {
			return fail(proto.ErrNoRoom)
		}
		if s.failIP.Blocked(ip) || s.failRoom.Blocked(code) {
			return fail(proto.ErrRateLimited)
		}
		seat, err = s.rooms.Join(code, m.Password, m.Name, m.IDKey, cm.Label(), build(cm), c)
		if errors.Is(err, room.ErrBadPassword) || errors.Is(err, room.ErrNoRoom) {
			s.failIP.Add(ip)
			s.failRoom.Add(code)
		}
	case proto.TResume:
		var m proto.Resume
		if json.Unmarshal(data, &m) != nil {
			return fail(proto.ErrBadRequest)
		}
		if s.failIP.Blocked(ip) {
			return fail(proto.ErrRateLimited)
		}
		seat, err = s.rooms.Resume(room.NormalizeCode(m.Room), m.PeerID, m.Token, c)
		if err != nil {
			s.failIP.Add(ip)
		}
	default:
		return fail(proto.ErrBadRequest)
	}
	if err != nil {
		return fail(err.Error())
	}
	c.Send(proto.Welcome{
		T: proto.TWelcome, V: proto.Version,
		Room: seat.Room, PeerID: seat.PeerID, Resume: seat.Resume,
		ICEServers: s.cfg.ICE.ICEServers(seat.PeerID),
		Peers:      seat.Others,
		RelayQueue: seat.RelayQueue,
		Owner:      seat.Owner,
		// Resume carries no ClientMeta; the nudge was given on create/join.
		UpdateRecommended: t != proto.TResume && build(cm) < s.cfg.MinRecommendedBuild,
	})
	s.cfg.Log.Info("seated", "op", t, "client", cm.Label(), "os", osOf(cm), "rooms", s.rooms.Count())
	return seat
}

//go:embed join.html
var joinHTML string

//go:embed join.js
var joinJS string

var joinTmpl = template.Must(template.New("join").Parse(joinHTML))

// handleJoinPage serves the share-link landing page. It never checks whether the
// room exists (no enumeration oracle) and never sees the password, which lives
// in the URL fragment.
func (s *Server) handleJoinPage(w http.ResponseWriter, r *http.Request) {
	code := room.NormalizeCode(r.PathValue("code"))
	if code == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	joinTmpl.Execute(w, map[string]string{"Code": code, "Display": room.Display(code)})
}
