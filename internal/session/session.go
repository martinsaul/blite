// Package session is one Blite call: signaling, the mesh, the local video source
// and the remote players. The CLI and the local UI both drive it.
package session

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"blite/internal/audio"
	"blite/internal/capture"
	"blite/internal/feeds"
	"blite/internal/i18n"
	"blite/internal/identity"
	"blite/internal/load"
	"blite/internal/media"
	"blite/internal/proto"
	"blite/internal/redact"
	"blite/internal/rtc"
	"blite/internal/sigclient"
)

// Config for a session.
type Config struct {
	Server   string // wss://.../ws
	Name     string
	View     media.ViewKind // window | grid | count | none
	Encoder  string         // force an encoder factory
	Relay    bool
	Identity identity.Key      // zero = ephemeral key (headless test peers)
	Known    *identity.Known   // trust-on-first-use store; nil = none
	Client   *proto.ClientMeta // our build, reported to the server and peers
	// HostRoom and HostKey (hosted relay): Create asks for this fixed room
	// code, or joins the room if it exists.
	HostRoom, HostKey string
	// HostOwners (hosted relay): identity keys of the members who take
	// ownership of the room from us while any of them is in it.
	HostOwners []string
	// NoPlayback skips all remote audio and video playback (hosted relay).
	NoPlayback bool
	// AutoWatch controls whether new remote shares open automatically
	// (nil defaults to true).
	AutoWatch *bool
	// PlaySounds controls whether quiet call notification chimes play on the
	// call's output device (nil defaults to true).
	PlaySounds *bool
	// SelfView is how our own preview shows when we start sharing: one of
	// the SelfView* values ("" = SelfViewCorner).
	SelfView string
	// OnSelfView gets the new SelfView when the user changes how our
	// preview shows (to keep it); nil: nothing.
	OnSelfView func(view string)
	// CanEncode overrides the encoder-headroom check (tests).
	CanEncode func() bool
	// UploadKbps (relay) caps what transcoded renditions may add to our
	// upload; 0 = no cap.
	UploadKbps int
	// E2EE (Create): ask for a room whose media is encrypted end to end.
	// Builds below proto.E2EEBuild (dev builds included) create plain rooms.
	E2EE bool
	// Channel ties a created room to this Discord voice channel ID.
	Channel string
	// Port is our UDP port for peers, shared by all sessions (nil = a
	// random port per connection).
	Port *rtc.Port
	Log  *slog.Logger
}

// Member is a remote peer as the UI sees it.
type Member struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Tag    string `json:"tag,omitempty"`    // from their identity key: shown as name#tag
	Client string `json:"client,omitempty"` // their build, e.g. "0.1.0 (57) windows"
	State  string `json:"state"`            // connecting | connected
	Frames int    `json:"frames,omitempty"` // decoded frames (count view)
	Pair   string `json:"pair,omitempty"`   // ICE pair type, e.g. host/srflx
	Layer  string `json:"layer,omitempty"`  // what we send them: high | 1080p | 720p | low | off
	// Recv is the height their provider (they, or the relay) says it sends
	// us; 0 = nothing or unknown.
	Recv int `json:"recv,omitempty"`
	// Notice: we can't see their stream, even LOW doesn't fit our link.
	Notice string  `json:"notice,omitempty"`
	Audio  int     `json:"audio,omitempty"` // audio buffers decoded from them
	RTTms  int     `json:"rttMs,omitempty"` // round trip on the selected ICE pair
	Tx     uint64  `json:"tx,omitempty"`    // bytes sent to them
	Rx     uint64  `json:"rx,omitempty"`    // bytes received from them
	Loss   float64 `json:"loss,omitempty"`  // fraction of our video they last reported lost
	// Rates over the last few seconds, why they get less than our
	// best (viewer | bandwidth | cpu), and a hint when the link looks bad.
	TxKbps int    `json:"txKbps,omitempty"`
	RxKbps int    `json:"rxKbps,omitempty"`
	FPSIn  int    `json:"fpsIn,omitempty"` // their video, decoded
	Limit  string `json:"limit,omitempty"`
	Hint   string `json:"hint,omitempty"`
	// Our viewer settings for them.
	Quality string `json:"quality"` // auto | low (best quality we ask them to send)
	// Auto is what we ask instead while nobody looks: low for a
	// small grid tile, off while every video window is minimized; "" none.
	Auto      string      `json:"auto,omitempty"`
	Sharing   bool        `json:"sharing,omitempty"`
	Watching  bool        `json:"watching,omitempty"`
	Eye       feeds.Eye   `json:"eye,omitempty"`
	FeedState feeds.State `json:"feedState,omitempty"`
	asked     int         // the qualityLayer we last asked them for
	top       int         // their HIGH height as last heard; 0 = not sharing
	Volume    float64     `json:"volume"` // playback gain, 0..2
	Muted     bool        `json:"muted,omitempty"`
	// Heard: their audio reaches us; Loud: it is sound, not silence.
	Heard bool `json:"heard,omitempty"`
	Loud  bool `json:"loud,omitempty"`
	// Owner: they own the room.
	Owner bool `json:"owner,omitempty"`
	// KeyChanged: this name was seen before with a different identity key.
	KeyChanged bool `json:"keyChanged,omitempty"`
}

// State is a snapshot for the UI.
type State struct {
	Room    string `json:"room"`    // XXXX-XXXX
	Link    string `json:"link"`    // share link with password
	Source  string `json:"source"`  // what we share
	Encoder string `json:"encoder"` // encoder in use
	LowKbps int    `json:"lowKbps"`
	// UpdateRecommended: the server would like us on a newer build.
	UpdateRecommended bool            `json:"updateRecommended,omitempty"` // LOW layer bitrate (follows the worst-off LOW viewer)
	Audio             audio.Selection `json:"audio"`                       // shared audio
	Output            string          `json:"output"`                      // playback device ("" = default)
	Members           []Member        `json:"members"`
	Safety            string          `json:"safety"` // compare aloud: same for everyone = nobody in the middle
	Started           string          `json:"started"`
	Password          string          `json:"-"`
	// AudioLevel is our shared audio's level over the last 30 s (nil
	// when we share none, or before the first 30 s).
	AudioLevel *media.AudioLevel `json:"audioLevel,omitempty"`
	// Relay is the active relays (the server's plan); empty = full mesh.
	Relay []RelayEntry `json:"relay,omitempty"`
	// RelayWaiting are the volunteers who don't relay now. RelayPaused:
	// an older member keeps the room on one relay and the room is bigger
	// than that serves (proto.RelayMaxPeers), so everyone meshes.
	RelayPaused  bool         `json:"relayPaused,omitempty"`
	RelayWaiting []RelayEntry `json:"relayWaiting,omitempty"`
	// RelayMaxPeers and MeshComfortPeers: see proto.
	RelayMaxPeers    int `json:"relayMaxPeers"`
	MeshComfortPeers int `json:"meshComfortPeers"`
	// Resolution is what we share at: 720p, 1080p or 2160p.
	Resolution string `json:"resolution"`
	// Mode is what the share is tuned for: desktop or game; HighFPS the
	// frame rate HIGH runs at now (lowered for a slow encoder or link).
	Mode            string       `json:"mode"`
	ShareOptions    ShareOptions `json:"shareOptions"`
	EffectiveSlow   string       `json:"effectiveSlow"`
	UploadLimitKbps int          `json:"uploadLimitKbps,omitempty"`
	AutoUploadKbps  int          `json:"autoUploadKbps,omitempty"`
	PeakUploadKbps  int          `json:"peakUploadKbps,omitempty"`
	TxKbps          int          `json:"txKbps,omitempty"`
	HighFPS         int          `json:"highFps,omitempty"`
	// SourceNote: sharing stopped on its own (the shared window closed, or
	// the capture failed).
	SourceNote string `json:"sourceNote,omitempty"`
	// ResolutionNote: we stepped the resolution down because our encoder
	// couldn't keep up.
	ResolutionNote string `json:"resolutionNote,omitempty"`
	// RelayRefused: the server refused our volunteering because a member's
	// build can't do relay mode.
	RelayRefused bool `json:"relayRefused,omitempty"`
	// Encrypted: the room's media is encrypted end to end.
	Encrypted bool `json:"encrypted,omitempty"`
	// Reconnecting: our signaling connection dropped and we are resuming
	// (media keeps flowing meanwhile). GiveUpAt is when resuming stops.
	Reconnecting bool   `json:"reconnecting,omitempty"`
	GiveUpAt     string `json:"giveUpAt,omitempty"`
	// IsOwner: we own the room and may transfer ownership and kick.
	IsOwner bool `json:"isOwner,omitempty"`
	// IDKey is our identity key (a hosted relay's owners list names these).
	IDKey string `json:"idKey"`
	// Name and Tag are ours: we show as Name#Tag.
	Name string `json:"name"`
	Tag  string `json:"tag"`
	// SelfEye and SelfState track our own preview feed ("self").
	SelfEye   feeds.Eye   `json:"selfEye,omitempty"`
	SelfState feeds.State `json:"selfState,omitempty"`
	// AutoWatch: start watching new remote streams automatically.
	AutoWatch bool `json:"autoWatch"`
	// ShareNotice: short note ("NAME started sharing") when auto-watch is off.
	ShareNotice string `json:"shareNotice,omitempty"`
}

// Slow-viewer degradation policy ("When a viewer's connection is slow").
const (
	SlowAuto       = ""           // follows FPS: >= 45 -> SlowResolution, else SlowFPS
	SlowResolution = "resolution" // lower resolution first (keeps frame rate)
	SlowFPS        = "fps"        // lower frame rate first (keeps sharpness)
)

// ShareOptions controls outgoing video frame rate, slow-viewer degradation,
// lowest resolution floor, smaller copy, and mesh/relay upload budget.
type ShareOptions struct {
	FPS         int    `json:"fps"`                  // 10..60 in steps of 5 (default 30)
	SlowViewer  string `json:"slowViewer,omitempty"` // "" (auto), "resolution", or "fps"
	MinHeight   int    `json:"minHeight"`            // 360, 540, or 720 (default 360)
	SmallerCopy bool   `json:"smallerCopy"`          // send LOW / renditions to slow viewers (default true)
	UploadKbps  int    `json:"uploadKbps,omitempty"` // 0 = Automatic, >0 = total upload limit in kbit/s
}

// DefaultShareOptions returns the default outgoing share options.
func DefaultShareOptions() ShareOptions {
	return ShareOptions{
		FPS:         30,
		SlowViewer:  SlowAuto,
		MinHeight:   360,
		SmallerCopy: true,
		UploadKbps:  0,
	}
}

// NormalizeShareOptions clamps and cleans o to valid values.
func NormalizeShareOptions(o ShareOptions) ShareOptions {
	if o.FPS <= 0 {
		o.FPS = 30
	} else {
		o.FPS = min(max(((o.FPS+2)/5)*5, 10), 60)
	}
	switch o.SlowViewer {
	case SlowResolution, SlowFPS:
	default:
		o.SlowViewer = SlowAuto
	}
	switch o.MinHeight {
	case 540, 720:
	default:
		o.MinHeight = 360
	}
	if o.UploadKbps < 0 {
		o.UploadKbps = 0
	}
	return o
}

// EffectiveSlowViewer returns SlowResolution or SlowFPS, resolving SlowAuto
// from FPS (>= 45 -> SlowResolution, else SlowFPS).
func (o ShareOptions) EffectiveSlowViewer() string {
	if o.SlowViewer == SlowResolution || o.SlowViewer == SlowFPS {
		return o.SlowViewer
	}
	if o.FPS >= 45 {
		return SlowResolution
	}
	return SlowFPS
}

func modeForOptions(o ShareOptions) media.ContentMode {
	if o.EffectiveSlowViewer() == SlowResolution {
		return media.ModeGame
	}
	return media.ModeDesktop
}

func orRes(r media.Resolution) media.Resolution {
	if r == "" {
		return media.Res720
	}
	return r
}

// RelayEntry is one member in the relay queue.
type RelayEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Me   bool   `json:"me,omitempty"`
	// Mine: our relay. Children: members homed on it (active relays).
	Mine     bool `json:"mine,omitempty"`
	Children int  `json:"children,omitempty"`
}

// Session is a live call.
type Session struct {
	cfg  Config
	sig  *sigclient.Client
	mesh *rtc.Mesh
	pw   string

	mu       sync.Mutex
	idleMu   sync.Mutex // serializes updateCaptureIdle
	src      *media.VideoSource
	srcGen   uint64
	srcKind  media.SourceKind
	encoder  string
	members  map[string]*Member
	players  map[string]*media.Player
	aplayers map[string]*media.AudioPlayer
	hear     map[string]hearing // what the People list shows of each peer's audio
	sharer   *audio.Sharer
	feeds    *feeds.Machine

	shareNotice     string
	shareNoticePeer string

	relayRefused bool
	reconnAt     time.Time         // our signaling resume started; zero = connected
	endReason    string            // why the session ended, for the user ("" = we left)
	slotLost     bool              // ended because the server no longer had our slot (SlotLost)
	res          media.Resolution  // what we share at ("" = 720p)
	mode         media.ContentMode // what the share is tuned for ("" = desktop)
	shareOpts    ShareOptions      // outgoing FPS, slow-viewer policy, floor, upload limit
	cpuFPS       int               // HIGH frame rate the encoder keeps up with (0: full)
	bwFPS        int               // HIGH frame rate congested viewers take (0: full)
	rends        []int             // rendition heights viewers were granted
	resNote      string            // why we share below the chosen size
	// resWant is the size chosen before a slow encoder stepped us down
	// ("" = not stepped down); we try it again after resRetryWait, which
	// doubles on each failed try.
	resWant    media.Resolution
	resRetries int
	srcNote    string            // why sharing stopped on its own
	keyAt      map[int]time.Time // last forced keyframe per layer
	keys       keyStats
	gridFast   atomic.Bool // video windows run at media.GridFastFPS
	// srcMu serializes starting and stopping the capture. Two overlapping
	// starts (a double-clicked source; a start takes ~8 s on Windows)
	// each began a capture and the first was never stopped: two encoders
	// fed one track, so viewers saw the picture flip between sources and
	// break up. Not held around mesh calls (setRenditions runs under the
	// mesh's rendition lock).
	srcMu            sync.Mutex
	topMu            sync.Mutex // serializes mesh top/share-config updates against srcGen; never held under mu, srcMu or rend.mu
	shareMu          sync.Mutex // serializes sig.Sharing so last sent matches s.src != nil
	rendRollbackHook func(run func())
	// audioMu serializes SetAudio the same way: two overlapping
	// selections (the app list sends one per click) each started a
	// capture and the first was never stopped, so two captures fed one
	// track and listeners heard it cut in and out and race ahead.
	audioMu  sync.Mutex
	asrc     *media.AudioSource
	asrcDesc string
	alevel   *media.AudioLevel // asrc's level over the last 30 s
	asel     audio.Selection
	// audioBack is the audio that stopped with our video (Mode "": none):
	// it comes back when we go live again, unless another was chosen.
	audioBack audio.Selection
	// noWinAudio: sharing a window keeps the audio chosen instead of
	// sharing its app's (sound turned off, or other audio picked).
	noWinAudio bool
	output     string
	started    time.Time
	closed     bool
	done       chan struct{}
	onChange   func()

	gridMu sync.Mutex // serializes regrid; held before mu
	// grids are the video windows: key "" is the main streaming window and
	// key feedID is a pop-out window. gridKeys is what each shows, so
	// regrid skips no-op rebuilds.
	grids    map[string]*media.Grid
	gridKeys map[string]string
	// wins are our native video windows by the same key: they stay open
	// across rebuilds. openWindow opens one (tests replace it); nil: no
	// own windows.
	wins       map[string]media.Window
	winStop    map[media.Window]chan struct{}
	winMeta    map[media.Window]winMeta
	openWindow func(title string, w, h int) (media.Window, error)
	gridView   media.ViewKind // how grids show (tests: ViewCount)

	autoQMu     sync.Mutex   // serializes applyAutoQuality (held above gridMu and mu)
	autoQState  atomic.Int32 // 0: idle, 1: running, 2: running + rerun queued
	regridState atomic.Int32 // 0: idle, 1: running, 2: running + rerun queued
	// selfCh is the channel our capture feeds the self-preview on;
	// previewOn opens its valve (mu).
	selfCh    string
	previewOn bool
	// selfTile: in the main window our preview is a tile, not the corner
	// picture; selfView is how it showed last (a SelfView* value) (mu).
	selfTile bool
	selfView string
	// previewBig: regrid wants the big preview (see SetPreviewBig), for a
	// restarted source to start the same (mu).
	previewBig bool
	rates      rates // (mu)
	// Grid tiles are small (tests); every video window is minimized.
	smallTiles, hiddenWins atomic.Bool
	// stopped are the peers who stopped sharing (mu): their tile says so
	// for stoppedShow, fades out and goes. stopGen tells a stop's timers
	// from a later one's.
	stopped map[string]stopInfo
	stopGen int

	soundMu    sync.Mutex         // serializes splayer lifecycle; never locked under mu
	splayer    *media.SoundPlayer // chime player on audioSink()
	soundPend  []media.SoundKind
	soundBusy  bool
	soundBufs  int
	playSounds atomic.Bool
	soundGate  media.SoundGate
}

