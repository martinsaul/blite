// Package app ties a Blite session to the local UI: it holds at most one live
// session and implements ui.Backend.
package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"blite/internal/audio"
	"blite/internal/capture"
	"blite/internal/identity"
	"blite/internal/media"
	"blite/internal/proto"
	"blite/internal/register"
	"blite/internal/session"
	"blite/internal/sigclient"
	"blite/internal/ui"
	"blite/internal/update"
)

// App is the running client.
type App struct {
	cfg session.Config
	log *slog.Logger
	bc  ui.Broadcaster

	mu      sync.Mutex
	sess    *session.Session
	lastErr string
	res     media.Resolution // last chosen share resolution; new sessions keep it
	layout  session.Layout   // how we show received streams; kept on disk
	// DefaultSource is what a new session shares first.
	DefaultSource media.SourceKind
	// Exe is the installed Blite binary; empty for dev builds, which don't
	// register for autostart.
	Exe string
	// Updates is nil for builds that don't self-update (dev builds).
	Updates *update.Updater
	// Exit stops the app (the tray's Quit); called after an update is handed
	// to molt-swap, which waits for us to exit.
	Exit func()
	// Logs describes the log files for the UI; SendLogs uploads them to the
	// signal server; OpenLogs shows their folder. nil = not available.
	Logs     func() any
	SendLogs func(ctx context.Context) error
	OpenLogs func() error
}

// ErrUpdateRequired: the installed build is below the update channel's
// minimum, so no new sessions until it's updated.
var ErrUpdateRequired = errors.New("this Blite is too old: install the update first")

// New returns an App that creates sessions with cfg.
func New(cfg session.Config, def media.SourceKind) *App {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &App{cfg: cfg, log: cfg.Log, DefaultSource: def, layout: loadLayout()}
}

// UIState is what /api/state returns.
type UIState struct {
	Session *session.State `json:"session"`
	Error   string         `json:"error,omitempty"`
	Version string         `json:"version"` // our build, e.g. "0.1.0 (57) linux"
	// Name and Tag: who we are (name#tag), also outside a session.
	Name   string       `json:"name"`
	Tag    string       `json:"tag"`
	Update *update.Info `json:"update,omitempty"`
	// Autostart is whether Blite starts at sign-in; nil where it can't
	// (dev builds).
	Autostart *bool `json:"autostart,omitempty"`
	// Layout is how received streams are shown, also outside a session.
	Layout session.Layout `json:"layout"`
	Logs   any            `json:"logs,omitempty"`
}

// State implements ui.Backend.
func (a *App) State() any {
	a.mu.Lock()
	s, e := a.sess, a.lastErr
	a.mu.Unlock()
	st := UIState{Error: e, Version: a.cfg.Client.Label(), Name: a.cfgName(), Layout: a.currentLayout()}
	if a.cfg.Identity.Private != nil {
		st.Tag = identity.Tag(a.cfg.Identity.Public())
	}
	if a.Exe != "" {
		on := register.AutostartEnabled()
		st.Autostart = &on
	}
	if a.Updates != nil {
		i := a.Updates.Info()
		st.Update = &i
	}
	if a.Logs != nil {
		st.Logs = a.Logs()
	}
	if s != nil {
		ss := s.State()
		st.Session = &ss
	}
	return st
}

func (a *App) config() session.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// DisplayName is our name#tag, for reports.
func (a *App) DisplayName() string {
	n := a.cfgName()
	if a.cfg.Identity.Private != nil {
		n += "#" + identity.Tag(a.cfg.Identity.Public())
	}
	return n
}

// SendLogsNow implements ui.Backend.
func (a *App) SendLogsNow(ctx context.Context) error {
	if a.SendLogs == nil {
		return errors.New("this build can't send logs")
	}
	err := a.SendLogs(ctx)
	a.bc.Notify()
	return err
}

// OpenLogFolder implements ui.Backend.
func (a *App) OpenLogFolder() error {
	if a.OpenLogs == nil {
		return errors.New("no log folder")
	}
	return a.OpenLogs()
}

