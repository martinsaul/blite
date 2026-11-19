package rtc

import (
	"cmp"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	autoProbeWindow = 60 * time.Second
	uploadHold      = 10 * time.Second
	audioPeerKbps   = 48
)

// UploadConfig describes our outgoing video layers and budget for mesh upload
// allocation.
type UploadConfig struct {
	BudgetKbps       int         // total upload budget across mesh peers (<= 0 means unlimited)
	TopHeight        int         // HIGH height (2160, 1080, 720); <= 0 means not sharing
	HighKbps         int         // nominal HIGH bitrate in kbit/s
	MeasuredHighKbps int         // smoothed measured HIGH send rate in kbit/s (0 = use HighKbps)
	MultiHighKbps    int         // per-peer HIGH rate when >= 2 viewers share HIGH (0 = same as single)
	LowHeight        int         // floor height (360, 540, 720); 0 if SmallerCopy is off or MinHeight >= TopHeight
	LowKbps          int         // nominal LOW layer bitrate in kbit/s
	MeasuredLowKbps  int         // smoothed measured LOW send rate in kbit/s (0 = use LowKbps)
	MeasuredRendKbps map[int]int // smoothed measured rendition send rate by height
	SmallerCopy      bool        // whether lower layers / renditions exist below HIGH
	CanEncode        bool        // whether a new rendition encoder may be started
	ActiveRends      []int       // rendition heights already running
	AudioKbps        int         // per-peer audio bitrate in kbit/s when sharing audio
	SingleViewerHigh bool        // manual limit with 1 viewer: keep HIGH when budget >= LowKbps
	RankOrder        []string    // optional committed priority order (lowest priority first)
}

// PeerDemand describes one direct mesh viewer for upload allocation.
type PeerDemand struct {
	ID         string
	WireCap    int           // levelHigh (auto), levelLow (viewer chose low), levelOff (not watching)
	LinkLvl    int           // current congestion level from layerPolicy (levelHigh..levelOff)
	MidCap     int           // viewer-requested MaxHeight cap from DecodeLag (0 = none)
	Loss       float64       // recent packet loss fraction (0..1)
	RTT        time.Duration // round-trip time
	QueueDelay time.Duration // RTT rise above baseline
	Owner      bool          // room owner
	JoinedAt   time.Time     // join timestamp (older = higher priority on tie)
}

// PeerAlloc is the upload-budget cap assigned to one mesh peer.
type PeerAlloc struct {
	CapLevel   int // levelHigh (uncapped by upload), levelMid, or levelLow
	RendHeight int // rendition height when CapLevel == levelMid
}

type tierOption struct {
	level  int // levelHigh, levelMid, levelLow
	height int
	kbps   int
}

func lowBitrateFor(lowH, worstLevel int) int {
	idx := min(max(worstLevel-levelLow, 0), 2)
	switch {
	case lowH >= 720:
		return [...]int{1800, 1200, 800}[idx]
	case lowH >= 540:
		return [...]int{1000, 650, 400}[idx]
	default:
		return [...]int{500, 300, 150}[idx]
	}
}

func rendBitrateFor(height, highKbps, lowKbps int) int {
	base := rendKbps[rendLayer(height)]
	if base <= 0 {
		base = 1200
	}
	if highKbps > 0 {
		base = min(base, max(lowKbps+100, highKbps*85/100))
	}
	return max(base, lowKbps)
}

// compareDemandPriority orders lowest priority first (demote first) to highest
// priority last (protect last).
func compareDemandPriority(a, b PeerDemand) int {
	if c := cmp.Compare(b.LinkLvl, a.LinkLvl); c != 0 {
		return c
	}
	if c := cmp.Compare(int(b.Loss*50), int(a.Loss*50)); c != 0 {
		return c
	}
	if a.Owner != b.Owner {
		if !a.Owner {
			return -1
		}
		return 1
	}
	if c := cmp.Compare(b.RTT/(25*time.Millisecond), a.RTT/(25*time.Millisecond)); c != 0 {
		return c
	}
	if !a.JoinedAt.Equal(b.JoinedAt) {
		if a.JoinedAt.After(b.JoinedAt) {
			return -1
		}
		return 1
	}
	return cmp.Compare(b.ID, a.ID)
}

