// Package ui is Blite's local control UI: a small HTTP server on 127.0.0.1 that
// the tray opens in the default browser. Media never goes through it.
//
// Hardening, because any website the user visits can try to talk to localhost:
//   - binds 127.0.0.1 only, on a random port;
//   - rejects any Host header except 127.0.0.1:<port> (defeats DNS rebinding);
//   - the tray opens /?t=<per-launch token>; that is exchanged for an HttpOnly,
//     SameSite=Strict cookie, and every other route needs the cookie;
//   - every POST needs the X-Blite: 1 header, which a cross-site page can't send
//     without a CORS preflight we never answer;
//   - a second `blite` process (a blite:// link) uses the token as a bearer.
package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

const cookieName = "blite_ui"

// Backend is what the UI controls (implemented by the app).
type Backend interface {
	State() any
	CheckUpdate(ctx context.Context) error
	InstallUpdate(ctx context.Context) error
	DismissUpdateResult()
	// SetAutostart turns starting at sign-in on or off.
	SetAutostart(on bool) error
	Create(ctx context.Context, password string) error
	JoinLink(ctx context.Context, link string) error
	Leave()
	SetSource(kind string) error
	// Sources lists monitors and windows that can be shared.
	Sources() any
	// AudioOptions lists apps playing audio and output devices.
	AudioOptions() any
	SetAudio(mode string, apps []string) error
	SetOutput(device string) error
	// SetPeer sets our viewer settings for one peer.
	SetPeer(id, quality string, volume float64, muted bool) error
	// SetResolution sets what we share at: 720p, 1080p or 2160p.
	SetResolution(res string) error
	// SetLayout shows received streams in separate windows ("windows") or
	// in one window as a grid ("grid", rows x cols, 0 = auto).
	SetLayout(mode string, rows, cols int) error
	// SendLogsNow uploads our recent logs for the developers.
	SendLogsNow(ctx context.Context) error
	// OpenLogFolder shows the log folder in the file manager.
	OpenLogFolder() error
	// Volunteer joins (on) or leaves the relay queue.
	Volunteer(on bool) error
	// SetName sets our display name (kept; shown as name#tag).
	SetName(name string) error
	// Transfer (room owner) makes member id the owner.
	Transfer(id string) error
	// Kick (room owner) removes member id; ban keeps them out.
	Kick(id string, ban bool) error
	// Subscribe returns a channel that receives a value on every state change.
	Subscribe() (<-chan struct{}, func())
}

// Server is the running UI.
type Server struct {
	Port  int
	Token string

	be  Backend
	log *slog.Logger
	srv *http.Server
	ln  net.Listener
}

// Start listens on 127.0.0.1 (port 0 = random) and serves the UI.
func Start(be Backend, port int, log *slog.Logger) (*Server, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	tok := make([]byte, 24)
	rand.Read(tok)
	s := &Server{Port: ln.Addr().(*net.TCPAddr).Port, Token: base64.RawURLEncoding.EncodeToString(tok), be: be, log: log, ln: ln}

	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	files := http.FileServerFS(static)
	mux.HandleFunc("GET /{$}", s.index(files))
	mux.Handle("GET /static/", s.authed(http.StripPrefix("/static/", files)))
	mux.Handle("GET /api/state", s.authed(http.HandlerFunc(s.state)))
	mux.Handle("GET /api/events", s.authed(http.HandlerFunc(s.events)))
	mux.Handle("POST /api/create", s.authed(s.post(s.create)))
	mux.Handle("POST /api/join", s.authed(s.post(s.join)))
	mux.Handle("POST /api/leave", s.authed(s.post(s.leave)))
	mux.Handle("POST /api/source", s.authed(s.post(s.source)))
	mux.Handle("GET /api/sources", s.authed(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.be.Sources())
	})))
	mux.Handle("GET /api/audio", s.authed(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.be.AudioOptions())
	})))
	mux.Handle("POST /api/audio", s.authed(s.post(s.audio)))
	mux.Handle("POST /api/output", s.authed(s.post(s.output)))
	mux.Handle("POST /api/peer", s.authed(s.post(s.peer)))
	mux.Handle("POST /api/update/check", s.authed(s.post(func(r *http.Request) error { return s.be.CheckUpdate(r.Context()) })))
	mux.Handle("POST /api/update/install", s.authed(s.post(func(r *http.Request) error { return s.be.InstallUpdate(r.Context()) })))
	mux.Handle("POST /api/autostart", s.authed(s.post(func(r *http.Request) error {
		var b struct {
			On bool `json:"on"`
		}
		if err := decode(r, &b); err != nil {
			return err
		}
		return s.be.SetAutostart(b.On)
	})))
	mux.Handle("POST /api/resolution", s.authed(s.post(func(r *http.Request) error {
		var b struct {
			Res string `json:"res"`
		}
		if err := decode(r, &b); err != nil {
			return err
		}
		return s.be.SetResolution(b.Res)
	})))
	mux.Handle("POST /api/logs/send", s.authed(s.post(func(r *http.Request) error { return s.be.SendLogsNow(r.Context()) })))
	mux.Handle("POST /api/logs/open", s.authed(s.post(func(*http.Request) error { return s.be.OpenLogFolder() })))
	mux.Handle("POST /api/layout", s.authed(s.post(func(r *http.Request) error {
		var b struct {
			Mode string `json:"mode"`
			Rows int    `json:"rows"`
			Cols int    `json:"cols"`
		}
		if err := decode(r, &b); err != nil {
			return err
		}
		return s.be.SetLayout(b.Mode, b.Rows, b.Cols)
	})))
	mux.Handle("POST /api/relay", s.authed(s.post(func(r *http.Request) error {
		var b struct {
			On bool `json:"on"`
		}
		if err := decode(r, &b); err != nil {
			return err
		}
		return s.be.Volunteer(b.On)
	})))
	mux.Handle("POST /api/name", s.authed(s.post(func(r *http.Request) error {
		var b struct {
			Name string `json:"name"`
		}
		if err := decode(r, &b); err != nil {
			return err
		}
		return s.be.SetName(b.Name)
	})))
	mux.Handle("POST /api/transfer", s.authed(s.post(func(r *http.Request) error {
		var b struct {
			ID string `json:"id"`
		}
		if err := decode(r, &b); err != nil {
			return err
		}
		return s.be.Transfer(b.ID)
	})))
	mux.Handle("POST /api/kick", s.authed(s.post(func(r *http.Request) error {
		var b struct {
			ID  string `json:"id"`
			Ban bool   `json:"ban"`
		}
		if err := decode(r, &b); err != nil {
			return err
		}
		return s.be.Kick(b.ID, b.Ban)
	})))
	mux.Handle("POST /api/update/dismiss", s.authed(s.post(func(*http.Request) error { s.be.DismissUpdateResult(); return nil })))

	s.srv = &http.Server{Handler: s.guard(mux), ReadHeaderTimeout: 5 * time.Second}
	go s.srv.Serve(ln)
	return s, nil
}

