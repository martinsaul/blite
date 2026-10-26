package rtc

// Relay mode. The server keeps a relay queue per room; its head is
// the active relay. While a relay is active, every other member connects only
// to the relay, which forwards each member's video and audio to everyone else
// over fixed "origin slots".
//
// Every PeerConnection carries the same tracks whatever the topology, so a
// topology change never renegotiates:
//
//	screen      our video, adaptive per peer (forwarder + layerPolicy)
//	screen-low  our LOW layer; only sent to the relay, and only while
//	            screen carries HIGH, so the relay can serve weak viewers
//	slotN       relay -> viewer: origin N's video (N < numSlots)
//	audio       our Opus
//	aslotN      relay -> viewer: origin N's audio
//
// The relay tells each viewer which origin is in which slot (signalMsg.Slots).
// Clients from before relay mode see the extra m-lines and never get RTP on
// them; the server refuses relay mode while any of them is in the room.

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

// numSlots is one slot per possible origin: everyone but the relay.
const numSlots = proto.MaxPeers - 1

const (
	trackScreen    = "screen"
	trackScreenLow = "screen-low"
	trackAudio     = "audio"
	slotPrefix     = "slot"
	aslotPrefix    = "aslot"
)

// Relay sources of one origin, used as forwarder "layers" at the relay.
const (
	srcScreen = 0 // the origin's screen track (HIGH, or LOW when its uplink is poor)
	srcLow    = 1 // the origin's screen-low track (LOW while screen carries HIGH)
)

const (
	pruneAfter  = 10 * time.Second // longest we keep PCs the topology no longer wants
	pliInterval = 500 * time.Millisecond
	lowSilence  = time.Second // screen-low quiet this long: the origin's screen carries LOW
)

// slotIndex parses "slotN" / "aslotN".
func slotIndex(id, prefix string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(id, prefix))
	if !strings.HasPrefix(id, prefix) || err != nil || n < 0 || n >= numSlots {
		return 0, false
	}
	return n, true
}

// relayFwd feeds one viewer's slot from one origin's two sources, one packet
// at a time. Unlike forwarder it keeps the source's sequence gaps and order
// (seq out = seq in + offset), so NACK retransmissions and reordering survive
// the hop.
type relayFwd struct {
	track *webrtc.TrackLocalStaticRTP

	mu         sync.Mutex
	cur, want  int // source being sent (-1 none yet) and wanted (layerOff = nothing)
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
	if src != f.cur {
		if src != f.want {
			f.mu.Unlock()
			return false
		}
		if !isKeyframeStart([]*rtp.Packet{p}) {
			f.mu.Unlock()
			return true
		}
		f.cur = src
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
	f.track.WriteRTP(&rtp.Packet{Header: h, Payload: p.Payload})
	return false
}

// relayTracks are the relay-mode tracks of one PeerConnection.
type relayTracks struct {
	lowFwd *forwarder // screen-low
	slotV  [numSlots]*relayFwd
	slotA  [numSlots]*webrtc.TrackLocalStaticRTP

	// Relay side, for this peer as an origin: its received video sources
	// (for PLIs upstream), and when screen-low last had a packet.
	srcMu sync.Mutex
	src   [2]*webrtc.TrackRemote
	lowAt time.Time
	pliAt [2]time.Time
	// Relay side, for this peer as a viewer: its quality cap per origin
	// (a level, levelHigh..levelOff). Guarded by remote.polMu.
	originCap map[string]int
}

// addRelayTracks adds screen-low and the slot tracks to pc. Video tracks go
// right after screen and audio slots right after audio (see newPeer), in the
// same order on every client.
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
	for i := range numSlots {
		t, err := webrtc.NewTrackLocalStaticRTP(m.cap, fmt.Sprintf("%s%d", slotPrefix, i), "blite")
		if err != nil {
			return err
		}
		r.slotV[i] = newRelayFwd(t)
		s, err := r.pc.AddTrack(t)
		if err != nil {
			return err
		}
		go m.readSlotRTCP(r, i, s)
	}
	return nil
}