func effectiveUploadRates(cfg UploadConfig) (nomLow, nomHigh, highRate, multiHighRate, lowRate int, rendRateFor func(int) int) {
	nomLow = cfg.LowKbps
	if nomLow <= 0 {
		nomLow = lowBitrateFor(cfg.LowHeight, levelLow)
	}
	nomHigh = max(cfg.HighKbps, nomLow)

	highRate = nomHigh
	if cfg.MeasuredHighKbps > 0 {
		highRate = cfg.MeasuredHighKbps
	}
	multiHighRate = highRate
	if cfg.MultiHighKbps > multiHighRate {
		multiHighRate = cfg.MultiHighKbps
	}
	lowRate = nomLow
	if cfg.MeasuredLowKbps > 0 {
		lowRate = min(cfg.MeasuredLowKbps, highRate)
	} else if cfg.MeasuredHighKbps > 0 && nomHigh > 0 {
		lowRate = max(1, min(nomLow, nomLow*highRate/nomHigh))
	}

	rendRateFor = func(h int) int {
		nom := rendBitrateFor(h, nomHigh, nomLow)
		if m := cfg.MeasuredRendKbps[h]; m > 0 {
			return min(max(m, lowRate), highRate)
		}
		if cfg.MeasuredHighKbps > 0 && nomHigh > 0 {
			return min(max(nom*highRate/nomHigh, lowRate), highRate)
		}
		return nom
	}
	return
}

// AllocateUpload distributes cfg.BudgetKbps across direct mesh viewers,
// keeping the highest-priority viewers on HIGH and moving the lowest-priority
// viewers to renditions / LOW first (VDO.Ninja &limittotalbitrate).
// It returns each peer's upload cap and the sorted rendition heights required.
func AllocateUpload(cfg UploadConfig, peers []PeerDemand) (map[string]PeerAlloc, []int) {
	out := make(map[string]PeerAlloc, len(peers))
	for _, p := range peers {
		out[p.ID] = PeerAlloc{CapLevel: levelHigh}
	}
	if cfg.BudgetKbps <= 0 || cfg.TopHeight <= 0 || !cfg.SmallerCopy || cfg.LowHeight <= 0 || cfg.LowHeight >= cfg.TopHeight || len(peers) == 0 {
		return out, nil
	}

	nomLow, _, highRate, multiHighRate, lowRate, rendRateFor := effectiveUploadRates(cfg)

	type watcher struct {
		p     PeerDemand
		tiers []tierOption
		cur   int // index into tiers (0 = best unconstrained tier)
	}

	var list []*watcher
	for _, p := range peers {
		if p.WireCap >= levelOff {
			continue // viewer turned video off: 0 video kbit/s
		}
		var tiers []tierOption
		if p.WireCap == levelHigh {
			if p.MidCap == 0 || cfg.TopHeight <= p.MidCap {
				tiers = append(tiers, tierOption{level: levelHigh, height: cfg.TopHeight, kbps: highRate})
			}
			for _, h := range rendHeights {
				if cfg.LowHeight < h && h < cfg.TopHeight && (p.MidCap == 0 || h <= p.MidCap) {
					if slices.Contains(cfg.ActiveRends, h) || cfg.CanEncode {
						tiers = append(tiers, tierOption{
							level:  levelMid,
							height: h,
							kbps:   rendRateFor(h),
						})
					}
				}
			}
		}
		tiers = append(tiers, tierOption{level: levelLow, height: cfg.LowHeight, kbps: lowRate})
		list = append(list, &watcher{p: p, tiers: tiers, cur: 0})
	}
	if len(list) == 0 {
		return out, nil
	}

	videoBudget := max(0, cfg.BudgetKbps-len(peers)*max(0, cfg.AudioKbps))
	if len(list) == 1 && cfg.SingleViewerHigh && videoBudget >= nomLow && list[0].tiers[0].level == levelHigh {
		return out, nil
	}

	rankIdx := make(map[string]int, len(cfg.RankOrder))
	for i, id := range cfg.RankOrder {
		rankIdx[id] = i
	}
	slices.SortFunc(list, func(a, b *watcher) int {
		ia, oka := rankIdx[a.p.ID]
		ib, okb := rankIdx[b.p.ID]
		if oka && okb && ia != ib {
			return cmp.Compare(ia, ib)
		}
		return compareDemandPriority(a.p, b.p)
	})

	totalCost := func() int {
		sum, nHigh := 0, 0
		for _, w := range list {
			t := w.tiers[w.cur]
			if t.level == levelHigh {
				nHigh++
			} else {
				sum += t.kbps
			}
		}
		if nHigh >= 2 {
			sum += nHigh * multiHighRate
		} else if nHigh == 1 {
			sum += highRate
		}
		return sum
	}

	if totalCost() <= videoBudget {
		return out, nil
	}

	// Pass 1: demote lowest-priority viewers first through renditions to LOW,
	// allowing at most one newly spawned rendition encoder beyond ActiveRends.
	newRend := 0
	tierAllowed := func(t tierOption) bool {
		if t.level != levelMid {
			return true
		}
		if slices.Contains(cfg.ActiveRends, t.height) {
			return true
		}
		return cfg.CanEncode && (newRend == 0 || newRend == t.height)
	}

	for _, w := range list {
		if totalCost() <= videoBudget {
			break
		}
		for totalCost() > videoBudget && w.cur+1 < len(w.tiers) {
			next := w.cur + 1
			for next < len(w.tiers) && !tierAllowed(w.tiers[next]) {
				next++
			}
			if next >= len(w.tiers) {
				break
			}
			w.cur = next
		}
		if t := w.tiers[w.cur]; t.level == levelMid && !slices.Contains(cfg.ActiveRends, t.height) {
			newRend = t.height
		}
	}

	// Pass 2: reclaim any leftover budget for higher-priority demoted viewers
	// using HIGH or renditions already active/selected in Pass 1.
	usedRends := slices.Clone(cfg.ActiveRends)
	if newRend > 0 && !slices.Contains(usedRends, newRend) {
		usedRends = append(usedRends, newRend)
	}
	for i := len(list) - 1; i >= 0; i-- {
		w := list[i]
		prev := w.cur
		for cand := 0; cand < prev; cand++ {
			t := w.tiers[cand]
			if t.level == levelMid && !slices.Contains(usedRends, t.height) {
				continue
			}
			w.cur = cand
			if totalCost() <= videoBudget {
				break
			}
			w.cur = prev
		}
	}

	var rendSet []int
	for _, w := range list {
		if w.cur == 0 {
			out[w.p.ID] = PeerAlloc{CapLevel: levelHigh}
			continue
		}
		t := w.tiers[w.cur]
		alloc := PeerAlloc{CapLevel: t.level}
		if t.level == levelMid {
			alloc.RendHeight = t.height
			if !slices.Contains(rendSet, t.height) {
				rendSet = append(rendSet, t.height)
			}
		}
		out[w.p.ID] = alloc
	}
	slices.SortFunc(rendSet, func(a, b int) int { return cmp.Compare(b, a) })
	return out, rendSet
}

