// Package app ties a Blite session to the local UI: it holds at most one live
// session and implements ui.Backend.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"blite/internal/audio"
	"blite/internal/capture"
	"blite/internal/i18n"
	"blite/internal/identity"
	"blite/internal/media"
	"blite/internal/pin"
	"blite/internal/proto"
	"blite/internal/redact"
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

	mu          sync.Mutex
	nameChosen  bool // the user saved a name; until then it's two random words
	sess        *session.Session
	lastErr     string
	homeSource  string               // pre-call source choice ("primary", "window:...", "screen", "test", "none")
	srcInfo     sourceInfo           // what we know of the picked "window:"/"monitor:" source
	res         media.Resolution     // last chosen share resolution; new sessions keep it
	mode        media.ContentMode    // last chosen content mode; new sessions keep it
	shareOpts   session.ShareOptions // outgoing FPS, slow-viewer policy, floor, smaller copy, upload limit
	audio       audio.Selection      // pre-call shared audio selection
	presets     []QualityPreset      // the user's own quality presets; kept on disk
	presetMu    sync.Mutex           // serializes preset changes and their saving
	srcAudioMu  sync.Mutex           // serializes SetSource and SetAudio: a call's sound follows the last one
	output      string               // pre-call playback device ("" = default)
	requirePw   bool                 // pre-call "Require a password" toggle
	roomPw      string               // pre-call room password
	e2ee        bool                 // pre-call "Encrypt end to end" toggle; kept on disk
	portMap     bool                 // "Open a port on my router automatically"; kept on disk
	sessPortMap bool                 // portMap setting active in the current session
	autoWatch   bool                 // start watching new remote streams automatically; kept on disk
	playSounds  bool                 // play notification sounds; kept on disk
	// hideChangelog: don't show what changed after an update; kept on disk.
	hideChangelog bool
	discord       Discord        // the user's Discord voice channel (RunDiscord)
	servers       serverSettings // the user's signaling servers; kept on disk
	picked        string         // the lobby's server for new sessions; "" = the default
	recent        []recentEntry  // last 5 rooms created or joined; kept on disk
	recentGen     uint64         // bumps on each ProbeRecentRooms pass
	// discordAuth wakes RunDiscord to ask Discord for permission.
	discordAuth chan struct{}
	// pins are the pinned hosts (PinnedHost).
	pins atomic.Pointer[map[string]bool]
	// discordUnlink wakes the follow socket to unlink the Discord account.
	discordUnlink chan struct{}
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
	Logs func() any
	// SendLogs: anon = an anonymous report (see AnonymizeLogs).
	SendLogs func(ctx context.Context, anon bool) error
	OpenLogs func() error
	// SendFeedback sends an idea or a problem the user wrote, with
	// diagnostics ("" = without); nil = not available.
	SendFeedback func(ctx context.Context, kind, text, diag string) error
	// System and RecentLog feed Diagnostics: this computer in a line, and
	// the last lines of our log. nil = left out.
	System    func() string
	RecentLog func() string
	// Show brings the native window to the front.
	Show func()
	// ConfirmServer asks the user whether to join a session on host, a
	// server not run by Bellion Share, and waits for the answer. nil =
	// such links are refused.
	ConfirmServer func(host string) bool
}

// ErrUpdateRequired: the installed build is below the update channel's
// minimum, so no new sessions until it's updated.
var ErrUpdateRequired = errors.New("this Bellion Share is too old: install the update first")

func modeFromShareOptions(o session.ShareOptions) media.ContentMode {
	if o.EffectiveSlowViewer() == session.SlowResolution {
		return media.ModeGame
	}
	return media.ModeDesktop
}

func orAppRes(r media.Resolution) media.Resolution {
	if r == "" {
		return media.Res720
	}
	return r
}