func (m *Mesh) addAudioRelayTracks(r *remote) error {
	for i := range numSlots {
		t, err := webrtc.NewTrackLocalStaticRTP(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1"},
			fmt.Sprintf("%s%d", aslotPrefix, i), "blite")
		if err != nil {
			return err
		}
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

// readSlotRTCP (relay side) turns a viewer's reports on a slot into layer
// decisions for that viewer, and its keyframe requests into PLIs to the origin.
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
				switch src := r.slotV[slot].current(); {
				case src == srcScreen || src == srcLow:
					m.pliUpstream(m.slotOrigin(slot), src)
				case src >= 0 && src < layerOff:
					m.transKeyframe(m.slotOrigin(slot), src) // a rendition we encode
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

// relayLocked is the active relay ("" = mesh). m.mu held.
func (m *Mesh) relayLocked() string {
	if len(m.queue) == 0 {
		return ""
	}
	return m.queue[0]
}

func (m *Mesh) relay() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.relayLocked()
}

func (m *Mesh) me() string { return m.sig.Welcome.PeerID }

// directLocked reports whether we exchange media with id over our own PC to
// it: always in a mesh, always as relay, and only with the relay otherwise.
func (m *Mesh) directLocked(id string) bool {
	rel := m.relayLocked()
	return rel == "" || rel == m.me() || id == rel
}

func (m *Mesh) direct(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.directLocked(id)
}

// Topology returns the relay queue (head = active relay).
func (m *Mesh) Topology() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.queue)
}

// Volunteer joins (on) or leaves the room's relay queue.
func (m *Mesh) Volunteer(on bool) error { return m.sig.Volunteer(on) }

// Name is a member's display name.
func (m *Mesh) Name(id string) string { return m.name(id) }

// Me is our peer ID.
func (m *Mesh) Me() string { return m.me() }

// setTopology applies a new relay queue: connects what the new topology
// needs, then prunes what it no longer does once that is up.
func (m *Mesh) setTopology(queue []string) {
	m.mu.Lock()
	old := m.relayLocked()
	m.queue = slices.Clone(queue)
	rel := m.relayLocked()
	if rel != old {
		m.topoAt = time.Now()
	}
	m.mu.Unlock()
	if rel == old {
		if m.opt.OnTopology != nil {
			m.opt.OnTopology() // the queue behind the relay changed
		}
		return
	}
	m.opt.Log.Info("topology", "relay", rel, "queue", queue)
	if old == m.me() || rel == m.me() {
		// Starting over either way; as a new relay, the slot map we had
		// as a viewer is the old relay's.
		m.stopRelaying()
	}
	if rel == m.me() {
		m.assignSlots()
	}
	m.reconcile()
	m.resendCaps()
	if m.opt.OnTopology != nil {
		m.opt.OnTopology()
	}
}

// reconcile offers to every member we should be connected to and aren't. For
// pairs a topology change creates, the lower peer ID offers; the other side
// waits (and glare, if both still offer, resolves the same way).
func (m *Mesh) reconcile() {
	m.mu.Lock()
	var want []string
	for id := range m.members {
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

// prune closes PCs the topology no longer wants once every wanted PC is
// connected (make-before-break), or after pruneAfter at the latest. It runs
// from pruneLoop.
func (m *Mesh) prune() {
	m.mu.Lock()
	ready := true
	var extra []*remote
	for id := range m.members {
		if m.directLocked(id) {
			if r := m.peers[id]; r == nil || r.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
				ready = false
			}
		}
	}
	for id, r := range m.peers {
		if !m.directLocked(id) {
			extra = append(extra, r)
		}
	}
	if len(extra) == 0 || (!ready && time.Since(m.topoAt) < pruneAfter) {
		m.mu.Unlock()
		return
	}
	for _, r := range extra {
		delete(m.peers, r.id)
	}
	m.mu.Unlock()
	for _, r := range extra {
		m.opt.Log.Info("topology: closing direct connection", "peer", r.id)
		m.closePC(r.pc)
	}
	m.updateLowBitrate()
}

func (m *Mesh) pruneLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-t.C:
			m.prune()
			m.rendTick()
		}
	}
}

