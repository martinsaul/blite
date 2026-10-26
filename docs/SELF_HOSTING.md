# Self-hosting Blite

A Blite deployment is one small signaling server. Media flows peer to peer
(or through a member acting as relay); the server only introduces peers.
Optionally add a TURN server for peers behind strict NAT, and a hosted relay
that keeps fixed rooms open.

## 1. Signaling server

`blite-signal` is a single static Go binary (no cgo) listening on plain HTTP.

```bash
go build -o bin/blite-signal ./cmd/blite-signal
BLITE_ADDR=:8080 ./bin/blite-signal
# or
docker build -f Dockerfile.signal -t blite-signal .
docker run -p 127.0.0.1:8080:8080 blite-signal
```

Routes: `GET /ws` (WebSocket signaling), `GET /j/{code}` (share-link landing
page), `POST /v1/report` (client log reports, only with `BLITE_REPORT_DIR`),
`GET /healthz`.

Put it behind any reverse proxy that terminates TLS and passes WebSocket
upgrades (nginx, Caddy, HAProxy, ...), e.g. with Caddy:

```
blite.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Set `BLITE_TRUST_PROXY=true` only when the proxy sets `X-Forwarded-For`
(rate limits are per client IP). Keep proxy idle timeouts above 30 s; the
server pings every 30 s.

| Env | Default | Meaning |
|---|---|---|
| `BLITE_ADDR` | `:8080` | Listen address |
| `BLITE_TRUST_PROXY` | `false` | Take the client IP from `CF-Connecting-IP` / `X-Forwarded-For` |
| `BLITE_TURN_SECRET` + `BLITE_TURN_URLS` | unset | coturn `use-auth-secret` credentials handed to peers (6 h TTL) |
| `BLITE_ICE_JSON` | unset | Fixed ICE server list (JSON array) instead |
| `BLITE_REPORT_DIR` | unset | Accept client log reports into this directory (kept 30 days) |
| `BLITE_HOST_KEY` | unset | Shared secret with a hosted relay (fixed room codes) |
| `BLITE_MIN_COMPATIBLE_BUILD` | `0` | Refuse clients below this build |
| `BLITE_MIN_RECOMMENDED_BUILD` | `0` | Ask clients below this build to update |

With no ICE settings peers get public STUN only, so peers behind symmetric
NAT can't connect; add TURN for those.

## 2. TURN (optional)

Run [coturn](https://github.com/coturn/coturn) with
`use-auth-secret` and `static-auth-secret=<secret>`, open its ports
(3478/udp, and 5349/tcp for TLS), and give the signal server the same secret
plus the URLs:

```
BLITE_TURN_SECRET=<secret>
BLITE_TURN_URLS=turn:turn.example.com:3478?transport=udp,turns:turn.example.com:5349?transport=tcp
```

## 3. Hosted relay (optional)

`blite -host rooms.json` keeps the listed rooms open around the clock and
volunteers as each room's relay, so a room works without a member's machine
relaying. It needs the GStreamer runtime (see `Dockerfile.host`) and the
signal server's `BLITE_HOST_KEY`. Give it good upload bandwidth and run it
with host networking.

```json
[
  {"code": "7K3Q-X9MD", "password": "a-room-password", "name": "Blite relay",
   "owners": ["<identity key shown in a member's Blite window>"]}
]
```

`docs/examples/` has a compose file running both services and an
`env.example`.

## 4. Clients

Clients connect to `ws://localhost:8080/ws` unless told otherwise. Point them
at your server with `-server wss://blite.example.com/ws` or
`BLITE_SERVER`, or bake it into your builds:

```bash
export BLITE_DEFAULT_SERVER=wss://blite.example.com/ws
scripts/build-windows.sh dist/windows   # or scripts/build-deb.sh dist/linux
```

Share links look like `https://blite.example.com/j/CODE#password`; the
joining client still uses its own configured server, so everyone in a room
must point at the same one.

Self-update is off unless the build is given an update server and release
signing keys (`BLITE_UPDATE_URL`, `BLITE_UPDATE_KEYS`; see
`scripts/version.sh`). The updater speaks the Molt protocol
(`third_party/molt-client`).
