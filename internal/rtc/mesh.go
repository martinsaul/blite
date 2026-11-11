// Package rtc runs Blite's WebRTC full mesh with Pion: one PeerConnection per
// remote peer. Video is encoded once per layer and packetized once per layer;
// each peer's forwarder picks HIGH or LOW for that peer alone (forward.go).
//
// Negotiation: the newcomer offers to every peer in its welcome; existing peers
// only answer. Tracks never change after setup (sources swap underneath the
// track), so there is no renegotiation and no glare in v0.
package rtc

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/rtcp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"

	"blite/internal/identity"
	"blite/internal/proto"
	"blite/internal/sigclient"
)

// signalMsg is the payload carried in proto.Signal.Data.
type signalMsg struct {
	SDP *webrtc.SessionDescription `json:"sdp,omitempty"`
	Sig []byte                     `json:"sig,omitempty"` // identity signature over the SDP's DTLS fingerprints
	ICE *webrtc.ICECandidateInit   `json:"ice,omitempty"`
	// MaxLayer is the best layer the receiver wants from us (0 high, 1 low,
	// 2 none): set from its viewer, e.g. a peer it only listens to.
	MaxLayer *int `json:"maxLayer,omitempty"`
	// Origin, with MaxLayer, sent to the relay: the cap is for this
	// member's relayed video, not the relay's own.
	Origin string `json:"origin,omitempty"`
	// Slots, from a relay: the slot key (origin peer ID, or with "/low"
	// its screen-low) in each slot ("" = free).
	Slots []string `json:"slots,omitempty"`
	// Bye: the sender closes our connection, which the plan no longer
	// wants (older clients ignore it and notice the close).
	Bye bool `json:"bye,omitempty"`
	// Renditions (resize.go); older clients ignore them.
	Resize      *resizeMsg      `json:"resize,omitempty"`
	ResizeReply *resizeReplyMsg `json:"resizeReply,omitempty"`
	Sending     *sendingMsg     `json:"sending,omitempty"`
	// End-to-end encryption (e2ee.go).
	KX  *kxMsg  `json:"kx,omitempty"`
	Key *keyMsg `json:"key,omitempty"`
}

// Options configure a Mesh.
type Options struct {
	// OnVideo receives each remote peer's H.264 access units (Annex-B), from
	// that peer's receive goroutine.
	OnVideo func(peerID string, au []byte)
	// OnPeer is told when a peer connects (state "connected"), drops and
	// reconnects ("reconnecting"), can't be reached for now ("unreachable",
	// we try again) or goes away ("gone").
	OnPeer func(peerID, name, state string)
	// OnAudio receives each remote peer's Opus frames.
	OnAudio func(peerID string, frame []byte)
	// OnKeyframe asks the local encoder of a layer for a keyframe (a peer
	// switched layer or reported picture loss).
	OnKeyframe func(layer int)
	// OnHighFPS caps our HIGH encoder's frame rate (0: full rate).
	OnHighFPS func(fps int)
	// OnLayer is told when a peer's forwarder starts sending a different layer.
	OnLayer func(peerID string, layer int)
	// OnLowBitrate asks the LOW encoder for a new bitrate (kbit/s).
	OnLowBitrate func(kbps int)
	// Identity signs our DTLS fingerprints; peers' signatures are checked
	// against the idKey the server announced for them. Required.
	Identity identity.Key
	// OnTopology is told when the relay changes (relay mode on, off, or
	// handed over).
	OnTopology func()
	// OnRelayRefused is told when the server refuses our volunteering: an
	// older client in the room can't do relay mode.
	OnRelayRefused func()
	// OnRenditions (sharer) asks for the source to run exactly these
	// rendition heights besides HIGH and LOW (a restart). Nil = we never
	// grant resize requests for our own video.
	OnRenditions func(heights []int) error
	// CanEncode reports whether one more encoder fits on this machine
	// (hardware encoder, or CPU headroom). Nil = never.
	CanEncode func() bool
	// NewTranscoder (relay) decodes an origin's top and encodes it at each
	// height, calling out per access unit. Nil = the relay refuses.
	NewTranscoder func(heights []int, out func(layer int, au []byte, d time.Duration)) (Transcoder, error)
	// UploadKbps (relay) caps what renditions may add to our upload
	// (0 = no cap).
	UploadKbps int
	// OnSending is told what a provider now sends us of origin: height
	// (0 = nothing), its top, and whether congestion is why it's lower.
	OnSending func(origin string, height, top int, congested bool)
	// OnRenamed is told a member's new display name.
	OnRenamed func(peerID, name string)
	// OnOwner is told the room's owner whenever it changes ("" = none).
	OnOwner func(peerID string)
	// OnKicked is told the owner (by name) removed us; ban: for good.
	OnKicked func(by string, ban bool)
	// ForceRelay sets ICETransportPolicy=relay (TURN tests).
	ForceRelay bool
	// Build is our client build: the slot count toward peers depends on
	// both builds (relay.go).
	Build int
	// Headless: we never play what we receive (a hosted relay), so as relay
	// we pull only what others want.
	Headless bool
	// CPUFree reports our idle CPU share in percent, for caps (nil = unknown).
	CPUFree func() int
	// Port is the UDP port to use for all peers (nil = a new one per
	// candidate, as Pion does by default).
	Port *Port
	Log  *slog.Logger
}