// SetUploadLimit sets the total upload limit in kbit/s (0 = Automatic, using
// the passive peakKbps estimate when congestion occurs across most viewers).
func (m *Mesh) SetUploadLimit(kbps int) {
	if kbps < 0 {
		kbps = 0
	}
	m.rebalMu.Lock()
	defer m.rebalMu.Unlock()
	prev := int(m.uploadKbps.Swap(int32(kbps)))
	if prev == kbps {
		return
	}
	if kbps > 0 {
		m.rend.resetAutoCap()
		if pc := m.pacer.Load(); pc != nil {
			pc.setRate(kbps)
		} else {
			np := newPacer(kbps, m.done)
			if !m.pacer.CompareAndSwap(nil, np) {
				m.pacer.Load().setRate(kbps)
			}
		}
	} else {
		if pc := m.pacer.Load(); pc != nil {
			pc.setRate(0)
		}
		if prev != 0 {
			m.rend.resetAutoCap()
		}
	}
	m.rebalanceUploadLocked(time.Now(), true)
}

// SetShareConfig updates the mesh's outgoing video parameters and slow-viewer
// policy atomically so concurrent ticks never observe a half-applied config.
func (m *Mesh) SetShareConfig(top, fps, highKbps, minHeight int, smallerCopy, fpsSteps bool) {
	if fps <= 0 {
		fps = 30
	}
	if minHeight <= 0 {
		minHeight = 360
	}
	m.rebalMu.Lock()
	defer m.rebalMu.Unlock()

	m.mu.Lock()
	prev, _ := m.rend.updateCfg(func(c *shareCfg) {
		c.top = top
		c.fps = fps
		if highKbps > 0 {
			c.highKbps = highKbps
		}
		c.minHeight = minHeight
		c.smallerCopy = smallerCopy
		c.fpsSteps = fpsSteps
	})
	if top <= 0 && prev.top > 0 {
		clear(m.watchingPeer)
		clear(m.rehomingWatch)
	}
	m.mu.Unlock()

	if !fpsSteps {
		for _, r := range m.peerList() {
			r.fpsCap.Store(0)
		}
		m.updateHighFPS()
	}
	if top <= 0 && prev.top > 0 {
		m.rend.resetAutoCap()
		m.rend.resetLayerRates(false)
	}
	if prev.top != top || prev.minHeight != minHeight || prev.smallerCopy != smallerCopy {
		m.ResetRenditions()
	}
	if prev.top <= 0 && top > 0 {
		m.checkAllWatching()
	}
	if lh := m.lowHeight(); lh > 0 {
		m.updateLowBitrate()
	}
	m.rebalanceUploadLocked(time.Now(), true)
}

