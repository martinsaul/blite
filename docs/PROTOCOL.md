# Bellion Share signaling protocol v0

JSON text messages over a WebSocket at `wss://<host>/ws`. Every message has a
`"t"` field. Max message size 64 KB; more than 50 messages/s (burst 100) closes the
connection. The server pings every 30 s. Message types and fields are defined in
`internal/proto`; unknown fields are ignored, so newer fields are optional and
older clients and servers keep working.

## Entering a room (first message on a connection)

| Client sends | Fields |
|---|---|
| `create` | `password` (empty for an open room, else 4-128 chars), `name` (1-64 chars, no `#`), `idKey` (base64 Ed25519 public key, <=128 chars), `client`, `e2ee`, `channel` |
| `join` | `room` (code, any case, dashes/spaces ignored), `password`, `name`, `idKey`, `client` |
| `resume` | `room`, `peerId`, `resume` (token from `welcome`) |

- `client` (optional) describes the build: `{platform, appVersion, build,
  buildDate, osVersion}`. The server compares `build` against its minimum
  builds, and peers see it as a label like `0.1.0 (84) windows`.
- `e2ee`: the room's media is encrypted end to end (see below).
- `channel`: ties the new room to a Discord voice channel ID, so others in
  that channel can find it (`GET /v1/channel/{id}`). Refused with
  `channel_busy` while another room holds the channel.
