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
	Log        *slog.Logger
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
	// Our viewer settings for them.
	Quality string  `json:"quality"` // auto | low | off (what we ask them to send)
	Volume  float64 `json:"volume"`  // playback gain, 0..2
	Muted   bool    `json:"muted,omitempty"`
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
	// Relay is the relay queue, active relay first; empty = full mesh.
	Relay []RelayEntry `json:"relay,omitempty"`
	// Resolution is what we share at: 720p, 1080p or 2160p.
	Resolution string `json:"resolution"`
	// SourceNote: sharing stopped on its own (the shared window closed, or
	// the capture failed).
	SourceNote string `json:"sourceNote,omitempty"`
	// ResolutionNote: we stepped the resolution down because our encoder
	// couldn't keep up.
	ResolutionNote string `json:"resolutionNote,omitempty"`
	// RelayRefused: the server refused our volunteering because a member's
	// build can't do relay mode.
	RelayRefused bool `json:"relayRefused,omitempty"`
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
	srcMu    sync.Mutex
	layout   Layout
	asrc     *media.AudioSource
	asrcDesc string
	asel     audio.Selection
	output   string
	started  time.Time
	closed   bool
	done     chan struct{}
	onChange func()

	gridMu  sync.Mutex // serializes regrid; held before mu
	grid    *media.Grid
	gridKey string // what grid shows, so regrid skips no-op rebuilds
}

// Layout is how we show the streams we receive.
type Layout struct {
	Mode string `json:"mode"` // windows (one per stream) | grid (all in one window)
	Rows int    `json:"rows"` // grid rows, 0 = auto
	Cols int    `json:"cols"` // grid columns, 0 = auto
}

const (
	LayoutWindows = "windows"
	LayoutGrid    = "grid"
	maxGridSide   = 8
)

// Valid reports whether l is a layout SetLayout accepts.
func (l Layout) Valid() bool {
	return (l.Mode == LayoutWindows || l.Mode == LayoutGrid) &&
		l.Rows >= 0 && l.Cols >= 0 && l.Rows <= maxGridSide && l.Cols <= maxGridSide
}

// Create makes a new room. An empty password gets a random one.
func Create(ctx context.Context, cfg Config, password string) (*Session, error) {
	if password == "" {
		password = RandomPassword()
	}
	s := newSession(cfg, password)
	sig, err := sigclient.Create(ctx, cfg.Server, proto.Create{Password: password, Name: cfg.Name, IDKey: s.idKey(), Client: cfg.Client, Room: cfg.HostRoom, HostKey: cfg.HostKey, Owners: cfg.HostOwners}, sigclient.Options{OnState: s.sigState})
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
		aplayers: map[string]*media.AudioPlayer{}, asel: audio.Selection{Mode: "none"},
		done: make(chan struct{}), started: time.Now()}
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
		Log:        s.cfg.Log,
	})
	if err != nil {
		sig.Leave()
		return nil, err
	}
	s.mesh = mesh
	go func() {
		<-mesh.Done()
		s.Close()
	}()
	go s.watchDecoders()
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
// can't see them at all.
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
	s.mu.Unlock()
	s.changed()
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
	f, grid := s.onChange, s.layout.Mode == LayoutGrid
	s.mu.Unlock()
	if grid { // someone left or was renamed: their tile goes or relabels
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
	case "untrusted":
		m := s.members[id]
		if m == nil {
			m = &Member{ID: id, Name: name, Quality: "auto", Volume: 1}
			s.members[id] = m
		}
		m.State = "untrusted"
	case "gone":
		delete(s.members, id)
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
	if pl == nil {
		var err error
		if pl, err = media.StartPlayer(s.viewLocked()); err != nil {
			s.mu.Unlock()
			s.cfg.Log.Error("player", "err", err)
			return
		}
		s.players[peer] = pl
		added = pl.Channel != ""
	}
	s.mu.Unlock()
	if added {
		go s.regrid(false)
	}
	pl.Push(au)
}

// viewLocked is how a new player shows its stream (mu held).
func (s *Session) viewLocked() media.ViewKind {
	if s.cfg.View == media.ViewWindow && s.layout.Mode == LayoutGrid {
		return media.ViewGrid
	}
	return s.cfg.View
}