func (a *App) currentLayout() session.Layout {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.layout
}

func (a *App) cfgName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Name
}

// Subscribe implements ui.Backend.
func (a *App) Subscribe() (<-chan struct{}, func()) { return a.bc.Subscribe() }

func (a *App) adopt(s *session.Session) {
	s.OnChange(a.bc.Notify)
	a.mu.Lock()
	a.sess, a.lastErr = s, ""
	res, layout := a.res, a.layout
	a.mu.Unlock()
	s.SetResolution(res)
	s.SetLayout(layout)
	go func() {
		<-s.Done()
		a.mu.Lock()
		if a.sess == s {
			a.sess = nil
			if r := s.EndReason(); r != "" {
				a.lastErr = r
			}
		}
		a.mu.Unlock()
		a.bc.Notify()
	}()
	if a.DefaultSource != "" && a.DefaultSource != "none" {
		if err := s.SetSource(a.DefaultSource); err != nil {
			a.setErr("could not start sharing: " + err.Error())
		}
	}
	a.bc.Notify()
}

func (a *App) setErr(e string) {
	a.mu.Lock()
	a.lastErr = e
	a.mu.Unlock()
	a.bc.Notify()
}

// ErrInSession is returned when a session is already running.
var ErrInSession = errors.New("already in a session; leave it first")

func (a *App) busy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sess != nil
}

