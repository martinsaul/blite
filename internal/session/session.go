// Package session is one Blite call: signaling, the mesh, the local video source
// and the remote players. The CLI and the local UI both drive it.
package session

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"blite/internal/audio"
	"blite/internal/capture"
	"blite/internal/identity"
	"blite/internal/load"
	"blite/internal/media"
	"blite/internal/proto"
	"blite/internal/rtc"
	"blite/internal/sigclient"
)

// Config for a session.
type Config struct {
	Server   string // wss://.../ws
	Name     string
	View     media.ViewKind // window | count | none
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
	Audio  int     `json:"audio,omitempty"` // decoded audio buffers (count view)
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
	Quality string `json:"quality"` // auto | low | off (what we ask them to send)
	// Auto is what we ask instead while nobody looks: low for a
	// small grid tile, off while every video window is minimized; "" none.
	Auto string `json:"auto,omitempty"`
	// WindowClosed: the user closed their video window.
	WindowClosed bool    `json:"windowClosed,omitempty"`
	asked        int     // the qualityLayer we last asked them for
	top          int     // their HIGH height as last heard; 0 = not sharing
	Volume       float64 `json:"volume"` // playback gain, 0..2
	Muted        bool    `json:"muted,omitempty"`
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
	Mode    string `json:"mode"`
	HighFPS int    `json:"highFps,omitempty"`
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
	// Layout is how we show the streams we receive.
	Layout Layout `json:"layout"`
	// GridClosed: the user closed the grid window.
	GridClosed bool `json:"gridClosed,omitempty"`
	// PreviewClosed: the user closed our floating self-preview.
	PreviewClosed bool `json:"previewClosed,omitempty"`
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
	src      *media.VideoSource
	srcKind  media.SourceKind
	encoder  string
	members  map[string]*Member
	players  map[string]*media.Player
	aplayers map[string]*media.AudioPlayer
	sharer   *audio.Sharer

	relayRefused bool
	reconnAt     time.Time         // our signaling resume started; zero = connected
	endReason    string            // why the session ended, for the user ("" = we left)
	res          media.Resolution  // what we share at ("" = 720p)
	mode         media.ContentMode // what the share is tuned for ("" = desktop)
	cpuFPS       int               // HIGH frame rate the encoder keeps up with (0: full)
	bwFPS        int               // HIGH frame rate congested viewers take (0: full)
	rends        []int             // rendition heights viewers were granted
	resNote      string            // why we share below the chosen size
	srcNote      string            // why sharing stopped on its own
	keyAt        map[int]time.Time // last forced keyframe per layer
	// srcMu serializes starting and stopping the capture. Two overlapping
	// starts (a double-clicked source; a start takes ~8 s on Windows)
	// each began a capture and the first was never stopped: two encoders
	// fed one track, so viewers saw the picture flip between sources and
	// break up. Not held around mesh calls (setRenditions runs under the
	// mesh's rendition lock).
	srcMu sync.Mutex
	// audioMu serializes SetAudio the same way: two overlapping
	// selections (the app list sends one per click) each started a
	// capture and the first was never stopped, so two captures fed one
	// track and listeners heard it cut in and out and race ahead.
	audioMu  sync.Mutex
	layout   Layout
	asrc     *media.AudioSource
	asrcDesc string
	asel     audio.Selection
	// aselBefore is the audio choice to go back to when we stop sharing a
	// window (whose app's audio we share meanwhile).
	aselBefore audio.Selection
	output     string
	started    time.Time
	closed     bool
	done       chan struct{}
	onChange   func()

	gridMu sync.Mutex // serializes regrid; held before mu
	// grids are the video windows: one per peer with separate windows
	// (keyed by peer id), or one grid window (key ""). gridKeys is what
	// each shows, so regrid skips no-op rebuilds.
	grids    map[string]*media.Grid
	gridKeys map[string]string
	// wins are our native video windows by the same key: they
	// stay open across rebuilds and layout switches. closedWins are keys
	// whose window the user closed, until ShowWindow, a layout change or
	// a new share of theirs.
	// openWindow opens one (tests replace it); nil: no own windows.
	wins       map[string]media.Window
	closedWins map[string]bool
	openWindow func(title string, w, h int) (media.Window, error)
	gridView   media.ViewKind // how grids show (tests: ViewCount)
	// selfCh is the channel our capture feeds the self-preview on
	//; previewOn opens it (mu). pv is the floating preview
	// window and pvClosed says the user closed it (gridMu).
	selfCh    string
	previewOn bool
	rates     rates // (mu)
	// Grid tiles are small; every video window is minimized.
	smallTiles, hiddenWins atomic.Bool
	pv                     *media.Preview
	pvClosed               bool
	pvWant                 bool // the layout wants the floating preview (gridMu)
	// stopped are the peers who stopped sharing (mu): their tile says so
	// for stoppedShow, fades out and goes. stopGen tells a stop's timers
	// from a later one's.
	stopped map[string]stopInfo
	stopGen int
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

// Layout is how we show the streams we receive.
type Layout struct {
	Mode string `json:"mode"` // windows (one per stream) | grid (all in one window)
	Rows int    `json:"rows"` // grid rows, 0 = auto
	Cols int    `json:"cols"` // grid columns, 0 = auto
	// Preview shows our own stream while we share: "" (off), "window"
	// (a small floating window) or "grid" (a corner of the grid window,
	// floating while there is no grid window).
	Preview string `json:"preview,omitempty"`
}

const (
	LayoutWindows = "windows"
	LayoutGrid    = "grid"
	maxGridSide   = 8

	PreviewOff    = ""
	PreviewWindow = "window"
	PreviewGrid   = "grid"
)

// Valid reports whether l is a layout SetLayout accepts.
func (l Layout) Valid() bool {
	return (l.Mode == LayoutWindows || l.Mode == LayoutGrid) &&
		l.Rows >= 0 && l.Cols >= 0 && l.Rows <= maxGridSide && l.Cols <= maxGridSide &&
		(l.Preview == PreviewOff || l.Preview == PreviewWindow || l.Preview == PreviewGrid)
}

// Create makes a new room. An empty password gets a random one.
func Create(ctx context.Context, cfg Config, password string) (*Session, error) {
	s := newSession(cfg, password)
	sig, err := sigclient.Create(ctx, cfg.Server, proto.Create{Password: password, Name: cfg.Name, IDKey: s.idKey(), Client: cfg.Client, Room: cfg.HostRoom, HostKey: cfg.HostKey, Owners: cfg.HostOwners, E2EE: cfg.E2EE && buildOf(cfg.Client) >= proto.E2EEBuild, Channel: cfg.Channel}, sigclient.Options{OnState: s.sigState})
	if err != nil {
		return nil, err
	}
	return s.start(sig)
}

// Join enters an existing room.
func Join(ctx context.Context, cfg Config, code, password string) (*Session, error) {
	s := newSession(cfg, password)
	sig, err := sigclient.Join(ctx, cfg.Server, proto.Join{Room: code, Password: password, Name: cfg.Name, IDKey: s.idKey(), Client: cfg.Client}, sigclient.Options{OnState: s.sigState})
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
	return &Session{cfg: cfg, pw: pw, members: map[string]*Member{}, players: map[string]*media.Player{},
		aplayers: map[string]*media.AudioPlayer{}, asel: audio.Selection{Mode: "none"}, stopped: map[string]stopInfo{},
		done: make(chan struct{}), started: time.Now(), selfCh: media.NewPreviewChannel()}
}

func (s *Session) idKey() string { return s.cfg.Identity.Public() }

// sigState follows our signaling connection: "reconnecting" while resuming.
func (s *Session) sigState(state string) {
	s.mu.Lock()
	if state == "reconnecting" {
		s.reconnAt = time.Now()
	} else {
		s.reconnAt = time.Time{}
	}
	s.mu.Unlock()
	s.cfg.Log.Info("signaling", "state", state)
	s.changed()
}

func (s *Session) start(sig *sigclient.Client) (*Session, error) {
	s.sig = sig
	for _, p := range sig.Welcome.Peers {
		s.members[p.PeerID] = &Member{ID: p.PeerID, Name: p.Name, State: "connecting", Quality: "auto", Volume: 1, KeyChanged: s.keyChanged(p.Name, p.IDKey)}
	}
	mesh, err := rtc.New(sig, rtc.Options{
		OnVideo:    s.pushVideo,
		OnAudio:    s.pushAudio,
		OnPeer:     s.peerEvent,
		OnKeyframe: s.forceKeyframe,
		OnTopology: s.changed,
		OnOwner:    func(string) { s.changed() },
		OnRenamed: func(id, name string) {
			s.mu.Lock()
			if m := s.members[id]; m != nil {
				m.Name = name
				m.KeyChanged = m.KeyChanged || s.keyChanged(name, s.mesh.IDKey(id))
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
	s.mesh = mesh
	mesh.SetFPSSteps(true) // desktop until SetMode says game
	go func() {
		<-mesh.Done()
		s.Close()
	}()
	if s.cfg.View == media.ViewWindow {
		s.openWindow = media.OpenVideoWindow
	}
	go s.watchDecoders()
	go s.watchVisibility()
	go s.watchWaiting()
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
	return s.startSource(kind)
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
// can't see them at all. A top of 0 says they stopped sharing: their tile
// says so instead of freezing on the last frame. A new share reopens the
// window the user closed on them.
func (s *Session) gotSending(origin string, height, top int, congested bool) {
	s.mu.Lock()
	m := s.members[origin]
	if m == nil {
		s.mu.Unlock()
		return
	}
	m.Recv, m.Notice = height, ""
	if height == 0 && top > 0 && congested {
		m.Notice = fmt.Sprintf("You can't see %s's stream: its quality is too high for your connection or computer right now.", m.Name)
	}
	started := m.top == 0 && top > 0
	m.top = top
	regrid := false
	if pl := s.players[origin]; top == 0 && pl != nil && !s.closed {
		go pl.Stop()
		delete(s.players, origin)
		s.stopGen++
		gen := s.stopGen
		s.stopped[origin] = stopInfo{time.Now(), gen}
		time.AfterFunc(stoppedShow, func() { s.fadeStopped(origin, gen) })
		time.AfterFunc(stoppedShow+stoppedFade+100*time.Millisecond, func() { s.dropStopped(origin, gen) })
		s.cfg.Log.Info("peer stopped sharing", "peer", origin)
		regrid = true
	}
	if _, ok := s.stopped[origin]; ok && top > 0 {
		delete(s.stopped, origin)
		regrid = true
		if s.players[origin] == nil && !s.closed && s.cfg.View != "none" && !s.cfg.NoPlayback {
			// Their tile goes live (waiting for the first frame) at once.
			if pl, err := media.StartPlayer(s.viewLocked()); err == nil {
				s.players[origin] = pl
			} else {
				s.cfg.Log.Error("player", "err", err)
			}
		}
	}
	grid := s.layout.Mode == LayoutGrid
	s.mu.Unlock()
	reopen := false
	if started {
		s.gridMu.Lock()
		reopen = s.closedWins[origin] || grid && s.closedWins[""]
		delete(s.closedWins, origin)
		if grid {
			delete(s.closedWins, "")
		}
		s.gridMu.Unlock()
		if reopen {
			s.cfg.Log.Info("peer shares again: reopening their window", "peer", origin)
		}
	}
	if regrid || reopen { // not on the mesh's goroutine: a window may take seconds
		go func() {
			s.regrid(false)
			if reopen {
				s.applyAutoQuality()
			}
		}()
	}
	s.changed()
}

// fadeStopped fades out the tile of peer id's ended share, unless they
// shared again since.
func (s *Session) fadeStopped(id string, gen int) {
	s.mu.Lock()
	cur := s.stopped[id].gen == gen && !s.closed
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

// dropStopped removes the faded tile of peer id's ended share.
func (s *Session) dropStopped(id string, gen int) {
	s.mu.Lock()
	cur := s.stopped[id].gen == gen
	if cur {
		delete(s.stopped, id)
	}
	s.mu.Unlock()
	if cur {
		s.regrid(false)
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

func (s *Session) changed() {
	s.mu.Lock()
	f, windows := s.onChange, s.cfg.View == media.ViewWindow
	s.mu.Unlock()
	if windows { // someone left or was renamed: their window goes or relabels
		go s.regrid(false)
	}
	if f != nil {
		f()
	}
}

func (s *Session) peerEvent(id, name, state string) {
	s.mu.Lock()
	switch state {
	case "connected":
		m := s.members[id]
		if m == nil {
			m = &Member{ID: id, Name: name, Quality: "auto", Volume: 1, KeyChanged: s.keyChanged(name, s.mesh.IDKey(id))}
			s.members[id] = m
		}
		m.State = "connected"
		if m.Quality != "auto" {
			// A new connection starts uncapped on their side: say it again.
			go s.mesh.SetMaxLayer(id, qualityLayer[m.Quality])
		}
	case "reconnecting":
		if m := s.members[id]; m != nil {
			m.State = "reconnecting"
		}
	case "unreachable": // still in the room; we try again
		m := s.members[id]
		if m == nil {
			m = &Member{ID: id, Name: name, Quality: "auto", Volume: 1}
			s.members[id] = m
		}
		m.State = "unreachable"
	case "untrusted":
		m := s.members[id]
		if m == nil {
			m = &Member{ID: id, Name: name, Quality: "auto", Volume: 1}
			s.members[id] = m
		}
		m.State = "untrusted"
	case "gone":
		delete(s.members, id)
		delete(s.stopped, id) // their tile goes at once
		if pl := s.players[id]; pl != nil {
			go pl.Stop()
			delete(s.players, id)
		}
		if ap := s.aplayers[id]; ap != nil {
			go ap.Stop()
			delete(s.aplayers, id)
		}
	}
	s.mu.Unlock()
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
	pl := s.players[peer]
	added := false
	if st, ok := s.stopped[peer]; ok && pl == nil {
		if time.Since(st.at) < stoppedGrace {
			s.mu.Unlock()
			return // in flight when they stopped
		}
		delete(s.stopped, peer) // they share again: we missed them saying so
		added = true
	}
	if pl == nil {
		var err error
		if pl, err = media.StartPlayer(s.viewLocked()); err != nil {
			s.mu.Unlock()
			s.cfg.Log.Error("player", "err", err)
			return
		}
		s.players[peer] = pl
		added = added || pl.Channel != ""
	}
	s.mu.Unlock()
	if added {
		go s.regrid(false)
	}
	pl.Push(au)
}

// viewLocked is how a new player shows its stream (mu held): video windows
// (separate or one grid) all show it through a Grid, which draws the
// mute and pause buttons.
func (s *Session) viewLocked() media.ViewKind {
	if s.cfg.View == media.ViewWindow {
		return media.ViewGrid
	}
	return s.cfg.View
}

// SetLayout chooses separate windows or one grid window for the streams we
// receive, and where our self-preview shows.
func (s *Session) SetLayout(l Layout) error {
	if !l.Valid() {
		return errors.New("unknown layout")
	}
	s.mu.Lock()
	s.layout = l
	s.mu.Unlock()
	s.regrid(true)
	s.changed()
	return nil
}

// Separate video windows start at this size.
const windowW, windowH = 1280, 720

// regrid makes the video windows show the current players (one window
// each, or all in one grid window in name order) and shows our
// self-preview where the layout wants it. It rebuilds a window only when
// something it shows changed, unless force (which also reopens windows the
// user closed).
func (s *Session) regrid(force bool) {
	s.gridMu.Lock()
	defer s.gridMu.Unlock()
	s.mu.Lock()
	l := s.layout
	src := s.src
	preview := src != nil && !s.closed && l.Preview != PreviewOff && s.cfg.View != "none"
	var tiles []media.Tile
	if !s.closed {
		for id, pl := range s.players {
			if pl.Channel == "" {
				continue
			}
			t := media.Tile{Channel: pl.Channel, Label: id, ID: id}
			if m := s.members[id]; m != nil {
				t.Label, t.Muted, t.Paused, t.Volume = m.Name, m.Muted, m.Quality == "off", m.Volume
			}
			if tag := identity.Tag(s.mesh.IDKey(id)); tag != "" {
				t.Label += "#" + tag
			}
			tiles = append(tiles, t)
		}
		for id := range s.stopped {
			m := s.members[id]
			if m == nil || s.players[id] != nil {
				continue
			}
			t := media.Tile{Label: m.Name, ID: id, Stopped: true}
			if tag := identity.Tag(s.mesh.IDKey(id)); tag != "" {
				t.Label += "#" + tag
			}
			tiles = append(tiles, t)
		}
	}
	s.mu.Unlock()
	sort.Slice(tiles, func(i, j int) bool {
		if a, b := strings.ToLower(tiles[i].Label), strings.ToLower(tiles[j].Label); a != b {
			return a < b
		}
		return tiles[i].ID < tiles[j].ID
	})
	grid := l.Mode == LayoutGrid
	pip := ""
	if preview && l.Preview == PreviewGrid && len(tiles) > 0 {
		pip = s.selfCh // in the grid, or in the first person's window
	}
	s.mu.Lock()
	s.previewOn = preview
	s.mu.Unlock()
	if src != nil {
		src.SetPreview(preview)
	}
	small := grid && len(tiles) > 0 && media.GridTileHeight(len(tiles), l.Rows, l.Cols) <= smallTileH
	if s.smallTiles.Swap(small) != small {
		go s.applyAutoQuality()
	}
	want := map[string]media.GridConfig{}
	if grid && len(tiles) > 0 {
		want[""] = media.GridConfig{Tiles: tiles, Rows: l.Rows, Cols: l.Cols, Pip: pip}
	}
	if !grid {
		for i, t := range tiles {
			cfg := media.GridConfig{Tiles: []media.Tile{t}, Width: windowW, Height: windowH}
			if i == 0 {
				cfg.Pip = pip
			}
			want[t.ID] = cfg
		}
	}
	if s.grids == nil {
		s.grids, s.gridKeys = map[string]*media.Grid{}, map[string]string{}
		s.wins, s.closedWins = map[string]media.Window{}, map[string]bool{}
	}
	if force {
		clear(s.closedWins) // a layout change shows them all again
	}
	for k := range s.closedWins {
		delete(want, k)
	}
	for k, g := range s.grids {
		if _, ok := want[k]; !ok {
			g.Stop()
			delete(s.grids, k)
			delete(s.gridKeys, k)
		}
	}
	// Windows no key wants any more go to the keys that lack one; only
	// the ones left over close (a layout switch keeps them open).
	var free []media.Window
	for k, w := range s.wins {
		if _, ok := want[k]; !ok {
			free = append(free, w)
			delete(s.wins, k)
		}
	}
	order := []string{""}
	for _, t := range tiles {
		order = append(order, t.ID)
	}
	for _, k := range order {
		cfg, ok := want[k]
		if !ok {
			continue
		}
		if s.wins[k] == nil && s.openWindow != nil {
			if len(free) > 0 {
				s.wins[k], free = free[0], free[1:]
			} else if w, err := s.openWindow(windowTitle(cfg), windowW, windowH); err == nil {
				s.wins[k] = w
				go s.watchWindow(w)
			} else {
				s.cfg.Log.Warn("own video window", "err", err)
			}
		}
		cfg.Window = s.wins[k]
		s.regridLocked(force, k, cfg)
	}
	for _, w := range free {
		w.Destroy()
	}
	s.repreview(force, preview && pip == "")
}

// windowTitle names a video window by who it shows.
func windowTitle(cfg media.GridConfig) string {
	if len(cfg.Tiles) == 1 && cfg.Rows == 0 && cfg.Cols == 0 && cfg.Width == windowW {
		return "Bellion Share: " + cfg.Tiles[0].Label
	}
	return "Bellion Share"
}

// watchWindow notes when the user closes w: its stream stops showing
// until ShowWindow (or a layout change, or their next share), and we ask
// for no video meanwhile.
func (s *Session) watchWindow(w media.Window) {
	select {
	case <-w.Closed():
	case <-s.done:
		return
	}
	s.gridMu.Lock()
	for k, v := range s.wins {
		if v != w {
			continue
		}
		if g := s.grids[k]; g != nil {
			g.Stop()
		}
		delete(s.grids, k)
		delete(s.gridKeys, k)
		delete(s.wins, k)
		s.closedWins[k] = true
		s.cfg.Log.Info("video window closed", "key", k)
	}
	s.gridMu.Unlock()
	w.Destroy()
	s.applyAutoQuality()
	s.changed()
}

// ShowWindow reopens the video window the user closed: peer id's, "" the
// grid window, "self" our preview, or "*" all of them.
func (s *Session) ShowWindow(id string) error {
	s.gridMu.Lock()
	switch id {
	case "*":
		clear(s.closedWins)
		s.pvClosed = false
	case "self":
		s.pvClosed = false
	default:
		delete(s.closedWins, id)
	}
	s.gridMu.Unlock()
	s.regrid(false)
	s.applyAutoQuality()
	s.changed()
	return nil
}

// unwatched are the window keys nobody looks at: closed or minimized.
func (s *Session) unwatched() map[string]bool {
	s.gridMu.Lock()
	defer s.gridMu.Unlock()
	out := map[string]bool{}
	for k := range s.closedWins {
		out[k] = true
	}
	for k, w := range s.wins {
		if w.Minimized() {
			out[k] = true
		}
	}
	return out
}

// regridLocked (re)starts video window k (gridMu held).
func (s *Session) regridLocked(force bool, k string, cfg media.GridConfig) {
	type shown struct { // not the state: SetState updates that
		channel, label, id string
		stopped            bool
	}
	var what []shown
	for _, t := range cfg.Tiles {
		what = append(what, shown{t.Channel, t.Label, t.ID, t.Stopped})
	}
	key := fmt.Sprint(cfg.Rows, cfg.Cols, what, cfg.Pip, fmt.Sprintf("%p", cfg.Window)) // a window handed over: rebuild into it
	if g := s.grids[k]; g != nil {
		if !force && key == s.gridKeys[k] {
			return
		}
		g.Stop()
		delete(s.grids, k)
	}
	s.gridKeys[k] = key
	if cfg.Window != nil {
		cfg.Window.SetTitle(windowTitle(cfg))
	}
	cfg.View, cfg.OnControl = media.ViewWindow, s.control
	if s.gridView != "" {
		cfg.View = s.gridView
	}
	g, err := media.StartGrid(cfg)
	if err != nil && cfg.Pip != "" {
		// Never lose their streams over our preview (build 78 lacked an
		// element for its frame on Windows).
		s.cfg.Log.Error("video window with preview", "err", err)
		cfg.Pip = ""
		g, err = media.StartGrid(cfg)
	}
	if err != nil {
		s.cfg.Log.Error("video window", "err", err)
		return
	}
	s.grids[k] = g
	if cfg.Window != nil && !g.InWindow() {
		// No sink draws into our windows here: let sinks open their own
		// from now on, and drop ours (it would stay empty).
		s.cfg.Log.Warn("own video windows unusable", "sink", g.Sink)
		s.openWindow = nil
		for wk, w := range s.wins {
			w.Destroy()
			delete(s.wins, wk)
		}
	}
	if k == "" {
		r, c := media.GridSize(len(cfg.Tiles), cfg.Rows, cfg.Cols)
		s.cfg.Log.Info("grid window", "streams", len(cfg.Tiles), "rows", r, "cols", c, "preview", cfg.Pip != "", "sink", g.Sink)
	}
}

// control handles a button clicked on peer id's video: mute
// toggles their audio, pause their video (we ask them to send none).
func (s *Session) control(id string, c media.Control, v float64) {
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
	case media.ControlPause:
		if quality == "off" {
			quality = "auto"
		} else {
			quality = "off"
		}
	case media.ControlVolume:
		volume = v
		if v > 0 {
			muted = false
		}
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

// repreview opens or closes the floating self-preview window (gridMu
// held). A window the user closed stays closed until force or ShowWindow.
func (s *Session) repreview(force, want bool) {
	if force {
		s.pvClosed = false
	}
	s.pvWant = want
	if s.pv != nil && (!want || s.pvClosed) {
		s.pv.Stop()
		s.pv = nil
	}
	if !want || s.pvClosed || s.pv != nil {
		return
	}
	view := media.ViewWindow
	if s.cfg.View == media.ViewCount || s.gridView == media.ViewCount {
		view = media.ViewCount
	}
	pv, err := media.StartPreview(s.selfCh, view)
	if err != nil {
		s.cfg.Log.Error("preview window", "err", err)
		return
	}
	s.pv = pv
	s.cfg.Log.Info("preview window", "sink", pv.Sink)
	go func() {
		var err error
		select {
		case <-pv.Closed():
		case err = <-pv.Err: // a failure, or the sink's own window was closed
			if err == nil {
				return
			}
		case <-s.done:
			return
		}
		s.gridMu.Lock()
		if s.pv == pv {
			s.pvClosed = true
			s.cfg.Log.Info("preview window closed", "err", err)
		}
		s.gridMu.Unlock()
		s.regrid(false)
	}()
}

// PreviewFrames is how many frames our floating self-preview has shown
// (tests; -1 if none or unknown).
func (s *Session) PreviewFrames() int {
	s.gridMu.Lock()
	defer s.gridMu.Unlock()
	if s.pv == nil {
		return -1
	}
	return s.pv.Frames()
}

func (s *Session) audioSink() string {
	if s.cfg.View == media.ViewCount {
		return "count"
	}
	if s.cfg.View == "none" {
		return "fakesink sync=false"
	}
	return audio.PlaybackSink(s.output)
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

// SetAudio chooses what audio we share. Mode "test" sends a tone (synthetic
// peers); "system" and "apps" need PipeWire (Linux) for now.
func (s *Session) SetAudio(sel audio.Selection) error {
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
		s.asrc, s.asrcDesc = nil, ""
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
	s.asel = sel
	s.mu.Unlock()
	s.changed()
	return nil
}

func (s *Session) ensureSharer() (*audio.Sharer, error) {
	s.mu.Lock()
	sharer := s.sharer
	s.mu.Unlock()
	if sharer != nil {
		return sharer, nil
	}
	sharer, err := audio.NewSharer()
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
	// Opus in-band FEC follows the loss peers report.
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for range t.C {
			s.mu.Lock()
			current := s.asrc == src && !s.closed
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

var qualityLayer = map[string]int{"auto": 0, "low": 1, "off": 2}

func (m *Member) gain() float64 {
	if m.Muted {
		return 0
	}
	return m.Volume
}

// SetPeer changes our viewer settings for one peer: the best quality we ask
// them to send (auto | low | off), and their playback volume and mute.
func (s *Session) SetPeer(id, quality string, volume float64, muted bool) error {
	layer, ok := qualityLayer[quality]
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
	layer = max(layer, qualityLayer[m.Auto])
	send := m.asked != layer
	m.asked = layer
	ap, gain := s.aplayers[id], m.gain()
	s.mu.Unlock()
	if ap != nil {
		ap.SetVolume(gain)
	}
	s.gridMu.Lock()
	for _, g := range s.grids {
		g.SetState(id, muted, quality == "off", volume)
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
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		waiting := map[string]bool{}
		s.mu.Lock()
		for id, pl := range s.players {
			m := s.members[id]
			_, decoded := pl.Progress() // Frames counts the headless view only
			waiting[id] = decoded == 0 && (m == nil || m.Quality != "off")
		}
		s.mu.Unlock()
		s.gridMu.Lock()
		for _, g := range s.grids {
			for id, w := range waiting {
				g.SetWaiting(id, w)
			}
		}
		s.gridMu.Unlock()
	}
}

// applyAutoQuality asks each peer for the most their stream needs now.
func (s *Session) applyAutoQuality() {
	auto := ""
	switch {
	case s.hiddenWins.Load():
		auto = "off"
	case s.smallTiles.Load():
		auto = "low"
	}
	unwatched := s.unwatched()
	type ask struct {
		id    string
		layer int
	}
	var asks []ask
	s.mu.Lock()
	grid := s.layout.Mode == LayoutGrid
	for id, m := range s.members {
		m.Auto = auto
		if grid && unwatched[""] || !grid && unwatched[id] {
			m.Auto = "off" // its window is minimized or closed
		}
		if l := max(qualityLayer[m.Quality], qualityLayer[m.Auto]); l != m.asked {
			m.asked = l
			asks = append(asks, ask{id, l})
		}
	}
	s.mu.Unlock()
	for _, a := range asks {
		if err := s.mesh.SetMaxLayer(a.id, a.layer); err != nil {
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
		return
	}
	s.keyAt[layer] = now
	s.mu.Unlock()
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
		s.mu.Unlock()
		if old != nil {
			old.Stop()
			s.cfg.Log.Info("stopped sharing")
		}
		s.srcMu.Unlock()
		s.mesh.SetTop(0)
		s.regrid(false) // the preview goes
		s.windowAudio(kind)
		s.changed()
		return nil
	}
	s.mu.Lock()
	newKind := s.srcKind != kind
	if newKind {
		s.rends = nil // a new source starts with HIGH and LOW only
	}
	s.mu.Unlock()
	if newKind {
		// Not SetTop(0): viewers would show we stopped sharing.
		s.mesh.ResetRenditions()
	}
	if err := s.startSource(kind); err != nil {
		s.mesh.SetTop(0)
		return err
	}
	s.mu.Lock()
	top := media.TopHeight(orRes(s.res))
	s.mu.Unlock()
	s.mesh.SetTop(top)
	s.regrid(false) // the preview shows
	s.windowAudio(kind)
	return nil
}

// windowAudio makes the audio follow what we share: sharing a window shares
// that window's app (and its child processes); sharing something else goes
// back to the audio chosen before.
func (s *Session) windowAudio(kind media.SourceKind) {
	pid := capture.WindowPID(string(kind))
	s.mu.Lock()
	cur, before := s.asel, s.aselBefore
	s.mu.Unlock()
	var want audio.Selection
	switch {
	case pid > 0:
		if cur.Mode != "window" {
			before = cur
		}
		want = audio.Selection{Mode: "window", PID: pid}
	case cur.Mode == "window":
		want = before
		if want.Mode == "" {
			want.Mode = "none"
		}
	default:
		return
	}
	s.mu.Lock()
	s.aselBefore = before
	s.mu.Unlock()
	if want.Mode == cur.Mode && want.PID == cur.PID && slices.Equal(want.Apps, cur.Apps) {
		return
	}
	if err := s.SetAudio(want); err != nil {
		s.cfg.Log.Warn("window audio", "err", err)
	}
}

// startSource (re)starts the capture of kind at our resolution and
// renditions. It doesn't touch the mesh's top (setRenditions runs under
// the mesh's rendition lock).
func (s *Session) startSource(kind media.SourceKind) error {
	s.srcMu.Lock()
	defer s.srcMu.Unlock()
	s.mu.Lock()
	old := s.src
	s.src, s.srcKind, s.encoder = nil, "", ""
	res, mode, rends := s.res, s.mode, slices.Clone(s.rends)
	s.cpuFPS = 0 // a restart (new size or mode) gets a fresh check
	s.mu.Unlock()
	if old != nil {
		old.Stop()
	}
	var highAUs atomic.Int64
	src, enc, err := media.StartVideoSource(kind, res, mode, rends, s.cfg.Encoder, s.selfCh, func(layer int, au media.AccessUnit) {
		if layer == media.High {
			highAUs.Add(1)
		}
		s.mesh.WriteVideo(layer, au.Data, au.Duration)
	})
	if err != nil {
		s.changed()
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		src.Stop()
		return errors.New("session closed")
	}
	s.src, s.srcKind, s.encoder = src, kind, enc
	preview := s.previewOn
	s.mu.Unlock()
	src.SetPreview(preview) // a restart (new resolution) keeps the preview
	s.cfg.Log.Info("sharing", "source", kind, "resolution", orRes(res), "encoder", enc)
	src.SetBitrate(media.Low, s.mesh.LowKbps())
	s.applyHighFPS()
	if orRes(res) != media.Res720 || mode == media.ModeGame {
		go s.checkEncodeRate(src, kind, orRes(res), &highAUs)
	}
	go s.watchSource(src, kind, orRes(res), enc, &highAUs)
	s.changed()
	return nil
}

// watchSource stops sharing, with a note for the user, when src fails or the
// window it captures closes (X11 capture keeps running on a dead window).
// shareStatsEvery is how often the log records what we share and how the
// encoder keeps up.
const shareStatsEvery = 30 * time.Second

func (s *Session) watchSource(src *media.VideoSource, kind media.SourceKind, res media.Resolution, enc string, encoded *atomic.Int64) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
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
					"cpuBusy", fmt.Sprintf("%.0f", busy), "lowKbps", s.mesh.LowKbps(), "highFPS", s.HighFPS())
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
)

var cpuFPSSteps = []int{20, 15}

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
	fps := media.HighFPS(s.mode)
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
	time.Sleep(encodeWarmup)
	strikes := 0
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
		s.cfg.Log.Info("encoder behind", "res", res, "fps", fps, "offeredFPS", offeredFPS, "targetFPS", target, "strike", strikes)
		if strikes < encodeStrikes {
			continue
		}
		if s.cpuStep(res, src, fps) {
			strikes, i = 0, -1 // a fresh set of checks at the lower rate
			continue
		}
		break
	}
	if strikes < encodeStrikes {
		return
	}
	s.mu.Lock()
	current := s.src == src && orRes(s.res) == res
	lower := media.LowerResolution(res)
	if current && lower == "" && s.mode == media.ModeGame && s.cpuFPS == 0 {
		s.cpuFPS = 30 // 720p60 is too much: 720p30
		s.resNote = fmt.Sprintf("Your computer can't encode %s at 60 fps (%.0f fps); sharing at 30 fps.", media.ResolutionLabel(res), fps)
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
	s.res, s.rends = lower, nil
	s.resNote = fmt.Sprintf("Your computer can't encode %s fast enough (%.0f fps); sharing at %s.", media.ResolutionLabel(res), fps, media.ResolutionLabel(lower))
	s.mu.Unlock()
	s.cfg.Log.Warn("encoder too slow, stepping down", "from", res, "to", lower, "fps", fps, "offeredFPS", offeredFPS, "encoder", s.encoderName())
	s.mesh.ResetRenditions()
	if err := s.SetSource(kind); err != nil {
		s.cfg.Log.Error("restart at lower resolution", "err", err)
	}
}

// cpuStep lowers a desktop share's frame rate one step for a slow encoder
// (false: none left, or a game share).
func (s *Session) cpuStep(res media.Resolution, src *media.VideoSource, fps float64) bool {
	s.mu.Lock()
	if s.src != src || s.mode == media.ModeGame {
		s.mu.Unlock()
		return false
	}
	next := 0
	for _, f := range cpuFPSSteps {
		if s.cpuFPS == 0 || f < s.cpuFPS {
			next = f
			break
		}
	}
	if next == 0 {
		s.mu.Unlock()
		return false
	}
	s.cpuFPS = next
	s.resNote = fmt.Sprintf("Your computer can't encode %s at 30 fps (%.0f fps); sharing at %d fps.", media.ResolutionLabel(res), fps, next)
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

// SetMode tunes the share for desktop (detail: 30 fps, lower the frame
// rate first when tight) or game (motion: 60 fps, lower the size first).
// If we are sharing, the source restarts.
func (s *Session) SetMode(mode media.ContentMode) error {
	if !media.ValidMode(mode) {
		return fmt.Errorf("unknown mode %q", mode)
	}
	if mode == "" {
		mode = media.ModeDesktop
	}
	s.mu.Lock()
	changed := orMode(s.mode) != mode
	s.mode = mode
	if changed {
		s.resNote = ""
	}
	kind := s.srcKind
	s.mu.Unlock()
	s.mesh.SetFPSSteps(mode == media.ModeDesktop)
	if !changed {
		return nil
	}
	if kind != "" {
		return s.SetSource(kind)
	}
	s.changed()
	return nil
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
	s.gridMu.Lock()
	closedWins := maps.Clone(s.closedWins)
	pvClosed := s.pvClosed && s.pvWant
	s.gridMu.Unlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{
		Room:              DisplayCode(s.sig.Welcome.Room),
		Link:              ShareLink(s.cfg.Server, s.sig.Welcome.Room, s.pw),
		Source:            string(s.srcKind),
		Encoder:           s.encoder,
		LowKbps:           s.mesh.LowKbps(),
		UpdateRecommended: s.sig.Welcome.UpdateRecommended,
		Audio:             s.asel,
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
		HighFPS:           s.highFPSLocked(),
		ResolutionNote:    s.resNote,
		SourceNote:        s.srcNote,
		Layout:            s.layout,
		GridClosed:        closedWins[""] && s.layout.Mode == LayoutGrid,
		PreviewClosed:     pvClosed,
	}
	if st.Layout.Mode == "" {
		st.Layout.Mode = LayoutWindows
	}
	if !s.reconnAt.IsZero() {
		st.GiveUpAt = s.reconnAt.Add(sigclient.ResumeWindow).UTC().Format(time.RFC3339)
	}
	for _, m := range s.members {
		c := *m
		c.Client = vers[m.ID]
		c.Owner = m.ID == owner
		c.Tag = identity.Tag(s.mesh.IDKey(m.ID))
		if pl := s.players[m.ID]; pl != nil {
			c.Frames = pl.Frames()
		}
		if ap := s.aplayers[m.ID]; ap != nil {
			c.Audio = ap.Buffers()
		}
		ps := stats[m.ID]
		c.Pair, c.RTTms, c.Tx, c.Rx, c.Loss = ps.Pair, int(ps.RTT*1000), ps.TxBytes, ps.RxBytes, ps.Loss
		c.TxKbps, c.RxKbps, c.FPSIn = s.rates.of(m.ID, c.Tx, c.Rx, c.Frames)
		c.Limit = ps.Limit
		if c.Limit == "" && s.resNote != "" {
			c.Limit = "cpu"
		}
		c.Hint = linkHint(c)
		c.WindowClosed = closedWins[m.ID] && s.layout.Mode != LayoutGrid
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
	fmt.Fprintf(&b, "sharing: %s at %s, %s mode, HIGH %d fps, encoder %s\n",
		orDefault(st.Source, "nothing"), st.Resolution, st.Mode, st.HighFPS, orDefault(st.Encoder, "-"))
	if st.ResolutionNote != "" {
		fmt.Fprintf(&b, "note: %s\n", st.ResolutionNote)
	}
	fmt.Fprintf(&b, "encrypted: %v, relays: %d, LOW %d kbit/s\n", st.Encrypted, len(st.Relay), st.LowKbps)
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
	Server     string            `json:"server,omitempty"` // "" = the built-in default (older files)
	Room       string            `json:"room"`
	Password   string            `json:"password"`
	Source     media.SourceKind  `json:"source,omitempty"`
	Resolution media.Resolution  `json:"resolution,omitempty"`
	Mode       media.ContentMode `json:"mode,omitempty"`
	Audio      audio.Selection   `json:"audio"`
	Output     string            `json:"output,omitempty"`
}

// Rejoin returns what to restore after an in-call update.
func (s *Session) Rejoin() Rejoin {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Rejoin{Server: s.cfg.Server, Room: s.sig.Welcome.Room, Password: s.pw, Source: s.srcKind, Resolution: s.res, Mode: s.mode, Audio: s.asel, Output: s.output}
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
	s.regrid(false) // closed: no tiles, so the window goes
	for _, ap := range aplayers {
		ap.Stop()
	}
	s.mesh.Close()
	close(s.done)
	s.changed()
}

// ParseLink accepts https://host/j/CODE#password and bshare:// or
// blite://join/CODE#password.
func ParseLink(s string) (code, pw string, err error) {
	u, err := url.Parse(strings.TrimSpace(s))
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