type winMeta struct {
	title string
	set   bool
}

// stopInfo is a peer's share that ended: when, and which stop.
type stopInfo struct {
	at  time.Time
	gen int
}

// A stopped share's tile shows for stoppedShow, then fades out over
// stoppedFade. Video from a stopped peer is dropped for stoppedGrace (in
// flight when they stopped); after that it means they share again and we
// missed the message saying so.
const (
	stoppedShow  = 5 * time.Second
	stoppedFade  = time.Second
	stoppedGrace = 1500 * time.Millisecond
)

// Create makes a new room. An empty password gets a random one.
func Create(ctx context.Context, cfg Config, password string) (*Session, error) {
	s := newSession(cfg, password)
	sig, err := sigclient.Create(ctx, cfg.Server, proto.Create{Password: password, Name: cfg.Name, IDKey: s.idKey(), Client: cfg.Client, Room: cfg.HostRoom, HostKey: cfg.HostKey, Owners: cfg.HostOwners, E2EE: cfg.E2EE && buildOf(cfg.Client) >= proto.E2EEBuild, Channel: cfg.Channel}, sigclient.Options{OnState: s.sigState, OnDrop: s.sigDrop})
	if err != nil {
		return nil, err
	}
	return s.start(sig)
}

// Join enters an existing room.
func Join(ctx context.Context, cfg Config, code, password string) (*Session, error) {
	s := newSession(cfg, password)
	sig, err := sigclient.Join(ctx, cfg.Server, proto.Join{Room: code, Password: password, Name: cfg.Name, IDKey: s.idKey(), Client: cfg.Client}, sigclient.Options{OnState: s.sigState, OnDrop: s.sigDrop})
	if err != nil {
		return nil, err
	}
	return s.start(sig)
}

func newSession(cfg Config, pw string) *Session {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Identity.Private == nil {
		cfg.Identity = identity.Ephemeral()
	}
	autoWatch := cfg.AutoWatch == nil || *cfg.AutoWatch
	playSounds := cfg.PlaySounds == nil || *cfg.PlaySounds
	switch cfg.SelfView {
	case SelfViewCorner, SelfViewTile, SelfViewWindow, SelfViewHidden:
	default:
		cfg.SelfView = SelfViewCorner
	}
	opts := DefaultShareOptions()
	opts.UploadKbps = max(0, cfg.UploadKbps)
	s := &Session{cfg: cfg, pw: pw, members: map[string]*Member{}, players: map[string]*media.Player{},
		aplayers: map[string]*media.AudioPlayer{}, feeds: feeds.New(autoWatch),
		shareOpts: opts, mode: modeForOptions(opts),
		asel: audio.Selection{Mode: "none"}, stopped: map[string]stopInfo{},
		done: make(chan struct{}), started: time.Now(), selfCh: media.NewPreviewChannel(),
		selfTile: cfg.SelfView == SelfViewTile, selfView: cfg.SelfView}
	if cfg.View == media.ViewWindow {
		s.openWindow = media.OpenVideoWindow
	}
	s.playSounds.Store(playSounds)
	return s
}

func (s *Session) idKey() string { return s.cfg.Identity.Public() }

// sigDrop logs why the signaling socket dropped.
func (s *Session) sigDrop(err error) {
	s.cfg.Log.Info("signaling dropped", "err", redact.Text(err.Error()))
}

// sigState follows our signaling connection: "reconnecting" while resuming.
func (s *Session) sigState(state string) {
	s.mu.Lock()
	if state == "reconnecting" {
		s.reconnAt = time.Now()
	} else {
		s.reconnAt = time.Time{}
	}
	s.mu.Unlock()
	if state == "connected" {
		s.reportSharing()
		s.mu.Lock()
		for _, m := range s.members {
			m.asked = -1
		}
		hasMesh := s.mesh != nil
		s.mu.Unlock()
		if hasMesh {
			s.triggerAutoQuality()
		}
	}
	s.cfg.Log.Info("signaling", "state", state)
	s.changed()
}

func (s *Session) reportSharing() {
	s.shareMu.Lock()
	defer s.shareMu.Unlock()
	s.mu.Lock()
	on, sig := s.src != nil, s.sig
	s.mu.Unlock()
	if sig != nil {
		sig.Sharing(on)
	}
}

func (s *Session) start(sig *sigclient.Client) (*Session, error) {
	s.mu.Lock()
	s.sig = sig
	s.mu.Unlock()
	for _, p := range sig.Welcome.Peers {
		s.members[p.PeerID] = &Member{ID: p.PeerID, Name: p.Name, State: "connecting", Quality: "auto", asked: -1, Volume: 1, KeyChanged: s.keyChanged(p.Name, p.IDKey)}
	}
	mesh, err := rtc.New(sig, rtc.Options{
		OnVideo: s.pushVideo,
		OnAudio: s.pushAudio,
		OnPeer:  s.peerEvent,
		OnJoined: func(id, name string) {
			s.playSound(media.SoundPeerJoined)
		},
		OnViewerWatching: func(id string) {
			s.playSound(media.SoundViewerWatching)
		},
		DefaultOff: !s.feeds.AutoWatch(),
		OnKeyframe: s.forceKeyframe,
		OnTopology: s.changed,
		OnOwner:    func(string) { s.changed() },
		OnRenamed: func(id, name string) {
			s.mu.Lock()
			if m := s.members[id]; m != nil {
				m.Name = name
				if s.mesh != nil {
					m.KeyChanged = m.KeyChanged || s.keyChanged(name, s.mesh.IDKey(id))
				}
			}
			s.mu.Unlock()
			s.changed()
		},
		OnKicked: func(by string, ban bool) {
			s.mu.Lock()
			s.endReason = by + " removed you from the room."
			if ban {
				s.endReason = by + " removed you from the room and banned you from rejoining."
			}
			s.mu.Unlock()
		},
		OnRelayRefused: func() {
			s.mu.Lock()
			s.relayRefused = true
			s.mu.Unlock()
			s.changed()
		},
		OnRenditions:  s.setRenditions,
		CanEncode:     s.canEncode,
		NewTranscoder: s.newTranscoder,
		UploadKbps:    s.cfg.UploadKbps,
		OnSending:     s.gotSending,
		OnHighFPS: func(fps int) {
			s.mu.Lock()
			s.bwFPS = fps
			s.mu.Unlock()
			s.applyHighFPS()
		},
		OnLowBitrate: func(kbps int) {
			s.mu.Lock()
			src := s.src
			s.mu.Unlock()
			if src != nil {
				src.SetBitrate(media.Low, kbps)
			}
		},
		OnBitrate: func(layer, kbps int) {
			s.mu.Lock()
			src := s.src
			s.mu.Unlock()
			if src != nil {
				src.SetBitrate(layer, kbps)
			}
		},
		Identity:   s.cfg.Identity,
		ForceRelay: s.cfg.Relay,
		Build:      buildOf(s.cfg.Client),
		CPUFree: func() int {
			cpuMeterOnce.Do(func() { cpuMeter = load.NewMeter(10 * time.Second) })
			return int(100 - 100*cpuMeter.Busy())
		},
		Headless: s.cfg.NoPlayback,
		Port:     s.cfg.Port,
		Log:      s.cfg.Log,
	})
	if err != nil {
		sig.Leave()
		return nil, err
	}
	s.mu.Lock()
	s.mesh = mesh
	s.mu.Unlock()
	s.syncMeshShareConfig()
	go func() {
		<-mesh.Done()
		if sigclient.SlotLost(mesh.SignalingErr()) {
			s.mu.Lock()
			s.slotLost = true
			if s.endReason == "" {
				s.endReason = "The server lost your place in the room (it restarted). Join again with the link."
			}
			s.mu.Unlock()
		}
		s.Close()
	}()
	go s.watchDecoders()
	go s.watchVisibility()
	go s.watchWaiting()
	go s.watchHearing()
	go s.watchAudioCapture()
	cpuMeterOnce.Do(func() { cpuMeter = load.NewMeter(10 * time.Second) })
	return s, nil
}

// cpuMeter measures our CPU use for rendition admission.
var (
	cpuMeterOnce sync.Once
	cpuMeter     *load.Meter
)

// canEncode reports whether one more encoder fits: a hardware encoder, or
// under half of all cores busy over the last 10 s.
func (s *Session) canEncode() bool {
	if s.cfg.CanEncode != nil {
		return s.cfg.CanEncode()
	}
	s.mu.Lock()
	enc := s.encoder
	s.mu.Unlock()
	if enc == "" {
		if e, err := media.PickEncoder(s.cfg.Encoder); err == nil {
			enc = e.Factory()
		}
	}
	if media.IsHardware(enc) {
		return true
	}
	cpuMeterOnce.Do(func() { cpuMeter = load.NewMeter(10 * time.Second) })
	busy := cpuMeter.Busy()
	s.cfg.Log.Info("encoder headroom", "encoder", enc, "busy", busy)
	return busy < 0.5
}

// setRenditions (mesh, as a provider of our own video) restarts our source
// with these renditions besides HIGH and LOW.
func (s *Session) setRenditions(heights []int) error {
	s.mu.Lock()
	s.rends = slices.Clone(heights)
	kind := s.srcKind
	s.mu.Unlock()
	if kind == "" {
		return errors.New("not sharing")
	}
	s.cfg.Log.Info("renditions", "heights", heights)
	gen, err := s.startSource(kind)
	if err != nil {
		s.mu.Lock()
		rollback := s.src == nil && s.srcGen == gen && !s.closed
		hook := s.rendRollbackHook
		s.mu.Unlock()
		if rollback {
			// setRenditions runs under mesh's rendition lock, so update mesh/server
			// and rebuild the grid asynchronously if startSource left us with no active source.
			run := func() {
				if s.clearMeshTop(gen) {
					s.reportSharing()
					s.regrid()
					s.windowAudio("")
					s.changed()
				}
			}
			go func() {
				if hook != nil {
					hook(run)
					return
				}
				run()
			}()
		}
	}
	return err
}

// newTranscoder (mesh, as relay) transcodes an origin's top to heights.
func (s *Session) newTranscoder(heights []int, out func(layer int, au []byte, d time.Duration)) (rtc.Transcoder, error) {
	t, err := media.StartTranscoder(heights, s.cfg.Encoder, func(layer int, au media.AccessUnit) {
		out(layer, au.Data, au.Duration)
	})
	if err != nil {
		return nil, err
	}
	go func() {
		if err := <-t.Err; err != nil {
			s.cfg.Log.Error("transcoder stopped", "err", err)
		}
	}()
	return t, nil
}

// gotSending records what a provider sends us of a member, and whether we
// can't see them at all. A top of 0 says they stopped sharing: if we were
// watching them, their tile says so and fades out instead of freezing on
// the last frame.
func (s *Session) gotSending(origin string, height, top int, congested bool) {
	s.mu.Lock()
	m := s.members[origin]
	if m == nil && s.mesh != nil {
		if name := s.mesh.Name(origin); name != "" {
			m = &Member{ID: origin, Name: name, State: "connecting", Quality: "auto", asked: -1, Volume: 1, KeyChanged: s.keyChanged(name, s.mesh.IDKey(origin))}
			s.members[origin] = m
		}
	}
	if m == nil {
		s.mu.Unlock()
		return
	}
	m.Recv, m.Notice = height, ""
	if height == 0 && top > 0 && congested {
		m.Notice = fmt.Sprintf("You can't see %s's stream: its quality is too high for your connection or computer right now.", m.Name)
	}
	m.top = top
	var regrid, autoQ, peerStarted bool
	if top > 0 {
		tr := s.feeds.Apply(origin, feeds.ShareStart)
		peerStarted = tr.StartedSharing
		if tr.NotifyUnwatched {
			s.shareNotice, s.shareNoticePeer = m.Name+" started sharing", origin
		}
		if _, ok := s.stopped[origin]; ok {
			delete(s.stopped, origin)
			regrid = true
		}
		if tr.Prev != tr.Next {
			regrid, autoQ = true, true
		}
		if tr.Next.Watched() {
			if s.players[origin] == nil && !s.closed && s.cfg.View != "none" && !s.cfg.NoPlayback {
				if pl, err := media.StartPlayer(s.viewLocked()); err == nil {
					s.players[origin] = pl
					regrid = regrid || pl.Channel != ""
				} else {
					s.cfg.Log.Error("player", "err", err)
				}
			}
			if height == 0 && !congested && m.asked != layerOff {
				m.asked = -1
				autoQ = true
			}
		} else {
			s.stopRemoteVideoLocked(origin)
			if m.asked != layerOff {
				autoQ = true
			}
		}
	} else {
		if s.shareNoticePeer == origin {
			s.shareNotice, s.shareNoticePeer = "", ""
		}
		tr := s.feeds.Apply(origin, feeds.ShareStop)
		if s.stopRemoteVideoLocked(origin) {
			regrid = true
		}
		if tr.Next == feeds.StoppedFading && tr.Prev != feeds.StoppedFading && !s.closed {
			s.stopGen++
			gen := s.stopGen
			s.stopped[origin] = stopInfo{time.Now(), gen}
			time.AfterFunc(stoppedShow, func() { s.fadeStopped(origin, gen) })
			time.AfterFunc(stoppedShow+stoppedFade+100*time.Millisecond, func() { s.dropStopped(origin, gen) })
			s.cfg.Log.Info("peer stopped sharing", "peer", origin)
			regrid = true
		} else if tr.Prev.Sharing() && tr.Next == feeds.Gone && !s.closed {
			s.stopGen++
			gen := s.stopGen
			s.stopped[origin] = stopInfo{time.Now(), gen}
			time.AfterFunc(stoppedGrace+20*time.Millisecond, func() { s.dropStopped(origin, gen) })
		}
		if tr.Prev != tr.Next {
			regrid = true
		}
	}
	s.mu.Unlock()
	if peerStarted {
		s.playSound(media.SoundPeerSharing)
	}
	if regrid {
		s.triggerRegrid()
	}
	if autoQ {
		s.triggerAutoQuality()
	}
	s.changed()
}

