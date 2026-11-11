// Package proto defines Blite's signaling wire protocol (v0): JSON objects over a
// WebSocket, discriminated by the "t" field. Shared by blite-signal and the client.
package proto

import (
	"encoding/json"
	"strconv"
)

// Version is sent in Welcome so clients can refuse a server they don't understand.
const Version = 0

// RelayMaxPeers is the largest room relay mode serves: a relay connection
// carries one fixed origin slot per other member (RelayMaxPeers-1). In a
// bigger room relay mode pauses (Topology.Paused) and everyone meshes.
// Rooms themselves have no size cap unless the server operator sets one.
const RelayMaxPeers = 5

// MultiRelayBuild is the first client build that understands Plan (several
// relays, slots for active origins only). While any member is older, the
// server keeps the room on one relay and RelayMaxPeers (legacy Topology).
const MultiRelayBuild = 56

// E2EEBuild is the first client build that encrypts media end to end
// . Encrypted rooms refuse older builds (update_required).
const E2EEBuild = 72

// MaxRelays is the most relays a Plan uses (they all trunk to each other).
const MaxRelays = 4

// MeshComfortPeers is the room size above which a full mesh usually strains
// home uplinks (every sharer uploads once per viewer); clients warn there.
const MeshComfortPeers = 5

// Client -> server message types.
const (
	TCreate = "create"
	TJoin   = "join"
	TResume = "resume"
	TSignal = "signal"
	TLeave  = "leave"
	// TRelayVolunteer joins (On) or leaves (!On) the room's relay queue.
	TRelayVolunteer = "relay-volunteer"
	// TTransfer (owner only) makes another member the owner.
	TTransfer = "transfer"
	// TKick (owner only) removes a member; Ban also bars its identity key
	// from rejoining while the room lives.
	TKick = "kick"
	// TRename changes our display name in the room.
	TRename = "rename"
	// TCaps reports what we could do as a relay (planner input).
	TCaps = "caps"
)

// Server -> client message types.
const (
	TWelcome    = "welcome"
	TPeerJoined = "peer-joined"
	TPeerLeft   = "peer-left"
	TError      = "error"
	// TTopology announces the relay queue whenever it changes (builds
	// below MultiRelayBuild).
	TTopology = "topology"
	// TPlan announces the relay plan whenever it changes (builds from
	// MultiRelayBuild on, instead of TTopology).
	TPlan = "plan"
	// TOwner announces the room's owner whenever it changes.
	TOwner = "owner"
	// TKicked tells a member it was removed; the server then closes it.
	TKicked = "kicked"
	// TPeerRenamed announces a member's new display name.
	TPeerRenamed = "peer-renamed"
	// TSignal is also used server -> client, with From set instead of To.
)

// Error codes carried in Error.Code.
const (
	ErrBadPassword = "bad_password"
	ErrNoRoom      = "no_room"
	ErrRoomFull    = "room_full"
	ErrRateLimited = "rate_limited"
	ErrBadRequest  = "bad_request"
	ErrNoPeer      = "no_peer"
	ErrResumeFail  = "resume_failed"
	// ErrUpdateRequired: the client's build is below the server's minimum
	// compatible build.
	ErrUpdateRequired = "update_required"
	// ErrRelayUnsupported: a member's build can't do relay mode, so nobody
	// may volunteer.
	ErrRelayUnsupported = "relay_unsupported"
	// ErrNotOwner: only the owner may transfer or kick.
	ErrNotOwner = "not_owner"
	// ErrChannelBusy: another room is already tied to that Discord channel.
	ErrChannelBusy = "channel_busy"
	// ErrBanned: the owner banned this identity from the room.
	ErrBanned = "banned"
)

// Envelope is the minimal shape used to read "t" before decoding the full message.
type Envelope struct {
	T string `json:"t"`
}

// Create asks the server for a new room. The creator becomes its first peer.
type Create struct {
	T        string      `json:"t"`
	Password string      `json:"password"`
	Name     string      `json:"name"`
	IDKey    string      `json:"idKey"` // base64 Ed25519 public key
	Client   *ClientMeta `json:"client,omitempty"`
	// Room and HostKey: a hosted relay (Phase 2) asks for a fixed room code,
	// so its share link survives restarts. Needs the server's host key. If
	// the room exists, this joins it (same password) instead.
	Room    string `json:"room,omitempty"`
	HostKey string `json:"hostKey,omitempty"`
	// Owners (hosted relay only): identity keys of the members who take
	// ownership from the relay while any of them is in the room.
	Owners []string `json:"owners,omitempty"`
	// E2EE: the room's media is encrypted end to end. Ignored
	// when joining an existing fixed room.
	E2EE bool `json:"e2ee,omitempty"`
	// Channel ties the new room to a Discord voice channel (its ID), so
	// people in that channel find it (GET /v1/channel/{id}). Refused with
	// ErrChannelBusy while another room holds the channel.
	Channel string `json:"channel,omitempty"`
}