// SetSharingAudio records whether we are sending audio so mesh upload
// allocation reserves audio headroom per peer.
func (m *Mesh) SetSharingAudio(on bool) {
	m.rebalMu.Lock()
	defer m.rebalMu.Unlock()
	if m.rend.sharingAudio.Swap(on) != on {
		m.rebalanceUploadLocked(time.Now(), true)
	}
}

// UploadStats returns our configured upload limit, active automatic budget (if
// any), passive peak upload rate, and current transmit rate, all in kbit/s.
func (m *Mesh) UploadStats() (limitKbps, autoBudgetKbps, peakKbps, txKbps int) {
	limitKbps = int(m.uploadKbps.Load())
	peakKbps = int(m.rend.peakKbps.Load())
	txKbps = int(m.rend.txKbps.Load())
	m.rend.autoMu.Lock()
	active := m.rend.autoCapActive.Load()
	headroom := int(m.rend.autoHeadroom.Load())
	m.rend.autoMu.Unlock()
	if limitKbps == 0 && active && peakKbps > 0 {
		autoBudgetKbps = peakKbps * (100 + headroom) / 100
	}
	return
}

// autoUploadBudget computes the Automatic mesh upload budget from passive
// peakKbps when most watching viewers (> 50%, at least 2) simultaneously show
// packet loss or rising round-trip time.
func (m *Mesh) autoUploadBudget(now time.Time, peers []PeerDemand) int {
	m.decayPeak(now)
	m.rend.autoMu.Lock()
	defer m.rend.autoMu.Unlock()

	peak := int(m.rend.peakKbps.Load())
	if peak <= 0 || len(peers) == 0 {
		return 0
	}
	watching := 0
	bad := 0
	unclean := 0
	for _, p := range peers {
		if p.WireCap >= levelOff {
			continue
		}
		watching++
		if p.Loss >= lossDown || p.QueueDelay >= delayDown {
			bad++
			unclean++
		} else if p.Loss >= lossUp || p.QueueDelay >= delayUp {
			unclean++
		}
	}
	if watching < 2 {
		return 0
	}
	if bad >= 2 && bad*2 > watching {
		m.rend.autoCapActive.Store(true)
		m.rend.autoCleanAt.Store(0)
		m.rend.autoHeadroom.Store(0)
		return peak
	}
	if !m.rend.autoCapActive.Load() {
		return 0
	}
	if unclean >= 2 && unclean*2 > watching {
		m.rend.autoCleanAt.Store(0)
	} else {
		since := m.rend.autoCleanAt.Load()
		if since == 0 {
			m.rend.autoCleanAt.Store(now.UnixNano())
		} else if now.Sub(time.Unix(0, since)) >= autoProbeWindow {
			m.rend.autoCleanAt.Store(now.UnixNano())
			next := m.rend.autoHeadroom.Add(20)
			if next >= 100 {
				m.rend.autoCapActive.Store(false)
				m.rend.autoHeadroom.Store(0)
				return 0
			}
		}
	}
	return peak * (100 + int(m.rend.autoHeadroom.Load())) / 100
}

// triggerRebalance coalesces asynchronous upload rebalance triggers onto a
// single serialized pass.
func (m *Mesh) triggerRebalance() {
	if !m.rebalPending.CompareAndSwap(false, true) {
		return
	}
	go func() {
		m.rebalMu.Lock()
		defer m.rebalMu.Unlock()
		m.rebalPending.Store(false)
		m.rebalanceUploadLocked(time.Now(), false)
	}()
}

// rebalanceUpload enforces our total mesh upload budget across connected peers,
// serialized so an older snapshot can never overwrite a newer allocation.
func (m *Mesh) rebalanceUpload(now time.Time) {
	m.rebalMu.Lock()
	defer m.rebalMu.Unlock()
	m.rebalanceUploadLocked(now, false)
}

func (rd *renditions) clampSimClockLocked(now time.Time) {
	if now.IsZero() {
		return
	}
	for _, p := range []*time.Time{&rd.rankStableAt, &rd.candOrderAt, &rd.lastSwapAt, &rd.candAllocsAt, &rd.candMidAt} {
		if !p.IsZero() && now.Before(*p) {
			*p = now
		}
	}
}

