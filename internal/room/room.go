// Package room holds Blite's in-memory rooms. A room exists from create until
// its last peer has been gone (left, or its resume window expired) for
// EmptyGrace; then it is deleted. Until then the link still works.
package room

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"blite/internal/proto"
)

var (
	ErrNoRoom       = errors.New(proto.ErrNoRoom)
	ErrBadPassword  = errors.New(proto.ErrBadPassword)
	ErrRoomFull     = errors.New(proto.ErrRoomFull)
	ErrBadRequest   = errors.New(proto.ErrBadRequest)
	ErrResumeFailed = errors.New(proto.ErrResumeFail)
	// ErrUpdateRequired: a relay is active and the joiner's build can't use it.
	ErrUpdateRequired   = errors.New(proto.ErrUpdateRequired)
	ErrRelayUnsupported = errors.New(proto.ErrRelayUnsupported)
	ErrNotOwner         = errors.New(proto.ErrNotOwner)
	ErrBanned           = errors.New(proto.ErrBanned)
	ErrNoPeer           = errors.New(proto.ErrNoPeer)
	ErrChannelBusy      = errors.New(proto.ErrChannelBusy)
)

// Outbox receives messages for one peer. The server implements it per connection.
// Send must not block; it reports false if the peer's buffer is full.
type Outbox interface {
	Send(msg any) bool
}

// closer is an Outbox that can be closed once its queued messages are out
// (a kicked member).
type closer interface {
	CloseSoon()
}

// Peer is one member's slot in a room.
type Peer struct {
	ID    string
	Name  string
	IDKey string
	Ver   string // client build label
	Build int    // client build number (0 = unknown)
	caps  proto.Caps

	resume string
	out    Outbox    // nil while disconnected
	lostAt time.Time // when out became nil
}

// Room is a set of peers sharing a password (at most Manager.MaxPeers).
type Room struct {
	Code  string
	pw    passwordHash
	peers map[string]*Peer
	order []string // join order, so peer lists are stable
	// relayQ is the relay queue in volunteer order; its head is the active
	// relay, and an empty queue means full mesh.
	relayQ []string
	paused bool       // older members were last told relay mode is paused (room too big)
	plan   proto.Plan // the relay plan newer members were last told (plan.go)
	e2ee   bool       // media is encrypted end to end: builds below proto.E2EEBuild may not join
	// channel: the Discord voice channel this room is tied to ("" none).
	channel string

	// Ownership. The owner is an online member, or "" while nobody is,
	// except while held (hold). An owner who leaves hands over at once; one
	// whose connection drops or who restarts into an update keeps the room
	// for OwnerReturnWindow. Handing over goes to the earliest-joined
	// online member of allowed if any, else to the host (a hosted relay,
	// owner of the room it created), else to the earliest-joined online
	// member.
	owner   string
	host    string          // hosted relay's peer ID ("" = an ordinary room)
	allowed map[string]bool // identity keys that take ownership from the host
	banned  map[string]bool // identity keys the owner banned
	// hold: the owner dropped or left to update. Until hold.until nobody
	// else gets the room; owner keeps the away peer's ID meanwhile.
	hold *ownerHold

	emptyAt time.Time // when the last peer left; zero while occupied
}

func (r *Room) online(id string) bool {
	p := r.peers[id]
	return p != nil && p.out != nil
}

// wantOwner is who should own r now (see Room.owner).
func (r *Room) wantOwner() string {
	firstAllowed := ""
	for _, id := range r.order {
		if id != r.host && r.online(id) && r.allowed[r.peers[id].IDKey] {
			firstAllowed = id
			break
		}
	}
	if r.online(r.owner) {
		if r.owner == r.host && firstAllowed != "" {
			return firstAllowed
		}
		return r.owner
	}
	if firstAllowed != "" {
		return firstAllowed
	}
	if r.online(r.host) {
		return r.host
	}
	for _, id := range r.order {
		if r.online(id) {
			return id
		}
	}
	return ""
}