func (s *Session) stopRemoteVideoLocked(id string) bool {
	pl := s.players[id]
	if pl == nil {
		return false
	}
	go pl.Stop()
	delete(s.players, id)
	return true
}

func (s *Session) stopRemoteMediaLocked(id string) {
	s.stopRemoteVideoLocked(id)
	if ap := s.aplayers[id]; ap != nil {
		go ap.Stop()
		delete(s.aplayers, id)
	}
}

// fadeStopped fades out the tile of peer id's ended share, unless they
// shared again since.
func (s *Session) fadeStopped(id string, gen int) {
	s.mu.Lock()
	cur := s.stopped[id].gen == gen && s.feeds.State(id) == feeds.StoppedFading && !s.closed
	s.mu.Unlock()
	if !cur {
		return
	}
	s.gridMu.Lock()
	for _, g := range s.grids {
		g.Fade(id, stoppedFade)
	}
	s.gridMu.Unlock()
}

// dropStopped removes the faded tile (or expired grace entry) of peer id's ended share.
func (s *Session) dropStopped(id string, gen int) {
	s.mu.Lock()
	info, ok := s.stopped[id]
	st := s.feeds.State(id)
	fading := ok && info.gen == gen && st == feeds.StoppedFading
	if fading {
		delete(s.stopped, id)
		s.feeds.Apply(id, feeds.FadeDone)
	} else if ok && info.gen == gen && st == feeds.Gone {
		if rem := stoppedGrace - time.Since(info.at); rem > 0 && !s.closed {
			time.AfterFunc(rem, func() { s.dropStopped(id, gen) })
		} else {
			delete(s.stopped, id)
		}
	}
	s.mu.Unlock()
	if fading {
		s.regrid()
		s.applyAutoQuality()
		s.changed()
	}
}

// Decode lag: a decoder that turns less than this share of the access units
// we push into frames, over decodeWindow, can't keep up.
const (
	decodeWindow  = 10 * time.Second
	decodeMinRate = 0.6
)

// watchDecoders asks for a smaller rendition of a member whose video our
// decoder can't keep up with.
func (s *Session) watchDecoders() {
	type mark struct{ pushed, decoded int }
	last := map[string]mark{}
	t := time.NewTicker(decodeWindow)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		s.mu.Lock()
		players := make(map[string]*media.Player, len(s.players))
		for id, pl := range s.players {
			players[id] = pl
		}
		s.mu.Unlock()
		now := map[string]mark{}
		for id, pl := range players {
			p, d := pl.Progress()
			if d < 0 {
				continue
			}
			now[id] = mark{p, d}
			prev, ok := last[id]
			dp, dd := p-prev.pushed, d-prev.decoded
			if ok && dp >= 50 && float64(dd) < decodeMinRate*float64(dp) {
				s.cfg.Log.Warn("decoder falling behind", "peer", id, "pushed", dp, "decoded", dd)
				s.mesh.DecodeLag(id)
			}
		}
		last = now
	}
}

// OnChange registers a callback for membership/source changes (UI push).
func (s *Session) OnChange(f func()) {
	s.mu.Lock()
	s.onChange = f
	s.mu.Unlock()
}

// triggerRegrid schedules regrid in the background, coalescing concurrent
// triggers so at most one background worker runs at a time with one trailing
// pass if another trigger arrived mid-run.
func (s *Session) triggerRegrid() {
	for {
		switch s.regridState.Load() {
		case 2:
			return
		case 1:
			if s.regridState.CompareAndSwap(1, 2) {
				return
			}
		case 0:
			if s.regridState.CompareAndSwap(0, 1) {
				go func() {
					for {
						s.regrid()
						if s.regridState.CompareAndSwap(1, 0) {
							return
						}
						s.regridState.Store(1)
					}
				}()
				return
			}
		}
	}
}

func (s *Session) changed() {
	s.mu.Lock()
	f, windows := s.onChange, s.cfg.View == media.ViewWindow || s.cfg.View == media.ViewGrid
	s.mu.Unlock()
	if windows { // someone left or was renamed: their tile goes or relabels
		s.triggerRegrid()
	}
	if f != nil {
		f()
	}
}

func (s *Session) peerEvent(id, name, state string) {
	s.mu.Lock()
	reapply := false
	switch state {
	case "connected":
		m := s.members[id]
		if m == nil {
			var key string
			if s.mesh != nil {
				key = s.mesh.IDKey(id)
			}
			m = &Member{ID: id, Name: name, Quality: "auto", asked: -1, Volume: 1, KeyChanged: s.keyChanged(name, key)}
			s.members[id] = m
		}
		m.State = "connected"
		m.asked = -1 // send our explicit cap (high, low or off) on every new connection
		reapply = true
	case "reconnecting":
		if m := s.members[id]; m != nil {
			m.State = "reconnecting"
		}
	case "unreachable": // still in the room; we try again
		m := s.members[id]
		if m == nil {
			m = &Member{ID: id, Name: name, Quality: "auto", asked: -1, Volume: 1}
			s.members[id] = m
		}
		m.State = "unreachable"
	case "untrusted":
		m := s.members[id]
		if m == nil {
			m = &Member{ID: id, Name: name, Quality: "auto", asked: -1, Volume: 1}
			s.members[id] = m
		}
		m.State = "untrusted"
	case "gone":
		delete(s.members, id)
		delete(s.stopped, id) // their tile goes at once
		s.feeds.Apply(id, feeds.Leave)
		if s.shareNoticePeer == id {
			s.shareNotice, s.shareNoticePeer = "", ""
		}
		s.stopRemoteMediaLocked(id)
	}
	s.mu.Unlock()
	if reapply {
		s.triggerAutoQuality()
	}
	s.changed()
}

func (s *Session) keyChanged(name, key string) bool {
	if s.cfg.Known == nil {
		return false
	}
	return s.cfg.Known.Check(name, key)
}