// Mesh is a running mesh for one room.
type Mesh struct {
	sig   *sigclient.Client
	api   *webrtc.API // apiMu
	apiMu sync.Mutex
	// newAPI builds the API around a UDP mux (to recycle the port).
	newAPI func(mux ice.UDPMux) *webrtc.API
	cfg    webrtc.Configuration
	pkz    [numLayers]*layerPacketizer
	audio  *audioTrack // one audio track bound to every peer (RED or Opus each)
	opt    Options
	cap    webrtc.RTPCodecCapability

	mu     sync.Mutex
	peers  map[string]*remote
	names  map[string]string
	idKeys map[string]string
	vers   map[string]string // client build labels
	done   chan struct{}

	members  map[string]bool // other members now in the room
	redials  map[string]int  // connection attempts given up per peer, until one connects
	builds   map[string]int  // their client builds
	joinedAt map[string]time.Time
	plan     proto.Plan      // the relay plan (relay.go)
	topoAt   time.Time       // when the plan last changed relays or homes
	caps     map[string]int  // our quality wish per member (SetMaxLayer)
	relaying bool            // we have relay state: as relay, or since
	served   map[string]bool // former children we still serve (relay.go)
	origins  map[string]*origin
	provider map[string]string // origin -> peer we take its media from (cache)
	demand   map[demandKey]int // what we asked upstream as relay (wire values)
	owner    string            // the room's owner

	pacer   *pacer // nil = forward unpaced (pacer.go)
	started time.Time
	lowKbps atomic.Int32
	// HIGH's frame rate steps: allowed (desktop), current cap.
	fpsSteps atomic.Bool
	highFPS  atomic.Int32
	rend     renditions
	// statsMu keeps GetStats and Close of a PeerConnection apart: Pion
	// races between the two.
	statsMu sync.RWMutex
	port    int       // our UDP port with opt.Port
	release sync.Once // of opt.Port
	crypt   *crypt    // nil = the room's media isn't encrypted end to end
}

// closePC closes a PeerConnection, never during Stats.
func (m *Mesh) closePC(pc *webrtc.PeerConnection) {
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	pc.Close()
}

type remote struct {
	id         string
	offerer    bool // we sent the first offer; we also drive ICE restarts
	restarts   int
	pc         *webrtc.PeerConnection
	fwd        *forwarder
	policy     layerPolicy
	pending    []webrtc.ICECandidateInit
	haveRemote bool
	polMu      sync.Mutex    // policy: readRTCP, watchReports and preferences
	level      atomic.Int32  // send level (levelHigh..levelOff); written under polMu
	loss       atomic.Uint64 // float64 bits of the last reported fraction lost
	fpsCap     atomic.Int32  // HIGH frame rate this peer can take (0: no cap); written under polMu
	offHigh    atomic.Bool   // its level maps to a layer other than HIGH (setLevel)
	lastRR     atomic.Int64  // unix nanos of the last receiver report for our video
	relayTracks
	rendState
}

// New starts a mesh on a seated signaling client and connects to the peers
// already in the room.
func New(sig *sigclient.Client, opt Options) (*Mesh, error) {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	me := &webrtc.MediaEngine{}
	if err := registerCodecs(me); err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	// Pion NACKs a missing packet every 100 ms until it arrives by default; on a
	// saturated link the retransmissions then multiply the overload (the soak
	// harness saw 9 Mbit/s offered to a 250 kbit/s uplink). Twice is enough.
	if err := webrtc.RegisterDefaultInterceptorsWithOptions(me, ir,
		webrtc.WithNackGeneratorOptions(nack.GeneratorMaxNacksPerPacket(2))); err != nil {
		return nil, err
	}
	newAPI := func(mux ice.UDPMux) *webrtc.API {
		se := webrtc.SettingEngine{}
		if mux != nil {
			se.SetICEUDPMux(mux)
		}
		return webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se))
	}
	var mux ice.UDPMux
	port := 0
	if opt.Port != nil {
		// Without the shared port we still connect, as before port mapping:
		// only the router mapping goes unused.
		if mx, p, err := opt.Port.acquire(); err != nil {
			opt.Log.Warn("no shared UDP port, using a random port per connection", "err", err)
			opt.Port = nil
		} else {
			mux, port = mx, p
		}
	}
	api := newAPI(mux)

	cfg := webrtc.Configuration{}
	for _, s := range sig.Welcome.ICEServers {
		cfg.ICEServers = append(cfg.ICEServers, webrtc.ICEServer{URLs: s.URLs, Username: s.Username, Credential: s.Credential})
	}
	if opt.ForceRelay {
		cfg.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}

	m := &Mesh{sig: sig, api: api, newAPI: newAPI, cfg: cfg, opt: opt, port: port,
		cap: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: clockRate,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"},
		peers: map[string]*remote{}, names: map[string]string{}, idKeys: map[string]string{}, vers: map[string]string{}, done: make(chan struct{}),
		members: map[string]bool{}, builds: map[string]int{}, joinedAt: map[string]time.Time{}, caps: map[string]int{}, origins: map[string]*origin{},
		provider: map[string]string{}, served: map[string]bool{}, demand: map[demandKey]int{}, owner: sig.Welcome.Owner}
	if w := sig.Welcome; w.Plan != nil {
		m.plan = *w.Plan
	} else {
		m.plan = planFromTopology(proto.Topology{Queue: w.RelayQueue, Paused: w.RelayPaused, Waiting: w.RelayWaiting})
	}
	if sig.Welcome.E2EE {
		m.crypt = newCrypt()
	}
	m.relaying = m.amRelayLocked()
	m.rend.init()
	m.started = time.Now()
	if opt.UploadKbps > 0 {
		m.pacer = newPacer(opt.UploadKbps, m.done)
	}
	if opt.Identity.Private == nil {
		m.releasePort()
		return nil, errors.New("mesh needs an identity key")
	}
	for i := range m.pkz {
		m.pkz[i] = newLayerPacketizer()
	}
	m.lowKbps.Store(int32(lowKbps[levelLow]))
	m.audio = newAudioTrack(trackAudio)
	for _, p := range sig.Welcome.Peers {
		m.names[p.PeerID] = p.Name
		m.idKeys[p.PeerID] = p.IDKey
		m.vers[p.PeerID] = p.Client
		m.builds[p.PeerID] = p.Build
		m.members[p.PeerID] = true
	}
	if m.crypt != nil {
		for _, p := range sig.Welcome.Peers {
			m.sendKX(p.PeerID)
		}
	}
	// As the newcomer we offer to everyone we connect to: all of them in a
	// mesh, only the relay in relay mode.
	for _, p := range sig.Welcome.Peers {
		if !m.direct(p.PeerID) {
			continue
		}
		if err := m.offer(p.PeerID); err != nil {
			opt.Log.Warn("offer failed", "peer", p.PeerID, "err", err)
		}
	}
	go m.loop()
	go m.pruneLoop()
	go m.capsLoop()
	return m, nil
}

