package rtc

// Relay mode. The server plans which volunteers relay (proto.Plan): every
// member is homed on one relay and connects only to it; relays connect to
// the members homed on them ("children") and to every other relay
// ("trunks"). A relay forwards each child's media to its other children and
// over every trunk, and what arrives over a trunk to its children, so any
// path is at most sharer -> relay -> relay -> viewer. With one relay this is
// the classic relay mode; with none, the full mesh.
//
// Every PeerConnection carries the same tracks whatever the topology, so a
// plan change never renegotiates:
//
//	screen      our video, adaptive per peer (forwarder + layerPolicy)
//	screen-low  our LOW layer; only sent to a relay that forwards our
//	            video, and only while screen carries HIGH, so the relay
//	            can serve weak viewers
//	slotN       relay -> peer: a forwarded origin's video
//	audio       our Opus
//	aslotN      relay -> peer: that origin's audio
//
// The number of slots is fixed per connection: maxSlots between builds from
// proto.MultiRelayBuild on, legacySlots with an older build (which assumes
// one slot per member and at most proto.RelayMaxPeers members). Slots go to
// origins, not members: the relay keeps a slot map per connection and tells
// the other side (signalMsg.Slots). While there are free slots every origin
// has one; beyond that an origin that starts sending takes the slot of one
// that has been quiet. Over a trunk, an origin can take two slots: "id"
// (its screen) and "id/low" (its screen-low), so the far relay can serve
// its own weak viewers.
//
// For every origin we take its media from one connection only (provider):
// its own if we connect to it directly, else its relay. While a plan
// change is under way that can be the old connection, until the new one
// carries it (make-before-break).

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"blite/internal/proto"
)

const (
	// legacySlots is what builds before proto.MultiRelayBuild carry: one
	// slot per possible origin, everyone but the relay.
	legacySlots = proto.RelayMaxPeers - 1
	// maxSlots is the slot count between newer builds: origins forwarded
	// at once, whatever the room size.
	maxSlots = 8
)

const (
	trackScreen    = "screen"
	trackScreenLow = "screen-low"
	trackAudio     = "audio"
	slotPrefix     = "slot"
	aslotPrefix    = "aslot"
	lowSuffix      = "/low" // slot key suffix: the origin's screen-low (trunks)
)

// Relay sources of one origin, used as forwarder "layers" at the relay.
const (
	srcScreen = 0 // the origin's screen track (HIGH, or LOW when its uplink is poor)
	srcLow    = 1 // the origin's screen-low track (LOW while screen carries HIGH)
)

const (
	pruneAfter  = 10 * time.Second // longest we keep PCs the plan no longer wants
	pliInterval = 500 * time.Millisecond
	lowSilence  = time.Second     // screen-low quiet this long: the origin's screen carries LOW
	activeFor   = 3 * time.Second // an origin with packets this recent is active
	idleFor     = 5 * time.Second // an origin quiet this long may lose its slot
	joinGrace   = 5 * time.Second // a newcomer offers itself this long
)

// slotIndex parses "slotN" / "aslotN".
func slotIndex(id, prefix string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(id, prefix))
	if !strings.HasPrefix(id, prefix) || err != nil || n < 0 || n >= maxSlots {
		return 0, false
	}
	return n, true
}

// slotKey is the slot map entry for an origin's source.
func slotKey(origin string, src int) string {
	if src == srcLow {
		return origin + lowSuffix
	}
	return origin
}

// parseSlotKey is slotKey's inverse ("" for a free slot).
func parseSlotKey(k string) (origin string, src int) {
	if o, ok := strings.CutSuffix(k, lowSuffix); ok {
		return o, srcLow
	}
	return k, srcScreen
}

// relayFwd feeds one viewer's slot from one origin's two sources, one packet
// at a time. Unlike forwarder it keeps the source's sequence gaps and order
// (seq out = seq in + offset), so NACK retransmissions and reordering survive
// the hop.
type relayFwd struct {
	track *webrtc.TrackLocalStaticRTP
	write func(src int, p *rtp.Packet) // nil = track.WriteRTP

	mu         sync.Mutex
	cur, want  int    // source being sent (-1 none yet) and wanted (layerOff = nothing)
	curSSRC    uint32 // the incoming stream cur comes from: a new one is a switch too
	started    bool
	seqOff     uint16
	tsOff      uint32
	switchSeq  uint16 // source seq of the keyframe we switched at
	pastSwitch bool   // far enough past switchSeq that only age matters
	highIn     uint16 // highest source seq seen in this run
	nextSeq    uint16 // one past the highest seq we sent
	lastTS     uint32
}

func newRelayFwd(track *webrtc.TrackLocalStaticRTP) *relayFwd {
	return &relayFwd{track: track, cur: -1, want: layerOff}
}

// reset starts over for a new origin in this slot; seq and timestamps continue.
func (f *relayFwd) reset() {
	f.mu.Lock()
	f.cur, f.want = -1, layerOff
	f.mu.Unlock()
}

func (f *relayFwd) setWant(src int) {
	f.mu.Lock()
	f.want = src
	f.mu.Unlock()
}

func (f *relayFwd) current() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cur
}