// Leave ends our membership. Reason LeaveUpdate: we restart into a new
// build and rejoin in a moment, so an owner gets the room back
// (OwnerReturnWindow). Servers from before it ignore Reason.
type Leave struct {
	T      string `json:"t"`
	Reason string `json:"reason,omitempty"`
}

// LeaveUpdate is Leave.Reason for an in-call update.
const LeaveUpdate = "update"

// Join enters an existing room.
type Join struct {
	T        string      `json:"t"`
	Room     string      `json:"room"`
	Password string      `json:"password"`
	Name     string      `json:"name"`
	IDKey    string      `json:"idKey"`
	Client   *ClientMeta `json:"client,omitempty"`
}

// ClientMeta describes the client build. All fields are
// optional so clients from before version reporting still parse.
type ClientMeta struct {
	Platform   string `json:"platform,omitempty"`   // linux | windows
	AppVersion string `json:"appVersion,omitempty"` // e.g. 0.1.0
	Build      int    `json:"build,omitempty"`      // monotonic build number; what the server compares
	BuildDate  string `json:"buildDate,omitempty"`  // UTC, e.g. 2026-10-24 14:30
	OSVersion  string `json:"osVersion,omitempty"`  // e.g. Linux Mint 22, Windows 11 26100
}

// Label is how peers see a client: "0.1.0 (57) windows".
func (c *ClientMeta) Label() string {
	if c == nil || c.AppVersion == "" {
		return ""
	}
	s := c.AppVersion
	if c.Build > 0 {
		s += " (" + strconv.Itoa(c.Build) + ")"
	}
	if c.Platform != "" {
		s += " " + c.Platform
	}
	return s
}

// Resume reattaches a dropped WebSocket to a peer slot that is still held.
type Resume struct {
	T      string `json:"t"`
	Room   string `json:"room"`
	PeerID string `json:"peerId"`
	Token  string `json:"resume"`
}

// Signal carries opaque SDP/ICE data between two peers in the same room.
// Client -> server sets To; server -> client sets From.
type Signal struct {
	T    string          `json:"t"`
	To   string          `json:"to,omitempty"`
	From string          `json:"from,omitempty"`
	Data json.RawMessage `json:"data"`
}

// Peer describes a room member as other members see it.
type Peer struct {
	PeerID string `json:"peerId"`
	Name   string `json:"name"`
	IDKey  string `json:"idKey"`
	Client string `json:"client,omitempty"` // ClientMeta.Label of their build
	Build  int    `json:"build,omitempty"`  // their build number (0 = unknown)
}

// ICEServer mirrors the WebRTC RTCIceServer dictionary.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// Welcome confirms create/join/resume.
type Welcome struct {
	T          string      `json:"t"`
	V          int         `json:"v"`
	Room       string      `json:"room"`
	PeerID     string      `json:"peerId"`
	Resume     string      `json:"resume"`
	ICEServers []ICEServer `json:"iceServers"`
	Peers      []Peer      `json:"peers"`
	// UpdateRecommended: our build is below the server's recommended minimum.
	// We are let in, but should tell the user to update.
	UpdateRecommended bool `json:"updateRecommended,omitempty"`
	// RelayQueue is the relay queue at the time of joining (see Topology).
	RelayQueue []string `json:"relayQueue,omitempty"`
	// RelayPaused / RelayWaiting: see Topology.Paused and Waiting.
	RelayPaused  bool     `json:"relayPaused,omitempty"`
	RelayWaiting []string `json:"relayWaiting,omitempty"`
	// Owner is the room owner's peer ID ("" = none online).
	Owner string `json:"owner,omitempty"`
	// Plan is the relay plan at the time of joining (MultiRelayBuild on).
	Plan *Plan `json:"plan,omitempty"`
	// E2EE: members encrypt media end to end.
	E2EE bool `json:"e2ee,omitempty"`
}

// PeerJoined announces a new member to existing members.
type PeerJoined struct {
	T string `json:"t"`
	Peer
}

// PeerLeft announces that a member is gone (left, or its resume window expired).
type PeerLeft struct {
	T      string `json:"t"`
	PeerID string `json:"peerId"`
}

// Error reports a failed request. The connection stays open unless the server says otherwise.
type Error struct {
	T    string `json:"t"`
	Code string `json:"code"`
}

