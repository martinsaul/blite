package rtc

// Renditions. Besides HIGH (the sharer's chosen size) and the
// always-on LOW, a provider can encode in-between sizes ("renditions",
// 1080p and 720p, only below the top) for viewers that can't take HIGH:
//
//   - The provider of an origin's video is the origin itself (mesh, or our
//     link to the relay) or the relay, which decodes the origin's top once
//     and encodes it per rendition (Transcoder).
//   - Providers tell each viewer what they send it: sending{origin, height,
//     top, congested}.
//   - A viewer whose provider has had it on LOW for congestion for 10 s,
//     with a top above 720p, asks resize{origin, maxHeight}; so does a viewer
//     whose decoder falls behind (DecodeLag), with cap: hold me there.
//   - The provider grants the best rendition no taller than maxHeight if one
//     runs or it has room for another encoder (CanEncode, maxTranscodes, and
//     for a relay UploadKbps), and refuses otherwise: the viewer stays on LOW.
//     The granted rendition becomes the viewer's levelMid, between HIGH and
//     LOW. A rendition nobody watched for 2 minutes is dropped.
//   - Viewers whose provider takes them OFF for congestion (even LOW doesn't
//     fit) get told (Options.OnSending), and the UI says so.
//
// Older clients ignore the new messages and never grant: viewers of them
// just stay on LOW, as before.

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// resizeMsg (viewer to provider) asks for a rendition of origin no taller
// than MaxHeight; MaxHeight 0 lifts a Cap asked before.
type resizeMsg struct {
	Origin    string `json:"origin,omitempty"` // "" = the provider's own video
	MaxHeight int    `json:"maxHeight"`
	Cap       bool   `json:"cap,omitempty"` // hold me at most there (decoder can't keep up)
}

// resizeReplyMsg (provider to viewer) answers a resize, or (OK false) says a
// granted rendition was dropped.
type resizeReplyMsg struct {
	Origin string `json:"origin,omitempty"`
	OK     bool   `json:"ok"`
	Height int    `json:"height,omitempty"`
}

// sendingMsg (provider to viewer) is what we send the viewer of origin now.
type sendingMsg struct {
	Origin    string `json:"origin,omitempty"` // "" = the provider's own video
	Height    int    `json:"height"`           // 0 = nothing
	Top       int    `json:"top"`              // origin's HIGH height; 0 = not sharing
	Congested bool   `json:"congested,omitempty"`
}

// Transcoder is a relay's decoder plus rendition encoders for one origin.
type Transcoder interface {
	Push(au []byte)
	ForceKeyframe(layer int)
	Stop()
}

const (
	rendIdle      = 2 * time.Minute  // drop a rendition nobody watched this long
	lowTrigger    = 10 * time.Second // congested on LOW this long: ask for a rendition
	askRetry      = 2 * time.Minute  // between asks for the same origin
	maxTranscodes = 6                // rendition encoders a relay runs at most
)

var rendKbps = map[int]int{layerR1080: 5000, layerR720: 2500}

// rendState is one remote's rendition state.
type rendState struct {
	// As a viewer of our own video (polMu).
	mid     int  // rendition layer granted (-1 none)
	midCap  bool // hold it at most at mid (it asked with cap)
	wireCap int  // its MaxLayer cap, as a level
	sent    sendingMsg
	// Relay side, as a viewer of the origins we relay (polMu).
	originMid    map[string]int
	originMidCap map[string]bool
	originSent   map[string]sendingMsg
	// Relay side, as an origin: what it says it sends us.
	heardMu sync.Mutex
	heard   sendingMsg
}

// renditions is a Mesh's rendition state.
type renditions struct {
	mu  sync.Mutex // serialises grants and drops (they restart encoders)
	top atomic.Int32
	own map[int]time.Time // our renditions: layer -> last watched

	trMu  sync.Mutex
	trans map[string]*originTrans // relay: per origin

	viewMu sync.Mutex
	view   map[string]*viewState // viewer: per origin

	txKbps  atomic.Int32
	txAt    time.Time
	txBytes uint64
}