// forward sends p if it belongs to the source this viewer should see now. It
// returns true if the viewer waits for a keyframe on its wanted source.
func (f *relayFwd) forward(src int, p *rtp.Packet) (needKey bool) {
	f.mu.Lock()
	if f.want == layerOff {
		f.cur = layerOff
		f.mu.Unlock()
		return false
	}
	if src != f.cur || p.SSRC != f.curSSRC {
		// A new source, or the same one arriving on another stream (the
		// origin moved to another relay, or now connects to us).
		if src != f.want {
			f.mu.Unlock()
			return false
		}
		if !isKeyframeStart([]*rtp.Packet{p}) {
			f.mu.Unlock()
			return true
		}
		f.cur, f.curSSRC = src, p.SSRC
		f.seqOff = f.nextSeq - p.SequenceNumber
		f.switchSeq, f.highIn, f.pastSwitch = p.SequenceNumber, p.SequenceNumber, false
		ts := f.lastTS + frameTicks
		if !f.started {
			ts = p.Timestamp
		}
		f.tsOff = ts - p.Timestamp
		f.started = true
	}
	if !f.pastSwitch {
		if int16(p.SequenceNumber-f.switchSeq) < 0 {
			f.mu.Unlock()
			return false // from before our switch keyframe: would overlap the old run
		}
	} else if int16(p.SequenceNumber-f.highIn) < -1000 {
		f.mu.Unlock()
		return false
	}
	if int16(p.SequenceNumber-f.highIn) > 0 {
		f.highIn = p.SequenceNumber
		if uint16(f.highIn-f.switchSeq) > 2000 {
			f.pastSwitch = true
		}
	}
	h := p.Header.Clone()
	h.SequenceNumber = p.SequenceNumber + f.seqOff
	h.Timestamp = p.Timestamp + f.tsOff
	if int16(h.SequenceNumber-f.nextSeq) >= 0 {
		f.nextSeq, f.lastTS = h.SequenceNumber+1, h.Timestamp
	}
	f.mu.Unlock()
	out := &rtp.Packet{Header: h, Payload: p.Payload}
	if f.write != nil {
		f.write(src, out)
	} else {
		f.track.WriteRTP(out)
	}
	return false
}

// relayTracks are the relay-mode tracks and state of one PeerConnection.
type relayTracks struct {
	lowFwd *forwarder // screen-low
	nslots int
	slotV  []*relayFwd
	slotA  []*audioTrack

	// Guarded by Mesh.mu: the slot map we send this peer (as relay), and
	// the one it sends us (it relays to us).
	txSlots []string
	rxSlots []string
	byeAt   time.Time // it said it closes this connection (zero = no)

	// Relay side, for this peer as a viewer or trunk: its quality cap per
	// origin (a level, levelHigh..levelOff), and whether we forward to it.
	// Guarded by remote.polMu.
	originCap map[string]int
	fanTarget bool
}

// origin (relay side) is one member whose media we forward or play: where
// its video sources arrive, for keyframe requests upstream, and what its
// provider says it sends.
type origin struct {
	id string

	mu     sync.Mutex
	via    [2]*remote // connection each source arrives on
	src    [2]*webrtc.TrackRemote
	lowAt  time.Time // screen-low's last packet
	lastAt time.Time // last packet of any kind
	pliAt  [2]time.Time
	heard  sendingMsg
}

// originLocked is id's origin, created on first use (m.mu held).
func (m *Mesh) originLocked(id string) *origin {
	o := m.origins[id]
	if o == nil {
		o = &origin{id: id}
		m.origins[id] = o
	}
	return o
}

func (m *Mesh) originOf(id string) *origin {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.originLocked(id)
}

func (o *origin) setSrc(src int, via *remote, t *webrtc.TrackRemote) {
	o.mu.Lock()
	o.via[src], o.src[src] = via, t
	o.mu.Unlock()
}

// saw records a packet of src (audio: -1).
func (o *origin) saw(src int, now time.Time) {
	o.mu.Lock()
	o.lastAt = now
	if src == srcLow {
		o.lowAt = now
	}
	o.mu.Unlock()
}

func (o *origin) lowQuiet() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return time.Since(o.lowAt) > lowSilence
}

func (o *origin) lastPacket() time.Time {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lastAt
}

func (o *origin) heardMsg() sendingMsg {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.heard
}

// slotCount is the slot count of our connection to id: maxSlots if both
// builds understand plans, else legacySlots (m.mu held).
func (m *Mesh) slotCountLocked(id string) int {
	if m.opt.Build >= proto.MultiRelayBuild && m.builds[id] >= proto.MultiRelayBuild {
		return maxSlots
	}
	return legacySlots
}