func (s *Session) pushVideo(peer string, au []byte) {
	if s.cfg.View == "none" || s.cfg.NoPlayback {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	st := s.feeds.State(peer)
	if st == feeds.NotWatched {
		s.mu.Unlock()
		return
	}
	pl := s.players[peer]
	var regrid, autoQ, notify, peerStarted bool
	if !st.Sharing() {
		if info, ok := s.stopped[peer]; ok && pl == nil && time.Since(info.at) < stoppedGrace {
			s.mu.Unlock()
			return // in flight when they stopped
		}
		delete(s.stopped, peer)
		tr := s.feeds.Apply(peer, feeds.ShareStart)
		peerStarted = tr.StartedSharing
		if tr.NotifyUnwatched {
			name := peer
			if m := s.members[peer]; m != nil && m.Name != "" {
				name = m.Name
			}
			s.shareNotice, s.shareNoticePeer = name+" started sharing", peer
			notify = true
		}
		if !tr.Next.Watched() {
			s.stopRemoteVideoLocked(peer)
			s.mu.Unlock()
			if peerStarted {
				s.playSound(media.SoundPeerSharing)
			}
			s.triggerAutoQuality()
			if notify {
				s.changed()
			}
			return
		}
		regrid, autoQ = true, true
	}
	if pl == nil {
		var err error
		if pl, err = media.StartPlayer(s.viewLocked()); err != nil {
			s.mu.Unlock()
			s.cfg.Log.Error("player", "err", err)
			return
		}
		s.players[peer] = pl
		regrid = regrid || pl.Channel != ""
	}
	s.mu.Unlock()
	if peerStarted {
		s.playSound(media.SoundPeerSharing)
	}
	if regrid {
		s.triggerRegrid()
	}
	if autoQ {
		s.triggerAutoQuality()
	}
	if notify {
		s.changed()
	}
	pl.Push(au)
}

// viewLocked is how a new player shows its stream (mu held): video windows
// (main or pop-out) show it through a Grid, which draws the tile controls.
func (s *Session) viewLocked() media.ViewKind {
	if s.cfg.View == media.ViewWindow {
		return media.ViewGrid
	}
	return s.cfg.View
}

// Video windows start at this size; our own preview's pop-out is smaller.
const (
	windowW, windowH         = 1280, 720
	selfWindowW, selfWindowH = 480, 270
)

// How our own preview shows (Config.SelfView).
const (
	// SelfViewCorner: a small picture in the main window's top right
	// corner, over the others' streams (it fills the window alone).
	SelfViewCorner = "corner"
	// SelfViewTile: a tile like the others', at PreviewBigW x PreviewBigH.
	SelfViewTile = "tile"
	// SelfViewWindow: popped out into a small window of its own.
	SelfViewWindow = "window"
	// SelfViewHidden: not shown (the eye is closed).
	SelfViewHidden = "hidden"
)

// regrid syncs the main streaming window (key "") and any pop-out windows
// (key feedID) with the per-feed state machine, and opens or closes our
// capture's preview valve. It rebuilds a window only when its tile set or
// pinned layout changed.
func (s *Session) regrid() {
	s.gridMu.Lock()
	defer s.gridMu.Unlock()

	s.mu.Lock()
	src := s.src
	preview := src != nil && !s.closed && s.feeds.Watched(feeds.Self) && s.cfg.View != "none" && !s.cfg.NoPlayback
	s.previewOn = preview
	selfSt := s.feeds.State(feeds.Self)
	pip := selfSt == feeds.InMain && !s.selfTile
	bigPreview := !pip
	compose := s.cfg.View == media.ViewWindow || s.cfg.View == media.ViewGrid || s.gridView != ""

	var mainTiles []media.Tile
	popTiles := map[string]media.Tile{}
	if !s.closed && compose {
		makeTile := func(id string, st feeds.State) (media.Tile, bool) {
			if id == feeds.Self {
				if !preview {
					return media.Tile{}, false
				}
				return media.Tile{
					Channel:   s.selfCh, // the big one: see below
					Label:     i18n.L("You"),
					ID:        feeds.Self,
					Self:      true,
					Pinned:    st == feeds.Pinned,
					PoppedOut: st == feeds.PoppedOut,
					PiP:       pip,
				}, true
			}
			m := s.members[id]
			if st == feeds.StoppedFading {
				if m == nil {
					return media.Tile{}, false
				}
				lbl := m.Name
				if tag := identity.Tag(s.mesh.IDKey(id)); tag != "" {
					lbl += "#" + tag
				}
				return media.Tile{Label: lbl, ID: id, Stopped: true}, true
			}
			pl := s.players[id]
			if pl == nil || pl.Channel == "" {
				return media.Tile{}, false
			}
			t := media.Tile{
				Channel:   pl.Channel,
				Label:     id,
				ID:        id,
				Pinned:    st == feeds.Pinned,
				PoppedOut: st == feeds.PoppedOut,
			}
			if m != nil {
				t.Label, t.Muted, t.Volume = m.Name, m.Muted, m.Volume
			}
			if tag := identity.Tag(s.mesh.IDKey(id)); tag != "" {
				t.Label += "#" + tag
			}
			return t, true
		}
		for _, id := range s.feeds.MainFeeds() {
			if t, ok := makeTile(id, s.feeds.State(id)); ok {
				mainTiles = append(mainTiles, t)
			}
		}
		for _, id := range s.feeds.PopOuts() {
			if t, ok := makeTile(id, feeds.PoppedOut); ok {
				popTiles[id] = t
			}
		}
		// The corner picture is small, unless alone (then it fills the
		// window); as a tile or in its own window our preview is big.
		if pip && len(mainTiles) == 1 {
			bigPreview = true
		}
		for i := range mainTiles {
			if mainTiles[i].Self && bigPreview {
				mainTiles[i].Channel = media.PreviewBigChannel(s.selfCh)
			}
		}
		if t, ok := popTiles[feeds.Self]; ok {
			t.Channel = media.PreviewBigChannel(s.selfCh)
			popTiles[feeds.Self] = t
		}
	}
	s.previewBig = bigPreview
	s.mu.Unlock()

	if src != nil {
		src.SetPreviewBig(bigPreview)
		src.SetPreview(preview)
		s.updateCaptureIdle()
	}
	if !compose {
		return
	}

	sort.Slice(mainTiles, func(i, j int) bool {
		if mainTiles[i].Self != mainTiles[j].Self {
			return !mainTiles[i].Self
		}
		if a, b := strings.ToLower(mainTiles[i].Label), strings.ToLower(mainTiles[j].Label); a != b {
			return a < b
		}
		return mainTiles[i].ID < mainTiles[j].ID
	})

	want := map[string]media.GridConfig{}
	if len(mainTiles) > 0 {
		want[""] = media.GridConfig{Tiles: mainTiles}
	}
	popIDs := make([]string, 0, len(popTiles))
	for id, t := range popTiles {
		want[id] = media.GridConfig{Tiles: []media.Tile{t}, Width: windowW, Height: windowH}
		popIDs = append(popIDs, id)
	}
	slices.Sort(popIDs)

	if s.grids == nil {
		s.grids, s.gridKeys = map[string]*media.Grid{}, map[string]string{}
		s.wins, s.winStop = map[string]media.Window{}, map[media.Window]chan struct{}{}
		s.winMeta = map[media.Window]winMeta{}
	}
	changed := false
	for k, g := range s.grids {
		if _, ok := want[k]; !ok {
			g.Stop()
			delete(s.grids, k)
			delete(s.gridKeys, k)
			changed = true
		}
	}
	// Windows no key wants any more go to keys that lack one; only left-over
	// ones close (popping in or out reuses an open window when possible).
	// Our preview's small window closes rather than become another's.
	var free, small []media.Window
	for k, w := range s.wins {
		if _, ok := want[k]; !ok {
			if k == feeds.Self {
				small = append(small, w)
			} else {
				free = append(free, w)
			}
			delete(s.wins, k)
			changed = true
		}
	}
	order := append([]string{""}, popIDs...)
	for _, k := range order {
		cfg, ok := want[k]
		if !ok {
			continue
		}
		title := windowTitle(k, cfg)
		ww, wh := windowW, windowH
		if k == feeds.Self {
			ww, wh = selfWindowW, selfWindowH
		}
		if s.wins[k] == nil && s.openWindow != nil {
			if len(free) > 0 && k != feeds.Self { // ours opens small, so new
				s.wins[k], free = free[0], free[1:]
			} else if w, err := s.openWindow(title, ww, wh); err == nil {
				s.wins[k] = w
				stop := make(chan struct{})
				s.winStop[w] = stop
				go s.watchWindow(w, stop)
			} else {
				s.cfg.Log.Warn("own video window", "err", err)
			}
			changed = true
		}
		if w := s.wins[k]; w != nil {
			meta := s.winMeta[w]
			if !meta.set || meta.title != title {
				w.SetTitle(title)
				meta.title = title
			}
			meta.set = true
			s.winMeta[w] = meta
		}
		cfg.Window = s.wins[k]
		if s.regridLocked(k, cfg) {
			changed = true
		}
	}
	for _, w := range append(free, small...) {
		s.destroyWindowLocked(w)
	}
	if changed {
		s.triggerAutoQuality()
	}
}

func (s *Session) destroyWindowLocked(w media.Window) {
	if stop := s.winStop[w]; stop != nil {
		close(stop)
		delete(s.winStop, w)
	}
	delete(s.winMeta, w)
	w.Destroy()
}

// windowTitle names the main streaming window or a pop-out window.
func windowTitle(k string, cfg media.GridConfig) string {
	if k != "" && len(cfg.Tiles) == 1 {
		return "Bellion Share: " + cfg.Tiles[0].Label
	}
	return "Bellion Share"
}

// watchWindow handles the user closing w: closing the main streaming window
// stops watching the streams it shows (the eye watches them again); closing
// a pop-out window returns its stream to the main window.
func (s *Session) watchWindow(w media.Window, stop <-chan struct{}) {
	for {
		select {
		case <-w.Closed():
			var closedKey string
			var wasOpen bool
			s.gridMu.Lock()
			delete(s.winStop, w)
			delete(s.winMeta, w)
			for k, v := range s.wins {
				if v != w {
					continue
				}
				closedKey = k
				wasOpen = true
				if g := s.grids[k]; g != nil {
					g.Stop()
				}
				delete(s.grids, k)
				delete(s.gridKeys, k)
				delete(s.wins, k)
				s.cfg.Log.Info("video window closed", "window", k)
			}
			if wasOpen {
				s.mu.Lock()
				s.closeWindowFeedsLocked(closedKey)
				s.mu.Unlock()
			}
			s.gridMu.Unlock()
			w.Destroy()
			if !wasOpen {
				return
			}
			if closedKey == feeds.Self {
				s.selfViewChanged()
			}
			s.regrid()
			s.applyAutoQuality()
			s.changed()
			return
		case <-stop:
			return
		case <-s.done:
			return
		}
	}
}

// regridLocked (re)starts video window k (gridMu held). Returns true if rebuilt.
func (s *Session) regridLocked(k string, cfg media.GridConfig) bool {
	type shown struct { // mute/volume/poppedOut update in place via SetState
		channel, label, id         string
		self, pinned, stopped, pip bool
		selfOut                    bool // our preview's buttons depend on it
	}
	var what []shown
	for _, t := range cfg.Tiles {
		what = append(what, shown{t.Channel, t.Label, t.ID, t.Self, t.Pinned, t.Stopped, t.PiP, t.Self && t.PoppedOut})
	}
	if s.gridFast.Load() {
		cfg.FPS = media.GridFastFPS
	}
	key := fmt.Sprint(cfg.Rows, cfg.Cols, cfg.FPS, what, fmt.Sprintf("%p", cfg.Window))
	if g := s.grids[k]; g != nil {
		if key == s.gridKeys[k] {
			for _, t := range cfg.Tiles {
				g.SetState(t.ID, t.Muted, t.Volume, t.Pinned, t.PoppedOut)
			}
			return false
		}
		g.Stop()
		delete(s.grids, k)
	}
	s.gridKeys[k] = key
	cfg.View, cfg.OnControl = media.ViewWindow, s.control
	if s.gridView != "" {
		cfg.View = s.gridView
	}
	g, err := media.StartGrid(cfg)
	if err != nil {
		s.cfg.Log.Error("video window", "err", err)
		return false
	}
	s.grids[k] = g
	if cfg.Window != nil && !g.InWindow() {
		// No sink draws into our windows here: let sinks open their own
		// from now on, and drop ours (it would stay empty).
		s.cfg.Log.Warn("own video windows unusable", "sink", g.Sink)
		s.openWindow = nil
		for wk, w := range s.wins {
			s.destroyWindowLocked(w)
			delete(s.wins, wk)
		}
	}
	if !g.InWindow() {
		go s.watchGridWindow(k, g)
	}
	if k == "" {
		r, c := media.GridSize(len(cfg.Tiles), cfg.Rows, cfg.Cols)
		s.cfg.Log.Info("grid window", "streams", len(cfg.Tiles), "rows", r, "cols", c, "pinned", s.pinnedID(), "sink", g.Sink)
	}
	return true
}

// closeWindowFeedsLocked applies the user closing video window k: a pop-out
// returns its feed to the main window; closing the main streaming window ("")
// stops watching the streams it shows (EyeToggle: NotWatched, or Gone for
// StoppedFading), sound included. Runs with gridMu and mu held, so no
// concurrent regrid() can reopen the window before feeds reflect the close.
func (s *Session) closeWindowFeedsLocked(k string) {
	if s.closed {
		return
	}
	if k != "" {
		s.feeds.Apply(k, feeds.PopOutClosed)
		return
	}
	ids := s.feeds.MainFeeds()
	for _, id := range ids {
		tr := s.feeds.Apply(id, feeds.EyeToggle)
		if id != feeds.Self && !tr.Next.Watched() {
			if tr.Prev != feeds.StoppedFading {
				delete(s.stopped, id)
			}
			s.stopRemoteMediaLocked(id)
		}
	}
	if len(ids) > 0 {
		s.cfg.Log.Info("main video window closed by the user: stopped watching", "streams", len(ids))
	}
}

func (s *Session) watchGridWindow(k string, g *media.Grid) {
	select {
	case err := <-g.Err:
		if err == nil {
			return
		}
	case <-g.Done():
		return
	case <-s.done:
		return
	}
	s.gridMu.Lock()
	if s.grids[k] != g {
		s.gridMu.Unlock()
		return
	}
	g.Stop()
	delete(s.grids, k)
	delete(s.gridKeys, k)
	s.mu.Lock()
	s.closeWindowFeedsLocked(k)
	s.mu.Unlock()
	s.gridMu.Unlock()
	if k == feeds.Self {
		s.selfViewChanged()
	}
	s.regrid()
	s.applyAutoQuality()
	s.changed()
}

func (s *Session) pinnedID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.feeds.PinnedFeed()
}

// ToggleWatch toggles the eye on feed id ("self" for our own preview, or a
// remote peer ID). Not watching a remote peer stops their video; turning
// watching back on puts the stream in the main window.
func (s *Session) ToggleWatch(id string) error {
	if id == feeds.Self {
		s.mu.Lock()
		tr := s.feeds.Apply(feeds.Self, feeds.EyeToggle)
		s.mu.Unlock()
		if tr.Prev == tr.Next {
			return nil
		}
		s.selfViewChanged()
		s.regrid()
		s.changed()
		return nil
	}
	s.mu.Lock()
	if s.members[id] == nil {
		s.mu.Unlock()
		return fmt.Errorf("no peer %s", id)
	}
	tr := s.feeds.Apply(id, feeds.EyeToggle)
	if tr.Prev == tr.Next {
		s.mu.Unlock()
		return nil
	}
	dismissedStopped := tr.Prev == feeds.StoppedFading && tr.Next == feeds.Gone
	if !tr.Next.Watched() {
		if dismissedStopped {
			s.stopped[id] = stopInfo{at: time.Now(), gen: s.stopped[id].gen}
		} else {
			delete(s.stopped, id)
		}
		s.stopRemoteMediaLocked(id)
	} else {
		if s.shareNoticePeer == id {
			s.shareNotice, s.shareNoticePeer = "", ""
		}
		if s.players[id] == nil && !s.closed && s.cfg.View != "none" && !s.cfg.NoPlayback {
			if pl, err := media.StartPlayer(s.viewLocked()); err == nil {
				s.players[id] = pl
			} else {
				s.cfg.Log.Error("player", "err", err)
			}
		}
	}
	s.mu.Unlock()
	s.regrid()
	if dismissedStopped {
		// Refresh stoppedGrace after regrid tears down the GStreamer pipeline
		// (which can take >1s under -race) if the peer hasn't started sharing again,
		// and schedule cleanup once stoppedGrace expires.
		s.mu.Lock()
		if s.feeds.State(id) == feeds.Gone {
			if info, ok := s.stopped[id]; ok {
				info.at = time.Now()
				s.stopped[id] = info
				if !s.closed {
					gen := info.gen
					time.AfterFunc(stoppedGrace+20*time.Millisecond, func() { s.dropStopped(id, gen) })
				}
			}
		}
		s.mu.Unlock()
	}
	s.applyAutoQuality()
	s.changed()
	return nil
}

// PopOut pops feed id ("self" or a peer ID) out into its own window, or back
// into the main streaming window if already popped out.
func (s *Session) PopOut(id string) error {
	s.mu.Lock()
	if id != feeds.Self && s.members[id] == nil {
		s.mu.Unlock()
		return fmt.Errorf("no peer %s", id)
	}
	tr := s.feeds.Apply(id, feeds.PopOut)
	s.mu.Unlock()
	if tr.Prev == tr.Next {
		return nil
	}
	if id == feeds.Self {
		s.selfViewChanged()
	}
	s.regrid()
	s.applyAutoQuality()
	s.changed()
	return nil
}

// Pin pins or unpins feed id ("self" or a peer ID) in the main window.
func (s *Session) Pin(id string) error {
	s.mu.Lock()
	if id != feeds.Self && s.members[id] == nil {
		s.mu.Unlock()
		return fmt.Errorf("no peer %s", id)
	}
	tr := s.feeds.Apply(id, feeds.Pin)
	if id == feeds.Self && tr.Next == feeds.Pinned {
		s.selfTile = true // pinned, it is a tile; unpinned it stays one
	}
	s.mu.Unlock()
	if tr.Prev == tr.Next {
		return nil
	}
	if id == feeds.Self {
		s.selfViewChanged()
	}
	s.regrid()
	s.applyAutoQuality()
	s.changed()
	return nil
}

// ToggleSelfTile turns our preview in the main window from the corner
// picture into a tile like the others' (at PreviewBigW x PreviewBigH), or
// back (unpinning it).
func (s *Session) ToggleSelfTile() {
	s.mu.Lock()
	st := s.feeds.State(feeds.Self)
	if st != feeds.InMain && st != feeds.Pinned {
		s.mu.Unlock()
		return
	}
	if st == feeds.Pinned {
		s.feeds.Apply(feeds.Self, feeds.Pin)
		s.selfTile = false
	} else {
		s.selfTile = !s.selfTile
	}
	s.mu.Unlock()
	s.selfViewChanged()
	s.regrid()
	s.applyAutoQuality()
	s.changed()
}

// SelfView is how our own preview shows now (a SelfView* value).
func (s *Session) SelfView() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.selfViewLocked()
}

func (s *Session) selfViewLocked() string {
	switch st := s.feeds.State(feeds.Self); {
	case st == feeds.NotWatched:
		return SelfViewHidden
	case st == feeds.PoppedOut:
		return SelfViewWindow
	case st == feeds.Gone: // not sharing: as it was set
		return s.selfView
	case s.selfTile:
		return SelfViewTile
	}
	return SelfViewCorner
}

// selfViewChanged keeps how our preview shows after the user changed it.
func (s *Session) selfViewChanged() {
	s.mu.Lock()
	v := s.selfViewLocked()
	changed := v != s.selfView
	s.selfView = v
	f := s.cfg.OnSelfView
	s.mu.Unlock()
	if changed && f != nil {
		f(v)
	}
}

// SetAutoWatch updates "Start watching new streams automatically" for
// subsequent remote shares in this session.
func (s *Session) SetAutoWatch(on bool) {
	s.mu.Lock()
	s.feeds.SetAutoWatch(on)
	if on {
		s.shareNotice, s.shareNoticePeer = "", ""
	}
	s.mu.Unlock()
	s.mesh.SetDefaultOff(!on)
	s.applyAutoQuality()
	s.changed()
}

// SetPlaySounds turns call notification chimes on or off for this session.
func (s *Session) SetPlaySounds(on bool) {
	s.playSounds.Store(on)
	if !on {
		s.stopSoundPlayer(true)
	}
	s.changed()
}

// PlaySounds reports whether call notification chimes are enabled.
func (s *Session) PlaySounds() bool {
	return s.playSounds.Load()
}

// PlayedSounds returns the sound chimes triggered during this session in
// chronological order (tests).
func (s *Session) PlayedSounds() []media.SoundKind {
	return s.soundGate.Played()
}

// SoundBuffers returns how many raw PCM buffers our GStreamer SoundPlayer has
// delivered to its sink (tests, ViewCount / ViewGrid).
func (s *Session) SoundBuffers() int {
	s.soundMu.Lock()
	base, sp := s.soundBufs, s.splayer
	s.soundMu.Unlock()
	if sp != nil {
		base += sp.Buffers()
	}
	return base
}

