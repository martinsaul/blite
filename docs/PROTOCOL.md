# Blite signaling protocol v0

JSON text messages over a WebSocket at `wss://<host>/ws`. Every message has a
`"t"` field. Max message size 64 KB; more than 50 messages/s (burst 100) closes the
connection. The server pings every 30 s.

## Entering a room (first message on a connection)

| Client sends | Fields |
|---|---|
| `create` | `password` (8-128 chars), `name` (1-64), `idKey` (base64 Ed25519 public key, <=128 chars) |
| `join` | `room` (code, any case, dashes/spaces ignored), `password`, `name`, `idKey` |
| `resume` | `room`, `peerId`, `resume` (token from `welcome`) |

Reply: `welcome` `{v, room, peerId, resume, iceServers[], peers[]}`, where `peers`
are the OTHER members `{peerId, name, idKey}` in join order; or `error {code}`.

A dropped connection keeps its slot for 60 s; `resume` reattaches it. After that
the peer is removed and others get `peer-left`. **A room ends when its last peer is
gone.** There is no way to list rooms.

## In a room

| Direction | Message |
|---|---|
| client -> server | `signal {to, data}`: `data` is opaque (SDP, ICE); routed only within the room |
| server -> client | `signal {from, data}` |
| server -> client | `peer-joined {peerId, name, idKey}` |
| server -> client | `peer-left {peerId}` |
| client -> server | `leave`: removes the peer immediately |

Negotiation: the newcomer sends offers to every peer listed in its `welcome`
(perfect negotiation, newcomer impolite).

## Error codes

`bad_password`, `no_room`, `room_full` (5 peers), `rate_limited` (5 failed joins per IP
or 20 per room in 10 min; 10 creates per IP in 10 min), `bad_request`, `no_peer`
(signal target not in room or disconnected), `resume_failed`.

## Share links

`https://<host>/j/<CODE>#<password>`. The landing page hands the link to the app as
`blite://join/<CODE>#<password>`. The password stays in the URL fragment, so the
server never sees it; the page doesn't check whether the room exists.