// addVideoRelayTracks adds screen-low and the slot tracks to pc. Video
// tracks go right after screen and audio slots right after audio (see
// newPeer), in the same order on every client.
func (m *Mesh) addVideoRelayTracks(r *remote) error {
	low, err := webrtc.NewTrackLocalStaticRTP(m.cap, trackScreenLow, "blite")
	if err != nil {
		return err
	}
	r.lowFwd = newForwarder(low, layerOff)
	s, err := r.pc.AddTrack(low)
	if err != nil {
		return err
	}
	go m.readLowRTCP(s)
	r.slotV = make([]*relayFwd, r.nslots)
	r.txSlots, r.rxSlots = make([]string, r.nslots), make([]string, r.nslots)
	for i := range r.nslots {
		t, err := webrtc.NewTrackLocalStaticRTP(m.cap, fmt.Sprintf("%s%d", slotPrefix, i), "blite")
		if err != nil {
			return err
		}
		r.slotV[i] = newRelayFwd(t)
		r.slotV[i].write = m.pacedWriter(r, i)
		s, err := r.pc.AddTrack(t)
		if err != nil {
			return err
		}
		go m.readSlotRTCP(r, i, s)
	}
	return nil
}

func (m *Mesh) addAudioRelayTracks(r *remote) error {
	r.slotA = make([]*audioTrack, r.nslots)
	for i := range r.nslots {
		t := newAudioTrack(fmt.Sprintf("%s%d", aslotPrefix, i))
		r.slotA[i] = t
		s, err := r.pc.AddTrack(t)
		if err != nil {
			return err
		}
		go drainSender(s)
	}
	return nil
}

func drainSender(s *webrtc.RTPSender) {
	for {
		if _, _, err := s.ReadRTCP(); err != nil {
			return
		}
	}
}

// readLowRTCP answers the relay's keyframe requests for our screen-low.
func (m *Mesh) readLowRTCP(s *webrtc.RTPSender) {
	for {
		pkts, _, err := s.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch p.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				if m.opt.OnKeyframe != nil {
					m.opt.OnKeyframe(layerLow)
				}
			}
		}
	}
}

// txKey is the origin and source we send r in a slot ("" = none).
func (m *Mesh) txKey(r *remote, slot int) (string, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return parseSlotKey(r.txSlots[slot])
}

// readSlotRTCP (relay side) turns a peer's reports on a slot into layer
// decisions for that peer, and its keyframe requests into PLIs upstream.
func (m *Mesh) readSlotRTCP(r *remote, slot int, s *webrtc.RTPSender) {
	for {
		pkts, _, err := s.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch p := p.(type) {
			case *rtcp.ReceiverReport:
				// Pion reports on every negotiated stream, silent ones
				// too: their "no loss" must not outvote the real ones.
				if c := r.slotV[slot].current(); c < 0 || c >= layerOff {
					continue
				}
				now := time.Now()
				r.lastRR.Store(now.UnixNano())
				for _, rep := range p.Reports {
					m.observe(r, rr{lost: float64(rep.FractionLost) / 256, rtt: rrRTT(rep, now)})
				}
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				o, _ := m.txKey(r, slot)
				switch src := r.slotV[slot].current(); {
				case o == "":
				case src == srcScreen || src == srcLow:
					m.pliUpstream(o, src)
				case src >= 0 && src < layerOff:
					m.transKeyframe(o, src) // a rendition we encode
				}
			}
		}
	}
}

// sendingVideo reports whether r gets any video from us: our own, or (as
// relay) another member's.
func (r *remote) sendingVideo() bool {
	if cur, _ := r.fwd.layers(); cur >= 0 && cur < numLayers {
		return true
	}
	for _, f := range r.slotV {
		if c := f.current(); c >= 0 && c < layerOff {
			return true
		}
	}
	return false
}

func (m *Mesh) me() string { return m.sig.Welcome.PeerID }

// isRelayLocked reports whether id relays in the current plan (m.mu held).
func (m *Mesh) isRelayLocked(id string) bool { return slices.Contains(m.plan.Relays, id) }

// amRelayLocked reports whether we relay (m.mu held).
func (m *Mesh) amRelayLocked() bool { return m.isRelayLocked(m.me()) }

// homeLocked is id's relay ("" = mesh, or not planned yet) (m.mu held).
func (m *Mesh) homeLocked(id string) string {
	if len(m.plan.Relays) == 0 {
		return ""
	}
	if h, ok := m.plan.Home[id]; ok {
		return h
	}
	if m.isRelayLocked(id) {
		return id
	}
	if len(m.plan.Relays) == 1 && m.plan.Home == nil {
		return m.plan.Relays[0] // a Topology from an older server
	}
	return ""
}

// childLocked reports whether id is homed on us (m.mu held).
func (m *Mesh) childLocked(id string) bool {
	return id != m.me() && !m.isRelayLocked(id) && m.homeLocked(id) == m.me()
}

// directLocked reports whether the plan wants our own PC to id: always in a
// mesh; as relay, to our children and the other relays; else only to our
// relay (m.mu held).
func (m *Mesh) directLocked(id string) bool {
	switch {
	case len(m.plan.Relays) == 0:
		return true
	case m.amRelayLocked():
		return m.isRelayLocked(id) || m.homeLocked(id) == m.me()
	}
	return id == m.homeLocked(m.me())
}

func (m *Mesh) direct(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.directLocked(id)
}

// forwardingLocked reports whether we forward media: as relay, and as a
// former relay while its children still hang on (m.mu held).
func (m *Mesh) forwardingLocked() bool { return m.amRelayLocked() || len(m.served) > 0 }