// URL is what the tray opens: it carries the per-launch token.
func (s *Server) URL() string {
	return "http://127.0.0.1:" + strconv.Itoa(s.Port) + "/?t=" + s.Token
}

// Close stops the server.
func (s *Server) Close() { s.srv.Close() }

func (s *Server) guard(next http.Handler) http.Handler {
	want := "127.0.0.1:" + strconv.Itoa(s.Port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != want {
			http.Error(w, "bad host", http.StatusMisdirectedRequest)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) tokenOK(v string) bool {
	return v != "" && subtle.ConstantTimeCompare([]byte(v), []byte(s.Token)) == 1
}

func (s *Server) authed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		bearer := r.Header.Get("Authorization")
		if (err == nil && s.tokenOK(c.Value)) || (len(bearer) > 7 && s.tokenOK(bearer[7:])) {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "open Blite from its tray icon", http.StatusUnauthorized)
	})
}

func (s *Server) index(files http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if t := r.URL.Query().Get("t"); t != "" {
			if !s.tokenOK(t) {
				http.Error(w, "stale link: open Blite from its tray icon", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusSeeOther) // drop the token from the address bar
			return
		}
		s.authed(files).ServeHTTP(w, r)
	}
}

func (s *Server) post(f func(r *http.Request) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Blite") != "1" {
			http.Error(w, "missing X-Blite header", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if err := f(r); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": s.be.State()})
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return errors.New("bad request body")
	}
	return nil
}

func (s *Server) state(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.be.State())
}

// events streams "change" server-sent events so the page refreshes its state.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", http.StatusInternalServerError)
		return
	}
	ch, unsub := s.be.Subscribe()
	defer unsub()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	tick := time.NewTicker(2 * time.Second) // stats refresh even without membership changes
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
		case <-tick.C:
		}
		if _, err := w.Write([]byte("event: change\ndata: {}\n\n")); err != nil {
			return
		}
		fl.Flush()
	}
}

func (s *Server) create(r *http.Request) error {
	var b struct {
		Password string `json:"password"`
	}
	if err := decode(r, &b); err != nil {
		return err
	}
	return s.be.Create(r.Context(), b.Password)
}

func (s *Server) join(r *http.Request) error {
	var b struct {
		Link string `json:"link"`
	}
	if err := decode(r, &b); err != nil {
		return err
	}
	return s.be.JoinLink(r.Context(), b.Link)
}

func (s *Server) leave(*http.Request) error {
	s.be.Leave()
	return nil
}

func (s *Server) source(r *http.Request) error {
	var b struct {
		Kind string `json:"kind"`
	}
	if err := decode(r, &b); err != nil {
		return err
	}
	// Logged with the page and what the user last did there, to explain
	// source changes nobody remembers making.
	s.log.Info("ui: share source", "kind", b.Kind, "tab", r.Header.Get("X-Blite-Tab"), "via", r.Header.Get("X-Blite-Via"))
	return s.be.SetSource(b.Kind)
}

func (s *Server) audio(r *http.Request) error {
	var b struct {
		Mode string   `json:"mode"`
		Apps []string `json:"apps"`
	}
	if err := decode(r, &b); err != nil {
		return err
	}
	return s.be.SetAudio(b.Mode, b.Apps)
}

func (s *Server) output(r *http.Request) error {
	var b struct {
		Device string `json:"device"`
	}
	if err := decode(r, &b); err != nil {
		return err
	}
	return s.be.SetOutput(b.Device)
}

func (s *Server) peer(r *http.Request) error {
	var b struct {
		ID      string  `json:"id"`
		Quality string  `json:"quality"`
		Volume  float64 `json:"volume"`
		Muted   bool    `json:"muted"`
	}
	if err := decode(r, &b); err != nil {
		return err
	}
	return s.be.SetPeer(b.ID, b.Quality, b.Volume, b.Muted)
}

// Broadcaster fans state-change notifications out to SSE subscribers.
type Broadcaster struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

// Notify wakes every subscriber (non-blocking).
func (b *Broadcaster) Notify() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for c := range b.subs {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

// Subscribe registers a subscriber.
func (b *Broadcaster) Subscribe() (<-chan struct{}, func()) {
	c := make(chan struct{}, 1)
	b.mu.Lock()
	if b.subs == nil {
		b.subs = map[chan struct{}]struct{}{}
	}
	b.subs[c] = struct{}{}
	b.mu.Unlock()
	return c, func() {
		b.mu.Lock()
		delete(b.subs, c)
		b.mu.Unlock()
	}
}
