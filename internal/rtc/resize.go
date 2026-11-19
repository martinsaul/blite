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
	"math"
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
	peakWindow    = 2 * time.Minute  // sliding window for passive peakKbps upload estimate
	lowTrigger    = 10 * time.Second // congested on LOW this long: ask for a rendition
	askRetry      = 2 * time.Minute  // between asks for the same origin
	maxTranscodes = 6                // rendition encoders a relay runs at most
)

var rendKbps = map[int]int{layerR1080: 5000, layerR720: 2500, layerR540: 1200}

// rendState is one remote's rendition state.
type rendState struct {
	// As a viewer of our own video (polMu).
	mid          int                 // rendition layer granted by viewer request (-1 none)
	midCap       bool                // hold it at most at mid (it asked with cap)
	uploadCap    int                 // upload budget cap (levelHigh = none, levelMid, levelLow)
	uploadMid    int                 // rendition layer granted by upload budget (-1 none)
	wireCap      int                 // its MaxLayer cap, as a level
	hasWireCap   bool                // it explicitly sent a MaxLayer cap
	watchers     []string            // viewer IDs reported by this peer when it relays our video
	sentCap      map[string]int      // last MaxLayer wire value sent to this remote per origin ("" = direct)
	sentWatchers map[string][]string // last Watchers set sent to this remote per origin
	sent         sendingMsg
	// Relay side, as a viewer of the origins we relay (polMu).
	originMid    map[string]int
	originMidCap map[string]bool
	originSent   map[string]sendingMsg
}

func (r *remote) effectiveMid() int {
	switch {
	case r.mid >= 0 && r.uploadMid >= 0:
		if layerHeight(r.uploadMid, 0) < layerHeight(r.mid, 0) {
			return r.uploadMid
		}
		return r.mid
	case r.uploadMid >= 0:
		return r.uploadMid
	default:
		return r.mid
	}
}

// shareCfg is an atomic snapshot of our outgoing share parameters so ticks
// never observe a half-applied SetShareConfig.
type shareCfg struct {
	top         int
	fps         int
	highKbps    int
	minHeight   int
	smallerCopy bool
	fpsSteps    bool
}

type txSample struct {
	at   time.Time
	kbps int32
}

// renditions is a Mesh's rendition state.
type renditions struct {
	mu         sync.Mutex        // serialises grants and drops (they restart encoders)
	own        map[int]time.Time // our renditions: layer -> last watched
	ownCreated map[int]time.Time // our renditions: layer -> when created

	cfgMu        sync.Mutex
	cfg          atomic.Pointer[shareCfg]
	sharingAudio atomic.Bool // whether we are currently sending audio

	autoMu        sync.Mutex
	autoCapActive atomic.Bool  // passive peakKbps upload budget triggered by congestion
	autoCleanAt   atomic.Int64 // unix nanos since peers have been clean under auto cap
	autoHeadroom  atomic.Int32 // extra probe % above peakKbps when links stay clean

	// Upload ranking & unit-swap hysteresis state (guarded by Mesh.rebalMu).
	rankOrder     []string
	rankStableAt  time.Time
	candOrder     []string
	candOrderAt   time.Time
	lastSwapAt    time.Time
	candAllocs    map[string]PeerAlloc
	candAllocsAt  time.Time
	candMidHeight int
	candMidAt     time.Time
	hadBudgetMid  atomic.Bool // true across ResetRenditions when a viewer had a budget mid copy

	trMu  sync.Mutex
	trans map[string]*originTrans // relay: per origin

	viewMu sync.Mutex
	view   map[string]*viewState // viewer: per origin

	txMu        sync.Mutex
	txKbps      atomic.Int32
	peakKbps    atomic.Int32 // highest txKbps over peakWindow (passive upload estimate)
	txAt        time.Time
	txBytes     uint64
	peakSamples []txSample

	// Per-layer measured RTP send rate (smoothed).
	layerBytes     [numLayers]atomic.Uint64
	rateMu         sync.Mutex
	layerSampleAt  time.Time
	layerPrevBytes [numLayers]uint64
	layerEWMA      [numLayers]float64
	layerKbps      [numLayers]atomic.Int32
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
	rd.ownCreated = map[int]time.Time{}
	rd.trans = map[string]*originTrans{}
	rd.view = map[string]*viewState{}
	rd.cfg.Store(&shareCfg{
		fps:         30,
		highKbps:    2500,
		minHeight:   360,
		smallerCopy: true,
	})
}

