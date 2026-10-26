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

	resume string
	out    Outbox    // nil while disconnected
	lostAt time.Time // when out became nil
}

// Room is a set of up to proto.MaxPeers peers sharing a password.
type Room struct {
	Code  string
	pw    passwordHash
	peers map[string]*Peer
	order []string // join order, so peer lists are stable
	// relayQ is the relay queue in volunteer order; its head is the active
	// relay, and an empty queue means full mesh.
	relayQ []string

	// Ownership. The owner is always an online member, or ""
	// while nobody is. It passes on as soon as the owner's connection
	// drops: to the earliest-joined online member of allowed if any, else
	// to the host (a hosted relay, owner of the room it created), else to
	// the earliest-joined online member.
	owner   string
	host    string          // hosted relay's peer ID ("" = an ordinary room)
	allowed map[string]bool // identity keys that take ownership from the host
	banned  map[string]bool // identity keys the owner banned

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
	if o := r.wantOwner(); o != r.owner {
		r.owner = o
		m.broadcastLocked(r, except, proto.Owner{T: proto.TOwner, PeerID: o})
	}
}

// canModerate reports whether peer may transfer ownership and kick: the
// owner. (Admin roles would join here.)
func (r *Room) canModerate(peer string) bool { return peer != "" && peer == r.owner }

// Manager owns all rooms. All methods are safe for concurrent use.
type Manager struct {
	mu          sync.Mutex
	rooms       map[string]*Room
	ResumeGrace time.Duration
	// EmptyGrace is how long an empty room survives, so people who all
	// dropped out (or everyone restarting Blite) can come back on the same link.
	EmptyGrace time.Duration
	// MinRelayBuild is the first client build that understands relay mode.
	// Nobody may volunteer while a member is below it, and while a relay is
	// active such clients can't join.
	MinRelayBuild int
	now           func() time.Time
}

// DefaultMinRelayBuild is MinRelayBuild for NewManager: the first release
// with relay mode (build 30 is the forced-rollback test).
const DefaultMinRelayBuild = 31

// DefaultEmptyGrace is EmptyGrace for NewManager.
const DefaultEmptyGrace = 5 * time.Minute

// NewManager returns a Manager whose peers keep their slot for resumeGrace after
// their connection drops.
func NewManager(resumeGrace time.Duration) *Manager {
	return &Manager{rooms: map[string]*Room{}, ResumeGrace: resumeGrace, EmptyGrace: DefaultEmptyGrace, MinRelayBuild: DefaultMinRelayBuild, now: time.Now}
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
	// RelayQueue is the room's relay queue (see proto.Topology).
	RelayQueue []string
	// Owner is the room owner's peer ID.
	Owner string
}

// Create makes a room with the caller as its first peer. Hashing happens
// outside the lock.
func (m *Manager) Create(password, name, idKey, ver string, build int, out Outbox) (Seat, error) {
	if len(password) < MinPasswordLen || len(password) > MaxPasswordLen || !validName(name) || !validIDKey(idKey) || !validVer(ver) {
		return Seat{}, ErrBadRequest
	}
	pw := hashPassword(password)

	m.mu.Lock()
	defer m.mu.Unlock()
	code := NewCode()
	for m.rooms[code] != nil {
		code = NewCode()
	}
	r := &Room{Code: code, pw: pw, peers: map[string]*Peer{}}
	m.rooms[code] = r
	seat := m.seatLocked(r, name, idKey, ver, build, out)
	m.electLocked(r, seat.PeerID)
	seat.Owner = r.owner
	return seat, nil
}

// CreateFixed is Create with a given room code (hosted relays). If the room
// already exists, it joins it instead, password and all. Either way the
// caller is the room's host; owners are the identity keys that take
// ownership from it.
func (m *Manager) CreateFixed(code, password, name, idKey, ver string, build int, owners []string, out Outbox) (Seat, error) {
	if NormalizeCode(code) != code || len(password) < MinPasswordLen || len(password) > MaxPasswordLen || !validName(name) || !validIDKey(idKey) || !validVer(ver) || len(owners) > 64 {
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
	r := &Room{Code: code, pw: pw, peers: map[string]*Peer{}, allowed: allowed}
	m.rooms[code] = r
	seat := m.seatLocked(r, name, idKey, ver, build, out)
	r.host = seat.PeerID
	m.electLocked(r, seat.PeerID)
	seat.Owner = r.owner
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
	if len(r.peers) >= proto.MaxPeers {
		return Seat{}, ErrRoomFull
	}
	if len(r.relayQ) > 0 && build < m.MinRelayBuild {
		return Seat{}, ErrUpdateRequired
	}
	seat := m.seatLocked(r, name, idKey, ver, build, out)
	r.emptyAt = time.Time{}
	joined := proto.PeerJoined{T: proto.TPeerJoined, Peer: proto.Peer{PeerID: seat.PeerID, Name: name, IDKey: idKey, Client: ver}}
	m.broadcastLocked(r, seat.PeerID, joined)
	m.electLocked(r, seat.PeerID)
	seat.Owner = r.owner
	return seat, nil
}

func (m *Manager) seatLocked(r *Room, name, idKey, ver string, build int, out Outbox) Seat {
	p := &Peer{ID: randToken(9), Name: name, IDKey: idKey, Ver: ver, Build: build, resume: randToken(24), out: out}
	others := m.othersLocked(r, "")
	r.peers[p.ID] = p
	r.order = append(r.order, p.ID)
	return Seat{Room: r.Code, PeerID: p.ID, Resume: p.resume, Others: others, RelayQueue: r.queue()}
}

// queue returns a copy of the relay queue.
func (r *Room) queue() []string { return append([]string(nil), r.relayQ...) }

func (m *Manager) othersLocked(r *Room, except string) []proto.Peer {
	others := make([]proto.Peer, 0, len(r.order))
	for _, id := range r.order {
		if id == except {
			continue
		}
		p := r.peers[id]
		others = append(others, proto.Peer{PeerID: p.ID, Name: p.Name, IDKey: p.IDKey, Client: p.Ver})
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
	if r.owner == p.ID {
		// Resumed before we noticed its old connection die: it dropped
		// all the same, and an owner who drops hands over.
		r.owner = ""
	}
	p.out, p.lostAt = out, time.Time{}
	m.electLocked(r, p.ID) // it may be the only one online
	return Seat{Room: code, PeerID: p.ID, Resume: p.resume, Others: m.othersLocked(r, p.ID), RelayQueue: r.queue(), Owner: r.owner}, nil
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
	m.topologyLocked(r)
	return nil
}

// unqueueLocked drops peerID from the relay queue and announces the change.
func (m *Manager) unqueueLocked(r *Room, peerID string) {
	if i := slices.Index(r.relayQ, peerID); i >= 0 {
		r.relayQ = slices.Delete(r.relayQ, i, i+1)
		m.topologyLocked(r)
	}
}

func (m *Manager) topologyLocked(r *Room) {
	m.broadcastLocked(r, "", proto.Topology{T: proto.TTopology, Queue: r.queue()})
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
		m.electLocked(r, "") // an owner who drops hands over at once
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
			}
			continue
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
		r.owner = ""        // whoever joins next owns it
		r.emptyAt = m.now() // Sweep deletes it after EmptyGrace
		return
	}
	m.broadcastLocked(r, peerID, proto.PeerLeft{T: proto.TPeerLeft, PeerID: peerID})
	m.unqueueLocked(r, peerID)
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