func (m *Mesh) updateRankOrderLocked(demands []PeerDemand, now time.Time, immediate, preserveStable bool) (order []string, swapped bool) {
	var watching []PeerDemand
	for _, d := range demands {
		if d.WireCap < levelOff {
			watching = append(watching, d)
		}
	}
	slices.SortFunc(watching, compareDemandPriority)
	raw := make([]string, len(watching))
	present := make(map[string]bool, len(watching))
	for i, d := range watching {
		raw[i] = d.ID
		present[d.ID] = true
	}
	if len(raw) == 0 {
		m.rend.rankOrder = nil
		m.rend.rankStableAt = time.Time{}
		m.rend.candOrder = nil
		m.rend.candOrderAt = time.Time{}
		return nil, false
	}
	if len(m.rend.rankOrder) == 0 {
		m.rend.rankOrder = raw
		m.rend.rankStableAt = now
		m.rend.candOrder = nil
		m.rend.candOrderAt = time.Time{}
		return raw, false
	}

	// Keep existing watching peers in their committed relative order; insert
	// brand-new watching peers at their natural raw position without counting
	// that insertion as a flip of existing peers.
	var kept []string
	keptSet := make(map[string]bool, len(m.rend.rankOrder))
	for _, id := range m.rend.rankOrder {
		if present[id] {
			kept = append(kept, id)
			keptSet[id] = true
		}
	}
	merged := make([]string, 0, len(raw))
	ki := 0
	for _, id := range raw {
		if !keptSet[id] {
			merged = append(merged, id)
		} else if ki < len(kept) {
			merged = append(merged, kept[ki])
			ki++
		}
	}
	if len(merged) > len(kept) && !preserveStable {
		m.rend.rankStableAt = now
	}
	if m.rend.rankStableAt.IsZero() {
		m.rend.rankStableAt = now
	}

	if immediate {
		if !slices.Equal(raw, merged) {
			m.rend.rankOrder = raw
			m.rend.rankStableAt = now
		} else {
			m.rend.rankOrder = merged
		}
		m.rend.candOrder = nil
		m.rend.candOrderAt = time.Time{}
		return m.rend.rankOrder, false
	}

	if slices.Equal(raw, merged) {
		m.rend.rankOrder = merged
		m.rend.candOrder = nil
		m.rend.candOrderAt = time.Time{}
		return merged, false
	}
	if !slices.Equal(raw, m.rend.candOrder) {
		m.rend.candOrder = slices.Clone(raw)
		m.rend.candOrderAt = now
		m.rend.rankOrder = merged
		return merged, false
	}
	heldLongEnough := !now.IsZero() && !m.rend.candOrderAt.IsZero() && now.Sub(m.rend.candOrderAt) >= uploadHold
	cooldownDone := m.rend.lastSwapAt.IsZero() || (!now.IsZero() && now.Sub(m.rend.lastSwapAt) >= uploadHold)
	if heldLongEnough && cooldownDone {
		m.rend.rankOrder = slices.Clone(raw)
		m.rend.rankStableAt = m.rend.candOrderAt
		m.rend.candOrder = nil
		m.rend.candOrderAt = time.Time{}
		return m.rend.rankOrder, true
	}
	m.rend.rankOrder = merged
	return merged, false
}

