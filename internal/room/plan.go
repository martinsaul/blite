package room

import (
	"maps"
	"slices"

	"blite/internal/proto"
)

// Relay planning. The server turns the volunteer queue and the members'
// Caps into a proto.Plan: which volunteers relay, and which relay each
// member is homed on. Builds below proto.MultiRelayBuild only understand one
// relay, so while one is in the room the plan is the legacy one: the queue's
// head relays for everyone, paused above proto.RelayMaxPeers.
//
// Plans are sticky: relays stay relays until they withdraw or leave, and
// members keep their home unless it is over capacity or gone.

const (
	// childKbps is what one child costs a relay's upload: about one HIGH
	// stream plus thumbnails.
	childKbps = 4500
	// defaultChildren is a relay's capacity when it reported no upload.
	defaultChildren = proto.RelayMaxPeers - 1
	minChildren     = 2
	maxChildren     = 16
)

// capacity is how many members peer p can serve as a relay. A passive
// upload estimate (the most it has sent so far) is only a lower bound, so
// it never counts for less than an unknown upload.
func (p *Peer) capacity() int {
	if p.caps.UpKbps <= 0 {
		return defaultChildren
	}
	n := min(max(p.caps.UpKbps*7/10/childKbps, minChildren), maxChildren)
	if p.caps.UpSource == "passive" {
		n = max(n, defaultChildren)
	}
	return n
}

// legacy reports whether a member's build only understands one relay.
func (r *Room) legacy() bool {
	for _, p := range r.peers {
		if p.Build < proto.MultiRelayBuild {
			return true
		}
	}
	return false
}

// computePlan is the plan for r now (Epoch not set).
func (r *Room) computePlan() proto.Plan {
	p := proto.Plan{T: proto.TPlan, Queue: slices.Clone(r.relayQ)}
	if len(r.relayQ) == 0 {
		return p
	}
	if r.legacy() {
		if len(r.peers) > proto.RelayMaxPeers {
			p.Paused = true
			return p
		}
		p.Relays = []string{r.relayQ[0]}
	} else {
		p.Relays = r.pickRelays()
	}
	if len(p.Relays) > 0 {
		p.Home = r.assignHomes(p.Relays)
	}
	return p
}

// pickRelays chooses the relays among the volunteers: the current ones
// first (sticky), then by capacity, as many as the room needs, at most
// proto.MaxRelays. Volunteers only reachable via TURN never relay.
func (r *Room) pickRelays() []string {
	var cands []string
	for _, id := range r.relayQ {
		if p := r.peers[id]; p != nil && p.caps.Reach != "turn" {
			cands = append(cands, id)
		}
	}
	was := map[string]bool{}
	for _, id := range r.plan.Relays {
		was[id] = true
	}
	slices.SortStableFunc(cands, func(a, b string) int {
		if was[a] != was[b] {
			if was[a] {
				return -1
			}
			return 1
		}
		return r.peers[b].capacity() - r.peers[a].capacity()
	})
	var chosen []string
	total := 0
	for _, id := range cands {
		if len(chosen) == proto.MaxRelays {
			break
		}
		if len(chosen) > 0 && !was[id] && total >= len(r.peers)-len(chosen) {
			break
		}
		chosen = append(chosen, id)
		total += r.peers[id].capacity()
	}
	return chosen
}

// assignHomes homes every member on a relay: relays on themselves; others
// on their current home while it has room, else on the relay with the most
// room left.
func (r *Room) assignHomes(relays []string) map[string]string {
	home := make(map[string]string, len(r.peers))
	load := map[string]int{}
	for _, id := range relays {
		home[id] = id
	}
	for _, id := range r.order {
		if _, done := home[id]; done {
			continue
		}
		if h := r.plan.Home[id]; slices.Contains(relays, h) && load[h] < r.peers[h].capacity() {
			home[id] = h
			load[h]++
		}
	}
	for _, id := range r.order {
		if _, done := home[id]; done {
			continue
		}
		best := relays[0]
		for _, rel := range relays[1:] {
			if r.peers[rel].capacity()-load[rel] > r.peers[best].capacity()-load[best] {
				best = rel
			}
		}
		home[id] = best
		load[best]++
	}
	return home
}

func samePlan(a, b proto.Plan) bool {
	return a.Paused == b.Paused && slices.Equal(a.Relays, b.Relays) &&
		slices.Equal(a.Queue, b.Queue) && maps.Equal(a.Home, b.Home)
}

// announceLocked tells members what changed about relaying: builds below
// proto.MultiRelayBuild the Topology (when the queue changed or relay mode
// paused or resumed), newer ones the Plan (when it changed). except (a
// member who just joined) gets both in its Welcome instead.
func (m *Manager) announceLocked(r *Room, except string, queueChanged bool) {
	if _, paused, _ := r.topology(); queueChanged || paused != r.paused {
		r.paused = paused
		m.broadcastBuildLocked(r, except, false, r.topologyMsg())
	}
	if p := r.computePlan(); !samePlan(p, r.plan) {
		p.Epoch = r.plan.Epoch + 1
		r.plan = p
		m.broadcastBuildLocked(r, except, true, p)
	}
}

// broadcastBuildLocked sends msg to the members (but except) whose build
// understands plans (newer) or doesn't.
func (m *Manager) broadcastBuildLocked(r *Room, except string, newer bool, msg any) {
	for _, id := range r.order {
		if p := r.peers[id]; id != except && p.out != nil && (p.Build >= proto.MultiRelayBuild) == newer {
			p.out.Send(msg)
		}
	}
}

// planFor is the plan peer p gets in its Welcome (nil for older builds).
func (r *Room) planFor(p *Peer) *proto.Plan {
	if p.Build < proto.MultiRelayBuild {
		return nil
	}
	plan := r.plan
	plan.T = proto.TPlan
	return &plan
}

// validCaps reports whether c is plausible.
func validCaps(c proto.Caps) bool {
	switch c.Reach {
	case "", "public", "nat", "turn":
	default:
		return false
	}
	switch c.UpSource {
	case "", "declared", "passive":
	default:
		return false
	}
	return c.UpKbps >= 0 && c.UpKbps <= 10_000_000 && c.CPUFree >= -1 && c.CPUFree <= 100 && c.Uptime >= 0
}

// SetCaps records peerID's Caps and replans.
func (m *Manager) SetCaps(code, peerID string, c proto.Caps) error {
	if !validCaps(c) {
		return ErrBadRequest
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rooms[code]
	if r == nil || r.peers[peerID] == nil {
		return ErrNoRoom
	}
	c.T = ""
	r.peers[peerID].caps = c
	m.announceLocked(r, "", false)
	return nil
}