// Create implements ui.Backend.
func (a *App) Create(ctx context.Context, password string) error {
	if a.busy() {
		return ErrInSession
	}
	if a.Updates.Mandatory() {
		return ErrUpdateRequired
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	s, err := session.Create(ctx, a.config(), password)
	if err != nil {
		return friendly(err)
	}
	a.adopt(s)
	return nil
}

// JoinLink implements ui.Backend.
func (a *App) JoinLink(ctx context.Context, link string) error {
	code, pw, err := session.ParseLink(link)
	if err != nil {
		return err
	}
	if a.busy() {
		return ErrInSession
	}
	if a.Updates.Mandatory() {
		return ErrUpdateRequired
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	s, err := session.Join(ctx, a.config(), code, pw)
	if err != nil {
		return friendly(err)
	}
	a.adopt(s)
	return nil
}

// Leave implements ui.Backend.
func (a *App) Leave() {
	a.mu.Lock()
	s := a.sess
	a.sess = nil
	a.mu.Unlock()
	if s != nil {
		s.Close()
	}
	a.bc.Notify()
}

// SetSource implements ui.Backend.
func (a *App) SetSource(kind string) error {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s == nil {
		return errors.New("not in a session")
	}
	if kind != "none" && !media.ValidSource(media.SourceKind(kind)) {
		return errors.New("unknown source")
	}
	return s.SetSource(media.SourceKind(kind))
}

// SetResolution implements ui.Backend: 720p, 1080p or 2160p for what we
// share, now and in later sessions.
func (a *App) SetResolution(res string) error {
	r := media.Resolution(res)
	if !media.ValidResolution(r) {
		return errors.New("unknown resolution")
	}
	a.mu.Lock()
	a.res = r
	s := a.sess
	a.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.SetResolution(r)
}

// Sources implements ui.Backend: monitors and windows that can be shared.
func (a *App) Sources() any {
	ts, err := capture.List()
	out := map[string]any{"targets": ts}
	if err != nil {
		out["error"] = err.Error()
	}
	return out
}

// AudioOptions implements ui.Backend.
func (a *App) AudioOptions() any {
	apps, err := audio.ListApps()
	devs, _ := audio.ListDevices()
	out := map[string]any{"apps": apps, "devices": devs}
	if err != nil {
		out["error"] = err.Error()
	}
	return out
}

func (a *App) current() (*session.Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sess == nil {
		return nil, errors.New("not in a session")
	}
	return a.sess, nil
}

// SetAudio implements ui.Backend.
func (a *App) SetAudio(mode string, apps []string) error {
	s, err := a.current()
	if err != nil {
		return err
	}
	if mode == "test" {
		return errors.New("unknown audio mode") // test tone is for headless peers only
	}
	return s.SetAudio(audio.Selection{Mode: mode, Apps: apps})
}

// SetOutput implements ui.Backend.
func (a *App) SetOutput(device string) error {
	s, err := a.current()
	if err != nil {
		return err
	}
	s.SetOutput(device)
	return nil
}

// SetPeer implements ui.Backend.
func (a *App) SetPeer(id, quality string, volume float64, muted bool) error {
	s, err := a.current()
	if err != nil {
		return err
	}
	return s.SetPeer(id, quality, volume, muted)
}

// Volunteer implements ui.Backend.
func (a *App) Volunteer(on bool) error {
	s, err := a.current()
	if err != nil {
		return err
	}
	return s.Volunteer(on)
}

// SetName implements ui.Backend: our display name, kept for later
// sessions, and changed in the current one.
func (a *App) SetName(name string) error {
	name = identity.CleanName(name)
	if name == "" {
		return errors.New("enter a name")
	}
	if err := identity.SaveName(name); err != nil {
		a.log.Warn("save name", "err", err)
	}
	a.mu.Lock()
	a.cfg.Name = name
	s := a.sess
	a.mu.Unlock()
	if s != nil {
		if err := s.Rename(name); err != nil {
			return err
		}
	}
	a.bc.Notify()
	return nil
}

// Transfer implements ui.Backend.
func (a *App) Transfer(id string) error {
	s, err := a.current()
	if err != nil {
		return err
	}
	return s.Transfer(id)
}

// Kick implements ui.Backend.
func (a *App) Kick(id string, ban bool) error {
	s, err := a.current()
	if err != nil {
		return err
	}
	return s.Kick(id, ban)
}

// CheckUpdate implements ui.Backend: check now (also runs on a timer).
func (a *App) CheckUpdate(ctx context.Context) error {
	if a.Updates == nil {
		return errors.New("this build doesn't update itself")
	}
	a.Updates.Check(ctx)
	a.bc.Notify()
	return nil
}

// InstallUpdate implements ui.Backend. Never during a call: molt-swap
// restarts Blite.
func (a *App) InstallUpdate(ctx context.Context) error {
	if a.Updates == nil {
		return errors.New("this build doesn't update itself")
	}
	if a.busy() {
		return errors.New("leave the session first: installing restarts Blite")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()
	if err := a.Updates.Install(ctx); err != nil {
		a.bc.Notify()
		return err
	}
	a.log.Info("update handed to molt-swap; exiting")
	if a.Exit != nil {
		go a.Exit()
	}
	return nil
}

// SetAutostart implements ui.Backend: start Blite at sign-in, or not.
func (a *App) SetAutostart(on bool) error {
	if a.Exe == "" {
		return errors.New("this build doesn't start at sign-in")
	}
	err := register.SetAutostart(on, a.Exe)
	a.bc.Notify()
	return err
}

// DismissUpdateResult implements ui.Backend: the "updated" notice was seen.
func (a *App) DismissUpdateResult() {
	if a.Updates != nil {
		a.Updates.ClearResult()
		a.bc.Notify()
	}
}

// Shutdown leaves any session.
func (a *App) Shutdown() { a.Leave() }

var serverMessages = map[string]string{
	proto.ErrBadPassword:    "wrong password for that room",
	proto.ErrNoRoom:         "that session doesn't exist (it ends when everyone leaves)",
	proto.ErrRoomFull:       "that session is full (5 people)",
	proto.ErrRateLimited:    "too many attempts; wait a few minutes",
	proto.ErrBadRequest:     "password must be at least 4 characters",
	proto.ErrUpdateRequired: "this Blite is too old for the server: please update it",
}

func friendly(err error) error {
	var se sigclient.ServerError
	if errors.As(err, &se) {
		if msg, ok := serverMessages[string(se)]; ok {
			return errors.New(msg)
		}
	}
	return err
}
