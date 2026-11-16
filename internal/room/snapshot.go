package room

import (
	"encoding/json"
	"errors"
	"time"

	"blite/internal/proto"
)

// Snapshot and Restore save the rooms as JSON, so a restarted server keeps
// them: members resume into their old slots instead of rejoining. A snapshot
// holds password hashes and resume-token hashes, never a password or a
// usable token, but keep it private anyway.

const snapshotVersion = 1

type snapshot struct {
	V        int               `json:"v"`
	At       time.Time         `json:"at"`
	Rooms    []snapRoom        `json:"rooms"`
	Channels map[string]string `json:"channels,omitempty"` // Discord channel ID -> room code
}

type snapRoom struct {
	Code    string          `json:"code"`
	Salt    []byte          `json:"salt,omitempty"`
	Key     []byte          `json:"key,omitempty"`
	Peers   []snapPeer      `json:"peers"` // in join order
	RelayQ  []string        `json:"relayQ,omitempty"`
	Paused  bool            `json:"paused,omitempty"`
	Plan    proto.Plan      `json:"plan"`
	E2EE    bool            `json:"e2ee,omitempty"`
	Channel string          `json:"channel,omitempty"`
	Owner   string          `json:"owner,omitempty"`
	Host    string          `json:"host,omitempty"`
	Allowed map[string]bool `json:"allowed,omitempty"`
	Banned  map[string]bool `json:"banned,omitempty"`
	Hold    *snapHold       `json:"hold,omitempty"`
	EmptyAt time.Time       `json:"emptyAt,omitzero"`
}

type snapPeer struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	IDKey  string     `json:"idKey,omitempty"`
	Ver    string     `json:"ver,omitempty"`
	Build  int        `json:"build,omitempty"`
	Caps   proto.Caps `json:"caps"`
	Resume []byte     `json:"resume"`          // SHA-256 of the resume token
	LostAt time.Time  `json:"lostAt,omitzero"` // zero = it was connected
}

type snapHold struct {
	IDKey string    `json:"idKey,omitempty"`
	Until time.Time `json:"until"`
}

// Snapshot returns every room as JSON.
func (m *Manager) Snapshot() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock() // snap shares the rooms' maps and slices
	s := snapshot{V: snapshotVersion, At: m.now(), Rooms: make([]snapRoom, 0, len(m.rooms)), Channels: m.channels}
	for _, r := range m.rooms {
		s.Rooms = append(s.Rooms, r.snap())
	}
	return json.Marshal(s)
}

func (r *Room) snap() snapRoom {
	sr := snapRoom{
		Code: r.Code, Salt: r.pw.salt, Key: r.pw.key,
		RelayQ: r.relayQ, Paused: r.paused, Plan: r.plan, E2EE: r.e2ee, Channel: r.channel,
		Owner: r.owner, Host: r.host, Allowed: r.allowed, Banned: r.banned, EmptyAt: r.emptyAt,
	}
	if r.hold != nil {
		sr.Hold = &snapHold{IDKey: r.hold.idKey, Until: r.hold.until}
	}
	for _, id := range r.order {
		p := r.peers[id]
		sr.Peers = append(sr.Peers, snapPeer{ID: p.ID, Name: p.Name, IDKey: p.IDKey, Ver: p.Ver, Build: p.Build, Caps: p.caps, Resume: p.resume[:], LostAt: p.lostAt})
	}
	return sr
}

// ErrSnapshot: Restore was given something that isn't a snapshot it can read.
var ErrSnapshot = errors.New("room: bad snapshot")

// Restore loads the rooms in data. Nobody is connected after a restore, so
// every member who was connected is marked as dropped now: each gets a full
// ResumeGrace to resume, and an owner gets OwnerReturnWindow to come back
// before the room passes on, as if its connection had just dropped. A room
// that already exists here is replaced only if it came from an earlier
// Restore and nobody has joined or resumed it since; a room this server
// has served is newer than any file. It returns the number of rooms loaded.
func (m *Manager) Restore(data []byte) (int, error) {
	var s snapshot
	if err := json.Unmarshal(data, &s); err != nil || s.V != snapshotVersion {
		return 0, ErrSnapshot
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rooms := make([]*Room, 0, len(s.Rooms)) // all or nothing
	seen := map[string]bool{}
	for _, sr := range s.Rooms {
		if sr.Code == "" || NormalizeCode(sr.Code) != sr.Code || seen[sr.Code] {
			return 0, ErrSnapshot
		}
		seen[sr.Code] = true
		r, err := m.restoreRoom(sr)
		if err != nil {
			return 0, err
		}
		rooms = append(rooms, r)
	}
	loaded := map[string]bool{}
	for _, r := range rooms {
		if old := m.rooms[r.Code]; old != nil {
			if !old.restored {
				continue
			}
			m.dropRoomLocked(old)
		}
		m.rooms[r.Code] = r
		loaded[r.Code] = true
	}
	for id, code := range s.Channels {
		if !loaded[code] || m.rooms[code].channel != id {
			continue
		}
		if cur := m.rooms[m.channels[id]]; cur == nil || cur.restored {
			m.channels[id] = code
		}
	}
	return len(loaded), nil
}

func (m *Manager) restoreRoom(sr snapRoom) (*Room, error) {
	now := m.now()
	r := &Room{
		Code: sr.Code, pw: passwordHash{salt: sr.Salt, key: sr.Key}, peers: map[string]*Peer{}, restored: true,
		relayQ: sr.RelayQ, paused: sr.Paused, plan: sr.Plan, e2ee: sr.E2EE, channel: sr.Channel,
		owner: sr.Owner, host: sr.Host, allowed: sr.Allowed, banned: sr.Banned, emptyAt: sr.EmptyAt,
	}
	if sr.Hold != nil {
		r.hold = &ownerHold{idKey: sr.Hold.IDKey, until: sr.Hold.Until}
	}
	for _, sp := range sr.Peers {
		if sp.ID == "" || r.peers[sp.ID] != nil || len(sp.Resume) != len(tokenHash{}) {
			return nil, ErrSnapshot
		}
		p := &Peer{ID: sp.ID, Name: sp.Name, IDKey: sp.IDKey, Ver: sp.Ver, Build: sp.Build, caps: sp.Caps, lostAt: sp.LostAt}
		copy(p.resume[:], sp.Resume)
		if p.lostAt.IsZero() {
			p.lostAt = now
			if p.ID == r.owner && r.hold == nil {
				m.holdLocked(r, p)
			}
		}
		r.peers[p.ID] = p
		r.order = append(r.order, p.ID)
	}
	if len(r.peers) == 0 && r.emptyAt.IsZero() {
		r.emptyAt = now
	}
	if !r.consistent() {
		return nil, ErrSnapshot
	}
	return r, nil
}

// consistent: every peer ID the room refers to is a member of it.
func (r *Room) consistent() bool {
	member := func(id string) bool { return id == "" || r.peers[id] != nil }
	ok := member(r.owner) && member(r.host)
	for _, id := range r.relayQ {
		ok = ok && id != "" && member(id)
	}
	for _, id := range r.plan.Relays {
		ok = ok && id != "" && member(id)
	}
	for id, home := range r.plan.Home {
		ok = ok && id != "" && home != "" && member(id) && member(home)
	}
	return ok
}

// dropRoomLocked deletes r and its channel tie.
func (m *Manager) dropRoomLocked(r *Room) {
	delete(m.rooms, r.Code)
	if r.channel != "" && m.channels[r.channel] == r.Code {
		delete(m.channels, r.channel)
	}
}