// servesLocked reports whether we forward to non-relay peer id: our child,
// or a former child that hasn't moved yet (m.mu held).
func (m *Mesh) servesLocked(id string) bool { return m.childLocked(id) || m.served[id] }

// targetLocked reports whether we forward origin o's media, which arrives
// from `from` (o itself or its relay), to peer r: to the members we serve
// (children, and former children until they move), and over trunks only
// what our own children send (m.mu held).
func (m *Mesh) targetLocked(r *remote, o, from string) bool {
	if r.id == o || r.id == from {
		return false
	}
	if m.isRelayLocked(r.id) {
		return m.amRelayLocked() && from == o && m.childLocked(o)
	}
	return m.servesLocked(r.id)
}

// Relays returns the active relays (empty = full mesh).
func (m *Mesh) Relays() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.plan.Relays)
}

// Queue returns every relay volunteer in volunteer order, relays or not.
func (m *Mesh) Queue() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.plan.Queue)
}

// Home returns the relay id is homed on ("" = mesh).
func (m *Mesh) Home(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.homeLocked(id)
}

// Paused reports whether relay mode is paused: an older member keeps the
// room on one relay and the room is bigger than proto.RelayMaxPeers.
func (m *Mesh) Paused() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.plan.Paused
}

// Volunteer joins (on) or leaves the room's relay queue.
func (m *Mesh) Volunteer(on bool) error { return m.sig.Volunteer(on) }

// Name is a member's display name.
func (m *Mesh) Name(id string) string { return m.name(id) }

// Me is our peer ID.
func (m *Mesh) Me() string { return m.me() }

// planFromTopology is the plan an older server's Topology means: its head
// relays for everyone.
func planFromTopology(t proto.Topology) proto.Plan {
	p := proto.Plan{T: proto.TPlan, Queue: slices.Clone(t.Queue), Paused: t.Paused}
	if t.Paused {
		p.Queue = slices.Clone(t.Waiting)
	}
	if rel := t.Relay(); rel != "" {
		p.Relays = []string{rel}
	}
	return p
}

// setPlan applies a new plan: connects what it needs, then prunes what it
// no longer does once that is up.
func (m *Mesh) setPlan(p proto.Plan) {
	m.mu.Lock()
	if p.Epoch != 0 && p.Epoch < m.plan.Epoch {
		m.mu.Unlock()
		return // stale
	}
	wasRelay := m.amRelayLocked()
	same := slices.Equal(p.Relays, m.plan.Relays) && homesEqual(p.Home, m.plan.Home)
	var was []string // our children so far
	for id := range m.members {
		if m.childLocked(id) {
			was = append(was, id)
		}
	}
	m.plan = p
	isRelay := m.amRelayLocked()
	if !same {
		m.topoAt = time.Now()
		if isRelay {
			m.relaying = true
		}
		// Keep serving former children until they have moved
		// (make-before-break): they say bye, or prune gives up.
		for _, id := range was {
			if !m.childLocked(id) && !m.isRelayLocked(id) && m.peers[id] != nil {
				m.served[id] = true
			}
		}
		clear(m.provider)
	}
	m.mu.Unlock()
	if same {
		if m.opt.OnTopology != nil {
			m.opt.OnTopology() // only the queue changed
		}
		return
	}
	m.opt.Log.Info("plan", "epoch", p.Epoch, "relays", p.Relays, "home", m.Home(m.me()))
	if !wasRelay && isRelay {
		// The slot maps we had as a viewer are another relay's.
		m.clearRx()
	}
	m.updateTargets()
	m.assignAllSlots()
	m.reconcile()
	m.resendCaps()
	m.pushDemand()
	if m.opt.OnTopology != nil {
		m.opt.OnTopology()
	}
}

func homesEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// clearRx forgets the slot maps peers sent us.
func (m *Mesh) clearRx() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.peers {
		clear(r.rxSlots)
	}
	clear(m.provider)
}

// updateTargets marks the peers we forward to, whose caps on our own video
// then stop limiting what we forward them (applyCap).
func (m *Mesh) updateTargets() {
	m.mu.Lock()
	type t struct {
		r   *remote
		fan bool
	}
	var ts []t
	for id, r := range m.peers {
		ts = append(ts, t{r, m.servesLocked(id) || m.isRelayLocked(id) && m.amRelayLocked()})
	}
	m.mu.Unlock()
	for _, x := range ts {
		x.r.polMu.Lock()
		if x.r.fanTarget != x.fan {
			x.r.fanTarget = x.fan
			m.applyCap(x.r)
		}
		x.r.polMu.Unlock()
	}
}

// reconcile offers to every member we should be connected to and aren't. For
// pairs a plan change creates, the lower peer ID offers; the other side
// waits (and glare, if both still offer, resolves the same way). A member
// that just joined offers to everyone its plan connects it to, so we leave
// it alone for joinGrace. It runs on plan changes and every second.
func (m *Mesh) reconcile() {
	m.mu.Lock()
	var want []string
	for id := range m.members {
		if time.Since(m.joinedAt[id]) < joinGrace {
			continue
		}
		if m.directLocked(id) && m.peers[id] == nil && m.me() < id {
			want = append(want, id)
		}
	}
	m.mu.Unlock()
	for _, id := range want {
		if err := m.offer(id); err != nil {
			m.opt.Log.Warn("offer failed", "peer", id, "err", err)
		}
	}
}