// assignSlots (as relay) gives every other member a slot, keeping existing
// assignments, and tells the viewers if anything changed.
func (m *Mesh) assignSlots() {
	m.mu.Lock()
	if m.relayLocked() != m.me() {
		m.mu.Unlock()
		return
	}
	changed := false
	for o, i := range m.slotOf {
		if !m.members[o] {
			delete(m.slotOf, o)
			m.slots[i] = ""
			changed = true
		}
	}
	ids := make([]string, 0, len(m.members))
	for id := range m.members {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if _, ok := m.slotOf[id]; ok {
			continue
		}
		for i, o := range m.slots {
			if o == "" {
				m.slots[i], m.slotOf[id] = id, i
				changed = true
				break
			}
		}
	}
	slots := m.slots
	viewers := make([]*remote, 0, len(m.peers))
	for _, r := range m.peers {
		viewers = append(viewers, r)
	}
	m.mu.Unlock()
	if !changed {
		return
	}
	for _, r := range viewers {
		for i := range numSlots {
			if slots[i] == "" || slots[i] == r.id {
				r.slotV[i].reset()
			}
		}
		m.sendSlots(r.id, slots)
	}
}

func (m *Mesh) sendSlots(to string, slots [numSlots]string) {
	m.sig.Signal(to, signalMsg{Slots: slots[:]})
}

// stopRelaying clears the slot map and our slot forwarders.
func (m *Mesh) stopRelaying() {
	m.stopTranscoders()
	m.mu.Lock()
	m.slotOf = map[string]int{}
	m.slots = [numSlots]string{}
	m.slotsFrom = ""
	peers := make([]*remote, 0, len(m.peers))
	for _, r := range m.peers {
		peers = append(peers, r)
	}
	m.mu.Unlock()
	for _, r := range peers {
		for _, f := range r.slotV {
			f.reset()
		}
	}
}

// slotOrigin is the origin in a slot: ours as relay, else the relay's map.
func (m *Mesh) slotOrigin(slot int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.slots[slot]
}

// relayedOrigin (viewer side) is the origin whose media arrives on slot from
// peer `from`, or "" if we shouldn't play it: from isn't our relay, or we
// get that origin directly.
func (m *Mesh) relayedOrigin(from string, slot int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if from != m.relayLocked() || from == m.me() || m.slotsFrom != from {
		return ""
	}
	o := m.slots[slot]
	if o == "" || m.directLocked(o) {
		return ""
	}
	return o
}

