# Self-hosting Bellion Share

A Bellion Share deployment is one small signaling server. Media flows peer to peer
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
`GET /v1/channel/{id}`, `POST /v1/discord/token` and `POST /v1/discord/interactions` (Discord, only with the
Discord settings), the website (`/`, `/download`, `/docs`, `/terms`,
`/privacy`; the download page only with `BLITE_UPDATE_URL`) and `GET /healthz`.

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
| `BLITE_MAX_PEERS` | `0` | Cap room size (0 = no cap) |
| `BLITE_STATE_FILE` | unset | Keep rooms across restarts: saved every 30 s and on shutdown, loaded on start, reloaded on `SIGHUP` (rooms served since the last load are kept). Members resume into their slots instead of rejoining; after a crash, changes since the last save are lost. It holds password and token hashes, so put it on a private volume |
| `BLITE_COLOR` | unset | Blue/green deploys: this instance's color. With `.ci-deploy-defer-<color>` next to the state file at startup it starts deferred (see [BLUE_GREEN.md](BLUE_GREEN.md)) |
| `BLITE_ADMIN_ADDR` | `127.0.0.1:8081` | Loopback-only listener for the deploy hooks (`-drain`, `-adopt`, `-undrain`, `-status`); `off` = none |
| `BLITE_MIN_COMPATIBLE_BUILD` | `0` | Refuse clients below this build |
| `BLITE_MIN_RECOMMENDED_BUILD` | `0` | Ask clients below this build to update |
| `BLITE_SITE_ALIASES` | unset | Other host names of this server (comma-separated); their web pages redirect to the main site |
| `BLITE_UPDATE_URL` | unset | Update server the download page reads releases from (no download page when unset) |
| `BLITE_DOWNLOAD_CHANNEL` | `beta` | Release channel the download page offers |
| `BLITE_DISCORD_CLIENT_ID` + `BLITE_DISCORD_SECRET` | unset | Discord app credentials for the token exchange (the app only uses Discord with its built-in server for now) |
| `BLITE_DISCORD_PUBLIC_KEY` | unset | Discord app Ed25519 public key (hex) for verifying `POST /v1/discord/interactions` |

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
with host networking, and tell it that bandwidth with
`-relay-upload-kbps` (for example `-relay-upload-kbps 200000` on a fast
port): the server then gives it up to 16 members per room instead of 4,
and it paces what it forwards to that rate. Member volunteers can relay
alongside it when a room outgrows it.

```json
[
  {"code": "7K3Q-X9MD", "password": "a-room-password", "name": "Bellion Share relay",
   "owners": ["<identity key shown in a member's Bellion Share window>"]}
]
```

`docs/examples/` has a compose file running both services and an
`env.example`.

Step by step, with upload sizing and troubleshooting: the relay guide,
[docs/RELAYS.md](RELAYS.md) or [bellion.app/docs/relays](https://bellion.app/docs/relays).
A hosted relay only works with your own server (it needs the server's
`BLITE_HOST_KEY`). With the official bellion.app server, run a
[dedicated relay machine](RELAYS.md#2-a-dedicated-relay-machine-for-your-group)
instead: it joins a room by its share link and volunteers.

## 4. Clients

Official builds can use your server as they are: under *Servers...* in the
gear menu, add `blite.example.com` (or `wss://blite.example.com/ws`) and, if
you like, make it the default. Once a server is added, a picker at the bottom right of
the window chooses where new sessions start. Share links name their server
(`https://blite.example.com/j/CODE#password`, which opens
`bshare://join/CODE?server=blite.example.com#password`), so anyone can join
from a link without adding the server; the first time, the app asks whether
to join a session on a server not run by Bellion Share.

Discord integration isn't available on self-hosted servers yet.

### Certificate pinning

The app can pin the certificate authorities a server's TLS certificate may
come from: Let's Encrypt (ISRG Root X1, X2, YE, YR), Google Trust Services
(GTS Root R1-R4) and SSL.com (its RSA and ECC roots, and the TLS 2022
roots). These are the CAs Cloudflare issues edge certificates from. A
pinned connection is refused when its certificate chains only to another
root the computer trusts, such as a company proxy, antivirus HTTPS
scanning or malware. The app then says the connection is being
intercepted. The roots are in `internal/pin/roots.pem`.

- The built-in server is always pinned. It can't be turned off in the app.
- Servers you add are not pinned until you tick *Pin certificate* next to
  them under *Servers...*. Tick it when your certificate comes from one of
  the CAs above, for example Caddy's or Traefik's Let's Encrypt
  certificates. Leave it off for a private CA or a self-signed certificate.
- `ws://` servers (no TLS) are never pinned.
- Builds with your own `BLITE_DEFAULT_SERVER` pin it like the official
  server does. If its certificate comes from another CA, build with
  `BLITE_PIN_DEFAULT_SERVER=false`.

The pins are CA roots, not the certificate or its key, so certificate
renewals and Cloudflare's own rotations need no app update.

Dev builds connect to `ws://localhost:8080/ws` unless told otherwise. Point
them at your server with `-server wss://blite.example.com/ws` or
`BLITE_SERVER`, or bake it into your builds:

```bash
export BLITE_DEFAULT_SERVER=wss://blite.example.com/ws
# export BLITE_PIN_DEFAULT_SERVER=false   # if its certificate isn't from a pinned CA
scripts/build-windows.sh dist/windows   # or scripts/build-deb.sh dist/linux
```

That server then replaces the built-in default.

Self-update is off unless the build is given an update server and release
signing keys (`BLITE_UPDATE_URL`, `BLITE_UPDATE_KEYS`; see
`scripts/version.sh`). The updater speaks the Molt protocol
(`third_party/molt-client`).