// prune closes PCs the plan no longer wants once every wanted PC is
// connected (make-before-break), or after pruneAfter at the latest. A
// member we served waits for its new relay instead: it says bye when that
// is up. It runs from pruneLoop.
func (m *Mesh) prune() {
	m.mu.Lock()
	ready := true
	for id := range m.members {
		if m.directLocked(id) {
			if r := m.peers[id]; r == nil || r.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
				ready = false
			}
		}
	}
	late := time.Since(m.topoAt) >= pruneAfter
	if late {
		clear(m.served)
	}
	var extra []*remote
	for id, r := range m.peers {
		if m.directLocked(id) {
			continue
		}
		switch {
		case !r.byeAt.IsZero(), late, ready && !m.served[id]:
			extra = append(extra, r)
		}
	}
	for _, r := range extra {
		delete(m.peers, r.id)
		delete(m.served, r.id)
	}
	stop := false
	if m.relaying && !m.forwardingLocked() {
		m.relaying, stop = false, true
	}
	if len(extra) > 0 {
		clear(m.provider)
	}
	m.mu.Unlock()
	for _, r := range extra {
		m.opt.Log.Info("plan: closing direct connection", "peer", r.id)
		if r.byeAt.IsZero() {
			m.sig.Signal(r.id, signalMsg{Bye: true})
		}
		m.closePC(r.pc)
	}
	if stop {
		m.stopRelaying()
	}
	if len(extra) > 0 {
		m.updateLowBitrate()
		m.assignAllSlots()
		m.pushDemand()
	}
}

// gotBye: from closes our connection, which our plan no longer wants
// either; prune closes it at once. (A former child that is direct again,
// as in a mesh, just stops being served.)
func (m *Mesh) gotBye(from string) {
	m.mu.Lock()
	if r := m.peers[from]; r != nil && !m.directLocked(from) {
		r.byeAt = time.Now()
	}
	delete(m.served, from)
	m.mu.Unlock()
	m.prune()
}

func (m *Mesh) pruneLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-t.C:
			m.reconcile()
			m.prune()
			m.assignAllSlots()
			m.rendTick()
		}
	}
}

// slotKeysLocked are the slot keys r may carry from us, in preference
// order: to a member we serve, every other member's media; over a trunk,
// our children's screens, then their screen-lows (m.mu held).
func (m *Mesh) slotKeysLocked(r *remote) []string {
	if !m.forwardingLocked() {
		return nil
	}
	var keys []string
	if m.isRelayLocked(r.id) {
		if !m.amRelayLocked() {
			return nil
		}
		for id := range m.members {
			if m.childLocked(id) {
				keys = append(keys, id)
			}
		}
		slices.Sort(keys)
		for _, id := range slices.Clone(keys) {
			keys = append(keys, id+lowSuffix)
		}
		return keys
	}
	if !m.servesLocked(r.id) {
		return nil
	}
	for id := range m.members {
		if id != r.id {
			keys = append(keys, id)
		}
	}
	slices.Sort(keys)
	return keys
}

// activeLocked reports whether origin id sent us anything lately (m.mu held).
func (m *Mesh) activeLocked(id string, now time.Time) bool {
	o := m.origins[id]
	return o != nil && now.Sub(o.lastPacket()) < activeFor
}

// assignLocked updates r's slot map: drops keys it may no longer carry,
// gives free slots to active origins first, then to the rest in order, and
// lets active origins without a slot take one from an idle origin. It
// reports whether the map changed (m.mu held).
func (m *Mesh) assignLocked(r *remote, now time.Time) bool {
	keys := m.slotKeysLocked(r)
	ok := make(map[string]bool, len(keys))
	for _, k := range keys {
		ok[k] = true
	}
	changed := false
	have := map[string]bool{}
	for i, k := range r.txSlots {
		if k != "" && !ok[k] {
			r.txSlots[i] = ""
			r.slotV[i].reset()
			changed = true
		} else if k != "" {
			have[k] = true
		}
	}
	active := func(k string) bool {
		o, _ := parseSlotKey(k)
		return m.activeLocked(o, now)
	}
	order := make([]string, 0, len(keys))
	for _, k := range keys {
		if active(k) {
			order = append(order, k)
		}
	}
	for _, k := range keys {
		if !active(k) {
			order = append(order, k)
		}
	}
	for _, k := range order {
		if have[k] {
			continue
		}
		free := slices.Index(r.txSlots, "")
		if free < 0 && active(k) {
			// Take the slot of the origin quiet longest, if quiet enough.
			var oldest time.Time
			for i, k2 := range r.txSlots {
				o2, _ := parseSlotKey(k2)
				last := time.Time{}
				if o := m.origins[o2]; o != nil {
					last = o.lastPacket()
				}
				if now.Sub(last) >= idleFor && (free < 0 || last.Before(oldest)) {
					free, oldest = i, last
				}
			}
			if free >= 0 {
				delete(have, r.txSlots[free])
			}
		}
		if free < 0 {
			continue
		}
		r.txSlots[free] = k
		r.slotV[free].reset()
		have[k] = true
		changed = true
	}
	return changed
}