// registerCodecs registers only what Blite sends: H.264 constrained baseline
// and Opus (plus RED around it), with Pion's default payload types
// and feedback. With Pion's whole
// default codec list, the 11 m-lines of relay mode made a 30 KB offer.
func registerCodecs(me *webrtc.MediaEngine) error {
	fb := []webrtc.RTCPFeedback{{Type: "goog-remb"}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"}, {Type: "nack", Parameter: "pli"}}
	for _, c := range []struct {
		p webrtc.RTPCodecParameters
		t webrtc.RTPCodecType
	}{
		{webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: clockRate,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f", RTCPFeedback: fb}, PayloadType: 125}, webrtc.RTPCodecTypeVideo},
		{webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeRTX, ClockRate: clockRate,
			SDPFmtpLine: "apt=125"}, PayloadType: 107}, webrtc.RTPCodecTypeVideo},
		{webrtc.RTPCodecParameters{RTPCodecCapability: redCap, PayloadType: redPT}, webrtc.RTPCodecTypeAudio},
		{webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			SDPFmtpLine: "minptime=10;useinbandfec=1"}, PayloadType: opusPT}, webrtc.RTPCodecTypeAudio},
	} {
		if err := me.RegisterCodec(c.p, c.t); err != nil {
			return err
		}
	}
	return nil
}

// WriteVideo packetizes one access unit of `layer` once and hands it to every
// peer's forwarder; each forwards it only if that peer is on (or switching to)
// this layer.
func (m *Mesh) WriteVideo(layer int, au []byte, d time.Duration) {
	if layer < 0 || layer >= numLayers {
		return
	}
	pkts := m.pkz[layer].packetize(m.encVideo(au), d)
	m.mu.Lock()
	fwds := make([]*forwarder, 0, 2*len(m.peers))
	for _, r := range m.peers {
		fwds = append(fwds, r.fwd)
		// screen-low (relay mode): its want follows the topology and screen.
		if want := m.lowWantLocked(r); want != lowWant(r) && r.lowFwd.setWant(want) && m.opt.OnKeyframe != nil {
			defer m.opt.OnKeyframe(layerLow)
		}
		fwds = append(fwds, r.lowFwd)
	}
	m.mu.Unlock()
	needKey := false
	for _, f := range fwds {
		if f.forward(layer, pkts) {
			needKey = true
		}
	}
	if needKey && m.opt.OnKeyframe != nil {
		m.opt.OnKeyframe(layer)
	}
}

// WriteAudio sends one Opus frame to every peer (audio has no layers).
func (m *Mesh) WriteAudio(frame []byte, d time.Duration) {
	m.audio.writeOwn(m.encAudio(frame), d)
}

// Layers returns the layer each peer is currently receiving from us.
func (m *Mesh) Layers() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for id, r := range m.peers {
		cur, _ := r.fwd.layers()
		out[id] = cur
	}
	return out
}

// Done is closed when the signaling connection ends.
func (m *Mesh) Done() <-chan struct{} { return m.done }

// Close leaves the room and closes every peer connection.
func (m *Mesh) Close() {
	m.sig.Leave()
	m.mu.Lock()
	for id, r := range m.peers {
		m.closePC(r.pc)
		delete(m.peers, id)
	}
	m.mu.Unlock()
	m.releasePort()
}

// releasePort gives our UDP port back, once.
func (m *Mesh) releasePort() {
	if m.opt.Port != nil {
		m.release.Do(m.opt.Port.release)
	}
}

func (m *Mesh) loop() {
	defer close(m.done)
	for ev := range m.sig.Events {
		switch {
		case ev.Err != nil:
			m.opt.Log.Info("signaling closed", "err", ev.Err)
			return
		case ev.Joined != nil:
			m.mu.Lock()
			m.names[ev.Joined.PeerID] = ev.Joined.Name
			m.idKeys[ev.Joined.PeerID] = ev.Joined.IDKey
			m.vers[ev.Joined.PeerID] = ev.Joined.Client
			m.builds[ev.Joined.PeerID] = ev.Joined.Build
			m.joinedAt[ev.Joined.PeerID] = time.Now()
			m.members[ev.Joined.PeerID] = true
			m.mu.Unlock()
			if m.crypt != nil {
				m.sendKX(ev.Joined.PeerID)
			}
			// The newcomer will offer; nothing to do until it does. As relay,
			// give it a slot.
			m.assignAllSlots()
		case ev.Left != "":
			m.mu.Lock()
			delete(m.members, ev.Left)
			delete(m.caps, ev.Left)
			delete(m.origins, ev.Left)
			delete(m.joinedAt, ev.Left)
			m.mu.Unlock()
			m.drop(ev.Left)
			if m.crypt != nil {
				m.cryptLeft(ev.Left)
			}
			m.assignAllSlots()
			m.pushDemand()
		case ev.Topology != nil:
			m.setPlan(planFromTopology(*ev.Topology))
		case ev.Plan != nil:
			m.setPlan(*ev.Plan)
		case ev.Renamed != nil:
			m.mu.Lock()
			m.names[ev.Renamed.PeerID] = ev.Renamed.Name
			m.mu.Unlock()
			if m.opt.OnRenamed != nil {
				m.opt.OnRenamed(ev.Renamed.PeerID, ev.Renamed.Name)
			}
		case ev.Owner != nil:
			m.mu.Lock()
			m.owner = *ev.Owner
			m.mu.Unlock()
			if m.opt.OnOwner != nil {
				m.opt.OnOwner(*ev.Owner)
			}
		case ev.Kicked != nil:
			m.opt.Log.Info("removed from the room", "by", ev.Kicked.By, "ban", ev.Kicked.Ban)
			if m.opt.OnKicked != nil {
				m.opt.OnKicked(ev.Kicked.By, ev.Kicked.Ban)
			}
		case ev.RelayRefused:
			if m.opt.OnRelayRefused != nil {
				m.opt.OnRelayRefused()
			}
		case ev.Resumed != nil:
			// Back on signaling: forget peers that left while we were away.
			present := map[string]bool{}
			for _, p := range ev.Resumed {
				present[p.PeerID] = true
				m.mu.Lock()
				m.names[p.PeerID], m.idKeys[p.PeerID], m.vers[p.PeerID] = p.Name, p.IDKey, p.Client
				m.mu.Unlock()
			}
			m.mu.Lock()
			m.members = present
			var gone []string
			for id := range m.peers {
				if !present[id] {
					gone = append(gone, id)
				}
			}
			m.mu.Unlock()
			for _, id := range gone {
				m.drop(id)
			}
			m.assignAllSlots()
		case ev.Signal != nil:
			if err := m.handleSignal(ev.Signal); err != nil {
				m.opt.Log.Warn("signal", "from", ev.Signal.From, "err", err)
			}
		}
	}
}