// New returns an App that creates sessions with cfg.
func New(cfg session.Config, def media.SourceKind) *App {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	autoWatch := loadAutoWatch()
	if cfg.AutoWatch != nil {
		autoWatch = *cfg.AutoWatch
	}
	playSounds := loadPlaySounds()
	if cfg.PlaySounds != nil {
		playSounds = *cfg.PlaySounds
	}
	audio.SetIncludeVoice(LoadVoiceInSystem())
	portMap := LoadPortMap()
	if cfg.Port != nil {
		cfg.Port.SetMapping(portMap)
	}
	res, opts, e2ee := loadShareSettings(cfg.UploadKbps)
	if cfg.SelfView == "" {
		cfg.SelfView = loadSettings().SelfView
	}
	a := &App{
		cfg:           cfg,
		nameChosen:    identity.LoadName() != "",
		log:           cfg.Log,
		DefaultSource: def,
		homeSource:    "primary",
		res:           res,
		mode:          modeFromShareOptions(opts),
		shareOpts:     opts,
		audio:         audio.Selection{Mode: "none"},
		presets:       loadPresets(),
		e2ee:          e2ee,
		portMap:       portMap,
		autoWatch:     autoWatch,
		playSounds:    playSounds,
		hideChangelog: loadSettings().HideChangelog,
		servers:       loadServers(),
		recent:        loadRecent(cfg.Server),
		discordAuth:   make(chan struct{}, 1),
		discordUnlink: make(chan struct{}, 1),
	}
	if a.cfg.OnSelfView == nil {
		a.cfg.OnSelfView = a.keepSelfView
	}
	a.updatePinsLocked()
	return a
}

// ShareConfig describes our current or pre-call share options for the UI.
type ShareConfig struct {
	Source             string          `json:"source"`                    // "primary", "none", "screen", "test", "monitor:...", "window:..."
	SourceName         string          `json:"-"`                         // a picked window's title (or app) or monitor's name; never a handle. Not in JSON: logs carry the state
	Resolution         string          `json:"resolution"`                // "720p", "1080p", "2160p"
	FPS                int             `json:"fps"`                       // 10..60 step 5
	SlowViewer         string          `json:"slowViewer,omitempty"`      // "" (auto), "resolution", "fps"
	EffectiveSlow      string          `json:"effectiveSlow"`             // "resolution" or "fps"
	MinHeight          int             `json:"minHeight"`                 // 360, 540, 720
	SmallerCopy        bool            `json:"smallerCopy"`               // default true
	KeepQuality        bool            `json:"keepQuality,omitempty"`     // a slow encoder never lowers the resolution
	UploadKbps         int             `json:"uploadKbps,omitempty"`      // 0 = Automatic, >0 = kbit/s
	Audio              audio.Selection `json:"audio"`                     // "none", "system", "apps", "window", "test"
	Output             string          `json:"output"`                    // "" = system default
	RequirePassword    bool            `json:"requirePassword,omitempty"` // pre-call room creation option
	RoomPassword       string          `json:"-"`                         // pre-call custom password (not exposed via JSON)
	HasPassword        bool            `json:"hasPassword,omitempty"`
	E2EE               bool            `json:"e2ee"`                         // pre-call E2EE option (default true)
	PortMap            bool            `json:"portMap"`                      // router port mapping (default true)
	PortMapNextSession bool            `json:"portMapNextSession,omitempty"` // true if changed during an active call
	NoWindowAudio      bool            `json:"noWindowAudio,omitempty"`      // the shared window's process is unknown: its app's audio can't be shared
	Presets            []QualityPreset `json:"presets,omitempty"`            // the user's own; BuiltinPresets come first
}

// UIState is what /api/state returns.
type UIState struct {
	Session *session.State `json:"session"`
	Share   ShareConfig    `json:"share"`
	Error   string         `json:"error,omitempty"`
	Version string         `json:"version"` // our build, e.g. "0.1.0 (57) linux"
	// Name and Tag: who we are (name#tag), also outside a session.
	Name string `json:"name"`
	// NameChosen: the user saved a name. Until then Name is two random
	// words, and the app asks once whether to keep or change them.
	NameChosen bool         `json:"nameChosen"`
	Tag        string       `json:"tag"`
	Update     *update.Info `json:"update,omitempty"`
	// Autostart is whether Blite starts at sign-in; nil where it can't
	// (dev builds).
	Autostart *bool `json:"autostart,omitempty"`
	// AutoWatch is whether new remote streams open automatically.
	AutoWatch bool `json:"autoWatch"`
	// PlaySounds is whether call notification sounds play.
	PlaySounds bool `json:"playSounds"`
	// VoiceInSystem: whole-system share audio includes voice chat apps.
	VoiceInSystem bool `json:"voiceInSystem,omitempty"`
	// Build is our build number (0 for dev builds).
	Build int `json:"build,omitempty"`
	// HideChangelog: the user doesn't want to see what changed after an
	// update (Help > What's new still shows it).
	HideChangelog bool `json:"hideChangelog,omitempty"`
	// AnonymizeLogs: the last choice of "Anonymize" for Send logs; crash
	// reports follow it too.
	AnonymizeLogs bool `json:"anonymizeLogs,omitempty"`
	Logs          any  `json:"logs,omitempty"`
	// Reach says whether others can reach us from the internet.
	Reach *Reach `json:"reach,omitempty"`
	// Discord: the user's voice channel and its session (nil until the
	// watcher runs).
	Discord *Discord `json:"discord,omitempty"`
	// Servers: the signaling servers and which one new sessions use.
	Servers *Servers `json:"servers,omitempty"`
	// Recent: the last 5 rooms created or joined, newest first (no passwords).
	Recent []RecentRoom `json:"recent,omitempty"`
}