type originTrans struct {
	t      Transcoder
	layers map[int]time.Time // rendition layer -> last watched
	pkz    [numLayers]*layerPacketizer
}

// viewState is what we know about our provider of one origin.
type viewState struct {
	from              string
	height, top       int
	congested         bool
	lowSince, askedAt time.Time
	granted           int // height granted, 0 none
}

func (rd *renditions) init() {
	rd.own = map[int]time.Time{}
	rd.trans = map[string]*originTrans{}
	rd.view = map[string]*viewState{}
}

// SetTop tells the mesh the height of the HIGH layer we share (0 = not
// sharing). A new top drops our renditions: the ladder below it changed.
func (m *Mesh) SetTop(height int) {
	if int(m.rend.top.Swap(int32(height))) == height {
		return
	}
	m.rend.mu.Lock()
	m.rend.own = map[int]time.Time{}
	m.rend.mu.Unlock()
	for _, r := range m.peerList() {
		r.polMu.Lock()
		r.mid, r.midCap = -1, false
		m.applyCap(r)
		m.noteSending(r)
		r.polMu.Unlock()
	}
}

func (m *Mesh) peerList() []*remote {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*remote, 0, len(m.peers))
	for _, r := range m.peers {
		out = append(out, r)
	}
	return out
}

// applyCap sets r's policy cap from its MaxLayer and rendition cap, and
// moves it if the cap requires (polMu held).
func (m *Mesh) applyCap(r *remote) {
	capLevel := r.wireCap
	if r.midCap && r.mid >= 0 && capLevel < levelMid {
		capLevel = levelMid
	}
	r.policy.hasMid = r.mid >= 0 || len(r.originMid) > 0
	if capLevel < r.policy.maxLevel {
		r.policy.nextProbe = time.Time{} // the cap held us back, not congestion: probe soon
	}
	r.policy.maxLevel = capLevel
	switch cur := int(r.level.Load()); {
	case cur < capLevel:
		m.setLevel(r, capLevel) // tighter cap: apply now
	case cur == levelOff && capLevel < levelOff:
		m.setLevel(r, levelLow) // lifting "none": resume on LOW, probe up as usual
	case cur == levelMid && !r.policy.hasMid:
		m.setLevel(r, levelLow) // its rendition is gone
	}
}

// tryMid moves r up to levelMid if congestion holds it below (polMu held):
// the rendition is the reason it asked. If it doesn't fit either, the policy
// steps down again.
func (m *Mesh) tryMid(r *remote) {
	if cur := int(r.level.Load()); cur > levelMid && cur < levelOff {
		r.policy.settle = time.Now().Add(settleTime)
		r.policy.badCount, r.policy.goodCount = 0, 0
		m.setLevel(r, levelMid)
	}
}

// noteSending tells r what we send it of our own video, if that changed
// (polMu held).
func (m *Mesh) noteSending(r *remote) {
	top := int(m.rend.top.Load())
	var msg sendingMsg
	if top > 0 {
		level := int(r.level.Load())
		msg = sendingMsg{Height: layerHeight(levelLayer(level, r.mid), top), Top: top, Congested: level > r.policy.maxLevel}
	}
	if msg == r.sent {
		return
	}
	r.sent = msg
	go m.sig.Signal(r.id, signalMsg{Sending: &msg})
}

// resendSending repeats our last sending to r (a new connection).
func (m *Mesh) resendSending(r *remote) {
	r.polMu.Lock()
	r.sent = sendingMsg{Height: -1}
	m.noteSending(r)
	r.originSent = nil
	r.polMu.Unlock()
}

func (m *Mesh) canEncode() bool { return m.opt.CanEncode != nil && m.opt.CanEncode() }