func (m *Mesh) newPeer(id string) (*remote, error) {
	m.apiMu.Lock()
	api := m.api
	m.apiMu.Unlock()
	pc, err := api.NewPeerConnection(m.cfg)
	if err != nil {
		return nil, err
	}
	track, err := webrtc.NewTrackLocalStaticRTP(m.cap, "screen", "blite")
	if err != nil {
		pc.Close()
		return nil, err
	}
	r := &remote{id: id, pc: pc, fwd: newForwarder(track, layerHigh)}
	r.mid = -1
	m.mu.Lock()
	r.nslots = m.slotCountLocked(id)
	m.mu.Unlock()
	sender, err := pc.AddTrack(track)
	if err != nil {
		pc.Close()
		return nil, err
	}
	go m.readRTCP(r, sender)
	go m.watchReports(r)
	if err := m.addVideoRelayTracks(r); err != nil {
		pc.Close()
		return nil, err
	}
	asender, err := pc.AddTrack(m.audio)
	if err != nil {
		pc.Close()
		return nil, err
	}
	if err := m.addAudioRelayTracks(r); err != nil {
		pc.Close()
		return nil, err
	}
	go func() { // drain RTCP so interceptors keep working
		for {
			if _, _, err := asender.ReadRTCP(); err != nil {
				return
			}
		}
	}()

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		ci := c.ToJSON()
		m.sig.Signal(id, signalMsg{ICE: &ci})
		if m.opt.Port != nil {
			if mc := m.opt.Port.mappedCandidate(c, m.port); mc != nil {
				m.opt.Log.Info("sending mapped candidate", "peer", id, "candidate", mc.Candidate)
				m.sig.Signal(id, signalMsg{ICE: mc})
			}
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		m.opt.Log.Info("peer state", "peer", id, "state", s.String())
		switch s {
		case webrtc.PeerConnectionStateConnected:
			m.mu.Lock()
			r.restarts = 0
			delete(m.redials, id)
			m.mu.Unlock()
			m.resendSending(r)
			if m.opt.OnPeer != nil {
				m.opt.OnPeer(id, m.name(id), "connected")
			}
			m.forgetProviders()
			m.mu.Lock()
			fwd := m.forwardingLocked() && m.peers[id] == r
			m.mu.Unlock()
			if fwd {
				m.updateTargets()
				m.sendSlots(r)
				m.forgetDemand(id)
				m.pushDemand()
			}
		case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed:
			m.forgetProviders()
			if m.opt.OnPeer != nil {
				m.opt.OnPeer(id, m.name(id), "reconnecting")
			}
			go m.watchdog(r)
		}
	})
	pc.OnTrack(func(t *webrtc.TrackRemote, rcv *webrtc.RTPReceiver) {
		go drainRTCP(rcv)
		go m.receive(r, t)
	})

	m.mu.Lock()
	if old := m.peers[id]; old != nil {
		m.closePC(old.pc)
	}
	m.peers[id] = r
	m.mu.Unlock()
	return r, nil
}

// watchdog gives a broken connection a chance: after 5 s still not connected,
// the offerer restarts ICE (new candidates over signaling), up to maxRestarts
// times; the answerer just waits for that. Then the peer is dropped.
const (
	restartAfter = 5 * time.Second
	maxRestarts  = 4
)

func (m *Mesh) watchdog(r *remote) {
	time.Sleep(restartAfter)
	if !m.current(r) {
		return
	}
	switch r.pc.ConnectionState() {
	case webrtc.PeerConnectionStateConnected, webrtc.PeerConnectionStateClosed:
		return
	}
	m.mu.Lock()
	r.restarts++
	n, offerer := r.restarts, r.offerer
	m.mu.Unlock()
	if n > maxRestarts {
		m.unreachable(r, offerer)
		return
	}
	if offerer {
		m.opt.Log.Info("ICE restart", "peer", r.id, "attempt", n)
		if err := m.restartICE(r); err != nil {
			m.opt.Log.Warn("ICE restart", "peer", r.id, "err", err)
		}
	}
	go m.watchdog(r)
}