// Reach is the reachability line; OK false is a warning.
type Reach struct {
	OK   bool   `json:"ok"`
	Text string `json:"text"`
}

// State implements ui.Backend.
func (a *App) State() any {
	a.mu.Lock()
	s, e, autoWatch, playSounds, hideChangelog := a.sess, a.lastErr, a.autoWatch, a.playSounds, a.hideChangelog
	portMap, sessPortMap := a.portMap, a.sessPortMap
	opts := session.NormalizeShareOptions(a.shareOpts)
	homeSrc := a.homeSource
	if homeSrc == "" {
		homeSrc = "primary"
	}
	asel := a.audio
	if asel.Mode == "" {
		asel.Mode = "none"
	}
	srcInfo := a.srcInfo
	presets := slices.Clone(a.presets)
	share := ShareConfig{
		Source:             homeSrc,
		Resolution:         string(orAppRes(a.res)),
		FPS:                opts.FPS,
		SlowViewer:         opts.SlowViewer,
		EffectiveSlow:      opts.EffectiveSlowViewer(),
		MinHeight:          opts.MinHeight,
		SmallerCopy:        opts.SmallerCopy,
		KeepQuality:        opts.KeepQuality,
		UploadKbps:         opts.UploadKbps,
		Audio:              asel,
		Output:             a.output,
		RequirePassword:    a.requirePw,
		RoomPassword:       a.roomPw,
		HasPassword:        a.roomPw != "",
		E2EE:               a.e2ee,
		PortMap:            portMap,
		PortMapNextSession: s != nil && portMap != sessPortMap,
		Presets:            presets,
	}
	var dc *Discord
	if a.discord.Status != "" {
		d := a.discord
		dc = &d
	}
	servers := a.serverState()
	recent := a.recentStateLocked()
	a.mu.Unlock()
	st := UIState{Share: share, Error: e, Version: a.cfg.Client.Label(), Name: a.cfgName(), NameChosen: a.isNameChosen(), AutoWatch: autoWatch, PlaySounds: playSounds, VoiceInSystem: audio.IncludeVoice(), HideChangelog: hideChangelog, AnonymizeLogs: AnonymizeLogs(), Discord: dc, Servers: servers, Recent: recent}
	if a.cfg.Client != nil {
		st.Build = a.cfg.Client.Build
	}
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
	if a.cfg.Port != nil && a.cfg.Port.Mapping() {
		ok, text := a.cfg.Port.State().Describe()
		st.Reach = &Reach{OK: ok, Text: text}
	}
	if s != nil {
		ss := s.State()
		st.Session = &ss
		curSrc := ss.Source
		if curSrc == "" {
			curSrc = "none"
		}
		st.Share.Source = curSrc
		st.Share.SourceName = ""
		st.Share.Resolution = ss.Resolution
		st.Share.FPS = ss.ShareOptions.FPS
		st.Share.SlowViewer = ss.ShareOptions.SlowViewer
		st.Share.EffectiveSlow = ss.EffectiveSlow
		st.Share.MinHeight = ss.ShareOptions.MinHeight
		st.Share.SmallerCopy = ss.ShareOptions.SmallerCopy
		st.Share.KeepQuality = ss.ShareOptions.KeepQuality
		st.Share.UploadKbps = ss.ShareOptions.UploadKbps
		st.Share.Audio = ss.Audio
		st.Share.Output = ss.Output
	}
	if st.Share.Source == srcInfo.src && srcInfo.src != "" {
		st.Share.SourceName = srcInfo.name
		st.Share.NoWindowAudio = isWindowSource(srcInfo.src) && srcInfo.pid == 0
	}
	return st
}