// gotSlots (viewer side) records the relay's slot map and reports the
// relayed members as connected.
func (m *Mesh) gotSlots(from string, slots []string) {
	if len(slots) != numSlots {
		return
	}
	m.mu.Lock()
	if from != m.relayLocked() || from == m.me() {
		m.mu.Unlock()
		return
	}
	copy(m.slots[:], slots)
	m.slotsFrom = from
	var up []string
	for _, o := range slots {
		if o != "" && o != m.me() && m.members[o] {
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
func (m *Mesh) slotWant(v, o *remote) int {
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
		o.srcMu.Lock()
		quiet := time.Since(o.lowAt) > lowSilence
		o.srcMu.Unlock()
		if quiet {
			return srcScreen
		}
		return srcLow
	}
	return layerOff
}

// fanVideo (relay side) forwards one packet of origin o's source src to
// every other member.
func (m *Mesh) fanVideo(o *remote, src int, p *rtp.Packet) {
	m.mu.Lock()
	slot, ok := m.slotOf[o.id]
	if !ok || m.relayLocked() != m.me() {
		m.mu.Unlock()
		return
	}
	viewers := make([]*remote, 0, len(m.peers))
	for id, r := range m.peers {
		if id != o.id {
			viewers = append(viewers, r)
		}
	}
	m.mu.Unlock()
	needKey := false
	for _, v := range viewers {
		f := v.slotV[slot]
		w := m.slotWant(v, o)
		f.setWant(w)
		m.noteRelaySending(v, o, w)
		if f.forward(src, p) {
			needKey = true
		}
	}
	if !needKey {
		return
	}
	for _, v := range viewers {
		if w := m.slotWant(v, o); (w == srcScreen || w == srcLow) && v.slotV[slot].current() != w {
			m.pliTo(o, w)
		}
	}
}

// fanAudio (relay side) forwards one Opus packet of origin o to every other
// member.
func (m *Mesh) fanAudio(o *remote, p *rtp.Packet) {
	m.mu.Lock()
	slot, ok := m.slotOf[o.id]
	if !ok || m.relayLocked() != m.me() {
		m.mu.Unlock()
		return
	}
	viewers := make([]*remote, 0, len(m.peers))
	for id, r := range m.peers {
		if id != o.id {
			viewers = append(viewers, r)
		}
	}
	m.mu.Unlock()
	for _, v := range viewers {
		v.slotA[slot].WriteRTP(p)
	}
}

func (m *Mesh) pliUpstream(origin string, src int) {
	m.mu.Lock()
	o := m.peers[origin]
	m.mu.Unlock()
	if o != nil {
		m.pliTo(o, src)
	}
}

// pliTo asks origin o for a keyframe on one source, at most every pliInterval.
func (m *Mesh) pliTo(o *remote, src int) {
	o.srcMu.Lock()
	t := o.src[src]
	if t == nil || time.Since(o.pliAt[src]) < pliInterval {
		o.srcMu.Unlock()
		return
	}
	o.pliAt[src] = time.Now()
	o.srcMu.Unlock()
	o.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(t.SSRC())}})
}

func lowWant(r *remote) int {
	_, w := r.lowFwd.layers()
	return w
}

// lowWantLocked (sender side) is what our screen-low should carry toward r: LOW
// while r is the relay and our screen to it carries HIGH.
func (m *Mesh) lowWantLocked(r *remote) int {
	if rel := m.relayLocked(); rel == "" || rel == m.me() || r.id != rel {
		return layerOff
	}
	if cur, _ := r.fwd.layers(); cur != layerHigh {
		return layerOff
	}
	return layerLow
}

// SetMaxLayer tells whoever sends us peerID's video the best layer we want of
// it (wire values: 0 high, 1 low, 2 none): peerID itself, or the relay. The
// wish is kept and re-sent when the topology changes.
func (m *Mesh) SetMaxLayer(peerID string, layer int) error {
	m.mu.Lock()
	m.caps[peerID] = layer
	m.mu.Unlock()
	return m.sendCap(peerID, layer)
}

func (m *Mesh) sendCap(peerID string, layer int) error {
	m.mu.Lock()
	rel := m.relayLocked()
	direct := m.directLocked(peerID)
	m.mu.Unlock()
	switch {
	case rel == m.me():
		// As relay we pull everyone's full video for the others; our own
		// "none" is applied locally (deliverVideo).
		high := wireHigh
		return m.sig.Signal(peerID, signalMsg{MaxLayer: &high})
	case direct:
		return m.sig.Signal(peerID, signalMsg{MaxLayer: &layer})
	default:
		return m.sig.Signal(rel, signalMsg{MaxLayer: &layer, Origin: peerID})
	}
}

// resendCaps re-sends every kept quality wish along the current topology.
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

// localCapOff reports whether we asked for no video of id (as relay we still
// receive it, for the others).
func (m *Mesh) localCapOff(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.caps[id] == wireOff && m.relayLocked() == m.me()
}