// A peer we can't connect to stays a member (they are in the room): we
// close the connection, say "unreachable", and the side that offered
// offers again after a growing pause, as long as they stay (the other side
// waits for that offer).
const (
	redialFirst = 10 * time.Second
	redialMax   = time.Minute
)

func (m *Mesh) unreachable(r *remote, offerer bool) {
	m.mu.Lock()
	if m.peers[r.id] == r {
		delete(m.peers, r.id)
	}
	if m.redials == nil {
		m.redials = map[string]int{}
	}
	n := m.redials[r.id]
	m.redials[r.id] = n + 1
	name := m.names[r.id]
	m.mu.Unlock()
	m.logPairs(r)
	m.closePC(r.pc)
	m.recyclePort()
	wait := min(redialFirst<<n, redialMax)
	m.opt.Log.Info("can't reach peer, trying again later", "peer", r.id, "offerer", offerer, "in", wait)
	if m.opt.OnPeer != nil {
		m.opt.OnPeer(r.id, name, "unreachable")
	}
	if !offerer {
		return
	}
	go func() {
		select {
		case <-m.done:
			return
		case <-time.After(wait):
		}
		m.mu.Lock()
		_, member := m.members[r.id]
		_, has := m.peers[r.id] // they offered meanwhile
		m.mu.Unlock()
		if !member || has {
			return
		}
		if err := m.offer(r.id); err != nil {
			m.opt.Log.Warn("offer again", "peer", r.id, "err", err)
		}
	}()
}

// recyclePort reopens our UDP socket when no connection uses it.
func (m *Mesh) recyclePort() {
	m.mu.Lock()
	busy := len(m.peers) > 0
	m.mu.Unlock()
	if busy || m.opt.Port == nil {
		return
	}
	if mux := m.opt.Port.recycle(); mux != nil {
		m.apiMu.Lock()
		m.api = m.newAPI(mux)
		m.apiMu.Unlock()
		m.opt.Log.Info("reopened the udp port: nobody else connected")
	}
}

// logPairs logs the ICE candidate pairs of a connection that never
// connected: which paths were tried and whether any answered.
func (m *Mesh) logPairs(r *remote) {
	m.statsMu.RLock()
	st := r.pc.GetStats()
	m.statsMu.RUnlock()
	n := 0
	for _, v := range st {
		cp, ok := v.(webrtc.ICECandidatePairStats)
		if !ok || n >= 12 {
			continue
		}
		n++
		l, _ := st[cp.LocalCandidateID].(webrtc.ICECandidateStats)
		rc, _ := st[cp.RemoteCandidateID].(webrtc.ICECandidateStats)
		m.opt.Log.Info("ice pair", "peer", r.id, "state", cp.State,
			"local", fmt.Sprintf("%s %s:%d", l.CandidateType, l.IP, l.Port), "remote", fmt.Sprintf("%s %s:%d", rc.CandidateType, rc.IP, rc.Port),
			"reqSent", cp.RequestsSent, "reqRecv", cp.RequestsReceived, "respSent", cp.ResponsesSent, "respRecv", cp.ResponsesReceived)
	}
}

func (m *Mesh) current(r *remote) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peers[r.id] == r
}

func (m *Mesh) restartICE(r *remote) error {
	if r.pc.SignalingState() != webrtc.SignalingStateStable {
		return errors.New("negotiation in progress")
	}
	o, err := r.pc.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
	if err != nil {
		return err
	}
	if err := r.pc.SetLocalDescription(o); err != nil {
		return err
	}
	return m.sendSDP(r.id, o)
}

func (m *Mesh) name(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.names[id]
}

// readRTCP turns a peer's receiver reports into layer decisions and its PLI/FIR
// into keyframe requests.
func (m *Mesh) readRTCP(r *remote, s *webrtc.RTPSender) {
	for {
		pkts, _, err := s.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch p := p.(type) {
			case *rtcp.ReceiverReport:
				now := time.Now()
				r.lastRR.Store(now.UnixNano())
				for _, rep := range p.Reports {
					m.observe(r, rr{lost: float64(rep.FractionLost) / 256, rtt: rrRTT(rep, now)})
				}
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				if cur, _ := r.fwd.layers(); cur >= 0 && cur < numLayers && m.opt.OnKeyframe != nil {
					m.opt.OnKeyframe(cur)
				}
			}
		}
	}
}

// drainRTCP reads a receiver's incoming RTCP: Pion's interceptors only see
// packets someone reads, and the report interceptor needs the sender reports
// to fill in the LSR/DLSR fields our RTT measurement relies on.
func drainRTCP(rcv *webrtc.RTPReceiver) {
	for {
		if _, _, err := rcv.ReadRTCP(); err != nil {
			return
		}
	}
}

// watchReports treats a connected peer that stops sending receiver reports as
// congested: a jammed uplink loses its RTCP before anything else, and without
// this nothing would ever step its video down.
func (m *Mesh) watchReports(r *remote) {
	const silence = 3 * time.Second
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var since time.Time // when the peer last became connected
	for range t.C {
		if !m.current(r) {
			return
		}
		if r.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
			since = time.Time{}
			continue
		}
		now := time.Now()
		if since.IsZero() {
			since = now
		}
		last := time.Unix(0, r.lastRR.Load())
		if last.Before(since) {
			last = since
		}
		if r.sendingVideo() && now.Sub(last) > silence {
			m.observe(r, rr{lost: 1})
		}
	}
}

func (m *Mesh) observe(r *remote, rep rr) {
	r.loss.Store(math.Float64bits(rep.lost))
	if !r.sendingVideo() {
		return
	}
	r.polMu.Lock()
	defer r.polMu.Unlock()
	cur := int(r.level.Load())
	r.policy.fpsSteps = m.fpsStepsFor(r)
	target := r.policy.observe(cur, rep, time.Now())
	r.fpsCap.Store(int32(r.policy.fpsCap()))
	defer m.updateHighFPS()
	if target == cur {
		return
	}
	m.opt.Log.Info("level change", "peer", r.id, "from", cur, "to", target, "loss", rep.lost, "rtt", rep.rtt)
	m.setLevel(r, target)
}