func (s *Session) playSound(kind media.SoundKind) {
	if s.cfg.NoPlayback || !s.playSounds.Load() {
		return
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed || !s.soundGate.Allow(kind) {
		return
	}
	s.soundMu.Lock()
	s.soundPend = append(s.soundPend, kind)
	if !s.soundBusy {
		s.soundBusy = true
		go s.drainSounds()
	}
	s.soundMu.Unlock()
}

func (s *Session) drainSounds() {
	for {
		s.soundMu.Lock()
		if len(s.soundPend) == 0 {
			s.soundBusy = false
			s.soundMu.Unlock()
			return
		}
		kind := s.soundPend[0]
		s.soundPend = s.soundPend[1:]
		sp := s.splayer
		s.soundMu.Unlock()

		if !s.playSounds.Load() {
			continue
		}
		if sp == nil {
			s.mu.Lock()
			closed := s.closed
			sink := s.audioSink()
			s.mu.Unlock()
			if closed || sink == "fakesink sync=false" {
				continue
			}
			created, err := media.StartSoundPlayer(sink)
			if err != nil {
				s.cfg.Log.Warn("sound player", "err", err)
				continue
			}
			s.soundMu.Lock()
			s.mu.Lock()
			nowClosed := s.closed
			curSink := s.audioSink()
			s.mu.Unlock()
			if nowClosed || !s.playSounds.Load() || curSink != sink {
				s.soundMu.Unlock()
				created.Stop()
				continue
			}
			if s.splayer == nil {
				s.splayer = created
			} else {
				created.Stop()
			}
			sp = s.splayer
			s.soundMu.Unlock()
		}
		if sp != nil {
			sp.Play(kind)
		}
	}
}

func (s *Session) stopSoundPlayer(dropPending bool) {
	s.soundMu.Lock()
	sp := s.splayer
	s.splayer = nil
	if dropPending {
		s.soundPend = nil
	}
	s.soundMu.Unlock()
	if sp != nil {
		sp.Stop()
		n := sp.Buffers()
		s.soundMu.Lock()
		s.soundBufs += n
		s.soundMu.Unlock()
	}
}

// control handles a button clicked on tile id's overlay bar.
func (s *Session) control(id string, c media.Control, v float64) {
	switch c {
	case media.ControlEye:
		if err := s.ToggleWatch(id); err != nil {
			s.cfg.Log.Warn("video window button", "peer", id, "button", c, "err", err)
		}
		return
	case media.ControlPopOut:
		if err := s.PopOut(id); err != nil {
			s.cfg.Log.Warn("video window button", "peer", id, "button", c, "err", err)
		}
		return
	case media.ControlPin:
		if err := s.Pin(id); err != nil {
			s.cfg.Log.Warn("video window button", "peer", id, "button", c, "err", err)
		}
		return
	case media.ControlExpand:
		if id == feeds.Self {
			s.ToggleSelfTile()
		}
		return
	}
	s.mu.Lock()
	m := s.members[id]
	if m == nil {
		s.mu.Unlock()
		return
	}
	quality, volume, muted := m.Quality, m.Volume, m.Muted
	s.mu.Unlock()
	switch c {
	case media.ControlMute:
		muted = !muted
	case media.ControlVolume:
		volume = v
		if v > 0 {
			muted = false
		}
	default:
		return
	}
	if quality == "" {
		quality = "auto"
	}
	if err := s.SetPeer(id, quality, volume, muted); err != nil {
		s.cfg.Log.Warn("video window button", "peer", id, "button", c, "err", err)
		return
	}
	s.cfg.Log.Info("video window button", "peer", id, "button", c, "muted", muted, "quality", quality)
}

// PreviewFrames is how many frames our capture has sent to the self-preview
// tile while preview is watched (tests; -1 if not sharing or preview hidden).
func (s *Session) PreviewFrames() int {
	s.mu.Lock()
	src, on := s.src, s.previewOn
	s.mu.Unlock()
	if src == nil || !on {
		return -1
	}
	return src.PreviewFrames()
}

func (s *Session) audioSink() string {
	if s.cfg.View == media.ViewCount || s.cfg.View == media.ViewGrid {
		return "count"
	}
	if s.cfg.View == "none" {
		return "fakesink sync=false"
	}
	return audio.PlaybackSink(s.output)
}

// watchingLocked reports whether we watch peer id's stream (mu held).
func (s *Session) watchingLocked(id string) bool {
	st := s.feeds.State(id)
	return st != feeds.NotWatched && (st.Watched() || s.feeds.AutoWatch())
}

func (s *Session) pushAudio(peer string, frame []byte) {
	if s.cfg.NoPlayback {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if s.feeds.State(peer) == feeds.NotWatched { // not watching their share: not hearing it either
		s.mu.Unlock()
		return
	}
	ap := s.aplayers[peer]
	if ap == nil {
		var err error
		if ap, err = media.StartAudioPlayer(s.audioSink()); err != nil {
			s.mu.Unlock()
			s.cfg.Log.Error("audio player", "err", err)
			return
		}
		s.aplayers[peer] = ap
		if m := s.members[peer]; m != nil {
			ap.SetVolume(m.gain())
		}
	}
	s.mu.Unlock()
	ap.Push(frame)
}

// SetAudio chooses what audio we share. Mode "test" sends beeps
// (media.TestAudio); "system", "apps" and "window" capture apps' audio.
// Audio can go out without video.
func (s *Session) SetAudio(sel audio.Selection) error {
	s.mu.Lock()
	s.audioBack = audio.Selection{}
	s.mu.Unlock()
	return s.setAudio(sel)
}

// setAudio is SetAudio for our own changes: it keeps audioBack.
func (s *Session) setAudio(sel audio.Selection) error {
	s.audioMu.Lock()
	defer s.audioMu.Unlock()
	var desc string // capture to run; "" = none
	switch sel.Mode {
	case "none":
	case "test":
		desc = media.TestAudio
	case "system", "apps", "window":
		sharer, err := s.ensureSharer()
		if err != nil {
			return err
		}
		if err := sharer.Select(sel); err != nil {
			s.cfg.Log.Warn("audio links", "err", err)
		}
		desc = string(sharer.Capture())
	default:
		return errors.New("unknown audio mode")
	}
	if sel.Mode != "system" && sel.Mode != "apps" && sel.Mode != "window" {
		s.mu.Lock()
		sharer := s.sharer
		s.mu.Unlock()
		if sharer != nil {
			sharer.Select(audio.Selection{Mode: "none"})
		}
	}

	s.mu.Lock()
	old, oldDesc := s.asrc, s.asrcDesc
	if oldDesc != desc {
		s.asrc, s.asrcDesc, s.alevel = nil, "", nil
	}
	s.mu.Unlock()
	if oldDesc != desc {
		if old != nil {
			old.Stop()
		}
		if desc != "" {
			if err := s.startAudio(desc); err != nil {
				return err
			}
		}
	}
	s.mu.Lock()
	prev := s.asel
	s.asel = sel
	s.mu.Unlock()
	if prev.Mode != sel.Mode || sel.Mode == "apps" && !slices.Equal(prev.Apps, sel.Apps) || prev.PID != sel.PID {
		if sel.Mode == "test" {
			s.cfg.Log.Info("shared audio", "mode", sel.Mode, "source", "test tone")
		} else {
			s.cfg.Log.Info("shared audio", "mode", sel.Mode, "apps", sel.Apps, "pid", sel.PID)
		}
	}
	s.mesh.SetSharingAudio(sel.Mode != "" && sel.Mode != "none")
	s.changed()
	return nil
}

// audioCaptureEvery is how often whole-system audio looks at which apps
// play: on Windows, while a voice chat app is left out, the capture is a mix
// of the others, so one that starts playing joins it within this time.
const audioCaptureEvery = 3 * time.Second

func (s *Session) watchAudioCapture() {
	t := time.NewTicker(audioCaptureEvery)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		s.RefreshAudio(false)
	}
}

// RefreshAudio rebuilds whole-system audio sharing when what it captures
// changed (or always, with force: e.g. the voice chat setting changed).
func (s *Session) RefreshAudio(force bool) {
	s.mu.Lock()
	sel, sharer, cur := s.asel, s.sharer, s.asrcDesc
	s.mu.Unlock()
	if sharer == nil || sel.Mode != "system" {
		return
	}
	if force || string(sharer.Capture()) != cur {
		if err := s.setAudio(sel); err != nil {
			s.cfg.Log.Warn("shared audio", "err", err)
		}
	}
}

func (s *Session) ensureSharer() (*audio.Sharer, error) {
	s.mu.Lock()
	sharer := s.sharer
	s.mu.Unlock()
	if sharer != nil {
		return sharer, nil
	}
	sharer, err := audio.NewSharer(s.cfg.Log)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		go sharer.Close()
		return nil, errors.New("session closed")
	}
	s.sharer = sharer
	return sharer, nil
}

func (s *Session) startAudio(desc string) error {
	var first sync.Once
	flowing := make(chan struct{})
	src, err := media.StartAudioSource(desc, func(frame []byte, d time.Duration) {
		first.Do(func() { close(flowing) })
		s.mesh.WriteAudio(frame, d)
	})
	if err != nil {
		s.cfg.Log.Error("audio source", "err", err)
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		go src.Stop()
		return errors.New("session closed")
	}
	s.asrc, s.asrcDesc = src, desc
	// Capture can fail after the pipeline starts (e.g. wasapi2 loopback);
	// say so, since the peer just hears nothing.
	go func() {
		if err := <-src.Err; err != nil {
			s.cfg.Log.Error("audio source stopped", "err", err)
		}
	}()
	// Opus in-band FEC follows the loss peers report; the level shows in
	// the stats log (silence vs sound).
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for i := 1; ; i++ {
			<-t.C
			var l *media.AudioLevel
			if i%15 == 0 {
				if v, ok := src.Level(); ok {
					l = &v
				}
			}
			s.mu.Lock()
			current := s.asrc == src && !s.closed
			if current && i%15 == 0 {
				s.alevel = l
			}
			s.mu.Unlock()
			if !current {
				return
			}
			src.SetPacketLoss(s.mesh.AudioLossPct())
		}
	}()
	go func() {
		select {
		case <-flowing:
			s.cfg.Log.Info("audio flowing", "source", desc)
		case <-time.After(5 * time.Second):
			s.mu.Lock()
			current := s.asrc == src
			s.mu.Unlock()
			if current {
				s.cfg.Log.Warn("no audio frames after 5s", "source", desc)
			}
		}
	}()
	return nil
}

const (
	layerAuto = 0
	layerLow  = 1
	layerOff  = 2
)

var (
	peerQualityLayer = map[string]int{"auto": layerAuto, "low": layerLow}
	autoQualityLayer = map[string]int{"": layerAuto, "low": layerLow, "off": layerOff}
)

func (m *Member) gain() float64 {
	if m.Muted {
		return 0
	}
	return m.Volume
}

// SetPeer changes our viewer settings for one peer: the best quality we ask
// them to send (auto | low), and their playback volume and mute.
func (s *Session) SetPeer(id, quality string, volume float64, muted bool) error {
	layer, ok := peerQualityLayer[quality]
	if !ok || volume < 0 || volume > 2 {
		return fmt.Errorf("bad peer settings %q %v", quality, volume)
	}
	s.mu.Lock()
	m := s.members[id]
	if m == nil {
		s.mu.Unlock()
		return fmt.Errorf("no peer %s", id)
	}
	m.Quality, m.Volume, m.Muted = quality, volume, muted
	st := s.feeds.State(id)
	if !s.watchingLocked(id) {
		layer = layerOff
	} else {
		layer = max(layer, autoQualityLayer[m.Auto])
	}
	send := m.asked != layer
	m.asked = layer
	ap, gain := s.aplayers[id], m.gain()
	pinned, popped := st == feeds.Pinned, st == feeds.PoppedOut
	s.mu.Unlock()
	if ap != nil {
		ap.SetVolume(gain)
	}
	s.gridMu.Lock()
	for _, g := range s.grids {
		g.SetState(id, muted, volume, pinned, popped)
	}
	s.gridMu.Unlock()
	if send {
		if err := s.mesh.SetMaxLayer(id, layer); err != nil {
			return err
		}
	}
	s.changed()
	return nil
}

// While nobody looks at a stream, we ask its sender for less:
// LOW when grid tiles are smallTileH lines or less (LOW is 360p), nothing
// while every video window is minimized. The sender's switch back brings a
// keyframe.
const (
	smallTileH   = 360
	visibleEvery = 2 * time.Second
)

func (s *Session) watchVisibility() {
	t := time.NewTicker(visibleEvery)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		hidden := media.VideoWindowsMinimized()
		if s.hiddenWins.Swap(hidden) != hidden {
			s.cfg.Log.Info("video windows", "minimized", hidden)
		}
		s.applyAutoQuality() // also covers peers that joined since
	}
}

// watchWaiting shows a spinner on a tile until its first frame.
// A share of a still window sends no frames for a while, so only the
// first frame counts.
func (s *Session) watchWaiting() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	var vr viewRates
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		waiting := map[string]bool{}
		decoded := map[string]int{}
		s.mu.Lock()
		for id, pl := range s.players {
			_, d := pl.Progress() // Frames counts the headless view only
			waiting[id] = d == 0 && s.feeds.Watched(id)
			if s.feeds.Watched(id) {
				decoded[id] = d
			}
		}
		s.mu.Unlock()
		s.gridMu.Lock()
		shown := map[string]int{}
		for k, g := range s.grids {
			shown[k] = g.Frames()
		}
		s.gridMu.Unlock()
		if fast, ok := vr.update(s.cfg.Log, decoded, shown, s.gridFast.Load()); ok {
			s.gridFast.Store(fast)
			s.cfg.Log.Info("video windows frame rate", "fps", windowRate(fast))
			go s.regrid()
		}
		s.gridMu.Lock()
		for _, g := range s.grids {
			for id, w := range waiting {
				g.SetWaiting(id, w)
			}
		}
		s.gridMu.Unlock()
	}
}

// triggerAutoQuality schedules applyAutoQuality in the background, coalescing
// concurrent triggers so at most one background worker runs at a time with one
// trailing pass if another trigger arrived mid-run.
func (s *Session) triggerAutoQuality() {
	for {
		switch s.autoQState.Load() {
		case 2:
			return
		case 1:
			if s.autoQState.CompareAndSwap(1, 2) {
				return
			}
		case 0:
			if s.autoQState.CompareAndSwap(0, 1) {
				go func() {
					for {
						s.applyAutoQuality()
						if s.autoQState.CompareAndSwap(1, 0) {
							return
						}
						s.autoQState.Store(1)
					}
				}()
				return
			}
		}
	}
}