// handleResize (provider) answers viewer r's request.
func (m *Mesh) handleResize(r *remote, q resizeMsg) {
	m.rend.mu.Lock()
	defer m.rend.mu.Unlock()
	relayed := q.Origin != "" && q.Origin != m.me()
	if q.MaxHeight <= 0 { // lift the cap
		r.polMu.Lock()
		if relayed {
			delete(r.originMidCap, q.Origin)
		} else {
			r.midCap = false
			m.applyCap(r)
		}
		r.polMu.Unlock()
		return
	}
	var h int
	if relayed {
		h = m.relayResizeLocked(r, q)
	} else {
		h = m.ownResizeLocked(r, q)
	}
	m.opt.Log.Info("resize", "viewer", r.id, "origin", q.Origin, "maxHeight", q.MaxHeight, "cap", q.Cap, "granted", h)
	m.sig.Signal(r.id, signalMsg{ResizeReply: &resizeReplyMsg{Origin: q.Origin, OK: h > 0, Height: h}})
}

// ownResizeLocked grants r a rendition of our own video; 0 = refused.
func (m *Mesh) ownResizeLocked(r *remote, q resizeMsg) int {
	h := rendFor(int(m.rend.top.Load()), q.MaxHeight)
	if h == 0 || m.opt.OnRenditions == nil {
		return 0
	}
	layer := rendLayer(h)
	if _, ok := m.rend.own[layer]; !ok {
		if !m.canEncode() {
			return 0
		}
		heights := []int{h}
		for l := range m.rend.own {
			heights = append(heights, layerHeight(l, 0))
		}
		if err := m.opt.OnRenditions(heights); err != nil {
			m.opt.Log.Warn("renditions", "heights", heights, "err", err)
			return 0
		}
	}
	m.rend.own[layer] = time.Now()
	r.polMu.Lock()
	r.mid, r.midCap = layer, q.Cap
	m.applyCap(r)
	m.tryMid(r)
	m.noteSending(r)
	r.polMu.Unlock()
	return h
}

// relayResizeLocked (relay) grants viewer v a transcoded rendition of
// q.Origin; 0 = refused.
func (m *Mesh) relayResizeLocked(v *remote, q resizeMsg) int {
	m.mu.Lock()
	o := m.peers[q.Origin]
	_, slotted := m.slotOf[q.Origin]
	relaying := m.relayLocked() == m.me()
	m.mu.Unlock()
	if !relaying || o == nil || !slotted || m.opt.NewTranscoder == nil {
		return 0
	}
	o.heardMu.Lock()
	top := o.heard.Top
	o.heardMu.Unlock()
	h := rendFor(top, q.MaxHeight)
	if h == 0 {
		return 0
	}
	layer := rendLayer(h)
	m.rend.trMu.Lock()
	ot := m.rend.trans[o.id]
	have := ot != nil && hasKey(ot.layers, layer)
	count := 0
	for _, t := range m.rend.trans {
		count += len(t.layers)
	}
	var layers []int
	if ot != nil {
		for l := range ot.layers {
			layers = append(layers, l)
		}
	}
	m.rend.trMu.Unlock()
	if !have {
		if !m.canEncode() || count >= maxTranscodes || !m.uploadOK(rendKbps[layer]) {
			return 0
		}
		if err := m.startTranscoderLocked(o.id, append(layers, layer)); err != nil {
			m.opt.Log.Warn("transcoder", "origin", o.id, "err", err)
			return 0
		}
	}
	m.rend.trMu.Lock()
	if ot := m.rend.trans[o.id]; ot != nil {
		ot.layers[layer] = time.Now()
	}
	m.rend.trMu.Unlock()
	v.polMu.Lock()
	if v.originMid == nil {
		v.originMid, v.originMidCap = map[string]int{}, map[string]bool{}
	}
	v.originMid[o.id], v.originMidCap[o.id] = layer, q.Cap
	v.policy.hasMid = true
	m.tryMid(v)
	v.polMu.Unlock()
	return h
}