// electLocked updates the owner and announces a change to everyone but
// except (a member whose Welcome, sent after this, carries it).
func (m *Manager) electLocked(r *Room, except string) {
	if m.holding(r) {
		return
	}
	if o := r.wantOwner(); o != r.owner {
		r.owner = o
		m.broadcastLocked(r, except, proto.Owner{T: proto.TOwner, PeerID: o})
	}
}

// canModerate reports whether peer may transfer ownership and kick: the
// owner. (Admin roles would join here.)
func (r *Room) canModerate(peer string) bool { return peer != "" && peer == r.owner }

// OwnerReturnWindow is how long an owner whose connection dropped, or who
// restarts into an update (proto.LeaveUpdate), keeps the room: resuming,
// or rejoining with the same identity key, takes it back. After that it
// passes on.
const OwnerReturnWindow = 2 * time.Minute

type ownerHold struct {
	idKey string // "" = only a resume of the same slot takes it back
	until time.Time
}

// holdLocked keeps the room for its owner, who just went away.
func (m *Manager) holdLocked(r *Room, p *Peer) {
	if p.ID == r.owner && p.ID != r.host {
		r.hold = &ownerHold{idKey: p.IDKey, until: m.now().Add(OwnerReturnWindow)}
	}
}

// holding reports whether the room is held for an away owner, dropping a
// hold that has run out or whose owner is back.
func (m *Manager) holding(r *Room) bool {
	if r.hold != nil && (r.online(r.owner) || !m.now().Before(r.hold.until)) {
		r.hold = nil
	}
	return r.hold != nil
}

// reclaimLocked gives a held room back to peerID if it is the away owner's
// identity rejoining. Call before electLocked.
func (m *Manager) reclaimLocked(r *Room, peerID string) {
	p := r.peers[peerID]
	if p == nil || p.IDKey == "" || !m.holding(r) || r.hold.idKey != p.IDKey {
		return
	}
	r.hold = nil
	old := r.peers[r.owner]
	r.owner = peerID
	if old != nil && old.out == nil {
		m.removeLocked(r, old.ID) // its dropped slot: it is back as peerID
	}
	m.broadcastLocked(r, peerID, proto.Owner{T: proto.TOwner, PeerID: peerID})
}

// Manager owns all rooms. All methods are safe for concurrent use.
type Manager struct {
	mu          sync.Mutex
	rooms       map[string]*Room
	channels    map[string]string // Discord channel ID -> room code
	ResumeGrace time.Duration
	// EmptyGrace is how long an empty room survives, so people who all
	// dropped out (or everyone restarting Blite) can come back on the same link.
	EmptyGrace time.Duration
	// MinRelayBuild is the first client build that understands relay mode.
	// Nobody may volunteer while a member is below it, and while a relay is
	// active such clients can't join.
	MinRelayBuild int
	// MaxPeers caps room size (0 = no cap). Set by the server operator.
	MaxPeers int
	now      func() time.Time
}

// DefaultMinRelayBuild is MinRelayBuild for NewManager: the first release
// with relay mode (build 30 is the forced-rollback test).
const DefaultMinRelayBuild = 31

// DefaultEmptyGrace is EmptyGrace for NewManager.
const DefaultEmptyGrace = 5 * time.Minute