// assignAllSlots updates every connection's slot map and tells the peers
// whose map changed.
func (m *Mesh) assignAllSlots() {
	now := time.Now()
	m.mu.Lock()
	var send []*remote
	for _, r := range m.peers {
		if m.assignLocked(r, now) {
			send = append(send, r)
		}
	}
	m.mu.Unlock()
	for _, r := range send {
		m.sendSlots(r)
	}
}

// slotFor is the slot r carries key in, assigning one if key's origin is
// active and one is free or idle (-1 = none).
func (m *Mesh) slotFor(r *remote, key string) int {
	m.mu.Lock()
	if i := slices.Index(r.txSlots, key); i >= 0 {
		m.mu.Unlock()
		return i
	}
	changed := m.assignLocked(r, time.Now())
	i := slices.Index(r.txSlots, key)
	m.mu.Unlock()
	if changed {
		m.sendSlots(r)
	}
	return i
}

func (m *Mesh) sendSlots(r *remote) {
	m.mu.Lock()
	slots := slices.Clone(r.txSlots)
	m.mu.Unlock()
	m.sig.Signal(r.id, signalMsg{Slots: slots})
}

// stopRelaying clears our slot maps and forwarders.
func (m *Mesh) stopRelaying() {
	m.stopTranscoders()
	m.mu.Lock()
	peers := make([]*remote, 0, len(m.peers))
	for _, r := range m.peers {
		clear(r.txSlots)
		peers = append(peers, r)
	}
	m.mu.Unlock()
	for _, r := range peers {
		for _, f := range r.slotV {
			f.reset()
		}
		m.sendSlots(r)
	}
	m.updateTargets()
}

// providerLocked is the peer we take origin o's media from ("" = none):
// o itself if the plan connects us; else our relay for it (as relay, o's
// relay over the trunk), if it carries o; else, while a plan change is
// under way, o itself or whoever still carries it (m.mu held).
func (m *Mesh) providerLocked(o string) string {
	if p, ok := m.provider[o]; ok {
		return p
	}
	p := m.findProviderLocked(o)
	m.provider[o] = p
	return p
}

func (m *Mesh) findProviderLocked(o string) string {
	up := func(id string) bool {
		r := m.peers[id]
		return r != nil && r.pc.ConnectionState() == webrtc.PeerConnectionStateConnected
	}
	if m.directLocked(o) && up(o) {
		return o
	}
	carries := func(id string) bool {
		r := m.peers[id]
		return r != nil && r.pc.ConnectionState() == webrtc.PeerConnectionStateConnected &&
			(slices.Contains(r.rxSlots, o) || slices.Contains(r.rxSlots, o+lowSuffix))
	}
	home := m.homeLocked(m.me())
	if m.amRelayLocked() {
		home = m.homeLocked(o)
	}
	if home != "" && home != m.me() && carries(home) {
		return home
	}
	if up(o) {
		return o
	}
	ids := make([]string, 0, len(m.peers))
	for id := range m.peers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if carries(id) {
			return id
		}
	}
	if m.directLocked(o) && m.peers[o] != nil {
		return o // connecting
	}
	return ""
}

func (m *Mesh) forgetProviders() {
	m.mu.Lock()
	clear(m.provider)
	m.mu.Unlock()
}

// relayedOrigin is the origin and source whose media arrives on slot from
// peer `from`, or "" if we don't take that origin from `from`.
func (m *Mesh) relayedOrigin(from string, slot int) (string, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.peers[from]
	if r == nil || slot >= len(r.rxSlots) {
		return "", 0
	}
	o, src := parseSlotKey(r.rxSlots[slot])
	if o == "" || o == m.me() || m.providerLocked(o) != from {
		return "", 0
	}
	return o, src
}

// ownProvided reports whether we take r's own media from r.
func (m *Mesh) ownProvided(r *remote) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.providerLocked(r.id) == r.id
}

// gotSlots records the slot map a relay sends us and reports the relayed
// members as connected.
func (m *Mesh) gotSlots(from string, slots []string) {
	m.mu.Lock()
	r := m.peers[from]
	if r == nil || len(slots) != r.nslots {
		m.mu.Unlock()
		return
	}
	copy(r.rxSlots, slots)
	clear(m.provider)
	// Everyone we reach through this relay is connected, slot or not:
	// those without one just aren't sending.
	var up []string
	if !m.amRelayLocked() && from == m.homeLocked(m.me()) {
		for o := range m.members {
			if !m.directLocked(o) {
				if p := m.providerLocked(o); p == from || p == "" {
					up = append(up, o)
				}
			}
		}
	}
	for _, k := range slots {
		if o, src := parseSlotKey(k); o != "" && src == srcScreen && o != m.me() && m.members[o] && m.providerLocked(o) == from && !slices.Contains(up, o) {
			up = append(up, o)
		}
	}
	m.mu.Unlock()
	for _, o := range up {
		if m.opt.OnPeer != nil {
			m.opt.OnPeer(o, m.name(o), "connected")
		}
	}
}