func (rd *renditions) loadCfg() shareCfg {
	if p := rd.cfg.Load(); p != nil {
		return *p
	}
	return shareCfg{fps: 30, highKbps: 2500, minHeight: 360, smallerCopy: true}
}

func (rd *renditions) updateCfg(fn func(*shareCfg)) (prev, next shareCfg) {
	rd.cfgMu.Lock()
	defer rd.cfgMu.Unlock()
	prev = rd.loadCfg()
	next = prev
	fn(&next)
	rd.cfg.Store(&next)
	return prev, next
}

func (rd *renditions) resetAutoCap() {
	rd.autoMu.Lock()
	rd.autoCapActive.Store(false)
	rd.autoCleanAt.Store(0)
	rd.autoHeadroom.Store(0)
	rd.autoMu.Unlock()
}

func (rd *renditions) resetLayerRates(onlyRenditions bool) {
	rd.rateMu.Lock()
	defer rd.rateMu.Unlock()
	if !onlyRenditions {
		rd.layerSampleAt = time.Time{}
	}
	for i := range numLayers {
		if onlyRenditions && (i == layerHigh || i == layerLow) {
			continue
		}
		rd.layerEWMA[i] = 0
		rd.layerKbps[i].Store(0)
		rd.layerPrevBytes[i] = rd.layerBytes[i].Load()
	}
}

// lowHeight returns our active LOW floor height (360, 540 or 720), or 0 when
// no smaller copy runs below HIGH (SmallerCopy is false or MinHeight >= top).
func (m *Mesh) lowHeight() int {
	sc := m.rend.loadCfg()
	if !sc.smallerCopy {
		return 0
	}
	mh := sc.minHeight
	if mh <= 0 {
		mh = 360
	}
	if sc.top > 0 && mh >= sc.top {
		return 0
	}
	return mh
}

// SetTop tells the mesh the height of the HIGH layer we share (0 = not
// sharing). A new top drops our renditions: the ladder below it changed.
func (m *Mesh) SetTop(height int) {
	m.mu.Lock()
	prev, _ := m.rend.updateCfg(func(c *shareCfg) { c.top = height })
	if prev.top == height {
		m.mu.Unlock()
		return
	}
	if height <= 0 {
		clear(m.watchingPeer)
		clear(m.rehomingWatch)
	}
	m.mu.Unlock()
	if height <= 0 {
		m.rend.resetAutoCap()
		m.rend.resetLayerRates(false)
	}
	m.ResetRenditions()
	if prev.top <= 0 && height > 0 {
		m.checkAllWatching()
	}
}