// NewManager returns a Manager whose peers keep their slot for resumeGrace after
// their connection drops.
func NewManager(resumeGrace time.Duration) *Manager {
	return &Manager{rooms: map[string]*Room{}, channels: map[string]string{}, ResumeGrace: resumeGrace, EmptyGrace: DefaultEmptyGrace, MinRelayBuild: DefaultMinRelayBuild, now: time.Now}
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// validName: '#' is reserved for the identity tag clients append (name#1234).
func validName(s string) bool  { return len(s) > 0 && len(s) <= 64 && !strings.Contains(s, "#") }
func validIDKey(s string) bool { return len(s) <= 128 }
func validVer(s string) bool   { return len(s) <= 64 }

// Seat is what a peer gets back from Create, Join or Resume.
type Seat struct {
	Room   string
	PeerID string
	Resume string
	Others []proto.Peer
	// RelayQueue, RelayPaused, RelayWaiting: the room's relay queue as
	// members see it (see proto.Topology and Room.topology).
	RelayQueue   []string
	RelayPaused  bool
	RelayWaiting []string
	// Owner is the room owner's peer ID.
	Owner string
	// Plan is the relay plan (nil for builds below proto.MultiRelayBuild).
	Plan *proto.Plan
	// E2EE: the room's media is encrypted end to end.
	E2EE bool
}

// Create makes a room with the caller as its first peer. Hashing happens
// outside the lock.
func (m *Manager) Create(password, name, idKey, ver string, build int, e2ee bool, out Outbox) (Seat, error) {
	if e2ee && build < proto.E2EEBuild {
		return Seat{}, ErrBadRequest
	}
	if !validPassword(password) || !validName(name) || !validIDKey(idKey) || !validVer(ver) {
		return Seat{}, ErrBadRequest
	}
	pw := hashPassword(password)

	m.mu.Lock()
	defer m.mu.Unlock()
	code := NewCode()
	for m.rooms[code] != nil {
		code = NewCode()
	}
	r := &Room{Code: code, pw: pw, peers: map[string]*Peer{}, e2ee: e2ee}
	m.rooms[code] = r
	seat := m.seatLocked(r, name, idKey, ver, build, out)
	m.electLocked(r, seat.PeerID)
	seat.Owner = r.owner
	seat.Plan = r.planFor(r.peers[seat.PeerID])
	return seat, nil
}

// CreateFixed is Create with a given room code (hosted relays). If the room
// already exists, it joins it instead, password and all. Either way the
// caller is the room's host; owners are the identity keys that take
// ownership from it.
func (m *Manager) CreateFixed(code, password, name, idKey, ver string, build int, e2ee bool, owners []string, out Outbox) (Seat, error) {
	if e2ee && build < proto.E2EEBuild {
		return Seat{}, ErrBadRequest
	}
	if NormalizeCode(code) != code || !validPassword(password) || !validName(name) || !validIDKey(idKey) || !validVer(ver) || len(owners) > 64 {
		return Seat{}, ErrBadRequest
	}
	allowed := map[string]bool{}
	for _, k := range owners {
		if k != "" && validIDKey(k) {
			allowed[k] = true
		}
	}
	pw := hashPassword(password)
	m.mu.Lock()
	if m.rooms[code] != nil {
		m.mu.Unlock()
		seat, err := m.Join(code, password, name, idKey, ver, build, out)
		if err != nil {
			return seat, err
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if r := m.rooms[code]; r != nil && r.peers[seat.PeerID] != nil {
			if r.owner == r.host {
				r.owner = "" // the old relay's seat hands over to the new one
			}
			r.host, r.allowed = seat.PeerID, allowed
			m.electLocked(r, seat.PeerID)
			seat.Owner = r.owner
		}
		return seat, nil
	}
	defer m.mu.Unlock()
	r := &Room{Code: code, pw: pw, peers: map[string]*Peer{}, allowed: allowed, e2ee: e2ee}
	m.rooms[code] = r
	seat := m.seatLocked(r, name, idKey, ver, build, out)
	r.host = seat.PeerID
	m.electLocked(r, seat.PeerID)
	seat.Owner = r.owner
	seat.Plan = r.planFor(r.peers[seat.PeerID])
	return seat, nil
}

// Join adds the caller to an existing room. It returns ErrBadPassword or ErrNoRoom
// without revealing which rooms exist beyond what the code itself implies.
func (m *Manager) Join(code, password, name, idKey, ver string, build int, out Outbox) (Seat, error) {
	if !validName(name) || !validIDKey(idKey) || !validVer(ver) || len(password) > MaxPasswordLen {
		return Seat{}, ErrBadRequest
	}
	m.mu.Lock()
	r := m.rooms[code]
	if r == nil {
		m.mu.Unlock()
		return Seat{}, ErrNoRoom
	}
	pw := r.pw
	m.mu.Unlock()

	if !pw.verify(password) {
		return Seat{}, ErrBadPassword
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rooms[code] != r { // room ended while we were hashing
		return Seat{}, ErrNoRoom
	}
	if idKey != "" && r.banned[idKey] {
		return Seat{}, ErrBanned
	}
	if m.MaxPeers > 0 && len(r.peers) >= m.MaxPeers {
		return Seat{}, ErrRoomFull
	}
	if len(r.relayQ) > 0 && build < m.MinRelayBuild || r.e2ee && build < proto.E2EEBuild {
		return Seat{}, ErrUpdateRequired
	}
	seat := m.seatLocked(r, name, idKey, ver, build, out)
	r.emptyAt = time.Time{}
	joined := proto.PeerJoined{T: proto.TPeerJoined, Peer: proto.Peer{PeerID: seat.PeerID, Name: name, IDKey: idKey, Client: ver, Build: build}}
	m.broadcastLocked(r, seat.PeerID, joined)
	m.reclaimLocked(r, seat.PeerID)
	m.electLocked(r, seat.PeerID)
	m.announceLocked(r, seat.PeerID, false)
	seat.Owner = r.owner
	seat.RelayQueue, seat.RelayPaused, seat.RelayWaiting = r.topology()
	seat.Plan = r.planFor(r.peers[seat.PeerID])
	return seat, nil
}

func (m *Manager) seatLocked(r *Room, name, idKey, ver string, build int, out Outbox) Seat {
	p := &Peer{ID: randToken(9), Name: name, IDKey: idKey, Ver: ver, Build: build, resume: randToken(24), out: out}
	others := m.othersLocked(r, "")
	r.peers[p.ID] = p
	r.order = append(r.order, p.ID)
	q, paused, waiting := r.topology()
	return Seat{Room: r.Code, PeerID: p.ID, Resume: p.resume, Others: others, RelayQueue: q, RelayPaused: paused, RelayWaiting: waiting, E2EE: r.e2ee}
}

// topology is the relay queue members are told: the queue itself, or, while
// the room is too big for relay mode, an empty queue (mesh) with the
// volunteers waiting.
func (r *Room) topology() (queue []string, paused bool, waiting []string) {
	q := append([]string(nil), r.relayQ...)
	if len(q) > 0 && len(r.peers) > proto.RelayMaxPeers {
		return nil, true, q
	}
	return q, false, nil
}

func (r *Room) topologyMsg() proto.Topology {
	q, paused, waiting := r.topology()
	return proto.Topology{T: proto.TTopology, Queue: q, Paused: paused, Waiting: waiting}
}

func (m *Manager) othersLocked(r *Room, except string) []proto.Peer {
	others := make([]proto.Peer, 0, len(r.order))
	for _, id := range r.order {
		if id == except {
			continue
		}
		p := r.peers[id]
		others = append(others, proto.Peer{PeerID: p.ID, Name: p.Name, IDKey: p.IDKey, Client: p.Ver, Build: p.Build})
	}
	return others
}

// Resume reattaches out to a held slot. The old connection, if any, is replaced.
func (m *Manager) Resume(code, peerID, token string, out Outbox) (Seat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rooms[code]
	if r == nil {
		return Seat{}, ErrResumeFailed
	}
	p := r.peers[peerID]
	if p == nil || subtle.ConstantTimeCompare([]byte(token), []byte(p.resume)) != 1 {
		return Seat{}, ErrResumeFailed
	}
	p.out, p.lostAt = out, time.Time{}
	m.electLocked(r, p.ID) // it may be the only one online
	q, paused, waiting := r.topology()
	return Seat{Room: code, PeerID: p.ID, Resume: p.resume, Others: m.othersLocked(r, p.ID), RelayQueue: q, RelayPaused: paused, RelayWaiting: waiting, Owner: r.owner, Plan: r.planFor(p), E2EE: r.e2ee}, nil
}

// Volunteer adds peerID to the end of its room's relay queue (on) or removes
// it (!on). Every member, including the caller, gets the new Topology if the
// queue changed. Volunteering is refused while any member's build is below
// MinRelayBuild.
func (m *Manager) Volunteer(code, peerID string, on bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rooms[code]
	if r == nil || r.peers[peerID] == nil {
		return ErrNoRoom
	}
	if !on {
		m.unqueueLocked(r, peerID)
		return nil
	}
	if slices.Contains(r.relayQ, peerID) {
		return nil
	}
	for _, p := range r.peers {
		if p.Build < m.MinRelayBuild {
			return ErrRelayUnsupported
		}
	}
	r.relayQ = append(r.relayQ, peerID)
	m.announceLocked(r, "", true)
	return nil
}

// unqueueLocked drops peerID from the relay queue and announces the change.
func (m *Manager) unqueueLocked(r *Room, peerID string) {
	if i := slices.Index(r.relayQ, peerID); i >= 0 {
		r.relayQ = slices.Delete(r.relayQ, i, i+1)
		m.announceLocked(r, "", true)
	}
}

// Relay delivers msg to peer `to` in the same room as `from`. Peers in other
// rooms are unreachable by construction.
func (m *Manager) Relay(code, from, to string, msg any) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rooms[code]
	if r == nil || r.peers[from] == nil {
		return false
	}
	p := r.peers[to]
	if p == nil || p.out == nil {
		return false
	}
	return p.out.Send(msg)
}

// Leave removes a peer now and deletes the room if it was the last one.
func (m *Manager) Leave(code, peerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.rooms[code]; r != nil {
		m.removeLocked(r, peerID)
	}
}

// LeaveForUpdate is Leave for a peer restarting into a new build: if it
// owns the room, its identity gets ownership back on rejoining within
// OwnerReturnWindow.
func (m *Manager) LeaveForUpdate(code, peerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rooms[code]
	if r == nil {
		return
	}
	if p := r.peers[peerID]; p != nil && p.IDKey != "" { // else it couldn't reclaim
		m.holdLocked(r, p)
	}
	m.removeLocked(r, peerID)
}

// Disconnected marks a peer's connection as lost, if out is still its current
// connection (a resumed peer's old socket closing must not detach the new one).
// The slot is held for ResumeGrace; Sweep removes it after that.
func (m *Manager) Disconnected(code, peerID string, out Outbox) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rooms[code]
	if r == nil {
		return
	}
	if p := r.peers[peerID]; p != nil && p.out == out {
		p.out, p.lostAt = nil, m.now()
		m.holdLocked(r, p)
		m.electLocked(r, "")
	}
}

// Rename changes peerID's display name and tells the others.
func (m *Manager) Rename(code, peerID, name string) error {
	if !validName(name) {
		return ErrBadRequest
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rooms[code]
	if r == nil || r.peers[peerID] == nil {
		return ErrNoRoom
	}
	if p := r.peers[peerID]; p.Name != name {
		p.Name = name
		m.broadcastLocked(r, peerID, proto.PeerRenamed{T: proto.TPeerRenamed, PeerID: peerID, Name: name})
	}
	return nil
}

// Transfer makes `to` the owner, if `by` may moderate and `to` is online.
func (m *Manager) Transfer(code, by, to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rooms[code]
	if r == nil {
		return ErrNoRoom
	}
	if !r.canModerate(by) {
		return ErrNotOwner
	}
	if !r.online(to) {
		return ErrNoPeer
	}
	if to != r.owner {
		r.owner = to
		m.broadcastLocked(r, "", proto.Owner{T: proto.TOwner, PeerID: to})
	}
	return nil
}

// Kick removes `to` if `by` may moderate; ban also bars its identity key
// from joining again while the room lives. The host can't be kicked.
func (m *Manager) Kick(code, by, to string, ban bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rooms[code]
	if r == nil {
		return ErrNoRoom
	}
	if !r.canModerate(by) {
		return ErrNotOwner
	}
	p := r.peers[to]
	if p == nil || to == by || to == r.host {
		return ErrNoPeer
	}
	if ban && p.IDKey != "" {
		if r.banned == nil {
			r.banned = map[string]bool{}
		}
		r.banned[p.IDKey] = true
	}
	if out := p.out; out != nil {
		out.Send(proto.Kicked{T: proto.TKicked, By: r.peers[by].Name, Ban: ban})
		if c, ok := out.(closer); ok {
			c.CloseSoon()
		}
	}
	m.removeLocked(r, to)
	return nil
}

// Sweep removes peers whose resume window has expired, and rooms that have
// been empty for EmptyGrace.
func (m *Manager) Sweep() {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := m.now().Add(-m.ResumeGrace)
	emptyCutoff := m.now().Add(-m.EmptyGrace)
	for code, r := range m.rooms {
		if len(r.peers) == 0 {
			if !r.emptyAt.After(emptyCutoff) {
				delete(m.rooms, code)
				if r.channel != "" && m.channels[r.channel] == code {
					delete(m.channels, r.channel)
				}
			}
			continue
		}
		if r.hold != nil {
			m.electLocked(r, "") // hands over once the hold runs out
		}
		for _, id := range append([]string(nil), r.order...) {
			if p := r.peers[id]; p.out == nil && !p.lostAt.IsZero() && p.lostAt.Before(cutoff) {
				m.removeLocked(r, id)
			}
		}
	}
}

func (m *Manager) removeLocked(r *Room, peerID string) {
	if r.peers[peerID] == nil {
		return
	}
	delete(r.peers, peerID)
	for i, id := range r.order {
		if id == peerID {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	if len(r.peers) == 0 {
		r.relayQ = nil
		r.paused = false
		r.plan = proto.Plan{Epoch: r.plan.Epoch}
		r.owner, r.hold = "", nil // whoever joins next owns it
		r.emptyAt = m.now()       // Sweep deletes it after EmptyGrace
		return
	}
	m.broadcastLocked(r, peerID, proto.PeerLeft{T: proto.TPeerLeft, PeerID: peerID})
	queued := slices.Contains(r.relayQ, peerID)
	if queued {
		r.relayQ = slices.DeleteFunc(r.relayQ, func(id string) bool { return id == peerID })
	}
	m.announceLocked(r, "", queued)
	m.electLocked(r, "")
}

func (m *Manager) broadcastLocked(r *Room, except string, msg any) {
	for _, id := range r.order {
		if p := r.peers[id]; id != except && p.out != nil {
			p.out.Send(msg)
		}
	}
}

// Count returns the number of live rooms (for logs).
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rooms)
}

// validChannel: a Discord snowflake (digits).
func validChannel(id string) bool {
	if len(id) < 5 || len(id) > 25 {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// TieChannel ties room code to Discord channel id. It fails while another
// room with people in it holds the channel.
func (m *Manager) TieChannel(code, id string) error {
	if !validChannel(id) {
		return ErrBadRequest
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rooms[code]
	if r == nil {
		return ErrNoRoom
	}
	if old := m.rooms[m.channels[id]]; old != nil && old != r && len(old.peers) > 0 {
		return ErrChannelBusy
	}
	r.channel = id
	m.channels[id] = code
	return nil
}

// ChannelRoom is what GET /v1/channel/{id} tells people in that channel.
type ChannelRoom struct {
	Room   string `json:"room"`
	Open   bool   `json:"open"` // no password needed
	People int    `json:"people"`
}

// Channel returns the room tied to Discord channel id, if it has people.
func (m *Manager) Channel(id string) (ChannelRoom, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	code := m.channels[id]
	r := m.rooms[code]
	if r == nil || len(r.peers) == 0 {
		return ChannelRoom{}, false
	}
	return ChannelRoom{Room: code, Open: r.pw.key == nil, People: len(r.peers)}, true
}