// slotWant (relay side) is the source viewer v should get of origin o: its
// link's level toward us, capped by what v asked for o; at levelMid the
// rendition we encode for v, and screen instead of screen-low
// while o's screen carries LOW anyway.
func (m *Mesh) slotWant(v *remote, o *origin) int {
	v.polMu.Lock()
	level := viewerLevelLocked(v, o.id)
	mid, hasMid := v.originMid[o.id]
	v.polMu.Unlock()
	if level == levelMid && hasMid && m.transHas(o.id, mid) {
		return mid
	}
	switch levelLayer(level, -1) {
	case layerHigh:
		return srcScreen
	case layerLow:
		if o.lowQuiet() {
			return srcScreen
		}
		return srcLow
	}
	return layerOff
}

// trunkWant (relay side) is what trunk peer t should get of origin o in
// its slot for src: screen while it wants HIGH, screen-low while it wants
// anything (screen instead when o's screen carries LOW anyway).
func (m *Mesh) trunkWant(t *remote, o *origin, src int) int {
	t.polMu.Lock()
	level := viewerLevelLocked(t, o.id)
	t.polMu.Unlock()
	if level >= levelOff {
		return layerOff
	}
	quiet := o.lowQuiet()
	switch {
	case src == srcScreen && (level <= levelMid || quiet):
		return srcScreen
	case src == srcLow && !quiet:
		return srcLow
	}
	return layerOff
}

// fanTargets are the peers that should get origin o's media from us,
// which arrived from `from`, or nil if we don't forward.
func (m *Mesh) fanTargets(o, from string) []*remote {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.forwardingLocked() {
		return nil
	}
	var out []*remote
	for _, r := range m.peers {
		if m.targetLocked(r, o, from) {
			out = append(out, r)
		}
	}
	return out
}

// isRelay reports whether id relays.
func (m *Mesh) isRelay(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isRelayLocked(id)
}

// fanVideo (relay side) forwards one packet of origin o's source src,
// which arrived from `from`, to the peers we serve.
func (m *Mesh) fanVideo(o *origin, from string, src int, p *rtp.Packet) {
	targets := m.fanTargets(o.id, from)
	if len(targets) == 0 {
		return
	}
	needKey := [2]bool{}
	for _, v := range targets {
		if m.isRelay(v.id) {
			i := m.slotFor(v, slotKey(o.id, src))
			if i < 0 {
				continue
			}
			f := v.slotV[i]
			w := m.trunkWant(v, o, src)
			f.setWant(w)
			if f.forward(src, p) && w == src {
				needKey[src] = true
			}
			continue
		}
		i := m.slotFor(v, o.id)
		if i < 0 {
			continue
		}
		f := v.slotV[i]
		w := m.slotWant(v, o)
		f.setWant(w)
		m.noteRelaySending(v, o, w)
		if f.forward(src, p) && (w == srcScreen || w == srcLow) {
			needKey[w] = true
		}
	}
	for s, need := range needKey {
		if need {
			m.pliTo(o, s)
		}
	}
}

// fanAudio (relay side) forwards one Opus frame of origin o, which arrived
// from `from`, to the peers we serve.
func (m *Mesh) fanAudio(o *origin, from string, f aframe) {
	for _, v := range m.fanTargets(o.id, from) {
		if i := m.slotFor(v, o.id); i >= 0 {
			v.slotA[i].write(f)
		}
	}
}

func (m *Mesh) pliUpstream(o string, src int) {
	m.pliTo(m.originOf(o), src)
}

// pliTo asks origin o's provider for a keyframe on one source, at most
// every pliInterval.
func (m *Mesh) pliTo(o *origin, src int) {
	o.mu.Lock()
	t, via := o.src[src], o.via[src]
	if t == nil || via == nil || time.Since(o.pliAt[src]) < pliInterval {
		o.mu.Unlock()
		return
	}
	o.pliAt[src] = time.Now()
	o.mu.Unlock()
	via.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(t.SSRC())}})
}

func lowWant(r *remote) int {
	_, w := r.lowFwd.layers()
	return w
}

// lowWantLocked (sender side) is what our screen-low should carry toward r:
// LOW while r forwards our video (our relay, or as relay a trunk peer) and
// our screen to it carries HIGH (m.mu held).
func (m *Mesh) lowWantLocked(r *remote) int {
	fwd := r.id == m.homeLocked(m.me()) || m.amRelayLocked() && m.isRelayLocked(r.id)
	if !fwd || r.id == m.me() {
		return layerOff
	}
	if cur, _ := r.fwd.layers(); cur != layerHigh {
		return layerOff
	}
	return layerLow
}

// SetMaxLayer tells whoever sends us peerID's video the best layer we want of
// it (wire values: 0 high, 1 low, 2 none): peerID itself, or our relay. The
// wish is kept and re-sent when the plan changes.
func (m *Mesh) SetMaxLayer(peerID string, layer int) error {
	m.mu.Lock()
	m.caps[peerID] = layer
	m.mu.Unlock()
	return m.sendCap(peerID, layer)
}