// setLevel moves r to a send level (polMu held): its forwarder's layer, and
// the shared LOW bitrate.
func (m *Mesh) setLevel(r *remote, level int) {
	r.level.Store(int32(level))
	target := levelLayer(max(level, r.wireCap), r.mid)
	r.offHigh.Store(target != layerHigh)
	m.noteSending(r)
	if _, want := r.fwd.layers(); want != target {
		if r.fwd.setWant(target) && target < numLayers && m.opt.OnKeyframe != nil {
			m.opt.OnKeyframe(target)
		}
		if m.opt.OnLayer != nil {
			m.opt.OnLayer(r.id, target)
		}
	}
	m.updateLowBitrate()
}

// SetFPSSteps allows (desktop shares) or forbids (game) lowering HIGH's
// frame rate for a congested viewer before switching it to a lower layer.
func (m *Mesh) SetFPSSteps(on bool) {
	m.fpsSteps.Store(on)
	if !on {
		for _, r := range m.peerList() {
			r.fpsCap.Store(0) // the policy forgets its step on the next report
		}
		m.updateHighFPS()
	}
}

// fpsStepsFor is how many frame rate steps r's policy may take: all of
// them when r is the only peer on our HIGH (lowering it for one viewer
// would slow the others too), else none.
func (m *Mesh) fpsStepsFor(r *remote) int {
	if !m.fpsSteps.Load() {
		return 0
	}
	for _, v := range m.peerList() {
		if v != r && v.onHigh() {
			return 0
		}
	}
	return len(highFPS) - 1
}

// limit is why r gets less than our best (PeerStats.Limit).
func (r *remote) limit() string {
	r.polMu.Lock()
	wire := r.wireCap
	r.polMu.Unlock()
	switch level := int(r.level.Load()); {
	case wire > levelHigh && wire >= level:
		return "viewer"
	case level > levelHigh || r.fpsCap.Load() > 0:
		return "bandwidth"
	}
	return ""
}

// onHigh reports whether r receives our HIGH.
func (r *remote) onHigh() bool { return !r.offHigh.Load() }

// updateHighFPS runs HIGH at the lowest frame rate a viewer on it can take.
func (m *Mesh) updateHighFPS() {
	fps := 0
	for _, r := range m.peerList() {
		if c := int(r.fpsCap.Load()); c > 0 && r.onHigh() && (fps == 0 || c < fps) {
			fps = c
		}
	}
	if int(m.highFPS.Swap(int32(fps))) != fps {
		m.opt.Log.Info("high frame rate", "fps", fps)
		if m.opt.OnHighFPS != nil {
			m.opt.OnHighFPS(fps)
		}
	}
}

// HighFPSCap is the frame rate HIGH runs at for congested viewers (0: full).
func (m *Mesh) HighFPSCap() int { return int(m.highFPS.Load()) }

// updateLowBitrate runs the LOW encoder at the bitrate of the worst-off peer
// that receives LOW.
func (m *Mesh) updateLowBitrate() {
	m.mu.Lock()
	worst := levelLow
	for _, r := range m.peers {
		if l := int(r.level.Load()); l > worst && l < levelOff {
			worst = l
		}
	}
	m.mu.Unlock()
	kbps := lowKbps[worst]
	if int(m.lowKbps.Swap(int32(kbps))) != kbps {
		m.opt.Log.Info("low bitrate", "kbps", kbps)
		if m.opt.OnLowBitrate != nil {
			m.opt.OnLowBitrate(kbps)
		}
	}
}

// LowKbps is the bitrate the LOW encoder should run at.
func (m *Mesh) LowKbps() int { return int(m.lowKbps.Load()) }

// rrRTT is the round trip a receiver report implies (RFC 3550 6.4.1): arrival
// minus the sender report it refers to minus the receiver's hold time, all in
// 1/65536 s units of the NTP timestamp's middle 32 bits.
func rrRTT(rep rtcp.ReceptionReport, now time.Time) time.Duration {
	if rep.LastSenderReport == 0 {
		return 0
	}
	d := ntpMiddle(now) - rep.LastSenderReport - rep.Delay
	if int32(d) <= 0 {
		return 0
	}
	return time.Duration(uint64(d) * uint64(time.Second) >> 16)
}

func ntpMiddle(t time.Time) uint32 {
	const ntpEpochOffset = 2208988800 // 1900 -> 1970
	secs := uint64(t.Unix() + ntpEpochOffset)
	frac := uint64(t.Nanosecond()) << 32 / uint64(time.Second)
	return uint32(secs<<16 | frac>>16)
}