func (a *App) config() session.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	cfg := a.cfg
	on := a.autoWatch
	ps := a.playSounds
	cfg.AutoWatch = &on
	cfg.PlaySounds = &ps
	return cfg
}

// KnownNames are the names the app knows of: ours, the members', Discord's,
// recent rooms' and servers'. An anonymous report replaces them.
func (a *App) KnownNames() []string {
	st, _ := a.State().(UIState)
	n := []string{st.Name}
	if s := st.Session; s != nil {
		n = append(n, s.Room)
		for _, m := range s.Members {
			n = append(n, m.Name)
		}
	}
	if d := st.Discord; d != nil {
		n = append(n, d.User, d.Guild, d.Channel, d.ChannelID)
	}
	for _, r := range st.Recent {
		n = append(n, r.Code, r.Display, r.Host)
	}
	if sv := st.Servers; sv != nil {
		for _, s := range sv.List {
			if !s.BuiltIn {
				n = append(n, s.URL, s.Host)
			}
		}
	}
	return n
}

// DisplayName is our name#tag, for reports.
func (a *App) DisplayName() string {
	n := a.cfgName()
	if a.cfg.Identity.Private != nil {
		n += "#" + identity.Tag(a.cfg.Identity.Public())
	}
	return n
}

// AnonymizeLogs reports whether reports should be anonymous (the user's
// last choice in Send logs).
func AnonymizeLogs() bool { return loadSettings().AnonymizeLogs }

// SendLogsNow implements ui.Backend: anon sends an anonymous report, and
// is remembered for the next one and for crash reports.
func (a *App) SendLogsNow(ctx context.Context, anon bool) error {
	if a.SendLogs == nil {
		return errors.New("this build can't send logs")
	}
	if err := updateSettings(func(s *appSettings) { s.AnonymizeLogs = anon }); err != nil {
		a.log.Warn("save anonymize setting", "err", err)
	}
	err := a.SendLogs(ctx, anon)
	a.bc.Notify()
	return err
}

// Feedback implements ui.Backend: kind is "idea" or "problem".
func (a *App) Feedback(ctx context.Context, kind, text string, withDiag bool) error {
	text = strings.TrimSpace(text)
	switch {
	case a.SendFeedback == nil:
		return errors.New("this build can't send feedback")
	case kind != "idea" && kind != "problem":
		return errors.New("unknown kind of feedback")
	case text == "":
		return errors.New("write something first")
	case len(text) > maxFeedback:
		return fmt.Errorf("that's too long: %d characters at most", maxFeedback)
	}
	diag := ""
	if withDiag {
		diag = a.Diagnostics()
	}
	return a.SendFeedback(ctx, kind, text, diag)
}

const maxFeedback = 8000

// Diagnostics implements ui.Backend: this computer, the call, and the
// last log lines, with links and passwords cut out.
func (a *App) Diagnostics() string {
	var b strings.Builder
	if a.System != nil {
		b.WriteString(a.System())
	}
	fmt.Fprintf(&b, "language: %s (system: %s, preference: %s)\n",
		i18n.Current(), orNone(strings.Join(i18n.SystemLocales(), ", ")), orNone(i18n.Preference()))
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s != nil {
		b.WriteString(s.Diagnostics())
	} else {
		b.WriteString("not in a call\n")
	}
	if a.RecentLog != nil {
		b.WriteString("\nrecent log:\n")
		b.WriteString(redact.Text(a.RecentLog()))
	}
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// OpenLogFolder implements ui.Backend.
func (a *App) OpenLogFolder() error {
	if a.OpenLogs == nil {
		return errors.New("no log folder")
	}
	return a.OpenLogs()
}

func (a *App) cfgName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Name
}

// ShowUI implements ui.Backend.
func (a *App) ShowUI() {
	if a.Show != nil {
		a.Show()
	}
}