func (m *Mesh) rebalanceUploadLocked(now time.Time, immediate bool) {
	tNow := now
	if tNow.IsZero() {
		tNow = time.Now()
	}
	m.rend.clampSimClockLocked(now)

	m.mu.Lock()
	relaying := m.forwardingLocked()
	owner := m.owner
	joined := make(map[string]time.Time, len(m.joinedAt))
	for id, t := range m.joinedAt {
		joined[id] = t
	}
	m.mu.Unlock()

	sc := m.rend.loadCfg()
	peers := m.peerList()
	if relaying || sc.top <= 0 {
		m.rend.rankOrder = nil
		m.rend.rankStableAt = time.Time{}
		m.rend.candOrder = nil
		m.rend.candOrderAt = time.Time{}
		m.rend.lastSwapAt = time.Time{}
		m.rend.candAllocs = nil
		m.rend.candAllocsAt = time.Time{}
		m.rend.candMidHeight = 0
		m.rend.candMidAt = time.Time{}
		m.rend.hadBudgetMid.Store(false)
		m.lowBudgetCap.Store(0)
		for _, r := range peers {
			r.polMu.Lock()
			if r.uploadCap != levelHigh || r.uploadMid != -1 {
				prevUp := r.uploadCap
				r.uploadCap, r.uploadMid = levelHigh, -1
				m.applyCap(r)
				if cur := int(r.level.Load()); prevUp > levelHigh && cur == prevUp && cur > r.policy.maxLevel {
					m.setLevel(r, r.policy.maxLevel)
				}
				m.noteSending(r)
			}
			r.polMu.Unlock()
		}
		m.updateEncoderBitratesLocked(sc, nil, 0)
		return
	}

	wasBudgetMid := m.rend.hadBudgetMid.Swap(false)

	var active []*remote
	var demands []PeerDemand
	for _, r := range peers {
		if r.pc != nil {
			switch r.pc.ConnectionState() {
			case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
				continue
			}
		}
		r.polMu.Lock()
		wire := r.wireCap
		midCapH := 0
		otherCap := wire
		if r.midCap && r.mid >= 0 {
			midCapH = layerHeight(r.mid, 0)
			if otherCap < levelMid {
				otherCap = levelMid
			}
		}
		fullCap := max(otherCap, r.uploadCap)
		rtt := r.policy.lastRTT
		if rtt <= 0 {
			rtt = r.policy.rttMin
		}
		qd := r.policy.lastQD
		hasCongestion := r.policy.backoff > 0 && !r.policy.nextProbe.IsZero()
		r.polMu.Unlock()

		lvl := int(r.level.Load())
		linkLvl := levelHigh
		if hasCongestion {
			if lvl > fullCap {
				linkLvl = lvl
			} else if r.fpsCap.Load() > 0 {
				linkLvl = levelMid
			}
		}

		active = append(active, r)
		demands = append(demands, PeerDemand{
			ID:         r.id,
			WireCap:    wire,
			LinkLvl:    linkLvl,
			MidCap:     midCapH,
			Loss:       math.Float64frombits(r.loss.Load()),
			RTT:        rtt,
			QueueDelay: qd,
			Owner:      r.id != "" && r.id == owner,
			JoinedAt:   joined[r.id],
		})
	}

	rankOrder, rankSwapped := m.updateRankOrderLocked(demands, tNow, immediate, wasBudgetMid)

	manualLimit := int(m.uploadKbps.Load())
	budget := manualLimit
	if budget <= 0 {
		budget = m.autoUploadBudget(tNow, demands)
	}

	m.rend.mu.Lock()
	var activeRends []int
	measuredRend := map[int]int{}
	for l := range m.rend.own {
		if h := layerHeight(l, 0); h > 0 {
			activeRends = append(activeRends, h)
			if rk := int(m.rend.layerKbps[l].Load()); rk > 0 {
				measuredRend[h] = rk
			}
		}
	}

	measuredHigh := int(m.rend.layerKbps[layerHigh].Load())
	measuredLow := int(m.rend.layerKbps[layerLow].Load())
	baseFPS := max(1, sc.fps)
	multiHigh := 0
	if capFPS := m.HighFPSCap(); capFPS > 0 && capFPS < baseFPS {
		if measuredHigh > 0 {
			multiHigh = min(sc.highKbps, max(measuredHigh, measuredHigh*baseFPS/capFPS))
		} else {
			multiHigh = sc.highKbps
		}
	}
	audioKbps := 0
	if m.rend.sharingAudio.Load() {
		audioKbps = audioPeerKbps
	}
	lh := 0
	if sc.smallerCopy {
		mh := max(sc.minHeight, 360)
		if mh < sc.top {
			lh = mh
		}
	}

	canStartRend := m.canEncode() && m.opt.OnRenditions != nil
	upCfg := UploadConfig{
		BudgetKbps:       budget,
		TopHeight:        sc.top,
		HighKbps:         sc.highKbps,
		MeasuredHighKbps: measuredHigh,
		MultiHighKbps:    multiHigh,
		LowHeight:        lh,
		LowKbps:          int(m.lowKbps.Load()),
		MeasuredLowKbps:  measuredLow,
		MeasuredRendKbps: measuredRend,
		SmallerCopy:      sc.smallerCopy,
		CanEncode:        canStartRend,
		ActiveRends:      activeRends,
		AudioKbps:        audioKbps,
		SingleViewerHigh: manualLimit > 0,
		RankOrder:        rankOrder,
	}

	idealAllocs, idealRends := AllocateUpload(upCfg, demands)
	newRendHeight := 0
	for _, h := range idealRends {
		if _, ok := m.rend.own[rendLayer(h)]; !ok {
			newRendHeight = h
			break
		}
	}

	var allocs map[string]PeerAlloc
	var wantRends []int
	midCreated := false
	if newRendHeight == 0 {
		m.rend.candMidHeight = 0
		m.rend.candMidAt = time.Time{}
		allocs, wantRends = idealAllocs, idealRends
	} else {
		if m.rend.candMidHeight != newRendHeight || m.rend.candMidAt.IsZero() {
			m.rend.candMidHeight = newRendHeight
			m.rend.candMidAt = tNow
		}
		rankHeld := len(m.rend.candOrder) == 0 && !m.rend.rankStableAt.IsZero() && tNow.Sub(m.rend.rankStableAt) >= uploadHold
		midHeld := !m.rend.candMidAt.IsZero() && tNow.Sub(m.rend.candMidAt) >= uploadHold
		if wasBudgetMid || (rankHeld && midHeld) {
			heights := slices.Clone(activeRends)
			for _, h := range idealRends {
				if !slices.Contains(heights, h) {
					heights = append(heights, h)
				}
			}
			if err := m.opt.OnRenditions(heights); err != nil {
				m.opt.Log.Warn("upload renditions", "heights", heights, "err", err)
				noEnc := upCfg
				noEnc.CanEncode = false
				allocs, wantRends = AllocateUpload(noEnc, demands)
			} else {
				if m.rend.ownCreated == nil {
					m.rend.ownCreated = map[int]time.Time{}
				}
				for _, h := range idealRends {
					if l := rendLayer(h); l >= 0 {
						m.rend.own[l] = tNow
						if _, ok := m.rend.ownCreated[l]; !ok {
							m.rend.ownCreated[l] = tNow
						}
					}
				}
				m.rend.candMidHeight = 0
				m.rend.candMidAt = time.Time{}
				midCreated = true
				allocs, wantRends = idealAllocs, idealRends
			}
		} else {
			noEnc := upCfg
			noEnc.CanEncode = false
			allocs, wantRends = AllocateUpload(noEnc, demands)
		}
	}

	for _, h := range wantRends {
		if l := rendLayer(h); l >= 0 {
			if _, ok := m.rend.own[l]; ok {
				m.rend.own[l] = tNow
			}
		}
	}

	// Build normalized desired allocation map and check whether the current
	// allocation across all peers already matches or exceeds videoBudget.
	_, _, highRate, multiHighRate, lowRate, rendRateFor := effectiveUploadRates(upCfg)
	videoBudget := max(0, budget-len(demands)*audioKbps)

	desired := make(map[string]PeerAlloc, len(active))
	sameAsCurrent := true
	curHighCount := 0
	curOtherCost := 0
	for _, r := range active {
		al, ok := allocs[r.id]
		if !ok {
			al = PeerAlloc{CapLevel: levelHigh}
		}
		upCap, upMid := al.CapLevel, -1
		if upCap == levelMid {
			l := rendLayer(al.RendHeight)
			if _, ok := m.rend.own[l]; ok {
				upMid = l
			} else {
				upCap, al.CapLevel, al.RendHeight = levelLow, levelLow, 0
			}
		}
		desired[r.id] = PeerAlloc{CapLevel: upCap, RendHeight: layerHeight(upMid, 0)}

		r.polMu.Lock()
		curCap, curMid := r.uploadCap, r.uploadMid
		wire := r.wireCap
		r.polMu.Unlock()
		if curCap != upCap || curMid != upMid {
			sameAsCurrent = false
		}
		if wire < levelOff && lh > 0 {
			switch curCap {
			case levelHigh:
				curHighCount++
			case levelMid:
				if h := layerHeight(curMid, 0); h > 0 {
					curOtherCost += rendRateFor(h)
				} else {
					curOtherCost += lowRate
				}
			default:
				curOtherCost += lowRate
			}
		}
	}
	curCost := curOtherCost
	if curHighCount >= 2 {
		curCost += curHighCount * multiHighRate
	} else if curHighCount == 1 {
		curCost += highRate
	}

	if sameAsCurrent {
		m.rend.candAllocs = nil
		m.rend.candAllocsAt = time.Time{}
	} else {
		overBudget := budget > 0 && lh > 0 && curCost > videoBudget
		cooldownDone := m.rend.lastSwapAt.IsZero() || tNow.Sub(m.rend.lastSwapAt) >= uploadHold
		applyNow := false
		switch {
		case immediate || wasBudgetMid || midCreated || rankSwapped:
			applyNow = true
		case overBudget && cooldownDone:
			applyNow = true
		default:
			if !maps.Equal(desired, m.rend.candAllocs) {
				m.rend.candAllocs = maps.Clone(desired)
				m.rend.candAllocsAt = tNow
			} else if cooldownDone && (budget == 0 || (!m.rend.candAllocsAt.IsZero() && tNow.Sub(m.rend.candAllocsAt) >= uploadHold)) {
				applyNow = true
			}
		}
		if applyNow {
			m.rend.candAllocs = nil
			m.rend.candAllocsAt = time.Time{}
			if !immediate && !wasBudgetMid {
				m.rend.lastSwapAt = tNow
			}
			for _, r := range active {
				al := desired[r.id]
				upCap, upMid := al.CapLevel, -1
				if upCap == levelMid {
					upMid = rendLayer(al.RendHeight)
				}
				r.polMu.Lock()
				if r.uploadCap != upCap || r.uploadMid != upMid {
					prevUp := r.uploadCap
					r.uploadCap, r.uploadMid = upCap, upMid
					m.applyCap(r)
					if cur := int(r.level.Load()); prevUp > upCap && cur == prevUp && cur > r.policy.maxLevel {
						m.setLevel(r, r.policy.maxLevel)
					}
					m.noteSending(r)
				}
				r.polMu.Unlock()
			}
		}
	}
	m.rend.mu.Unlock()

	m.updateEncoderBitratesLocked(sc, active, manualLimit)
}