// receive reads one remote track until it ends. What it carries depends on
// its ID (relay.go): the sender's own media, or screen-low, or a relay slot
// carrying another origin's. Either way, if the sender is that origin's
// provider, we play it (not screen-low) and, as relay, forward it.
func (m *Mesh) receive(r *remote, t *webrtc.TrackRemote) {
	id := t.ID()
	video := t.Kind() == webrtc.RTPCodecTypeVideo
	slot, isSlot := slotIndex(id, slotPrefix)
	if !video {
		slot, isSlot = slotIndex(id, aslotPrefix)
	}
	own := srcScreen // screen, or an older client's video track
	if video && id == trackScreenLow {
		own = srcLow
	}
	if video && !isSlot {
		m.originOf(r.id).setSrc(own, r, t)
	}
	var sb *samplebuilder.SampleBuilder
	var kg *keyGate
	newDecode := func() {
		sb = samplebuilder.New(512, &codecs.H264Packet{}, t.Codec().ClockRate)
		kg = &keyGate{pli: func() {
			r.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(t.SSRC())}})
		}}
	}
	var last *origin // the origin this track carried last
	var rx audioRx   // audio: in order, gaps filled from RED
	var rxOf *origin // the origin rx followed
	for {
		pkt, _, err := t.ReadRTP()
		if err != nil {
			return
		}
		oid, src := r.id, own
		if isSlot {
			if oid, src = m.relayedOrigin(r.id, slot); oid == "" {
				continue
			}
		} else if !m.ownProvided(r) {
			continue
		}
		o := last
		if o == nil || o.id != oid {
			o = m.originOf(oid)
			if video {
				if isSlot {
					o.setSrc(src, r, t)
				}
				newDecode()
			}
			last = o
		}
		o.saw(src, time.Now())
		if !video {
			if o != rxOf {
				rx.reset()
				rxOf = o
			}
			for _, f := range rx.push(pkt) {
				m.fanAudio(o, r.id, f)
				if m.opt.OnAudio != nil && len(f.data) > 0 && !m.opt.Headless {
					if d, ok := m.decAudio(f.data); ok {
						m.opt.OnAudio(oid, d)
					}
				}
			}
			continue
		}
		m.fanVideo(o, r.id, src, pkt)
		if src == srcLow {
			continue
		}
		play := !m.localCapOff(oid)
		tr := m.transcoderOf(oid)
		if !play && tr == nil {
			continue
		}
		sb.Push(pkt)
		for s := sb.Pop(); s != nil; s = sb.Pop() {
			au, ok := m.decVideo(s.Data)
			if !ok {
				kg.lost() // what follows references frames we couldn't decode
				continue
			}
			if !kg.pass(au, s.PrevDroppedPackets) {
				continue
			}
			if tr != nil {
				tr.Push(au)
			}
			if play && m.opt.OnVideo != nil {
				m.opt.OnVideo(oid, au)
			}
		}
	}
}

// sendSDP signs our fingerprints for this peer and sends the description.
func (m *Mesh) sendSDP(to string, d webrtc.SessionDescription) error {
	w := m.sig.Welcome
	sig := m.opt.Identity.SignSDP(w.Room, w.PeerID, to, d.SDP)
	return m.sig.Signal(to, signalMsg{SDP: &d, Sig: sig})
}

// checkSDP verifies a received description against the sender's identity key.
func (m *Mesh) checkSDP(from string, msg signalMsg) error {
	m.mu.Lock()
	key := m.idKeys[from]
	m.mu.Unlock()
	return identity.VerifySDP(key, m.sig.Welcome.Room, from, m.sig.Welcome.PeerID, msg.SDP.SDP, msg.Sig)
}

func (m *Mesh) reject(from string, err error) {
	m.opt.Log.Warn("rejected peer: identity check failed", "peer", from, "err", err)
	m.mu.Lock()
	r := m.peers[from]
	delete(m.peers, from)
	name := m.names[from]
	m.mu.Unlock()
	if r != nil {
		m.closePC(r.pc)
	}
	if m.opt.OnPeer != nil {
		m.opt.OnPeer(from, name, "untrusted")
	}
}

func (m *Mesh) offer(id string) error {
	r, err := m.newPeer(id)
	if err != nil {
		return err
	}
	r.offerer = true
	o, err := r.pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := r.pc.SetLocalDescription(o); err != nil {
		return err
	}
	return m.sendSDP(id, o)
}

func (m *Mesh) handleSignal(s *proto.Signal) error {
	var msg signalMsg
	if err := json.Unmarshal(s.Data, &msg); err != nil {
		return err
	}
	from := s.From
	m.mu.Lock()
	r := m.peers[from]
	m.mu.Unlock()

	switch {
	case msg.SDP != nil && m.checkSDP(from, msg) != nil:
		m.reject(from, m.checkSDP(from, msg))
		return nil

	case msg.SDP != nil && msg.SDP.Type == webrtc.SDPTypeOffer:
		if r != nil && r.pc.SignalingState() == webrtc.SignalingStateHaveLocalOffer && r.offerer {
			// Glare after a topology change: the lower peer ID's offer wins.
			if m.me() < from {
				return nil
			}
			m.mu.Lock()
			if m.peers[from] == r {
				delete(m.peers, from)
			}
			m.mu.Unlock()
			m.closePC(r.pc)
			r = nil
		}
		if r != nil && r.pc.SignalingState() != webrtc.SignalingStateStable {
			return errors.New("offer while negotiating")
		}
		var err error
		if r == nil {
			if r, err = m.newPeer(from); err != nil {
				return err
			}
		}
		if err := r.pc.SetRemoteDescription(*msg.SDP); err != nil {
			return err
		}
		m.flushICE(r)
		a, err := r.pc.CreateAnswer(nil)
		if err != nil {
			return err
		}
		if err := r.pc.SetLocalDescription(a); err != nil {
			return err
		}
		return m.sendSDP(from, a)

	case msg.SDP != nil && msg.SDP.Type == webrtc.SDPTypeAnswer:
		if r == nil {
			return fmt.Errorf("answer from unknown peer")
		}
		if err := r.pc.SetRemoteDescription(*msg.SDP); err != nil {
			return err
		}
		m.flushICE(r)
		return nil

	case msg.Slots != nil:
		m.gotSlots(from, msg.Slots)
		return nil

	case msg.KX != nil:
		if m.crypt != nil {
			m.gotKX(from, *msg.KX)
		}
		return nil

	case msg.Key != nil:
		if m.crypt != nil {
			m.gotKey(from, *msg.Key)
		}
		return nil

	case msg.Bye:
		m.gotBye(from)
		return nil

	case msg.MaxLayer != nil && msg.Origin != "":
		// A viewer's cap on a member we relay to it.
		capLevel, ok := wireLevel(*msg.MaxLayer)
		if r == nil || !ok {
			return nil
		}
		r.polMu.Lock()
		if r.originCap == nil {
			r.originCap = map[string]int{}
		}
		r.originCap[msg.Origin] = capLevel
		r.polMu.Unlock()
		m.pushDemand()
		return nil

	case msg.MaxLayer != nil:
		capLevel, ok := wireLevel(*msg.MaxLayer)
		if r == nil || !ok {
			return nil
		}
		r.polMu.Lock()
		defer r.polMu.Unlock()
		r.wireCap = capLevel
		m.applyCap(r)
		return nil

	case msg.Resize != nil:
		if r != nil {
			go m.handleResize(r, *msg.Resize)
		}
		return nil

	case msg.ResizeReply != nil:
		m.gotResizeReply(from, *msg.ResizeReply)
		return nil

	case msg.Sending != nil:
		m.gotSending(from, *msg.Sending)
		return nil

	case msg.ICE != nil:
		if r == nil {
			return nil // offer not seen yet; candidates before it are dropped (rare with trickle order)
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if !r.haveRemote {
			r.pending = append(r.pending, *msg.ICE)
			return nil
		}
		return r.pc.AddICECandidate(*msg.ICE)
	}
	return nil
}

func (m *Mesh) flushICE(r *remote) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r.haveRemote = true
	for _, c := range r.pending {
		r.pc.AddICECandidate(c)
	}
	r.pending = nil
}