func (m *Mesh) sendCap(peerID string, layer int) error {
	m.mu.Lock()
	relaying := m.amRelayLocked()
	direct := m.directLocked(peerID)
	home := m.homeLocked(m.me())
	m.mu.Unlock()
	switch {
	case relaying:
		// We pull what anyone we serve wants, our own wish included.
		m.pushDemand()
		return nil
	case direct:
		return m.sig.Signal(peerID, signalMsg{MaxLayer: &layer})
	case home != "":
		return m.sig.Signal(home, signalMsg{MaxLayer: &layer, Origin: peerID})
	}
	return nil
}

// resendCaps re-sends every kept quality wish along the current plan.
func (m *Mesh) resendCaps() {
	m.mu.Lock()
	caps := make(map[string]int, len(m.caps))
	for id, l := range m.caps {
		if m.members[id] {
			caps[id] = l
		}
	}
	m.mu.Unlock()
	for id, l := range caps {
		m.sendCap(id, l)
	}
}

// ownWishLocked is the level we want of id for ourselves (m.mu held).
func (m *Mesh) ownWishLocked(id string) int {
	if m.opt.Headless {
		return levelOff
	}
	if c, ok := m.caps[id]; ok {
		if l, ok := wireLevel(c); ok {
			return l
		}
	}
	return levelHigh
}

// demandKey is one upstream wish of a relay: to's own video (origin "") or
// an origin to relays to us.
type demandKey struct{ to, origin string }

// forgetDemand drops what we asked of id (a new connection starts afresh).
func (m *Mesh) forgetDemand(id string) {
	m.mu.Lock()
	for k := range m.demand {
		if k.to == id {
			delete(m.demand, k)
		}
	}
	m.mu.Unlock()
}

func levelWire(l int) int {
	switch {
	case l <= levelMid:
		return wireHigh
	case l >= levelOff:
		return wireOff
	}
	return wireLow
}

// pushDemand (relay) asks each origin's provider for the best layer anyone
// we serve wants of it, ourselves included: an origin nobody watches sends
// us nothing. It sends only what changed.
func (m *Mesh) pushDemand() {
	type up = demandKey
	m.mu.Lock()
	if !m.forwardingLocked() {
		// Back to asking for ourselves: undo what we asked as relay.
		var undo []string
		for k := range m.demand {
			if k.origin == "" && m.peers[k.to] != nil {
				undo = append(undo, k.to)
			}
		}
		clear(m.demand)
		m.mu.Unlock()
		for _, id := range undo {
			m.mu.Lock()
			w := levelWire(m.ownWishLocked(id))
			direct := m.directLocked(id)
			m.mu.Unlock()
			if direct {
				m.sig.Signal(id, signalMsg{MaxLayer: &w})
			}
		}
		return
	}
	type need struct {
		from    string
		own     int
		targets []*remote
	}
	needs := map[string]*need{}
	for id := range m.members {
		from := id
		if !m.directLocked(id) {
			if !m.amRelayLocked() || m.homeLocked(id) == "" || m.homeLocked(id) == m.me() {
				continue
			}
			from = m.homeLocked(id) // over the trunk
		}
		n := &need{from: from, own: m.ownWishLocked(id)}
		for _, r := range m.peers {
			if m.targetLocked(r, id, from) {
				n.targets = append(n.targets, r)
			}
		}
		needs[id] = n
	}
	m.mu.Unlock()
	want := map[up]int{}
	for id, n := range needs {
		best := n.own
		for _, r := range n.targets {
			r.polMu.Lock()
			c, ok := r.originCap[id]
			r.polMu.Unlock()
			if !ok {
				c = levelHigh
			}
			best = min(best, c)
		}
		k := up{to: n.from}
		if n.from != id {
			k.origin = id
		}
		want[k] = levelWire(best)
	}
	m.mu.Lock()
	var send []up
	for k, w := range want {
		if old, ok := m.demand[k]; !ok || old != w {
			send = append(send, k)
		}
	}
	m.demand = want
	m.mu.Unlock()
	for _, k := range send {
		w := want[k]
		m.sig.Signal(k.to, signalMsg{MaxLayer: &w, Origin: k.origin})
	}
}

// localCapOff reports whether we don't play id's video: we asked for none
// (as relay we still receive it, for the others), or we never play.
func (m *Mesh) localCapOff(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opt.Headless || m.caps[id] == wireOff && m.forwardingLocked()
}

// pacedWriter is how slot of r writes its packets (nil = straight out).
func (m *Mesh) pacedWriter(r *remote, slot int) func(int, *rtp.Packet) {
	if m.pacer == nil {
		return nil
	}
	t := r.slotV[slot].track
	return func(src int, p *rtp.Packet) {
		class := classHigh
		if src == srcLow {
			class = classLow
		}
		m.pacer.send(class, t, p)
	}
}

// Members returns the other members now in the room.
func (m *Mesh) Members() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.members))
	for id := range m.members {
		out = append(out, id)
	}
	return out
}

// SlotCount is the slot count of our connection to id (tests).
func (m *Mesh) SlotCount(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.peers[id]; r != nil {
		return r.nslots
	}
	return 0
}