// applyAutoQuality asks each peer for the most their stream needs now.
func (s *Session) applyAutoQuality() {
	s.autoQMu.Lock()
	defer s.autoQMu.Unlock()

	hidden := s.hiddenWins.Load()
	small := s.smallTiles.Load()

	minWins := map[string]bool{}
	effH := map[string]int{}
	s.gridMu.Lock()
	for k, w := range s.wins {
		if w != nil && w.Minimized() {
			minWins[k] = true
		}
	}
	for _, g := range s.grids {
		for _, t := range g.Tiles() {
			if t.ID != "" && !t.Self {
				effH[t.ID] = g.EffectiveTileHeight(t.ID)
			}
		}
	}
	s.gridMu.Unlock()

	type ask struct {
		id    string
		layer int
	}
	var asks []ask
	s.mu.Lock()
	mesh := s.mesh
	if mesh == nil {
		s.mu.Unlock()
		return
	}
	for id, m := range s.members {
		st := s.feeds.State(id)
		if st == feeds.NotWatched || (!st.Watched() && !s.feeds.AutoWatch()) {
			m.Auto = ""
			if m.asked != layerOff {
				m.asked = layerOff
				asks = append(asks, ask{id, layerOff})
			}
			continue
		}
		winKey := ""
		if st == feeds.PoppedOut {
			winKey = id
		}
		auto := ""
		switch {
		case hidden || minWins[winKey]:
			auto = "off"
		case small || (effH[id] > 0 && effH[id] <= smallTileH):
			auto = "low"
		}
		m.Auto = auto
		if l := max(peerQualityLayer[m.Quality], autoQualityLayer[auto]); l != m.asked {
			m.asked = l
			asks = append(asks, ask{id, l})
		}
	}
	s.mu.Unlock()
	for _, a := range asks {
		if err := mesh.SetMaxLayer(a.id, a.layer); err != nil {
			s.cfg.Log.Warn("auto quality", "peer", a.id, "err", err)
			s.mu.Lock()
			if m := s.members[a.id]; m != nil {
				m.asked = -1 // try again next time
			}
			s.mu.Unlock()
		}
	}
	if len(asks) > 0 {
		s.changed()
	}
}

// SetOutput plays the call on device ("" = system default). Players restart on
// the new device; the next frame from each peer recreates them.
func (s *Session) SetOutput(device string) {
	s.mu.Lock()
	s.output = device
	old := s.aplayers
	s.aplayers = map[string]*media.AudioPlayer{}
	s.mu.Unlock()
	for _, ap := range old {
		ap.Stop()
	}
	s.stopSoundPlayer(false)
	s.changed()
}

// keyframeMin spaces the keyframes viewers ask for. Every viewer (and,
// since 46, every viewer that lost a packet) asks the one encoder per layer;
// unthrottled, a few lossy viewers kept it making keyframes, which starved
// the frames between them of bits: blocky video and stale patches for
// everyone. A viewer still waiting asks again.
const keyframeMin = time.Second

func (s *Session) forceKeyframe(layer int) {
	now := time.Now()
	s.mu.Lock()
	src := s.src
	if s.keyAt == nil {
		s.keyAt = map[int]time.Time{}
	}
	if now.Sub(s.keyAt[layer]) < keyframeMin {
		s.mu.Unlock()
		s.keys.asked(layer, false)
		return
	}
	s.keyAt[layer] = now
	s.mu.Unlock()
	s.keys.asked(layer, src != nil)
	if src != nil {
		src.ForceKeyframe(layer)
	}
}

// SetSource switches what we share; "" or "none" stops sharing. Peers keep their
// tracks; they just see the new picture from the next keyframe.
func (s *Session) SetSource(kind media.SourceKind) error {
	s.mu.Lock()
	s.srcNote = ""
	s.mu.Unlock()
	if kind == "" || kind == "none" {
		s.srcMu.Lock()
		s.mu.Lock()
		old := s.src
		s.src, s.srcKind, s.encoder, s.rends = nil, "", "", nil
		s.srcGen++
		gen := s.srcGen
		s.previewOn = false
		wasSharing := s.feeds.Sharing(feeds.Self)
		s.feeds.Apply(feeds.Self, feeds.ShareStop)
		s.mu.Unlock()
		if old != nil {
			old.Stop()
			s.cfg.Log.Info("stopped sharing")
		}
		s.srcMu.Unlock()
		s.soundGate.ClearKind(media.SoundSelfLive)
		s.clearMeshTop(gen)
		s.reportSharing()
		s.regrid() // the preview goes
		s.windowAudio(kind)
		if wasSharing {
			s.stopAudioWithVideo()
		}
		s.changed()
		return nil
	}
	s.mu.Lock()
	wasSharing := s.feeds.Sharing(feeds.Self)
	s.feeds.Apply(feeds.Self, feeds.ShareStart)
	if !wasSharing { // our preview shows the way it did last
		switch s.selfView {
		case SelfViewWindow:
			s.feeds.Apply(feeds.Self, feeds.PopOut)
		case SelfViewHidden:
			s.feeds.Apply(feeds.Self, feeds.EyeToggle)
		}
	}
	newKind := s.srcKind != kind
	if newKind {
		s.rends = nil // a new source starts with HIGH and LOW only
	}
	s.mu.Unlock()
	if newKind {
		// Not SetTop(0): viewers would show we stopped sharing.
		s.mesh.ResetRenditions()
	}
	gen, err := s.startSource(kind)
	if err != nil {
		if s.clearMeshTop(gen) {
			s.reportSharing()
			s.regrid()
			s.windowAudio("")
			s.changed()
		}
		return err
	}
	if !wasSharing {
		s.playSound(media.SoundSelfLive)
	}
	s.syncMeshShareConfig()
	s.reportSharing()
	s.regrid() // the preview shows
	if !wasSharing {
		s.audioBackWithVideo()
	}
	s.windowAudio(kind)
	return nil
}

// stopAudioWithVideo stops the audio we share as our video stops, so nobody
// keeps sending sound without knowing; audioBackWithVideo brings it back.
// Audio shared on its own (no video) isn't touched.
func (s *Session) stopAudioWithVideo() {
	s.mu.Lock()
	cur := s.asel
	s.mu.Unlock()
	if cur.Mode == "" || cur.Mode == "none" {
		return
	}
	if err := s.setAudio(audio.Selection{Mode: "none"}); err != nil {
		s.cfg.Log.Warn("shared audio", "err", err)
		return
	}
	s.mu.Lock()
	if s.asel.Mode == "none" {
		s.audioBack = cur
	}
	s.mu.Unlock()
	s.cfg.Log.Info("shared audio stopped with the video", "mode", cur.Mode)
}

func (s *Session) audioBackWithVideo() {
	s.mu.Lock()
	back, cur := s.audioBack, s.asel
	s.audioBack = audio.Selection{}
	s.mu.Unlock()
	if back.Mode == "" || (cur.Mode != "" && cur.Mode != "none") {
		return
	}
	if err := s.setAudio(back); err != nil {
		s.cfg.Log.Warn("shared audio", "err", err)
	}
}

func (s *Session) clearMeshTop(gen uint64) bool {
	s.topMu.Lock()
	defer s.topMu.Unlock()
	s.mu.Lock()
	cur := s.src == nil && s.srcGen == gen && !s.closed
	s.mu.Unlock()
	if cur {
		s.mesh.SetTop(0)
	}
	return cur
}

// syncMeshShareConfig pushes our current top height, target FPS, HIGH bitrate,
// floor height, smaller-copy flag, slow-viewer policy, and upload limit to mesh.
// Must NOT be called under s.mu or from startSource (which runs under m.rend.mu
// during setRenditions).
func (s *Session) syncMeshShareConfig() {
	s.topMu.Lock()
	defer s.topMu.Unlock()
	s.mu.Lock()
	res := orRes(s.res)
	opts := NormalizeShareOptions(s.shareOpts)
	sharing := s.src != nil
	s.mu.Unlock()

	top, highKbps := 0, 0
	if sharing {
		sc := opts.SmallerCopy
		layers := media.LayersForOpts(media.LayerOpts{
			Resolution:  res,
			FPS:         opts.FPS,
			MinHeight:   opts.MinHeight,
			SmallerCopy: &sc,
		})
		top = layers[media.High].Height
		highKbps = layers[media.High].BitrateKbs
	}
	s.mesh.SetUploadLimit(opts.UploadKbps)
	s.mesh.SetShareConfig(top, opts.FPS, highKbps, opts.MinHeight, opts.SmallerCopy, opts.EffectiveSlowViewer() == SlowFPS)
}

// SetWindowAudio sets whether sharing a window shares its app's audio (the
// default). Turning it on while a window is shared switches to it now;
// turning it off changes nothing until the next SetSource (the caller sets
// the audio it wants instead).
func (s *Session) SetWindowAudio(on bool) {
	s.mu.Lock()
	s.noWinAudio = !on
	kind := s.srcKind
	s.mu.Unlock()
	if on {
		s.windowAudio(kind)
	}
}

// FollowsWindowAudio reports whether sharing a window shares its app's audio.
func (s *Session) FollowsWindowAudio() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.noWinAudio
}

// windowAudio makes the audio follow what we share: sharing a window shares
// that window's app (and its child processes) unless SetWindowAudio(false);
// when the audio followed a window and we share something else, it turns
// off (it never turns sound on for a screen).
func (s *Session) windowAudio(kind media.SourceKind) {
	pid := capture.WindowPID(string(kind))
	s.mu.Lock()
	cur, off := s.asel, s.noWinAudio
	s.mu.Unlock()
	var want audio.Selection
	switch {
	case pid > 0 && !off:
		want = audio.Selection{Mode: "window", PID: pid}
	case cur.Mode == "window": // not following a window any more
		want = audio.Selection{Mode: "none"}
	default:
		return
	}
	if want.Mode == cur.Mode && want.PID == cur.PID && slices.Equal(want.Apps, cur.Apps) {
		return
	}
	if err := s.setAudio(want); err != nil {
		s.cfg.Log.Warn("window audio", "err", err)
	}
}

// startSource (re)starts the capture of kind at our resolution, share options
// and renditions. It doesn't touch the mesh's top or run regrid synchronously
// (setRenditions runs under the mesh's rendition lock). If starting fails and
// leaves s.src nil, our own feed rolls back to not sharing and queues a
// background regrid + changed.
func (s *Session) startSource(kind media.SourceKind) (uint64, error) {
	gen, err := s.startSourceLocked(kind)
	if err != nil {
		s.mu.Lock()
		stopped := s.src == nil && s.srcGen == gen
		closed := s.closed
		if stopped {
			s.rends = nil
			s.previewOn = false
			s.feeds.Apply(feeds.Self, feeds.ShareStop)
		}
		s.mu.Unlock()
		if stopped {
			s.soundGate.ClearKind(media.SoundSelfLive)
			if !closed {
				s.triggerRegrid()
				s.changed()
			}
		}
	}
	return gen, err
}

func (s *Session) startSourceLocked(kind media.SourceKind) (uint64, error) {
	s.srcMu.Lock()
	defer s.srcMu.Unlock()
	s.mu.Lock()
	old := s.src
	s.src, s.srcKind, s.encoder = nil, "", ""
	s.srcGen++
	gen := s.srcGen
	res, opts, rends := s.res, NormalizeShareOptions(s.shareOpts), slices.Clone(s.rends)
	s.cpuFPS = 0 // a restart (new size or options) gets a fresh check
	s.mu.Unlock()
	if old != nil {
		old.Stop()
	}
	var highAUs atomic.Int64
	sc := opts.SmallerCopy
	src, enc, err := media.StartVideoSourceOpts(kind, media.LayerOpts{
		Resolution:  res,
		FPS:         opts.FPS,
		MinHeight:   opts.MinHeight,
		SmallerCopy: &sc,
		Renditions:  rends,
	}, s.cfg.Encoder, s.selfCh, func(layer int, au media.AccessUnit) {
		if layer == media.High {
			highAUs.Add(1)
		}
		s.keys.sent(s.cfg.Log, layer, rtc.HasIDR(au.Data))
		s.mesh.WriteVideo(layer, au.Data, au.Duration)
	})
	if err != nil {
		return gen, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		src.Stop()
		return gen, errors.New("session closed")
	}
	s.src, s.srcKind, s.encoder = src, kind, enc
	s.srcGen++
	gen = s.srcGen
	preview := s.feeds.Watched(feeds.Self) && s.cfg.View != "none" && !s.cfg.NoPlayback
	s.previewOn = preview
	// As regrid last chose: a restart (setRenditions) has no regrid after
	// it, and the corner picture alone shows the big preview.
	big := s.previewBig
	if s.feeds.State(feeds.Self) == feeds.PoppedOut || s.selfTile {
		big = true
	}
	s.mu.Unlock()
	src.SetPreviewBig(big)
	src.SetPreview(preview) // a restart (new resolution) keeps the preview
	s.updateCaptureIdle()
	s.cfg.Log.Info("sharing", "source", kind, "resolution", orRes(res), "fps", opts.FPS, "encoder", enc)
	src.SetBitrate(media.Low, s.mesh.LowKbps())
	if opts.UploadKbps > 0 {
		for layer := 0; layer < media.NumLayers; layer++ {
			if kbps := s.mesh.LayerBitrate(layer); kbps > 0 {
				src.SetBitrate(layer, kbps)
			}
		}
	}
	s.applyHighFPS()
	if orRes(res) != media.Res720 || opts.FPS > 30 {
		go s.checkEncodeRate(src, kind, orRes(res), &highAUs)
	}
	go s.watchSource(src, kind, orRes(res), enc, &highAUs)
	s.changed()
	return gen, nil
}

// watchSource stops sharing, with a note for the user, when src fails or the
// window it captures closes (X11 capture keeps running on a dead window).
// shareStatsEvery is how often the log records what we share and how the
// encoder keeps up.
const shareStatsEvery = 30 * time.Second

func (s *Session) watchSource(src *media.VideoSource, kind media.SourceKind, res media.Resolution, enc string, encoded *atomic.Int64) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	idleT := time.NewTicker(idleCheckEvery)
	defer idleT.Stop()
	var note string
	lastAt, lastOut, lastIn := time.Now(), encoded.Load(), src.OfferedFrames()
	for note == "" {
		select {
		case err := <-src.Err:
			if err == nil {
				return
			}
			s.cfg.Log.Error("video source stopped", "err", err)
			if windowClosed(kind) {
				note = "Sharing stopped: the window you shared was closed."
			} else {
				note = "Sharing stopped: Bellion Share lost what you were sharing. Pick it again, or something else. If it keeps happening, send your logs."
			}
		case <-idleT.C:
			s.updateCaptureIdle()
		case <-t.C:
			s.mu.Lock()
			current := s.src == src
			s.mu.Unlock()
			if !current { // replaced or stopped
				return
			}
			if capture.WindowGone(string(kind)) {
				s.cfg.Log.Warn("shared window closed", "source", kind)
				note = "Sharing stopped: the window you shared was closed."
			}
			if dt := time.Since(lastAt); dt >= shareStatsEvery {
				out, in := encoded.Load(), src.OfferedFrames()
				cpuMeterOnce.Do(func() { cpuMeter = load.NewMeter(10 * time.Second) })
				busy := 100 * cpuMeter.Busy()
				s.cfg.Log.Info("share stats", "source", kind, "res", res, "encoder", enc,
					"offeredFPS", fmt.Sprintf("%.1f", float64(in-lastIn)/dt.Seconds()),
					"encodedFPS", fmt.Sprintf("%.1f", float64(out-lastOut)/dt.Seconds()),
					"cpuBusy", fmt.Sprintf("%.0f", busy), "lowKbps", s.mesh.LowKbps(), "highFPS", s.HighFPS(), "idle", src.Idle())
				lastAt, lastOut, lastIn = time.Now(), out, in
			}
		case <-s.done:
			return
		}
	}
	s.mu.Lock()
	current := s.src == src
	s.mu.Unlock()
	if !current {
		return
	}
	s.SetSource("none")
	s.mu.Lock()
	s.srcNote = note
	s.mu.Unlock()
	s.changed()
}