func (m *Mesh) drop(id string) {
	m.mu.Lock()
	r := m.peers[id]
	delete(m.peers, id)
	name := m.names[id]
	m.mu.Unlock()
	m.stopTranscoder(id)
	m.forgetView(id)
	m.forgetProviders()
	if r != nil {
		m.closePC(r.pc)
		m.updateLowBitrate() // the worst-off peer may have left
		if m.opt.OnPeer != nil {
			m.opt.OnPeer(id, name, "gone")
		}
	}
}

// Rename sets our display name in the room.
func (m *Mesh) Rename(name string) error { return m.sig.Rename(name) }

// Owner is the room owner's peer ID ("" = none online).
func (m *Mesh) Owner() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.owner
}

// Transfer (owner) makes peerID the owner; the server enforces who may.
func (m *Mesh) Transfer(peerID string) error { return m.sig.Transfer(peerID) }

// Kick (owner) removes peerID; ban also bars it from coming back.
func (m *Mesh) Kick(peerID string, ban bool) error { return m.sig.Kick(peerID, ban) }

// IDKey returns the identity key the server announced for a peer.
func (m *Mesh) IDKey(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.idKeys[id]
}

// Versions returns each known peer's client build.
func (m *Mesh) Versions() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.vers))
	for id, v := range m.vers {
		out[id] = v
	}
	return out
}

// SafetyCode is the code everyone in the room should see identically.
func (m *Mesh) SafetyCode() string {
	m.mu.Lock()
	// Everyone in the room, not just our connections: in relay mode we
	// connect only to the relay.
	keys := []string{m.opt.Identity.Public()}
	for id := range m.members {
		keys = append(keys, m.idKeys[id])
	}
	m.mu.Unlock()
	return identity.SafetyCode(keys)
}

// PeerStats is a per-peer transport snapshot.
type PeerStats struct {
	Pair    string  // selected ICE pair as "local/remote" candidate types (host, srflx, prflx, relay)
	RTT     float64 // seconds, from the nominated candidate pair
	TxBytes uint64  // bytes sent to this peer (all candidate pairs)
	RxBytes uint64  // bytes received from this peer
	Loss    float64 // last fraction lost they reported for our video (0..1)
	// Limit is why we send them less than our best: "viewer" (their
	// choice), "bandwidth" (congestion: a lower layer or frame rate); ""
	// when not limited.
	Limit string
}

// AudioLossPct is the worst loss (%) a direct peer reported lately, for
// Opus in-band FEC.
func (m *Mesh) AudioLossPct() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	worst := 0.0
	for _, r := range m.peers {
		worst = max(worst, math.Float64frombits(r.loss.Load()))
	}
	return int(math.Ceil(min(worst, 0.3) * 100))
}

// Stats returns a transport snapshot per peer.
func (m *Mesh) Stats() map[string]PeerStats {
	m.mu.Lock()
	peers := make([]*remote, 0, len(m.peers))
	for _, r := range m.peers {
		peers = append(peers, r)
	}
	m.mu.Unlock()
	out := map[string]PeerStats{}
	for _, r := range peers {
		ps := PeerStats{Loss: math.Float64frombits(r.loss.Load()), Limit: r.limit()}
		switch r.pc.ConnectionState() {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			// Pion may be closing it itself (DTLS failure), which races
			// with GetStats.
			out[r.id] = ps
			continue
		}
		m.statsMu.RLock()
		st := r.pc.GetStats()
		m.statsMu.RUnlock()
		for _, v := range st {
			if ts, ok := v.(webrtc.TransportStats); ok {
				ps.TxBytes, ps.RxBytes = ts.BytesSent, ts.BytesReceived
			}
			cp, ok := v.(webrtc.ICECandidatePairStats)
			if !ok || !cp.Nominated {
				continue
			}
			l, _ := st[cp.LocalCandidateID].(webrtc.ICECandidateStats)
			rc, _ := st[cp.RemoteCandidateID].(webrtc.ICECandidateStats)
			ps.Pair = l.CandidateType.String() + "/" + rc.CandidateType.String()
			ps.RTT = cp.CurrentRoundTripTime
		}
		out[r.id] = ps
	}
	m.mu.Lock()
	for id := range m.members {
		if _, ok := out[id]; !ok && !m.directLocked(id) {
			if via := m.providerLocked(id); via != "" {
				out[id] = PeerStats{Pair: "via " + m.names[via]}
			}
		}
	}
	m.mu.Unlock()
	return out
}