func hasKey[K comparable, V any](mp map[K]V, k K) bool {
	_, ok := mp[k]
	return ok
}

// uploadOK (relay) reports whether kbps more fits under UploadKbps.
func (m *Mesh) uploadOK(kbps int) bool {
	if m.opt.UploadKbps <= 0 {
		return true
	}
	return int(m.rend.txKbps.Load())+kbps <= m.opt.UploadKbps*8/10
}

// startTranscoderLocked (re)starts origin's transcoder for exactly layers,
// keeping its packetizers so viewers' streams continue (rend.mu held).
func (m *Mesh) startTranscoderLocked(origin string, layers []int) error {
	m.rend.trMu.Lock()
	old := m.rend.trans[origin]
	m.rend.trMu.Unlock()
	ot := &originTrans{layers: map[int]time.Time{}}
	now := time.Now()
	for _, l := range layers {
		ot.layers[l] = now
		if old != nil {
			if t, ok := old.layers[l]; ok {
				ot.layers[l] = t
			}
		}
	}
	for i := range ot.pkz {
		if old != nil {
			ot.pkz[i] = old.pkz[i]
		} else {
			ot.pkz[i] = newLayerPacketizer()
		}
	}
	var heights []int
	for _, l := range layers {
		heights = append(heights, layerHeight(l, 0))
	}
	slices.Sort(heights)
	t, err := m.opt.NewTranscoder(heights, func(layer int, au []byte, d time.Duration) {
		m.fanRendition(origin, ot, layer, au, d)
	})
	if err != nil {
		return err
	}
	ot.t = t
	m.rend.trMu.Lock()
	m.rend.trans[origin] = ot
	m.rend.trMu.Unlock()
	if old != nil {
		old.t.Stop()
	}
	m.opt.Log.Info("transcoding", "origin", origin, "heights", heights)
	m.pliUpstream(origin, srcScreen) // the decoder starts at a keyframe
	return nil
}

// fanRendition (relay) forwards one transcoded access unit to the viewers
// that should get this rendition of origin.
func (m *Mesh) fanRendition(origin string, ot *originTrans, layer int, au []byte, d time.Duration) {
	if layer < 0 || layer >= numLayers {
		return
	}
	pkts := ot.pkz[layer].packetize(au, d)
	m.mu.Lock()
	slot, ok := m.slotOf[origin]
	o := m.peers[origin]
	viewers := make([]*remote, 0, len(m.peers))
	for id, r := range m.peers {
		if id != origin {
			viewers = append(viewers, r)
		}
	}
	m.mu.Unlock()
	if !ok || o == nil {
		return
	}
	needKey := false
	for _, v := range viewers {
		f := v.slotV[slot]
		w := m.slotWant(v, o)
		f.setWant(w)
		if w != layer {
			continue
		}
		for _, p := range pkts {
			if f.forward(layer, p) {
				needKey = true
				break
			}
		}
	}
	if needKey {
		ot.t.ForceKeyframe(layer)
	}
}

// transcoderOf (relay) is origin's transcoder, nil if none.
func (m *Mesh) transcoderOf(origin string) Transcoder {
	m.rend.trMu.Lock()
	defer m.rend.trMu.Unlock()
	if ot := m.rend.trans[origin]; ot != nil {
		return ot.t
	}
	return nil
}

func (m *Mesh) transHas(origin string, layer int) bool {
	m.rend.trMu.Lock()
	defer m.rend.trMu.Unlock()
	ot := m.rend.trans[origin]
	return ot != nil && hasKey(ot.layers, layer)
}

func (m *Mesh) transKeyframe(origin string, layer int) {
	if t := m.transcoderOf(origin); t != nil {
		t.ForceKeyframe(layer)
	}
}