// idleCheckEvery is how often a share looks for viewers coming or going,
// so a viewer waits at most this long (plus a keyframe) for a paused capture.
const idleCheckEvery = 200 * time.Millisecond

// updateCaptureIdle pauses our capture while nobody can see it (no peer
// wants our video and our self-preview is hidden) and resumes it when
// someone can (a 5K screen grabbed and converted 30 times a
// second for nobody made the whole app sluggish).
func (s *Session) updateCaptureIdle() {
	s.idleMu.Lock() // a stale look must not undo a newer one
	defer s.idleMu.Unlock()
	s.mu.Lock()
	src, preview, closed := s.src, s.previewOn, s.closed
	s.mu.Unlock()
	if src == nil || closed {
		return
	}
	src.SetIdle(!preview && !s.mesh.WantsVideo())
}

// windowClosed reports whether kind is a window that no longer exists.
// Windows capture fails the moment its window closes, which can be before
// the window is fully gone, so look twice.
func windowClosed(kind media.SourceKind) bool {
	if !strings.HasPrefix(string(kind), "window:") {
		return false
	}
	if capture.WindowGone(string(kind)) {
		return true
	}
	time.Sleep(500 * time.Millisecond)
	return capture.WindowGone(string(kind))
}

// The encoder must keep up with HIGH. After encodeWarmup we compare, over
// windows of encodeWindow, the frames the encoder put out with the frames
// it was handed: a still window offers few frames, and that is not the
// encoder's fault. Only encodeStrikes bad windows in a row (under
// keepUpShare of the frame rate we run HIGH at, and of what was offered)
// count as too slow (x264 on a 2012 Xeon does 4K at 9 fps). Then a desktop
// share first lowers the frame rate (cpuFPSSteps, text stays sharp), a game
// share the size; at 720p a game share drops to 30 fps. We watch for
// encodeChecks windows after each (re)start or step.
const (
	encodeWarmup  = 3 * time.Second
	encodeWindow  = 10 * time.Second
	encodeStrikes = 2
	encodeChecks  = 6
	keepUpShare   = 0.8
	// A hardware encoder (NVENC, QSV, AMF, Media Foundation) gets a longer
	// start: capture, a preview window or audio starting can stall the first
	// seconds (a 32-thread PC with NVENC was stepped down to 720p after 23
	// s). With the CPU mostly idle a slow window is more likely such a stall
	// than the encoder: it takes encodeStrikesIdle in a row then.
	encodeWarmupHW    = 15 * time.Second
	encodeStrikesIdle = 4
	encodeIdleBusy    = 0.5
	// After a step down we try the chosen size again after resRetryWait,
	// doubled after each try that stepped down again (up to resRetryMax).
	resRetryWait = 5 * time.Minute
	resRetryMax  = time.Hour
)

// hardwareEncoder reports whether a GStreamer encoder runs on the GPU.
func hardwareEncoder(name string) bool {
	return name != "" && name != "x264enc" && name != "openh264enc"
}

// strikesNeeded is how many slow windows in a row step down: more for a
// hardware encoder on a mostly idle CPU.
func strikesNeeded(encoder string, busy float64) int {
	if hardwareEncoder(encoder) && busy >= 0 && busy < encodeIdleBusy {
		return encodeStrikesIdle
	}
	return encodeStrikes
}

// resRetryAfter is how long to share at the lower size before trying the
// chosen one again, after tries earlier tries.
func resRetryAfter(tries int) time.Duration {
	d := resRetryWait
	for range tries {
		if d *= 2; d >= resRetryMax {
			return resRetryMax
		}
	}
	return d
}

var cpuFPSSteps = []int{30, 20, 15}

func fpsDropNote(res media.Resolution, target int, fps float64, next int) string {
	switch {
	case target == 60 && next == 30:
		return fmt.Sprintf("Your computer can't encode %s at 60 fps (%.0f fps); sharing at 30 fps.", media.ResolutionLabel(res), fps)
	case target == 30:
		return fmt.Sprintf("Your computer can't encode %s at 30 fps (%.0f fps); sharing at %d fps.", media.ResolutionLabel(res), fps, next)
	default:
		return fmt.Sprintf("Your computer can't encode %s at %d fps (%.0f fps); sharing at %d fps.", media.ResolutionLabel(res), target, fps, next)
	}
}

// encoderBehind reports whether out encoded frames in a window fall short
// of fps, our frame rate, and of the in frames offered (in < 0: unknown,
// judge by the rate alone).
func encoderBehind(in, out, fps int, window time.Duration) bool {
	if float64(out)/window.Seconds() >= keepUpShare*float64(fps) {
		return false
	}
	return in < 0 || float64(out) < keepUpShare*float64(in)
}

// highFPSLocked is the frame rate HIGH runs at now (s.mu held).
func (s *Session) highFPSLocked() int {
	fps := s.shareOpts.FPS
	if fps <= 0 {
		fps = media.HighFPS(s.mode)
	}
	for _, c := range []int{s.cpuFPS, s.bwFPS} {
		if c > 0 && c < fps {
			fps = c
		}
	}
	return fps
}

// HighFPS is the frame rate HIGH runs at now.
func (s *Session) HighFPS() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.highFPSLocked()
}

// applyHighFPS hands HIGH's frame rate to the encoder.
func (s *Session) applyHighFPS() {
	s.mu.Lock()
	src, fps := s.src, s.highFPSLocked()
	s.mu.Unlock()
	if src != nil {
		src.SetHighFPS(fps)
	}
}

// checkEncodeRate steps our resolution down if the encoder of src can't
// keep up with res, and tells the sharer why.
func (s *Session) checkEncodeRate(src *media.VideoSource, kind media.SourceKind, res media.Resolution, n *atomic.Int64) {
	enc := s.encoderName()
	if hardwareEncoder(enc) {
		time.Sleep(encodeWarmupHW)
	} else {
		time.Sleep(encodeWarmup)
	}
	strikes, need := 0, encodeStrikes
	var fps, offeredFPS float64
	for i := 0; i < encodeChecks; i++ {
		a, ia := n.Load(), src.HighOfferedFrames()
		s.mu.Lock()
		target := s.highFPSLocked()
		s.mu.Unlock()
		time.Sleep(encodeWindow)
		out, in := int(n.Load()-a), -1
		if ia >= 0 {
			in = src.HighOfferedFrames() - ia
		}
		s.mu.Lock()
		current := s.src == src && orRes(s.res) == res
		s.mu.Unlock()
		if !current {
			return
		}
		fps, offeredFPS = float64(out)/encodeWindow.Seconds(), float64(in)/encodeWindow.Seconds()
		if !encoderBehind(in, out, target, encodeWindow) {
			strikes = 0
			continue
		}
		strikes++
		busy := cpuMeter.Busy()
		need = strikesNeeded(enc, busy)
		s.cfg.Log.Info("encoder behind", "res", res, "fps", fps, "offeredFPS", offeredFPS, "targetFPS", target, "strike", strikes, "of", need, "cpuBusy", fmt.Sprintf("%.0f", 100*busy))
		if strikes < need {
			continue
		}
		if s.cpuStep(res, src, fps) {
			strikes, i = 0, -1 // a fresh set of checks at the lower rate
			continue
		}
		break
	}
	if strikes < need {
		s.retryWantedRes(src, kind, res)
		return
	}
	s.mu.Lock()
	current := s.src == src && orRes(s.res) == res
	lower := media.LowerResolution(res)
	targetFPS := NormalizeShareOptions(s.shareOpts).FPS
	if current && lower == "" && s.shareOpts.EffectiveSlowViewer() == SlowResolution && s.cpuFPS == 0 && targetFPS > 30 {
		s.cpuFPS = 30 // 720p60 is too much: 720p30
		s.resNote = fpsDropNote(res, targetFPS, fps, 30)
		s.mu.Unlock()
		s.cfg.Log.Warn("encoder too slow, lowering the frame rate", "res", res, "fps", fps, "to", 30)
		s.applyHighFPS()
		s.changed()
		return
	}
	if !current || lower == "" {
		s.mu.Unlock()
		return
	}
	if s.resWant == "" {
		s.resWant = res
	} else {
		s.resRetries++ // the chosen size failed again
	}
	s.res, s.rends = lower, nil
	s.resNote = fmt.Sprintf("Your computer can't encode %s fast enough (%.0f fps); sharing at %s.", media.ResolutionLabel(res), fps, media.ResolutionLabel(lower))
	s.mu.Unlock()
	s.cfg.Log.Warn("encoder too slow, stepping down", "from", res, "to", lower, "fps", fps, "offeredFPS", offeredFPS, "encoder", s.encoderName())
	s.mesh.ResetRenditions()
	if err := s.SetSource(kind); err != nil {
		s.cfg.Log.Error("restart at lower resolution", "err", err)
	}
}

// cpuStep lowers a framerate-first share's frame rate one step for a slow
// encoder (false: none left, or resolution-first).
func (s *Session) cpuStep(res media.Resolution, src *media.VideoSource, fps float64) bool {
	s.mu.Lock()
	opts := NormalizeShareOptions(s.shareOpts)
	if s.src != src || opts.EffectiveSlowViewer() == SlowResolution {
		s.mu.Unlock()
		return false
	}
	cur := opts.FPS
	if s.cpuFPS > 0 && s.cpuFPS < cur {
		cur = s.cpuFPS
	}
	next := 0
	for _, f := range cpuFPSSteps {
		if f < cur {
			next = f
			break
		}
	}
	if next == 0 {
		s.mu.Unlock()
		return false
	}
	s.cpuFPS = next
	s.resNote = fpsDropNote(res, opts.FPS, fps, next)
	s.mu.Unlock()
	s.cfg.Log.Warn("encoder too slow, lowering the frame rate", "res", res, "fps", fps, "to", next)
	s.applyHighFPS()
	s.changed()
	return true
}

func (s *Session) encoderName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.encoder
}

// SetResolution sets the size we share at: 720p, 1080p or 2160p (4K). If we
// are sharing, the source restarts at the new size; viewers see it from the
// next keyframe.
func (s *Session) SetResolution(res media.Resolution) error {
	if !media.ValidResolution(res) {
		return fmt.Errorf("unknown resolution %q", res)
	}
	s.mu.Lock()
	changed := s.res != res
	s.res, s.resNote = res, ""
	s.resWant, s.resRetries = "", 0
	kind := s.srcKind
	if changed {
		s.rends = nil // the ladder below the new top differs
	}
	s.mu.Unlock()
	if !changed {
		return nil
	}
	if kind != "" {
		s.mesh.ResetRenditions()
		return s.SetSource(kind)
	}
	s.changed()
	return nil
}

// ShareOptions returns our current outgoing share options.
func (s *Session) ShareOptions() ShareOptions {
	s.mu.Lock()
	defer s.mu.Unlock()
	return NormalizeShareOptions(s.shareOpts)
}

// SetShareOptions updates our outgoing FPS, slow-viewer policy, lowest
// resolution floor, smaller-copy toggle, and upload limit.
func (s *Session) SetShareOptions(o ShareOptions) error {
	o = NormalizeShareOptions(o)
	s.mu.Lock()
	prev := NormalizeShareOptions(s.shareOpts)
	s.shareOpts = o
	s.mode = modeForOptions(o)
	pipelineChanged := prev.FPS != o.FPS || prev.MinHeight != o.MinHeight || prev.SmallerCopy != o.SmallerCopy
	slowChanged := prev.EffectiveSlowViewer() != o.EffectiveSlowViewer()
	uploadChanged := prev.UploadKbps != o.UploadKbps
	floorChanged := prev.MinHeight != o.MinHeight || prev.SmallerCopy != o.SmallerCopy
	if pipelineChanged || slowChanged {
		s.resNote = ""
	}
	if floorChanged {
		s.rends = nil
	}
	if slowChanged && !pipelineChanged && s.cpuFPS > 0 {
		s.cpuFPS = 0
	}
	kind := s.srcKind
	s.mu.Unlock()

	if !pipelineChanged && !slowChanged && !uploadChanged {
		return nil
	}
	if pipelineChanged && kind != "" {
		if floorChanged {
			s.mesh.ResetRenditions()
		}
		return s.SetSource(kind)
	}
	s.syncMeshShareConfig()
	if slowChanged && kind != "" {
		s.applyHighFPS()
	}
	s.changed()
	return nil
}

// SetMode tunes the share for desktop (detail: 30 fps, lower the frame
// rate first when tight) or game (motion: 60 fps, lower the size first).
// If we are sharing, the source restarts.
func (s *Session) SetMode(mode media.ContentMode) error {
	if !media.ValidMode(mode) {
		return fmt.Errorf("unknown mode %q", mode)
	}
	o := s.ShareOptions()
	if mode == media.ModeGame {
		o.FPS = 60
		o.SlowViewer = SlowResolution
	} else {
		o.FPS = 30
		o.SlowViewer = SlowFPS
	}
	return s.SetShareOptions(o)
}

func orMode(m media.ContentMode) media.ContentMode {
	if m == "" {
		return media.ModeDesktop
	}
	return m
}

// Rename changes our display name in the room (the app keeps it for later
// sessions).
func (s *Session) Rename(name string) error {
	name = identity.CleanName(name)
	if name == "" {
		return errors.New("enter a name")
	}
	s.mu.Lock()
	s.cfg.Name = name
	s.mu.Unlock()
	if err := s.mesh.Rename(name); err != nil {
		return err
	}
	s.changed()
	return nil
}

// Transfer makes member id the room's owner (we must own it).
func (s *Session) Transfer(id string) error {
	if s.mesh.Owner() != s.mesh.Me() {
		return errors.New("only the room's owner can do that")
	}
	return s.mesh.Transfer(id)
}

// Kick removes member id from the room (we must own it); ban also keeps
// them from rejoining while the room lives.
func (s *Session) Kick(id string, ban bool) error {
	if s.mesh.Owner() != s.mesh.Me() {
		return errors.New("only the room's owner can do that")
	}
	return s.mesh.Kick(id, ban)
}

// SlotLost reports whether the session ended because, after a signaling
// drop, the server no longer had our slot (resume_failed): the
// room is likely still there, and a fresh join (Rejoin) gets back in.
func (s *Session) SlotLost() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.slotLost
}