// ResetRenditions drops our renditions, as a new top does, without telling
// viewers we stopped sharing (a new source or size: SetTop follows).
// Active upload budget allocations are kept across resets while sharing so
// capped viewers do not jump to HIGH, and any budget mid-size copy is
// remembered so the following rebalance restores it if the budget allows.
func (m *Mesh) ResetRenditions() {
	m.rend.mu.Lock()
	m.rend.own = map[int]time.Time{}
	m.rend.ownCreated = map[int]time.Time{}
	m.rend.mu.Unlock()
	m.rend.resetLayerRates(true)
	keepUpload := m.rend.loadCfg().top > 0
	sawMid := false
	for _, r := range m.peerList() {
		r.polMu.Lock()
		r.mid, r.midCap = -1, false
		if !keepUpload {
			r.uploadCap, r.uploadMid = levelHigh, -1
		} else {
			if r.uploadCap == levelMid {
				sawMid = true
				r.uploadCap = levelLow
			}
			r.uploadMid = -1
		}
		m.applyCap(r)
		m.noteSending(r)
		r.polMu.Unlock()
	}
	if !keepUpload {
		m.rend.hadBudgetMid.Store(false)
	} else if sawMid {
		m.rend.hadBudgetMid.Store(true)
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
// moves it if the cap requires (polMu held). A peer we forward to caps only
// our own video with MaxLayer (setLevel), not what we forward it.
func (m *Mesh) applyCap(r *remote) {
	capLevel := r.wireCap
	if r.fanTarget {
		capLevel = levelHigh
	}
	if r.midCap && r.mid >= 0 && capLevel < levelMid {
		capLevel = levelMid
	}
	if !r.fanTarget && r.uploadCap > capLevel {
		capLevel = r.uploadCap
	}
	r.policy.hasMid = r.effectiveMid() >= 0 || len(r.originMid) > 0
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
	case r.fanTarget:
		m.setLevel(r, cur) // its cap on our own video may have changed
	default:
		// If uploadMid or smallerCopy changed while level stayed the same, refresh want.
		m.setLevel(r, cur)
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

// targetLayerFor maps r's send level to the encoded layer to send it (polMu held).
func (m *Mesh) targetLayerFor(r *remote, level int) int {
	eff := max(level, r.wireCap)
	if m.lowHeight() == 0 {
		if r.wireCap >= levelOff {
			return layerOff
		}
		return layerHigh
	}
	return levelLayer(eff, r.effectiveMid())
}

// noteSending tells r what we send it of our own video, if that changed
// (polMu held).
func (m *Mesh) noteSending(r *remote) {
	top := m.rend.loadCfg().top
	var msg sendingMsg
	if top > 0 {
		link := int(r.level.Load())
		target := m.targetLayerFor(r, link)
		lh := m.lowHeight()
		msg = sendingMsg{
			Height:    layerHeightFloor(target, top, lh),
			Top:       top,
			Congested: lh > 0 && link > max(r.policy.maxLevel, r.wireCap),
		}
	}
	if msg == r.sent {
		return
	}
	r.sent = msg
	if m.sig != nil {
		go m.sig.Signal(r.id, signalMsg{Sending: &msg})
	}
}

// resendSending repeats our last sending to r (a new connection or resume).
func (m *Mesh) resendSending(r *remote) {
	r.polMu.Lock()
	r.sent = sendingMsg{Height: -1}
	m.noteSending(r)
	r.originSent = nil
	r.polMu.Unlock()
}

// resendRelaySending tells child r about every relayed origin currently
// sharing, including unwatched streams sending 0 packets.
func (m *Mesh) resendRelaySending(r *remote) {
	m.mu.Lock()
	if !m.forwardingLocked() || m.isRelayLocked(r.id) || !m.servesLocked(r.id) {
		m.mu.Unlock()
		return
	}
	var origins []*origin
	for _, k := range r.txSlots {
		if oid, src := parseSlotKey(k); oid != "" && src == srcScreen && oid != r.id {
			if o := m.origins[oid]; o != nil && o.heardMsg().Top > 0 {
				origins = append(origins, o)
			}
		}
	}
	m.mu.Unlock()
	for _, o := range origins {
		m.noteRelaySendingOpt(r, o, m.slotWant(r, o), false)
	}
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
	lh := m.lowHeight()
	if lh <= 0 {
		return 0
	}
	h := rendForFloor(m.rend.loadCfg().top, q.MaxHeight, lh)
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
	tNow := time.Now()
	m.rend.own[layer] = tNow
	if m.rend.ownCreated == nil {
		m.rend.ownCreated = map[int]time.Time{}
	}
	if _, ok := m.rend.ownCreated[layer]; !ok {
		m.rend.ownCreated[layer] = tNow
	}
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
	o := m.origins[q.Origin]
	ok := m.forwardingLocked() && o != nil && m.members[q.Origin] && slices.Contains(v.txSlots, q.Origin)
	m.mu.Unlock()
	if !ok || m.opt.NewTranscoder == nil {
		return 0
	}
	top := o.heardMsg().Top
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

// uploadOK (relay) reports whether kbps more fits under our upload limit.
func (m *Mesh) uploadOK(kbps int) bool {
	ul := int(m.uploadKbps.Load())
	if ul <= 0 {
		return true
	}
	return int(m.rend.txKbps.Load())+kbps <= ul*8/10
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
	pkts := ot.pkz[layer].packetize(m.encVideo(au), d)
	m.mu.Lock()
	o := m.origins[origin]
	from := m.providerLocked(origin)
	m.mu.Unlock()
	if o == nil {
		return
	}
	needKey := false
	for _, v := range m.fanTargets(origin, from) {
		if m.isRelay(v.id) {
			continue // trunks carry the origin's own layers
		}
		slot := m.slotFor(v, origin)
		if slot < 0 {
			continue
		}
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
func (m *Mesh) noteRelaySending(v *remote, o *origin, w int) {
	m.noteRelaySendingOpt(v, o, w, true)
}

func (m *Mesh) noteRelaySendingOpt(v *remote, o *origin, w int, async bool) {
	heard := o.heardMsg()
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
	if async {
		go m.sig.Signal(v.id, signalMsg{Sending: &msg})
	} else {
		m.sig.Signal(v.id, signalMsg{Sending: &msg})
	}
}

// gotSending (viewer) records what a provider says it sends us.
func (m *Mesh) gotSending(from string, q sendingMsg) {
	origin := q.Origin
	if origin == "" {
		origin = from
	}
	m.mu.Lock()
	provided := origin != m.me() && m.providerLocked(origin) == from
	o := m.originLocked(origin)
	m.mu.Unlock()
	if !provided {
		return
	}
	o.mu.Lock()
	o.heard = q
	o.mu.Unlock()
	// As relay, pass it on over trunks; and when no packets flow (stopped
	// sharing, or upstream capped at off), tell those we serve directly.
	for _, v := range m.fanTargets(origin, from) {
		if m.isRelay(v.id) {
			msg := q
			msg.Origin = origin
			go m.sig.Signal(v.id, signalMsg{Sending: &msg})
		} else if q.Top == 0 || q.Height == 0 {
			m.noteRelaySending(v, o, m.slotWant(v, o))
		}
	}
	if q.Top > 0 && q.Height == 0 && !q.Congested {
		// Upstream reports our cap is off; if we actually want this stream
		// (e.g. a MaxLayer signal was lost while upstream was resuming),
		// re-assert our wish.
		m.mu.Lock()
		if m.forwardingLocked() {
			dk := demandKey{to: from}
			if from != origin {
				dk.origin = origin
			}
			if dw, ok := m.demand[dk]; ok && dw.wire < wireOff {
				rFrom := m.peers[from]
				delete(m.demand, dk)
				m.mu.Unlock()
				if rFrom != nil {
					rFrom.polMu.Lock()
					delete(rFrom.sentCap, dk.origin)
					delete(rFrom.sentWatchers, dk.origin)
					rFrom.polMu.Unlock()
				}
				go m.pushDemand()
			} else {
				m.mu.Unlock()
			}
		} else if w := levelWire(m.ownWishLocked(origin)); w < wireOff {
			m.mu.Unlock()
			go func() { _ = m.sendCap(origin, w, false) }()
		} else {
			m.mu.Unlock()
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
	if q.Congested && (q.Height == 360 || q.Height == 540 || q.Height == 720) && q.Height < q.Top {
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

// recordTxSample records a measured transmit rate sample and updates peakKbps
// to the maximum over the recent peakWindow (2 minutes).
func (m *Mesh) recordTxSample(now time.Time, kbps int32) {
	m.rend.txMu.Lock()
	defer m.rend.txMu.Unlock()
	m.rend.txKbps.Store(kbps)
	m.rend.peakSamples = append(m.rend.peakSamples, txSample{at: now, kbps: kbps})
	m.prunePeakLocked(now)
}

func (m *Mesh) decayPeak(now time.Time) {
	m.rend.txMu.Lock()
	defer m.rend.txMu.Unlock()
	if len(m.rend.peakSamples) == 0 {
		return
	}
	m.prunePeakLocked(now)
}

func (m *Mesh) prunePeakLocked(now time.Time) {
	keep := m.rend.peakSamples[:0]
	var peak int32
	for _, s := range m.rend.peakSamples {
		if now.Sub(s.at) <= peakWindow {
			keep = append(keep, s)
			if s.kbps > peak {
				peak = s.kbps
			}
		}
	}
	m.rend.peakSamples = keep
	m.rend.peakKbps.Store(peak)
}

// sampleLayerRates updates the smoothed per-layer RTP send rate in kbit/s.
func (m *Mesh) sampleLayerRates(now time.Time) {
	sc := m.rend.loadCfg()
	if sc.top <= 0 {
		return
	}
	m.rend.rateMu.Lock()
	defer m.rend.rateMu.Unlock()
	if m.rend.layerSampleAt.IsZero() {
		m.rend.layerSampleAt = now
		for i := range numLayers {
			m.rend.layerPrevBytes[i] = m.rend.layerBytes[i].Load()
		}
		return
	}
	dt := now.Sub(m.rend.layerSampleAt).Seconds()
	if dt < 0.5 {
		return
	}
	m.rend.layerSampleAt = now
	for i := range numLayers {
		cur := m.rend.layerBytes[i].Load()
		delta := cur - m.rend.layerPrevBytes[i]
		m.rend.layerPrevBytes[i] = cur
		inst := float64(delta) * 8 / 1000 / dt
		switch {
		case m.rend.layerEWMA[i] <= 0 && inst <= 0:
			continue
		case m.rend.layerEWMA[i] <= 0:
			m.rend.layerEWMA[i] = inst
		case inst > m.rend.layerEWMA[i]:
			m.rend.layerEWMA[i] = 0.4*inst + 0.6*m.rend.layerEWMA[i]
		default:
			m.rend.layerEWMA[i] = 0.2*inst + 0.8*m.rend.layerEWMA[i]
		}
		kbps := int32(math.Round(m.rend.layerEWMA[i]))
		if kbps <= 0 && (inst > 0 || m.rend.layerEWMA[i] >= 0.5) {
			kbps = 1
		}
		m.rend.layerKbps[i].Store(kbps)
	}
}

// rendTick runs every second: viewer triggers, upload budget, idle renditions, upload rate.
func (m *Mesh) rendTick() {
	now := time.Now()
	m.mu.Lock()
	relaying := m.forwardingLocked()
	m.mu.Unlock()
	if !relaying { // a relay is a hub, it doesn't ask
		m.rend.viewMu.Lock()
		for origin, vs := range m.rend.view {
			if vs.lowSince.IsZero() || now.Sub(vs.lowSince) < lowTrigger || vs.top <= 540 {
				continue
			}
			if !vs.askedAt.IsZero() && now.Sub(vs.askedAt) < askRetry {
				continue
			}
			below := vs.top
			if vs.granted > 0 && vs.granted < below {
				below = vs.granted // that didn't fit either: one rung lower
			}
			if h := rendForFloor(vs.top, below-1, vs.height); h > 0 {
				m.askResizeLocked(origin, vs, h, false, now)
			}
		}
		m.rend.viewMu.Unlock()
	}
	m.sampleLayerRates(now)
	m.rend.txMu.Lock()
	needTx := now.Sub(m.rend.txAt) >= 5*time.Second
	prevAt, prevBytes := m.rend.txAt, m.rend.txBytes
	m.rend.txMu.Unlock()
	if needTx {
		var tx uint64
		for _, ps := range m.Stats() {
			tx += ps.TxBytes
		}
		m.rend.txMu.Lock()
		m.rend.txAt, m.rend.txBytes = now, tx
		m.rend.txMu.Unlock()
		if !prevAt.IsZero() && tx >= prevBytes {
			kbps := int32(float64(tx-prevBytes) * 8 / 1000 / now.Sub(prevAt).Seconds())
			m.recordTxSample(now, kbps)
		} else {
			m.decayPeak(now)
		}
	} else {
		m.decayPeak(now)
	}
	if !relaying {
		m.rebalanceUpload(now)
	}
	if m.rend.mu.TryLock() { // a grant in progress: next tick
		m.dropIdleLocked(now)
		m.rend.mu.Unlock()
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
			cur, want := r.fwd.layers()
			r.polMu.Lock()
			upHold := r.uploadMid == layer
			r.polMu.Unlock()
			if cur == layer || want == layer || upHold {
				used = true
			}
		}
		created := m.rend.ownCreated[layer]
		if !created.IsZero() && now.Before(created) {
			created = now
			m.rend.ownCreated[layer] = now
		}
		if !last.IsZero() && now.Before(last) {
			last = now
			m.rend.own[layer] = now
		}
		switch {
		case used:
			m.rend.own[layer] = now
		case now.Sub(last) > rendIdle && (created.IsZero() || now.Sub(created) > rendIdle):
			delete(m.rend.own, layer)
			delete(m.rend.ownCreated, layer)
			dropped = append(dropped, layer)
		}
	}
	if len(dropped) > 0 {
		m.rend.rateMu.Lock()
		for _, l := range dropped {
			if l >= 0 && l < numLayers {
				m.rend.layerEWMA[l] = 0
				m.rend.layerKbps[l].Store(0)
			}
		}
		m.rend.rateMu.Unlock()
		var heights []int
		for l := range m.rend.own {
			heights = append(heights, layerHeight(l, 0))
		}
		m.opt.Log.Info("dropping idle renditions", "layers", dropped, "keep", heights)
		for _, r := range peers {
			r.polMu.Lock()
			changed := false
			if slices.Contains(dropped, r.uploadMid) {
				r.uploadMid = -1
				if r.uploadCap == levelMid {
					r.uploadCap = levelLow
				}
				changed = true
			}
			if slices.Contains(dropped, r.mid) {
				r.mid, r.midCap = -1, false
				changed = true
				go m.sig.Signal(r.id, signalMsg{ResizeReply: &resizeReplyMsg{}})
			}
			if changed {
				m.applyCap(r)
				m.noteSending(r)
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
		slotOf := map[*remote]int{}
		for _, v := range peers {
			if i := slices.Index(v.txSlots, origin); i >= 0 {
				slotOf[v] = i
			}
		}
		m.mu.Unlock()
		var gone, keep []int
		m.rend.trMu.Lock()
		for layer, last := range ot.layers {
			used := false
			for v, slot := range slotOf {
				if v.id != origin && v.slotV[slot].current() == layer {
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