// stopTranscoder (relay) stops origin's transcoder and forgets its grants.
func (m *Mesh) stopTranscoder(origin string) {
	m.rend.trMu.Lock()
	ot := m.rend.trans[origin]
	delete(m.rend.trans, origin)
	m.rend.trMu.Unlock()
	if ot == nil {
		return
	}
	ot.t.Stop()
	for _, v := range m.peerList() {
		v.polMu.Lock()
		delete(v.originMid, origin)
		delete(v.originMidCap, origin)
		v.policy.hasMid = v.mid >= 0 || len(v.originMid) > 0
		v.polMu.Unlock()
	}
}

func (m *Mesh) stopTranscoders() {
	m.rend.trMu.Lock()
	var origins []string
	for o := range m.rend.trans {
		origins = append(origins, o)
	}
	m.rend.trMu.Unlock()
	for _, o := range origins {
		m.stopTranscoder(o)
	}
}

// viewerLevelLocked (relay) is viewer v's level for origin o: its link
// level, held down by its caps for o (v.polMu held).
func viewerLevelLocked(v *remote, o string) int {
	level := int(v.level.Load())
	if c := viewerCapLocked(v, o); c > level {
		level = c
	}
	return level
}

func viewerCapLocked(v *remote, o string) int {
	c := v.policy.maxLevel
	if oc, ok := v.originCap[o]; ok && oc > c {
		c = oc
	}
	if v.originMidCap[o] && hasKey(v.originMid, o) && c < levelMid {
		c = levelMid
	}
	return c
}

// noteRelaySending (relay) tells viewer v what it gets of origin o now, if
// that changed; w is its slot's wanted source.
func (m *Mesh) noteRelaySending(v, o *remote, w int) {
	o.heardMu.Lock()
	heard := o.heard
	o.heardMu.Unlock()
	var msg sendingMsg
	if heard.Top > 0 {
		h := 0
		switch {
		case w == srcScreen:
			h = heard.Height
		case w >= 0 && w < layerOff:
			h = layerHeight(w, heard.Top)
		}
		v.polMu.Lock()
		congested := heard.Congested || int(v.level.Load()) > viewerCapLocked(v, o.id)
		v.polMu.Unlock()
		msg = sendingMsg{Height: h, Top: heard.Top, Congested: congested && h < heard.Top}
	}
	msg.Origin = o.id
	v.polMu.Lock()
	if v.originSent == nil {
		v.originSent = map[string]sendingMsg{}
	}
	if old, ok := v.originSent[o.id]; ok && old == msg {
		v.polMu.Unlock()
		return
	}
	v.originSent[o.id] = msg
	v.polMu.Unlock()
	go m.sig.Signal(v.id, signalMsg{Sending: &msg})
}

// gotSending (viewer) records what a provider says it sends us.
func (m *Mesh) gotSending(from string, q sendingMsg) {
	origin := q.Origin
	if origin == "" {
		origin = from
	}
	m.mu.Lock()
	rel := m.relayLocked()
	ownLink := origin == from && m.directLocked(from)
	viaRelay := from == rel && rel != m.me() && origin != from && !m.directLocked(origin)
	r := m.peers[from]
	relaying := rel == m.me()
	m.mu.Unlock()
	if !ownLink && !viaRelay {
		return
	}
	if ownLink && r != nil {
		r.heardMu.Lock()
		r.heard = q
		r.heardMu.Unlock()
		if relaying && q.Top == 0 {
			// It stopped sharing: no packets will come to say so.
			for _, v := range m.peerList() {
				if v.id != from {
					m.noteRelaySending(v, r, layerOff)
				}
			}
		}
	}
	now := time.Now()
	m.rend.viewMu.Lock()
	vs := m.rend.view[origin]
	if vs == nil {
		vs = &viewState{}
		m.rend.view[origin] = vs
	}
	changed := vs.height != q.Height || vs.top != q.Top || vs.congested != q.Congested || vs.from != from
	vs.from, vs.height, vs.top, vs.congested = from, q.Height, q.Top, q.Congested
	if q.Congested && q.Height == layerHeight(layerLow, 0) {
		if vs.lowSince.IsZero() {
			vs.lowSince = now
		}
	} else {
		vs.lowSince = time.Time{}
	}
	m.rend.viewMu.Unlock()
	if changed {
		m.opt.Log.Info("provider sends", "origin", origin, "from", from, "height", q.Height, "top", q.Top, "congested", q.Congested)
		if m.opt.OnSending != nil {
			m.opt.OnSending(origin, q.Height, q.Top, q.Congested)
		}
	}
}