- A hosted relay (`blite -host`) also sends `room` (a fixed code, so its link
  survives restarts), `hostKey` (the server's `BLITE_HOST_KEY`) and `owners`
  (identity keys that take ownership from the relay while one of them is in the
  room). If that room exists, `create` joins it instead.

Reply: `welcome` or `error {code}`. `welcome` carries:

| Field | Meaning |
|---|---|
| `v` | protocol version (0) |
| `room`, `peerId`, `resume` | the room code, our peer ID, our resume token |
| `iceServers[]` | STUN/TURN servers (`{urls, username, credential}`) |
| `peers[]` | the OTHER members `{peerId, name, idKey, client, build}` in join order |
| `owner` | the owner's peer ID (`""` = none online) |
| `e2ee` | members encrypt media end to end |
| `plan` | the current relay plan (see Relays), if any |
| `relayQueue`, `relayPaused`, `relayWaiting` | the relay queue for builds that only know one relay |
| `updateRecommended` | our build is below the server's recommended build: we are in, but should tell the user to update |

A dropped connection keeps its slot for 60 s; `resume` reattaches it. After that
the peer is removed and others get `peer-left`. A room that has become empty
survives 5 minutes, so people who all dropped out can come back on the same
link; then it is deleted. There is no way to list rooms.

## In a room

| Direction | Message |
|---|---|
| client -> server | `signal {to, data}`: `data` is routed only within the room (see Peer messages) |
| server -> client | `signal {from, data}` |
| server -> client | `peer-joined {peerId, name, idKey, client, build}` |
| server -> client | `peer-left {peerId}` |
| client -> server | `rename {name}`: change our display name |
| server -> client | `peer-renamed {peerId, name}` |
| client -> server | `relay-volunteer {on}`: join (`true`) or leave the relay queue |
| client -> server | `caps {upKbps, upSource, cpuFree, reach, uptime}`: what we could do as a relay |
| server -> client | `plan {epoch, relays, home, queue, paused}`: the relay plan, whenever it changes |
| server -> client | `topology {queue, paused, waiting}`: the relay queue, for builds below 56 instead of `plan` |
| server -> client | `owner {peerId}`: the owner changed (`""` = none online) |
| client -> server | `transfer {peerId}` (owner only): make another member the owner |
| client -> server | `kick {peerId, ban}` (owner only): remove a member; `ban` also bars its identity key while the room lives |
| server -> client | `kicked {by, ban}`: we were removed (by the owner's name); the server then closes the connection |
| client -> server | `leave {reason}`: removes the peer immediately |

Negotiation: the newcomer sends offers to every peer it connects to (perfect
negotiation, newcomer impolite).

### Ownership

The creator owns the room. An owner who leaves hands over at once, to the
earliest-joined online member. An owner whose connection drops, or who sends
`leave {reason: "update"}` to restart into a new build (in-call update), keeps
the room for 2 minutes: resuming, or rejoining with the same identity key,
takes it back. In a hosted relay's room, ownership goes to the earliest-joined
member listed in `owners`, else to the relay.

### Relays

Members volunteer with `relay-volunteer`; nobody may volunteer while a member
runs a build below 31 (`relay_unsupported`), and while a relay is active such
builds can't join (`update_required`). The server turns the queue and the
members' `caps` into a `plan`: `relays` (up to 4, all connected to each other),
`home` (every member's relay; a relay maps to itself) and `queue` (every
volunteer, in order). Everyone else connects only to its home relay. No relays
means full mesh. `epoch` grows with every change. While a member runs a build
below 56, the room uses one relay (the queue's head) and pauses relay mode
above 5 people (`paused`: everyone meshes).

## Peer messages

`signal.data` is opaque to the server. Clients send a JSON object with any of:

| Field | Meaning |
|---|---|
| `sdp`, `sig` | a session description, and the sender's identity-key signature over its DTLS fingerprints (peers refuse a connection whose signature doesn't verify) |
| `ice` | an ICE candidate |
| `maxLayer`, `origin` | the best layer the receiver wants (0 high, 1 low, 2 none); with `origin`, sent to a relay, for that member's relayed video |
| `slots` | from a relay: which origin's video each media slot carries (`""` = free) |
| `bye` | the sender closes this connection, which the plan no longer wants |
| `resize {origin, maxHeight, cap}` | viewer to provider: ask for a rendition no taller than `maxHeight` (`0` lifts a cap) |
| `resizeReply {origin, ok, height}` | provider to viewer: the answer, or (`ok: false`) a granted rendition was dropped |
| `sending {origin, height, top, congested}` | provider to viewer: what it sends now; `top` is the origin's full height, `0` when the origin stopped sharing |
| `kx {pub, sig}` | end-to-end encryption: the sender's exchange key, signed by its identity key |
| `key {sealed}` | end-to-end encryption: the sender's media key, sealed to the recipient's exchange key |

With `e2ee` on, each member encrypts what it sends with its own sender key and
hands that key to every other member in `key`, sealed to the exchange key they
announced in `kx`. When a member leaves, everyone makes a new sender key. Relays
forward encrypted media unchanged. Encrypted rooms refuse builds below 72
(`update_required`).

## Error codes

| Code | Meaning |
|---|---|
| `bad_password` | wrong password |
| `no_room` | no such room |
| `room_full` | only if the operator set `BLITE_MAX_PEERS` |
| `rate_limited` | 5 failed joins per IP or 20 per room in 10 min; 10 creates per IP in 10 min |
| `bad_request` | malformed or unknown message |
| `no_peer` | signal/transfer/kick target not in the room or disconnected |
| `resume_failed` | the slot or token is gone |
| `update_required` | the client's build is below the server's minimum, or too old for this room (relay mode, end-to-end encryption) |
| `relay_unsupported` | a member's build can't do relay mode, so nobody may volunteer |
| `not_owner` | only the owner may transfer or kick |
| `channel_busy` | another room is already tied to that Discord channel |
| `banned` | the owner banned this identity from the room |

## HTTP endpoints

- `GET /j/{code}`: the share-link landing page.
- `GET /v1/channel/{id}`: the room tied to a Discord voice channel, as
  `{room, open, people}` (`open`: no password needed); 404 if none. Rate
  limited per IP.
- `POST /v1/discord/token`: exchanges a Discord authorization code
  (`{"code": ...}`) or refresh token (`{"refresh": ...}`) with the server's
  Discord app secret, so the secret never ships in the app. Only when the
  operator configured a Discord app.
- `POST /v1/report`: client log reports, only when the operator set
  `BLITE_REPORT_DIR`.
- `GET /healthz`.

## Share links

`https://<host>/j/<CODE>#<password>` (no `#` part for an open room). The landing
page hands the link to the app as `blite://join/<CODE>#<password>`; the app
also handles `bshare://join/<CODE>#<password>`. The password stays in the URL
fragment, so the server never sees it; the page doesn't check whether the room
exists.