// RelayVolunteer asks to join the relay queue (On) or to leave it. Leaving is
// how a member cancels: the next volunteer takes over, or the room goes back
// to full mesh.
type RelayVolunteer struct {
	T  string `json:"t"`
	On bool   `json:"on"`
}

// Topology is the room's relay queue, in volunteer order. Its head is the
// active relay; empty means full mesh. Sent to every member whenever it
// changes.
type Topology struct {
	T     string   `json:"t"`
	Queue []string `json:"queue"`
	// Paused: the room is bigger than RelayMaxPeers, so Queue is sent empty
	// (full mesh) and Waiting holds the volunteers until it shrinks.
	Paused  bool     `json:"paused,omitempty"`
	Waiting []string `json:"waiting,omitempty"`
}

// Relay is the active relay's peer ID, or "" for full mesh.
func (t Topology) Relay() string {
	if len(t.Queue) == 0 {
		return ""
	}
	return t.Queue[0]
}

// Plan is the room's relay plan, computed by the server from the volunteer
// queue and the members' Caps. No relays means full mesh. Every relay
// connects to every other relay (trunks) and to the members homed on it;
// everyone else connects only to its home relay. Sent to every member
// whenever it changes; Epoch grows with each change.
type Plan struct {
	T      string   `json:"t"`
	Epoch  int      `json:"epoch"`
	Relays []string `json:"relays,omitempty"`
	// Home maps every member to its relay (a relay to itself).
	Home map[string]string `json:"home,omitempty"`
	// Queue is every volunteer in volunteer order, relays or not.
	Queue []string `json:"queue,omitempty"`
	// Paused: an older member keeps the room on one relay, and the room
	// is bigger than RelayMaxPeers, so everyone meshes.
	Paused bool `json:"paused,omitempty"`
}

// Caps is what a member reports about itself for relay planning. Sent on
// change, rate-limited by the client.
type Caps struct {
	T string `json:"t"`
	// UpKbps is its usable upload estimate (0 = unknown), UpSource how it
	// was found: declared (the user's setting) or passive (highest
	// sustained send rate seen).
	UpKbps   int    `json:"upKbps,omitempty"`
	UpSource string `json:"upSource,omitempty"`
	// CPUFree is the idle CPU share in percent (-1 unknown).
	CPUFree int `json:"cpuFree"`
	// Reach is how peers reach it: public, nat, or turn (only via TURN:
	// useless as a relay). "" = not known yet.
	Reach string `json:"reach,omitempty"`
	// Uptime is how long it has been in the room, in seconds.
	Uptime int `json:"uptime,omitempty"`
}

// Transfer (owner) makes PeerID the owner.
type Transfer struct {
	T      string `json:"t"`
	PeerID string `json:"peerId"`
}

// Kick (owner) removes PeerID; Ban also bars its identity key.
type Kick struct {
	T      string `json:"t"`
	PeerID string `json:"peerId"`
	Ban    bool   `json:"ban,omitempty"`
}

// Owner announces the room owner ("" = none online).
type Owner struct {
	T      string `json:"t"`
	PeerID string `json:"peerId"`
}

// Kicked tells a removed member who removed it and whether it is banned.
type Kicked struct {
	T   string `json:"t"`
	By  string `json:"by"` // the owner's name
	Ban bool   `json:"ban,omitempty"`
}

// Rename sets our display name. Names can't contain '#': clients show
// every name as name#tag, the tag derived from the identity key.
type Rename struct {
	T    string `json:"t"`
	Name string `json:"name"`
}

// PeerRenamed announces a member's new display name.
type PeerRenamed struct {
	T      string `json:"t"`
	PeerID string `json:"peerId"`
	Name   string `json:"name"`
}

// Report is a client's log report (POST /v1/report on the signal server):
// a crash found at the next start, or logs the user chose to send.
type Report struct {
	Platform  string `json:"platform"`
	Version   string `json:"version"` // e.g. "0.1.0 (43) windows"
	Build     int    `json:"build"`
	OSVersion string `json:"osVersion,omitempty"`
	Name      string `json:"name,omitempty"`     // the user's display name#tag
	Kind      string `json:"kind"`               // crash | unclean-exit | user
	Note      string `json:"note,omitempty"`     // e.g. why the guard isn't running
	File      string `json:"file,omitempty"`     // the log file's name
	Log       string `json:"log"`                // the log tail
	Received  string `json:"received,omitempty"` // set by the server
	// Zip holds whole log files and a system summary (Send logs). The
	// server stores it next to the report as ZipFile. Servers from before
	// it reject such a large report; clients then send without.
	Zip     []byte `json:"zip,omitempty"`
	ZipFile string `json:"zipFile,omitempty"`
}