// updateEncoderBitratesLocked adjusts local encoder bitrates to fit manualLimit
// on a sharer (without packet queueing), and restores nominal bitrates when
// manualLimit is 0 (rebalMu held).
func (m *Mesh) updateEncoderBitratesLocked(sc shareCfg, active []*remote, manualLimit int) {
	if sc.top <= 0 {
		m.lowBudgetCap.Store(0)
		return
	}
	lh := m.lowHeight()
	worstLow := levelLow
	for _, r := range active {
		if l := int(r.level.Load()); l > worstLow && l < levelOff {
			worstLow = l
		}
	}
	baseLow := 0
	if lh > 0 {
		baseLow = lowBitrateFor(lh, worstLow)
	}

	m.rend.mu.Lock()
	var rendLayers []int
	for l := range m.rend.own {
		rendLayers = append(rendLayers, l)
	}
	m.rend.mu.Unlock()

	var target [numLayers]int
	target[layerHigh] = sc.highKbps
	target[layerLow] = baseLow
	for _, l := range rendLayers {
		target[l] = rendBitrateFor(layerHeight(l, 0), sc.highKbps, baseLow)
	}

	if manualLimit <= 0 {
		m.lowBudgetCap.Store(0)
	} else {
		audioTotal := 0
		if m.rend.sharingAudio.Load() {
			audioTotal = len(active) * audioPeerKbps
		}
		videoBudget := max(150, manualLimit-audioTotal)
		nHigh, nLow, nWatching := 0, 0, 0
		var nMid [numLayers]int
		for _, r := range active {
			r.polMu.Lock()
			wire := r.wireCap
			lay := m.targetLayerFor(r, int(r.level.Load()))
			r.polMu.Unlock()
			if wire >= levelOff || lay >= layerOff {
				continue
			}
			nWatching++
			switch lay {
			case layerHigh:
				nHigh++
			case layerLow:
				nLow++
			default:
				if lay >= 0 && lay < numLayers {
					nMid[lay]++
				}
			}
		}
		if nWatching == 0 {
			target[layerHigh] = min(sc.highKbps, max(300, videoBudget))
			if baseLow > 0 {
				target[layerLow] = min(baseLow, max(150, videoBudget))
			}
			for _, l := range rendLayers {
				target[l] = min(target[l], max(250, videoBudget))
			}
		} else {
			nomSum := nHigh*sc.highKbps + nLow*baseLow
			for _, l := range rendLayers {
				nomSum += nMid[l] * target[l]
			}
			if nomSum > videoBudget && nomSum > 0 {
				if nHigh > 0 {
					target[layerHigh] = max(300, sc.highKbps*videoBudget/nomSum)
				} else {
					target[layerHigh] = min(sc.highKbps, max(300, videoBudget))
				}
				if baseLow > 0 {
					if nLow > 0 {
						target[layerLow] = max(150, baseLow*videoBudget/nomSum)
					} else {
						target[layerLow] = min(baseLow, max(150, videoBudget))
					}
				}
				for _, l := range rendLayers {
					if nMid[l] > 0 {
						target[l] = max(250, target[l]*videoBudget/nomSum)
					} else {
						target[l] = min(target[l], max(250, videoBudget))
					}
				}
			} else if nHigh == 0 {
				target[layerHigh] = min(sc.highKbps, max(300, videoBudget))
			}
		}
		if baseLow > 0 && target[layerLow] < baseLow {
			m.lowBudgetCap.Store(int32(target[layerLow]))
		} else {
			m.lowBudgetCap.Store(0)
		}
	}

	for l, kbps := range target {
		if kbps <= 0 {
			continue
		}
		prev := int(m.layerTargetKbps[l].Swap(int32(kbps)))
		if l == layerLow {
			if int(m.lowKbps.Swap(int32(kbps))) != kbps {
				m.opt.Log.Info("low bitrate", "kbps", kbps)
				if m.opt.OnLowBitrate != nil {
					m.opt.OnLowBitrate(kbps)
				}
			}
		}
		if prev != kbps && m.opt.OnBitrate != nil {
			m.opt.OnBitrate(l, kbps)
		}
	}
}