// gotResizeReply (viewer) records a provider's answer.
func (m *Mesh) gotResizeReply(from string, q resizeReplyMsg) {
	origin := q.Origin
	if origin == "" {
		origin = from
	}
	m.rend.viewMu.Lock()
	if vs := m.rend.view[origin]; vs != nil && vs.from == from {
		if q.OK {
			vs.granted = q.Height
		} else {
			vs.granted = 0
		}
	}
	m.rend.viewMu.Unlock()
	m.opt.Log.Info("resize reply", "origin", origin, "from", from, "ok", q.OK, "height", q.Height)
}

// forgetView drops what we know about a member as an origin.
func (m *Mesh) forgetView(id string) {
	m.rend.viewMu.Lock()
	delete(m.rend.view, id)
	m.rend.viewMu.Unlock()
}

// askResize sends a resize for origin to its provider (viewMu held).
func (m *Mesh) askResizeLocked(origin string, vs *viewState, h int, capped bool, now time.Time) {
	vs.askedAt = now
	q := resizeMsg{MaxHeight: h, Cap: capped}
	if origin != vs.from {
		q.Origin = origin
	}
	m.opt.Log.Info("asking for a rendition", "origin", origin, "provider", vs.from, "maxHeight", h, "cap", capped)
	go m.sig.Signal(vs.from, signalMsg{Resize: &q})
}

// DecodeLag (viewer) is told that our decoder can't keep up with origin's
// video: ask its provider to hold us at a rendition below what we get now.
// It reports whether a request went out.
func (m *Mesh) DecodeLag(origin string) bool {
	now := time.Now()
	m.rend.viewMu.Lock()
	defer m.rend.viewMu.Unlock()
	vs := m.rend.view[origin]
	if vs == nil || vs.height <= 0 || (!vs.askedAt.IsZero() && now.Sub(vs.askedAt) < askRetry) {
		return false
	}
	h := rendFor(vs.top, vs.height-1)
	if h == 0 {
		return false
	}
	m.askResizeLocked(origin, vs, h, true, now)
	return true
}

// ForgetAsk lets the next request for origin go out now (tests).
func (m *Mesh) ForgetAsk(origin string) {
	m.rend.viewMu.Lock()
	if vs := m.rend.view[origin]; vs != nil {
		vs.askedAt = time.Time{}
	}
	m.rend.viewMu.Unlock()
}

// rendTick runs every second: viewer triggers, idle renditions, upload rate.
func (m *Mesh) rendTick() {
	now := time.Now()
	if m.relay() != m.me() { // the relay is the hub, it doesn't ask
		m.rend.viewMu.Lock()
		for origin, vs := range m.rend.view {
			if vs.lowSince.IsZero() || now.Sub(vs.lowSince) < lowTrigger || vs.top <= 720 {
				continue
			}
			if !vs.askedAt.IsZero() && now.Sub(vs.askedAt) < askRetry {
				continue
			}
			below := vs.top
			if vs.granted > 0 {
				below = vs.granted // that didn't fit either: one rung lower
			}
			if h := rendFor(vs.top, below-1); h > 0 {
				m.askResizeLocked(origin, vs, h, false, now)
			}
		}
		m.rend.viewMu.Unlock()
	}
	if m.rend.mu.TryLock() { // a grant in progress: next tick
		m.dropIdleLocked(now)
		m.rend.mu.Unlock()
	}
	if m.opt.UploadKbps > 0 && now.Sub(m.rend.txAt) >= 5*time.Second {
		var tx uint64
		for _, ps := range m.Stats() {
			tx += ps.TxBytes
		}
		if !m.rend.txAt.IsZero() && tx >= m.rend.txBytes {
			m.rend.txKbps.Store(int32(float64(tx-m.rend.txBytes) * 8 / 1000 / now.Sub(m.rend.txAt).Seconds()))
		}
		m.rend.txAt, m.rend.txBytes = now, tx
	}
}