// Subscribe implements ui.Backend.
func (a *App) Subscribe() (<-chan struct{}, func()) { return a.bc.Subscribe() }

// Notify tells the UI that the state changed.
func (a *App) Notify() { a.bc.Notify() }

func (a *App) adopt(s *session.Session) { a.adoptAs(s, a.DefaultSource) }

func (a *App) applyPortMap(on bool) {
	if a.cfg.Port != nil {
		a.cfg.Port.SetMapping(on)
	}
}

func (a *App) syncPortMapForNextSession() bool {
	a.mu.Lock()
	want := a.portMap
	a.mu.Unlock()
	a.applyPortMap(want)
	return want
}

func (a *App) applyPortMapIfIdle() {
	a.mu.Lock()
	idle := a.sess == nil
	want := a.portMap
	a.mu.Unlock()
	if idle {
		a.applyPortMap(want)
	}
}

// adoptAs makes s the live session, sharing src first ("" or "none" =
// nothing).
func (a *App) adoptAs(s *session.Session, src media.SourceKind) {
	// The first audio and source, in order with SetSource and SetAudio
	// (never called with srcAudioMu held: those don't start sessions).
	a.srcAudioMu.Lock()
	defer a.srcAudioMu.Unlock()
	rj := s.Rejoin()
	a.recordRecent(rj.Server, rj.Room, rj.Password)
	s.OnChange(a.bc.Notify)
	a.mu.Lock()
	a.sess, a.lastErr = s, ""
	if a.cfg.Port != nil {
		a.sessPortMap = a.cfg.Port.Mapping()
	} else {
		a.sessPortMap = a.portMap
	}
	if isWindowSource(string(src)) && string(src) != a.homeSource {
		a.audio = audioForSource(string(src), a.audio) // a window from elsewhere (flag, link)
	}
	res, opts, asel, out, playSounds := a.res, a.shareOpts, a.audio, a.output, a.playSounds
	a.mu.Unlock()
	s.SetPlaySounds(playSounds)
	s.SetResolution(res)
	s.SetShareOptions(opts)
	if out != "" {
		s.SetOutput(out)
	}
	// A shared window's app audio, if that's the choice (the session finds
	// the process once the window is shared); else the audio chosen.
	s.SetWindowAudio(asel.Mode == "window")
	if asel.Mode != "" && asel.Mode != "none" && asel.Mode != "window" {
		if err := s.SetAudio(asel); err != nil {
			a.log.Warn("initial audio", "err", err)
		}
	}
	go func() {
		<-s.Done()
		a.mu.Lock()
		lost := false
		if a.sess == s {
			a.sess = nil
			if r := s.EndReason(); r != "" {
				a.lastErr = r
			}
			lost = s.SlotLost()
		}
		a.mu.Unlock()
		if lost {
			a.rejoinLost(s.Rejoin())
			return
		}
		a.applyPortMapIfIdle()
		a.bc.Notify()
	}()
	if src == "primary" {
		src = media.SourceKind(capture.PrimarySource())
	}
	if src != "" && src != "none" {
		if err := s.SetSource(src); err != nil {
			a.setErr("could not start sharing: " + err.Error())
		}
	}
	a.bc.Notify()
}