// SetLayout chooses separate windows or one grid window for the streams we
// receive. Switching between the two restarts the decoders; each stream
// shows again from its next keyframe (at most 2 s).
func (s *Session) SetLayout(l Layout) error {
	if !l.Valid() {
		return errors.New("unknown layout")
	}
	s.mu.Lock()
	var restart []*media.Player
	if s.layout.Mode != l.Mode && !s.closed {
		for id, pl := range s.players {
			restart = append(restart, pl)
			delete(s.players, id)
		}
	}
	s.layout = l
	s.mu.Unlock()
	for _, pl := range restart {
		pl.Stop()
	}
	s.regrid(true)
	s.changed()
	return nil
}

// regrid makes the grid window show the current grid players, in name
// order. It rebuilds only when something it shows changed, unless force
// (which also reopens a window the user closed).
func (s *Session) regrid(force bool) {
	s.gridMu.Lock()
	defer s.gridMu.Unlock()
	s.mu.Lock()
	l := s.layout
	type entry struct{ id, label, channel string }
	var es []entry
	if !s.closed && l.Mode == LayoutGrid {
		for id, pl := range s.players {
			if pl.Channel == "" {
				continue
			}
			label := id
			if m := s.members[id]; m != nil {
				label = m.Name
			}
			if tag := identity.Tag(s.mesh.IDKey(id)); tag != "" {
				label += "#" + tag
			}
			es = append(es, entry{id, label, pl.Channel})
		}
	}
	s.mu.Unlock()
	sort.Slice(es, func(i, j int) bool {
		if a, b := strings.ToLower(es[i].label), strings.ToLower(es[j].label); a != b {
			return a < b
		}
		return es[i].id < es[j].id
	})
	tiles := make([]media.Tile, len(es))
	for i, e := range es {
		tiles[i] = media.Tile{Channel: e.channel, Label: e.label}
	}
	key := fmt.Sprint(l.Rows, l.Cols, tiles)
	if !force && key == s.gridKey {
		return
	}
	if s.grid != nil {
		s.grid.Stop()
		s.grid = nil
	}
	s.gridKey = key
	if len(tiles) == 0 {
		return
	}
	g, err := media.StartGrid(tiles, l.Rows, l.Cols, media.ViewWindow)
	if err != nil {
		s.cfg.Log.Error("grid window", "err", err)
		return
	}
	s.grid = g
	r, c := media.GridSize(len(tiles), l.Rows, l.Cols)
	s.cfg.Log.Info("grid window", "streams", len(tiles), "rows", r, "cols", c)
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
	var desc string // capture to run; "" = none
	switch sel.Mode {
	case "none":
	case "test":
		desc = media.TestAudio
	case "system", "apps":
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
	if sel.Mode != "system" && sel.Mode != "apps" {
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
	send := m.Quality != quality
	m.Quality, m.Volume, m.Muted = quality, volume, muted
	ap, gain := s.aplayers[id], m.gain()
	s.mu.Unlock()
	if ap != nil {
		ap.SetVolume(gain)
	}
	if send {
		if err := s.mesh.SetMaxLayer(id, layer); err != nil {
			return err
		}
	}
	s.changed()
	return nil
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
		s.mesh.SetTop(0)
	}
	if err := s.startSource(kind); err != nil {
		s.mesh.SetTop(0)
		return err
	}
	s.mu.Lock()
	top := media.TopHeight(orRes(s.res))
	s.mu.Unlock()
	s.mesh.SetTop(top)
	return nil
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
	res, rends := s.res, slices.Clone(s.rends)
	s.mu.Unlock()
	if old != nil {
		old.Stop()
	}
	var highAUs atomic.Int64
	src, enc, err := media.StartVideoSource(kind, res, rends, s.cfg.Encoder, func(layer int, au media.AccessUnit) {
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
	s.mu.Unlock()
	s.cfg.Log.Info("sharing", "source", kind, "resolution", orRes(res), "encoder", enc)
	src.SetBitrate(media.Low, s.mesh.LowKbps())
	if orRes(res) != media.Res720 {
		go s.checkEncodeRate(src, kind, orRes(res), &highAUs)
	}
	go s.watchSource(src, kind)
	s.changed()
	return nil
}

// watchSource stops sharing, with a note for the user, when src fails or the
// window it captures closes (X11 capture keeps running on a dead window).
func (s *Session) watchSource(src *media.VideoSource, kind media.SourceKind) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	var note string
	for note == "" {
		select {
		case err := <-src.Err:
			if err == nil {
				return
			}
			s.cfg.Log.Error("video source stopped", "err", err)
			note = "Sharing stopped: the capture failed (" + err.Error() + ")."
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

// The encoder must keep up with HIGH: measured over encodeWindow after
// encodeWarmup, under minEncodeFPS steps HIGH down one size (x264 on
// a 2012 Xeon does 4K at 9 fps).
const (
	encodeWarmup = 2 * time.Second
	encodeWindow = 10 * time.Second
	minEncodeFPS = 24
)

// checkEncodeRate steps our resolution down if the encoder of src can't
// keep up with res, and tells the sharer why.
func (s *Session) checkEncodeRate(src *media.VideoSource, kind media.SourceKind, res media.Resolution, n *atomic.Int64) {
	time.Sleep(encodeWarmup)
	a := n.Load()
	time.Sleep(encodeWindow)
	fps := float64(n.Load()-a) / encodeWindow.Seconds()
	s.mu.Lock()
	current := s.src == src && orRes(s.res) == res
	lower := media.LowerResolution(res)
	if !current || fps >= minEncodeFPS || lower == "" {
		s.mu.Unlock()
		return
	}
	s.res, s.rends = lower, nil
	s.resNote = fmt.Sprintf("Your computer can't encode %s fast enough (%.0f fps); sharing at %s.", media.ResolutionLabel(res), fps, media.ResolutionLabel(lower))
	s.mu.Unlock()
	s.cfg.Log.Warn("encoder too slow, stepping down", "from", res, "to", lower, "fps", fps, "encoder", s.encoderName())
	s.mesh.SetTop(0)
	if err := s.SetSource(kind); err != nil {
		s.cfg.Log.Error("restart at lower resolution", "err", err)
	}
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
		s.mesh.SetTop(0)
		return s.SetSource(kind)
	}
	s.changed()
	return nil
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
	stats := s.mesh.Stats()
	owner := s.mesh.Owner()
	vers := s.mesh.Versions()
	layers := s.mesh.Layers()
	var relay []RelayEntry
	for _, id := range s.mesh.Topology() {
		e := RelayEntry{ID: id, Name: s.mesh.Name(id), Me: id == s.mesh.Me()}
		if e.Me {
			e.Name = s.cfg.Name
		}
		relay = append(relay, e)
	}
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
		RelayRefused:      s.relayRefused,
		Reconnecting:      !s.reconnAt.IsZero(),
		IsOwner:           owner == s.mesh.Me(),
		IDKey:             s.idKey(),
		Name:              s.cfg.Name,
		Tag:               identity.Tag(s.idKey()),
		Resolution:        string(orRes(s.res)),
		ResolutionNote:    s.resNote,
		SourceNote:        s.srcNote,
		Layout:            s.layout,
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

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.done }

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

// ParseLink accepts https://host/j/CODE#password and blite://join/CODE#password.
func ParseLink(s string) (code, pw string, err error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return "", "", err
	}
	var path string
	switch u.Scheme {
	case "blite":
		path = strings.TrimPrefix(u.Host+u.Path, "join")
	case "https", "http":
		path = strings.TrimPrefix(u.Path, "/j")
	default:
		return "", "", fmt.Errorf("unknown link scheme %q", u.Scheme)
	}
	code = strings.Trim(path, "/")
	pw = u.Fragment // already percent-decoded by url.Parse
	if code == "" || pw == "" {
		return "", "", errors.New("link needs a code and a #password")
	}
	return code, pw, nil
}

// ShareLink builds the https share link for a room on the given signaling server.
func ShareLink(server, room, pw string) string {
	u, err := url.Parse(server)
	if err != nil {
		return ""
	}
	return "https://" + u.Host + "/j/" + room + "#" + url.PathEscape(pw)
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