// dropIdleLocked drops renditions nobody watched for rendIdle (rend.mu held).
func (m *Mesh) dropIdleLocked(now time.Time) {
	peers := m.peerList()
	// Our own.
	var dropped []int
	for layer, last := range m.rend.own {
		used := false
		for _, r := range peers {
			if cur, _ := r.fwd.layers(); cur == layer {
				used = true
			}
		}
		switch {
		case used:
			m.rend.own[layer] = now
		case now.Sub(last) > rendIdle:
			delete(m.rend.own, layer)
			dropped = append(dropped, layer)
		}
	}
	if len(dropped) > 0 {
		var heights []int
		for l := range m.rend.own {
			heights = append(heights, layerHeight(l, 0))
		}
		m.opt.Log.Info("dropping idle renditions", "layers", dropped, "keep", heights)
		for _, r := range peers {
			r.polMu.Lock()
			if slices.Contains(dropped, r.mid) {
				r.mid, r.midCap = -1, false
				m.applyCap(r)
				m.noteSending(r)
				go m.sig.Signal(r.id, signalMsg{ResizeReply: &resizeReplyMsg{}})
			}
			r.polMu.Unlock()
		}
		if m.opt.OnRenditions != nil {
			if err := m.opt.OnRenditions(heights); err != nil {
				m.opt.Log.Warn("renditions", "heights", heights, "err", err)
			}
		}
	}
	// Relay transcodes.
	m.rend.trMu.Lock()
	origins := make(map[string]*originTrans, len(m.rend.trans))
	for o, ot := range m.rend.trans {
		origins[o] = ot
	}
	m.rend.trMu.Unlock()
	for origin, ot := range origins {
		m.mu.Lock()
		slot, ok := m.slotOf[origin]
		m.mu.Unlock()
		var gone, keep []int
		m.rend.trMu.Lock()
		for layer, last := range ot.layers {
			used := false
			for _, v := range peers {
				if ok && v.id != origin && v.slotV[slot].current() == layer {
					used = true
				}
			}
			switch {
			case used:
				ot.layers[layer] = now
				keep = append(keep, layer)
			case now.Sub(last) > rendIdle:
				gone = append(gone, layer)
			default:
				keep = append(keep, layer)
			}
		}
		m.rend.trMu.Unlock()
		if len(gone) == 0 {
			continue
		}
		m.opt.Log.Info("dropping idle transcodes", "origin", origin, "layers", gone)
		for _, v := range peers {
			v.polMu.Lock()
			if l, ok := v.originMid[origin]; ok && slices.Contains(gone, l) {
				delete(v.originMid, origin)
				delete(v.originMidCap, origin)
				v.policy.hasMid = v.mid >= 0 || len(v.originMid) > 0
				go m.sig.Signal(v.id, signalMsg{ResizeReply: &resizeReplyMsg{Origin: origin}})
			}
			v.polMu.Unlock()
		}
		if len(keep) == 0 {
			m.stopTranscoder(origin)
		} else if err := m.startTranscoderLocked(origin, keep); err != nil {
			m.opt.Log.Warn("transcoder", "origin", origin, "err", err)
			m.stopTranscoder(origin)
		}
	}
}