// rejoinLost joins the room again, once, after the server lost our slot
// (session.SlotLost): a restart of the server we resumed on, or a
// failover that put us on another server. It is a fresh join, as after an
// in-call update: new peer ID and connections, same room, password,
// source and audio. If it fails, the session's end reason stays.
func (a *App) rejoinLost(rj session.Rejoin) {
	a.log.Info("the server lost our slot; rejoining the room")
	a.bc.Notify()
	if a.busy() {
		return // the user started another session meanwhile
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a.syncPortMapForNextSession()
	cfg := a.config()
	if rj.Server != "" {
		cfg.Server = rj.Server
	}
	s, err := session.Join(ctx, cfg, rj.Room, rj.Password)
	if err != nil {
		a.log.Warn("rejoin after a lost slot", "err", err)
		a.setErr("lost the connection to the room, and could not rejoin: " + friendly(err).Error())
		a.applyPortMapIfIdle()
		return
	}
	a.mu.Lock()
	busy := a.sess != nil
	a.mu.Unlock()
	if busy { // the user was quicker
		s.Close()
		return
	}
	a.adoptAs(s, rj.Source)
	a.log.Info("rejoined the room after a lost slot")
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
func (a *App) Create(ctx context.Context, password string, e2ee bool) error {
	return a.create(ctx, password, e2ee, "")
}

// create starts a session, tied to Discord voice channel channel if set.
func (a *App) create(ctx context.Context, password string, e2ee bool, channel string) error {
	return friendly(a.createRaw(ctx, password, e2ee, channel, a.DefaultSource))
}

func (a *App) createRaw(ctx context.Context, password string, e2ee bool, channel string, src media.SourceKind) error {
	if a.busy() {
		return ErrInSession
	}
	if a.Updates.Mandatory() {
		return ErrUpdateRequired
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	a.syncPortMapForNextSession()
	a.mu.Lock()
	cfg := a.cfg
	on := a.autoWatch
	ps := a.playSounds
	cfg.AutoWatch = &on
	cfg.PlaySounds = &ps
	cfg.Server = a.selectedLocked()
	a.mu.Unlock()
	cfg.E2EE, cfg.Channel = e2ee, channel
	s, err := session.Create(ctx, cfg, password)
	if err != nil {
		return err
	}
	a.adoptAs(s, src)
	return nil
}

// JoinLink implements ui.Backend: accepts either a share link (whose room is
// on the link's server) or a bare room code (joined on the selected server).
func (a *App) JoinLink(ctx context.Context, link string) error {
	server, code, pw, isLink, err := session.ParseJoinInput(link)
	if err != nil {
		return err
	}
	if isLink {
		server = a.linkServer(server)
		if a.busy() {
			return ErrInSession
		}
		if err := a.confirmServer(server); err != nil {
			return err
		}
	} else {
		if a.busy() {
			return ErrInSession
		}
		a.mu.Lock()
		server = a.selectedLocked()
		a.mu.Unlock()
	}
	return a.join(ctx, server, code, pw)
}

// join joins room code on server.
func (a *App) join(ctx context.Context, server, code, pw string) error {
	return friendly(a.joinRaw(ctx, server, code, pw, a.DefaultSource))
}

func (a *App) joinRaw(ctx context.Context, server, code, pw string, src media.SourceKind) error {
	if a.busy() {
		return ErrInSession
	}
	if a.Updates.Mandatory() {
		return ErrUpdateRequired
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	a.syncPortMapForNextSession()
	cfg := a.config()
	cfg.Server = server
	s, err := session.Join(ctx, cfg, code, pw)
	if err != nil {
		return err
	}
	a.adoptAs(s, src)
	return nil
}

func isProtoErr(err error, code string) bool {
	var se sigclient.ServerError
	return errors.As(err, &se) && string(se) == code
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
	a.applyPortMapIfIdle()
	a.bc.Notify()
}

// SetSource implements ui.Backend. Outside a session it sets the source new
// "Go live" sessions start sharing ("primary", "window:...", "screen", "test",
// or "none").
//
// Picking another window turns sound on (its app's audio); picking anything
// else turns off sound that followed a window and keeps a choice the user
// made. Applying the same source again (Go live) keeps the sound as it is.
func (a *App) SetSource(kind string) error {
	if kind == "" {
		kind = "none"
	}
	a.srcAudioMu.Lock()
	defer a.srcAudioMu.Unlock()
	a.nameSource(kind)
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s == nil {
		if kind != "primary" && kind != "none" && !media.ValidSource(media.SourceKind(kind)) {
			return errors.New("unknown source")
		}
		a.mu.Lock()
		a.pickSourceLocked(kind)
		a.mu.Unlock()
		a.bc.Notify()
		return nil
	}
	orig := kind
	if kind == "primary" { // Go live: the main monitor, not all of them combined
		kind = capture.PrimarySource()
	}
	if kind != "none" && !media.ValidSource(media.SourceKind(kind)) {
		return errors.New("unknown source")
	}
	a.mu.Lock()
	if orig != "none" {
		a.pickSourceLocked(orig)
	}
	follow := a.audio.Mode == "window"
	a.mu.Unlock()
	// Before SetSource when off, so it doesn't share the window's app even
	// for a moment; after it when on, so the audio restarts once.
	if !follow {
		s.SetWindowAudio(false)
	}
	err := s.SetSource(media.SourceKind(kind))
	if follow {
		s.SetWindowAudio(true)
	}
	return err
}

// pickSourceLocked makes kind the home source; a different one sets the
// sound by audioForSource. A window whose process we can't find (no
// _NET_WM_PID, an elevated app) gets no sound: its app's audio can't be
// told apart. a.mu is held.
func (a *App) pickSourceLocked(kind string) {
	if kind != a.homeSource {
		a.audio = audioForSource(kind, a.audio)
		if a.audio.Mode == "window" && a.srcInfo.src == kind && a.srcInfo.pid == 0 {
			a.audio = audio.Selection{Mode: "none"}
		}
	}
	a.homeSource = kind
}

// sourceInfo is a picked window or monitor: its name for people and, for a
// window, the process that owns it (0 = unknown).
type sourceInfo struct {
	src, name string
	pid       int
}

// windowPID is capture.WindowPID; tests replace it.
var windowPID = capture.WindowPID

// nameSource looks up what to call a picked window or monitor on Home: its
// title, or its app when untitled; each pick looks again (titles change).
// Never the handle in the source string.
func (a *App) nameSource(kind string) {
	if !strings.HasPrefix(kind, "window:") && !strings.HasPrefix(kind, "monitor:") {
		return
	}
	name, pid := capture.SourceName(kind), windowPID(kind)
	a.mu.Lock()
	if name == "" && a.srcInfo.src == kind { // a window that just closed keeps its last name
		name = a.srcInfo.name
	}
	a.srcInfo = sourceInfo{kind, name, pid}
	a.mu.Unlock()
}

// SetResolution implements ui.Backend: 720p, 1080p or 2160p for what we
// share, now and in later sessions.
func (a *App) SetResolution(res string) error {
	r := media.Resolution(res)
	if !media.ValidResolution(r) {
		return errors.New("unknown resolution")
	}
	if err := saveResolution(string(r)); err != nil {
		a.log.Warn("save settings", "err", err)
	}
	a.mu.Lock()
	a.res = r
	s := a.sess
	a.mu.Unlock()
	if s == nil {
		a.bc.Notify()
		return nil
	}
	return s.SetResolution(r)
}

// SetShareOptions implements ui.Backend: frame rate (10..60 step 5),
// slow-viewer policy ("" auto, "resolution", "fps"), lowest resolution floor
// (360/540/720), smaller copy toggle, keep the chosen resolution, and upload
// limit (kbit/s, 0 = Automatic).
func (a *App) SetShareOptions(fps int, slowViewer string, minHeight int, smallerCopy, keepQuality bool, uploadKbps int) error {
	opts := session.NormalizeShareOptions(session.ShareOptions{
		FPS:         fps,
		SlowViewer:  slowViewer,
		MinHeight:   minHeight,
		SmallerCopy: smallerCopy,
		KeepQuality: keepQuality,
		UploadKbps:  uploadKbps,
	})
	a.mu.Lock()
	prev := session.NormalizeShareOptions(a.shareOpts)
	flagSet := a.cfg.UploadKbps > 0
	a.shareOpts = opts
	a.mode = modeFromShareOptions(opts)
	s := a.sess
	a.mu.Unlock()
	if err := saveShareOptions(opts, opts.UploadKbps != prev.UploadKbps, flagSet); err != nil {
		a.log.Warn("save settings", "err", err)
	}
	if s == nil {
		a.bc.Notify()
		return nil
	}
	return s.SetShareOptions(opts)
}

// SetMode implements ui.Backend: desktop or game, now and in later sessions.
func (a *App) SetMode(mode string) error {
	m := media.ContentMode(mode)
	if !media.ValidMode(m) {
		return errors.New("unknown mode")
	}
	a.mu.Lock()
	opts := a.shareOpts
	a.mu.Unlock()
	if m == media.ModeGame {
		opts.FPS = 60
		opts.SlowViewer = session.SlowResolution
	} else {
		opts.FPS = 30
		opts.SlowViewer = session.SlowFPS
	}
	return a.SetShareOptions(opts.FPS, opts.SlowViewer, opts.MinHeight, opts.SmallerCopy, opts.KeepQuality, opts.UploadKbps)
}

// SetRoomSecurity implements ui.Backend: pre-call room password and E2EE
// toggles for new rooms created from Home.
func (a *App) SetRoomSecurity(requirePw bool, password string, e2ee bool) error {
	if err := saveE2EE(e2ee); err != nil {
		a.log.Warn("save settings", "err", err)
	}
	a.mu.Lock()
	a.requirePw = requirePw
	a.roomPw = password
	a.e2ee = e2ee
	a.mu.Unlock()
	a.bc.Notify()
	return nil
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
	if mode == "" {
		mode = "none"
	}
	switch mode {
	case "none", "system", "apps", "window", "test":
	default:
		return errors.New("unknown audio mode")
	}
	sel := audio.Selection{Mode: mode, Apps: slices.Clone(apps)}
	a.srcAudioMu.Lock()
	defer a.srcAudioMu.Unlock()
	a.mu.Lock()
	a.audio = sel
	s := a.sess
	a.mu.Unlock()
	if s == nil {
		a.bc.Notify()
		return nil
	}
	if mode == "window" { // the shared window's app: the session knows which
		s.SetWindowAudio(true)
		return nil
	}
	// Any other choice holds until another window is picked.
	s.SetWindowAudio(false)
	return s.SetAudio(sel)
}

// SetOutput implements ui.Backend.
func (a *App) SetOutput(device string) error {
	a.mu.Lock()
	a.output = device
	s := a.sess
	a.mu.Unlock()
	if s == nil {
		a.bc.Notify()
		return nil
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

func (a *App) isNameChosen() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.nameChosen
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
	a.cfg.Name, a.nameChosen = name, true
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

// InstallUpdate implements ui.Backend. molt-swap restarts Blite; during a
// call we write the call to the resume file first and leave saying we
// update, so the next build rejoins it (and an owner keeps the room).
func (a *App) InstallUpdate(ctx context.Context) error {
	if a.Updates == nil {
		return errors.New("this build doesn't update itself")
	}
	// Download and verify while the call goes on, however long a slow link
	// takes (a stalled download is resumed). A second click while it runs
	// changes nothing: the progress shows.
	if err := a.Updates.Install(context.WithoutCancel(ctx)); err != nil {
		a.bc.Notify()
		if errors.Is(err, update.ErrInstalling) {
			return nil
		}
		return err
	}
	inCall := a.leaveForUpdate()
	a.log.Info("update handed to molt-swap; exiting", "inCall", inCall)
	if a.Exit != nil {
		go a.Exit()
	}
	return nil
}

// leaveForUpdate writes the call (if any) to the resume file and leaves it
// saying we update.
func (a *App) leaveForUpdate() (inCall bool) {
	a.mu.Lock()
	s := a.sess
	a.sess = nil
	a.mu.Unlock()
	if s == nil {
		return false
	}
	if err := writeResume(s.Rejoin(), time.Now()); err != nil {
		a.log.Warn("resume file: the call won't rejoin by itself", "err", err)
	}
	s.CloseForUpdate()
	a.bc.Notify()
	return true
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

// SetHideChangelog implements ui.Backend: "Don't show change logs again",
// kept on disk.
func (a *App) SetHideChangelog(on bool) error {
	if err := saveHideChangelog(on); err != nil {
		a.log.Warn("save settings", "err", err)
	}
	a.mu.Lock()
	a.hideChangelog = on
	a.mu.Unlock()
	a.bc.Notify()
	return nil
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
	proto.ErrChannelBusy:    "someone already started a session in this Discord channel: watch it instead",
	proto.ErrNoRoom:         "that session doesn't exist (it ends when everyone leaves)",
	proto.ErrRoomFull:       "that session is full (the server limits room size)",
	proto.ErrRateLimited:    "too many attempts; wait a few minutes",
	proto.ErrBadRequest:     "password must be at least 4 characters",
	proto.ErrUpdateRequired: "this Bellion Share is too old for the server: please update it",
}

func friendly(err error) error {
	if errors.Is(err, pin.ErrUnpinned) {
		return pin.ErrUnpinned // without the dial and TLS wrapping
	}
	var se sigclient.ServerError
	if errors.As(err, &se) {
		if msg, ok := serverMessages[string(se)]; ok {
			return errors.New(msg)
		}
	}
	return err
}