// EndReason is why the session ended, if not by our leaving (e.g. kicked).
func (s *Session) EndReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.endReason
}

// Volunteer joins (on) or leaves the room's relay queue. Leaving as the
// active relay hands over to the next volunteer, or back to full mesh.
func (s *Session) Volunteer(on bool) error {
	s.mu.Lock()
	s.relayRefused = false
	s.mu.Unlock()
	return s.mesh.Volunteer(on)
}

// State returns a snapshot.
func (s *Session) State() State {
	stats := s.mesh.Stats()
	owner := s.mesh.Owner()
	vers := s.mesh.Versions()
	layers := s.mesh.Layers()
	relays, me := s.mesh.Relays(), s.mesh.Me()
	home := s.mesh.Home(me)
	children := map[string]int{}
	for _, id := range append(s.mesh.Members(), me) {
		if h := s.mesh.Home(id); h != "" && h != id {
			children[h]++
		}
	}
	entries := func(ids []string) []RelayEntry {
		var out []RelayEntry
		for _, id := range ids {
			e := RelayEntry{ID: id, Name: s.mesh.Name(id), Me: id == me, Mine: id == home && id != me, Children: children[id]}
			if e.Me {
				e.Name = s.cfg.Name
			}
			out = append(out, e)
		}
		return out
	}
	relay := entries(relays)
	var waitIDs []string
	for _, id := range s.mesh.Queue() {
		if !slices.Contains(relays, id) {
			waitIDs = append(waitIDs, id)
		}
	}
	waiting := entries(waitIDs)
	upLimit, autoUp, peakUp, txUp := s.mesh.UploadStats()
	s.mu.Lock()
	defer s.mu.Unlock()
	selfState := s.feeds.State(feeds.Self)
	opts := NormalizeShareOptions(s.shareOpts)
	st := State{
		Room:              DisplayCode(s.sig.Welcome.Room),
		Link:              ShareLink(s.cfg.Server, s.sig.Welcome.Room, s.pw),
		Source:            string(s.srcKind),
		Encoder:           s.encoder,
		LowKbps:           s.mesh.LowKbps(),
		UpdateRecommended: s.sig.Welcome.UpdateRecommended,
		Audio:             s.asel,
		AudioLevel:        s.alevel,
		Output:            s.output,
		Safety:            s.mesh.SafetyCode(),
		Started:           s.started.UTC().Format(time.RFC3339),
		Password:          s.pw,
		Relay:             relay,
		RelayPaused:       s.mesh.Paused(),
		RelayWaiting:      waiting,
		RelayMaxPeers:     proto.RelayMaxPeers,
		MeshComfortPeers:  proto.MeshComfortPeers,
		RelayRefused:      s.relayRefused,
		Encrypted:         s.mesh.Encrypted(),
		Reconnecting:      !s.reconnAt.IsZero(),
		IsOwner:           owner == s.mesh.Me(),
		IDKey:             s.idKey(),
		Name:              s.cfg.Name,
		Tag:               identity.Tag(s.idKey()),
		Resolution:        string(orRes(s.res)),
		Mode:              string(orMode(s.mode)),
		ShareOptions:      opts,
		EffectiveSlow:     opts.EffectiveSlowViewer(),
		UploadLimitKbps:   upLimit,
		AutoUploadKbps:    autoUp,
		PeakUploadKbps:    peakUp,
		TxKbps:            txUp,
		HighFPS:           s.highFPSLocked(),
		ResolutionNote:    s.resNote,
		SourceNote:        s.srcNote,
		SelfEye:           selfState.Eye(),
		SelfState:         selfState,
		AutoWatch:         s.feeds.AutoWatch(),
		ShareNotice:       s.shareNotice,
	}
	if !s.reconnAt.IsZero() {
		st.GiveUpAt = s.reconnAt.Add(sigclient.ResumeWindow).UTC().Format(time.RFC3339)
	}
	for _, m := range s.members {
		c := *m
		c.Client = vers[m.ID]
		c.Owner = m.ID == owner
		c.Tag = identity.Tag(s.mesh.IDKey(m.ID))
		fst := s.feeds.State(m.ID)
		c.FeedState = fst
		c.Sharing = fst.Sharing()
		c.Watching = fst.Watched()
		c.Eye = fst.Eye()
		if pl := s.players[m.ID]; pl != nil {
			c.Frames = pl.Frames()
		}
		if ap := s.aplayers[m.ID]; ap != nil {
			c.Audio = ap.Buffers()
		}
		h := s.hear[m.ID]
		c.Heard, c.Loud = h.heard, h.loud
		ps := stats[m.ID]
		c.Pair, c.RTTms, c.Tx, c.Rx, c.Loss = ps.Pair, int(ps.RTT*1000), ps.TxBytes, ps.RxBytes, ps.Loss
		c.TxKbps, c.RxKbps, c.FPSIn = s.rates.of(m.ID, c.Tx, c.Rx, c.Frames)
		c.Limit = ps.Limit
		if c.Limit == "" && s.resNote != "" {
			c.Limit = "cpu"
		}
		c.Hint = linkHint(c)
		if l, ok := layers[m.ID]; ok {
			switch l {
			case media.High:
				c.Layer = "high"
			case media.Low:
				c.Layer = "low"
			case media.Rend1080:
				c.Layer = "1080p"
			case media.Rend720:
				c.Layer = "720p"
			case media.Rend540:
				c.Layer = "540p"
			case rtc.LayerOff:
				c.Layer = "off"
			}
		}
		st.Members = append(st.Members, c)
	}
	sort.Slice(st.Members, func(i, j int) bool { return st.Members[i].Name < st.Members[j].Name })
	return st
}

// rates turns the byte and frame counters State reads into per-second
// rates over at least rateWindow.
type rates struct {
	last map[string]rateSample
}

type rateSample struct {
	at            time.Time
	tx, rx        uint64
	frames        int
	txK, rxK, fps int
}

const rateWindow = 2 * time.Second

// of returns tx and rx kbit/s and frames/s for id (s.mu held).
func (r *rates) of(id string, tx, rx uint64, frames int) (int, int, int) {
	if r.last == nil {
		r.last = map[string]rateSample{}
	}
	now := time.Now()
	p, ok := r.last[id]
	if !ok || tx < p.tx || rx < p.rx || frames < p.frames {
		r.last[id] = rateSample{at: now, tx: tx, rx: rx, frames: frames}
		return 0, 0, 0
	}
	if dt := now.Sub(p.at); dt >= rateWindow {
		sec := dt.Seconds()
		p = rateSample{at: now, tx: tx, rx: rx, frames: frames,
			txK: int(float64(tx-p.tx) * 8 / 1000 / sec), rxK: int(float64(rx-p.rx) * 8 / 1000 / sec),
			fps: int(float64(frames-p.frames)/sec + 0.5)}
		r.last[id] = p
	}
	return p.txK, p.rxK, p.fps
}

// linkHint says, in a line, what looks wrong with the link to m.
func linkHint(m Member) string {
	switch {
	case m.State == "unreachable":
		return "No connection between you yet; trying again. If it keeps failing, a firewall may block it: send logs from both sides."
	case m.State != "connected":
		return ""
	case m.Loss >= 0.05:
		return "Heavy loss: your upload or their download is overloaded. Try a lower resolution."
	case m.Loss >= 0.01:
		return "Some loss: the link is close to full."
	case m.RTTms >= 300:
		return "Long delay: a far route, or a full buffer on the way."
	case strings.Contains(m.Pair, "relay"):
		return "Through a TURN server: a direct path was blocked (firewall or NAT)."
	case m.Limit == "cpu":
		return "Your computer can't encode faster: see the note under Resolution."
	}
	return ""
}

// Diagnostics is a text summary for bug reports: no room
// password, keys or names; peers are numbered.
func (s *Session) Diagnostics() string {
	st := s.State()
	var b strings.Builder
	fmt.Fprintf(&b, "sharing: %s at %s, target %d fps (HIGH %d fps, slow=%s, floor=%dp, smaller=%v), encoder %s\n",
		orDefault(st.Source, "nothing"), st.Resolution, st.ShareOptions.FPS, st.HighFPS,
		st.EffectiveSlow, st.ShareOptions.MinHeight, st.ShareOptions.SmallerCopy, orDefault(st.Encoder, "-"))
	if st.ResolutionNote != "" {
		fmt.Fprintf(&b, "note: %s\n", st.ResolutionNote)
	}
	upLimit := "automatic"
	if st.UploadLimitKbps > 0 {
		upLimit = fmt.Sprintf("%d kbit/s", st.UploadLimitKbps)
	} else if st.AutoUploadKbps > 0 {
		upLimit = fmt.Sprintf("auto (%d kbit/s)", st.AutoUploadKbps)
	}
	fmt.Fprintf(&b, "encrypted: %v, relays: %d, LOW %d kbit/s, upload tx %d kbit/s (peak %d kbit/s, limit %s)\n",
		st.Encrypted, len(st.Relay), st.LowKbps, st.TxKbps, st.PeakUploadKbps, upLimit)
	for i, m := range st.Members {
		fmt.Fprintf(&b, "peer %d: %s, %s, path %s, rtt %d ms, loss %.1f%%, out %d kbit/s (%s",
			i+1, orDefault(m.Client, "?"), m.State, orDefault(m.Pair, "-"), m.RTTms, m.Loss*100, m.TxKbps, orDefault(m.Layer, "-"))
		if m.Limit != "" {
			fmt.Fprintf(&b, ", limited by %s", m.Limit)
		}
		fmt.Fprintf(&b, "), in %d kbit/s %d fps", m.RxKbps, m.FPSIn)
		if m.Recv > 0 {
			fmt.Fprintf(&b, " (%dp)", m.Recv)
		}
		if m.Auto != "" || m.Quality != "auto" {
			fmt.Fprintf(&b, ", asked %s/%s", m.Quality, orDefault(m.Auto, "-"))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.done }

// Rejoin is what an in-call update needs to come back to the same room
// sharing the same things.
type Rejoin struct {
	Server       string            `json:"server,omitempty"` // "" = the built-in default (older files)
	Room         string            `json:"room"`
	Password     string            `json:"password"`
	Source       media.SourceKind  `json:"source,omitempty"`
	Resolution   media.Resolution  `json:"resolution,omitempty"`
	Mode         media.ContentMode `json:"mode,omitempty"`
	ShareOptions *ShareOptions     `json:"shareOptions,omitempty"`
	Audio        audio.Selection   `json:"audio"`
	Output       string            `json:"output,omitempty"`
}

// Rejoin returns what to restore after an in-call update.
func (s *Session) Rejoin() Rejoin {
	s.mu.Lock()
	defer s.mu.Unlock()
	opts := NormalizeShareOptions(s.shareOpts)
	return Rejoin{Server: s.cfg.Server, Room: s.sig.Welcome.Room, Password: s.pw, Source: s.srcKind, Resolution: orRes(cmp.Or(s.resWant, s.res)), Mode: s.mode, ShareOptions: &opts, Audio: s.asel, Output: s.output}
}

// CloseForUpdate is Close telling the server we restart into a new build
// and rejoin shortly, so an owner gets the room back.
func (s *Session) CloseForUpdate() {
	s.sig.LeaveFor(proto.LeaveUpdate)
	s.Close()
}

// Close leaves the room and stops all media. Safe to call more than once.
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.feeds.LeaveAll()
	src, asrc, sharer := s.src, s.asrc, s.sharer
	players, aplayers := s.players, s.aplayers
	s.src, s.asrc, s.sharer = nil, nil, nil
	s.players, s.aplayers = map[string]*media.Player{}, map[string]*media.AudioPlayer{}
	s.mu.Unlock()

	if src != nil {
		src.Stop()
	}
	if asrc != nil {
		asrc.Stop()
	}
	if sharer != nil {
		sharer.Close()
	}
	for _, pl := range players {
		pl.Stop()
	}
	s.regrid() // closed: no tiles, so the windows go
	for _, ap := range aplayers {
		ap.Stop()
	}
	s.stopSoundPlayer(true)
	s.mesh.Close()
	close(s.done)
	s.changed()
}

// ParseLink accepts https://host/j/CODE#password and bshare:// or
// blite://join/CODE#password.
func ParseLink(s string) (code, pw string, err error) {
	u, err := parseURLRedacted(strings.TrimSpace(s))
	if err != nil {
		return "", "", err
	}
	var path string
	switch u.Scheme {
	case "bshare", "blite":
		path = strings.TrimPrefix(u.Host+u.Path, "join")
	case "https", "http":
		path = strings.TrimPrefix(u.Path, "/j")
	default:
		return "", "", fmt.Errorf("unknown link scheme %q", u.Scheme)
	}
	code = strings.Trim(path, "/")
	pw = u.Fragment // already percent-decoded by url.Parse
	if code == "" {
		return "", "", errors.New("link needs a room code")
	}
	return code, pw, nil
}

// ShareLink builds the https share link for a room on the given signaling server.
func ShareLink(server, room, pw string) string {
	u, err := url.Parse(server)
	if err != nil {
		return ""
	}
	l := "https://" + u.Host + "/j/" + room
	if pw != "" {
		l += "#" + url.PathEscape(pw)
	}
	return l
}

// DisplayCode formats a room code as XXXX-XXXX.
func DisplayCode(c string) string {
	if len(c) == 8 {
		return c[:4] + "-" + c[4:]
	}
	return c
}

// RandomPassword returns 72 random bits as 12 URL-safe characters.
func RandomPassword() string {
	b := make([]byte, 9)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func buildOf(c *proto.ClientMeta) int {
	if c == nil {
		return 0
	}
	return c.Build
}

// retryWantedRes shares at the size chosen before a slow encoder stepped us
// down again, once the lower size ran well for resRetryAfter.
func (s *Session) retryWantedRes(src *media.VideoSource, kind media.SourceKind, res media.Resolution) {
	s.mu.Lock()
	want, tries := s.resWant, s.resRetries
	s.mu.Unlock()
	if want == "" || want == res {
		return
	}
	wait := resRetryAfter(tries)
	for end := time.Now().Add(wait); time.Now().Before(end); {
		select {
		case <-s.done:
			return
		case <-time.After(encodeWindow):
		}
		s.mu.Lock()
		current := s.src == src && orRes(s.res) == res && s.resWant == want
		s.mu.Unlock()
		if !current {
			return
		}
	}
	s.mu.Lock()
	if s.src != src || orRes(s.res) != res || s.resWant != want {
		s.mu.Unlock()
		return
	}
	s.res, s.rends, s.resNote = want, nil, ""
	s.mu.Unlock()
	s.cfg.Log.Info("trying the chosen resolution again", "from", res, "to", want, "after", wait.String())
	s.mesh.ResetRenditions()
	if err := s.SetSource(kind); err != nil {
		s.cfg.Log.Error("restart at the chosen resolution", "err", err)
	}
}
