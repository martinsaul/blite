# Relays: bigger groups

Sharing with more than about 5 people? Set up a relay. This is the same
guide as [bellion.app/docs/relays](https://bellion.app/docs/relays).

## Why bigger groups need a relay

In a new room everyone sends their video directly to everyone else (a full
mesh): every sharer uploads their screen once per viewer. In a room of 8,
each sharer uploads it 7 times, which most home connections can't keep up at
good quality. With more than 5 people in a full mesh, Bellion Share warns
about it. With a relay, everyone sends their video once, to the relay, and
the relay sends it on to whoever watches it.

## Relay mode

- Relay mode starts as soon as someone volunteers; the server decides who
  relays.
- The server picks as many volunteers as the room needs, up to 4, preferring
  the ones with the most upload. Each member is homed on one relay; relays
  forward to each other, so a stream crosses at most two. Volunteers the room
  doesn't need yet stand by.
- A relay serves 2 to 16 members, depending on its upload (see
  [How much upload do I need?](#how-much-upload-do-i-need)).
- Relays pull only the streams someone watches. Up to 8 people can share at
  once.
- A relay can make smaller versions (1080p, 720p) of a stream for viewers who
  can't take the top size, if it has a hardware encoder or a CPU less than
  half busy.
- When a relay leaves, its members move to another relay without dropping
  the call, or back to full mesh.
- With end-to-end encryption (the default) a relay is a member like any
  other: it gets the room's keys, forwards the encrypted media unchanged and
  decrypts only what it shrinks for a viewer. Only run a relay on a machine
  you trust with your stream.
- Relay mode needs everyone on build 31 or newer; several relays need build
  56 or newer.

## Three ways to set up a relay

1. [Volunteer from the app](#1-volunteer-from-the-app): easiest; any server.
2. [A dedicated relay machine for your group](#2-a-dedicated-relay-machine-for-your-group):
   a spare PC, home server or VPS; any server, including bellion.app.
3. [A hosted relay on your own server](#3-a-hosted-relay-on-your-own-server):
   fixed rooms open around the clock; needs your own server.

## 1. Volunteer from the app

1. Join the call.
2. Open the **People** menu and choose **Volunteer as relay**.
3. The line under the People table should say *Relays: You relay for 4 people.*
   (how many people you serve), or *You are standing by* if the room doesn't need
   you yet.

To stop, choose **Stop relaying** (or **Leave the relay queue** while
standing by). Volunteer when you have a fast, stable upload (wired helps) and
a computer that isn't busy with a demanding game.

## 2. A dedicated relay machine for your group

Any machine with a fast upload can relay for your group without a screen or
desktop: it runs Bellion Share without a window, joins your room by its share
link and volunteers. Works on any server, including bellion.app. Forwarding
uses little CPU; making smaller versions for viewers is video encoding, so a
hardware encoder or a few free cores help.

**Install.** Linux (Ubuntu 24.04, Mint 22 or newer, server installs too):
`sudo apt install ./blite_*.deb` (installs `/usr/bin/blite` and the GStreamer
plugins). Windows 10/11: unzip the download; the program is `bin\blite.exe`
in the `blite` folder.

**Get the link.** In the app on your own PC: *Room > Copy share link*
(`https://bellion.app/j/CODE#password`). It contains the room password; keep
it private.

**Start the relay.** Linux:

```bash
blite -headless -link 'https://bellion.app/j/CODE#password' \
  -volunteer -source none -view none \
  -name "Group relay" -relay-upload-kbps 50000
```

Windows (in the `blite` folder):

```
bin\blite.exe -headless -link "https://bellion.app/j/CODE#password" -volunteer -source none -view none -name "Group relay" -relay-upload-kbps 50000
```

It prints `room CODE link ...`, then `volunteered as relay`, and a status
line per member every 5 seconds. In the app, the line under the People table
says *Relays: Group relay relays for N people.* Ctrl+C leaves the room cleanly.

| Flag | What it does |
|---|---|
| `-headless` | No window or tray: one session from the command line |
| `-link URL` | The share link to join, on the link's server |
| `-volunteer` | Volunteer as relay as soon as it's in |
| `-source none` | Share nothing. Required: otherwise it tries to share its screen and, without a display, stops right away |
| `-view none` | Show and play nothing (no windows, no decoding) |
| `-name NAME` | Its name in the People table (default: the computer's name) |
| `-relay-upload-kbps N` | Its upload in kbit/s: decides how many members it serves; it never sends faster than this |
| `-udp-port N` | UDP port for all connections (default 30471, or the next free one) |

Don't add `-relay`: it forces all connections through TURN, and a volunteer
only reachable through TURN is never picked as a relay.

**Keep it running.** The relay keeps the room open while it's in it: everyone
else can leave and the same link still works later. It rides out network
drops of up to about a minute; after a longer outage or a server restart it
exits, so let the system restart it.

Linux, systemd: put the link in `/etc/bellion-relay.env` (`chmod 600`):

```
RELAY_LINK="https://bellion.app/j/CODE#password"
```

create a user (`sudo useradd --system --create-home --home-dir /var/lib/bellion-relay bellion-relay`)
and `/etc/systemd/system/bellion-relay.service`:

```ini
[Unit]
Description=Bellion Share relay
Wants=network-online.target
After=network-online.target

[Service]
User=bellion-relay
EnvironmentFile=/etc/bellion-relay.env
ExecStart=/usr/bin/blite -headless -link ${RELAY_LINK} -volunteer \
  -source none -view none -name "Group relay" -relay-upload-kbps 50000
Restart=always
RestartSec=3min

[Install]
WantedBy=multi-user.target
```

then `sudo systemctl daemon-reload && sudo systemctl enable --now bellion-relay`
(`journalctl -u bellion-relay -f` shows its log). Use `Restart=always`, not
`on-failure`: when its session ends the relay exits normally. Keep `RestartSec` at a few
minutes: the server counts failed joins per address, and too many in 10
minutes block joins from it for a while.

Windows: leave the command running in its terminal, or create a Task
Scheduler task (trigger *At startup*; program `...\blite\bin\blite.exe` with
the flags above; *Run whether user is logged on or not*; *If the task fails,
restart every* 5 minutes). The headless relay doesn't update itself: install
new versions from time to time.

**When things change.**
- The room's creator leaves: the room carries on. Ownership passes to whoever
  has been in it longest, which can be the relay, and a relay can't
  disconnect or ban anyone. Owners: hand ownership to a person first
  (*People > their name > Make owner...*).
- The link stops working: a room ends 5 minutes after the last person (relay
  included) leaves, and when the server restarts. Start a new session and give
  the relay its link.
- The relay is a room member with the room's keys. Its name tag changes each
  time it restarts, and the safety code includes it.

## 3. A hosted relay on your own server

`blite -host rooms.json` keeps the listed rooms open around the clock with
fixed codes (links never change) and volunteers as each room's relay. It
needs your own signaling server: fixed codes are authorized with the
server's `BLITE_HOST_KEY`, which the official bellion.app server doesn't hand
out. On bellion.app, use a dedicated relay machine instead.

1. Set up your server ([SELF_HOSTING.md](SELF_HOSTING.md)) with
   `BLITE_HOST_KEY` set to a long random secret.
2. Write `rooms.json` (codes: 8 letters and digits, no I, L, O or U;
   passwords: at least 4 characters; `owners`: identity keys from *Room > Your
   identity key...* of the people who own the room while they're in it):

   ```json
   [
     {"code": "7K3Q-X9MD", "password": "a-room-password", "name": "Our relay",
      "owners": ["<identity key from Room > Your identity key...>"]}
   ]
   ```
3. Run it with good upload and host networking:
   `BLITE_HOST_KEY=<secret> blite -host rooms.json -server wss://share.example.com/ws -relay-upload-kbps 200000`
   (`Dockerfile.host` and `docs/examples/` have an image and a compose file).
4. Share `https://share.example.com/j/7K3QX9MD#a-room-password`.

The `Dockerfile.host` image includes the GStreamer plugins a relay needs to
make smaller versions for viewers (software x264 encoding, so give it a few
free CPU cores).

## How much upload do I need?

Each member costs a relay about 4.5 Mbit/s of upload. The server plans with
70% of the relay's upload (about 6.5 Mbit/s per member), at least 2 and at
most 16 members:

| Relay's upload | Members it serves |
|---|---|
| up to 25 Mbit/s | 2 to 3 |
| 30 Mbit/s | 4 |
| 50 Mbit/s | 7 |
| 100 Mbit/s | 15 |
| 105 Mbit/s or more | 16 |
| not known yet | 4 |

In the app the upload isn't known at first (4 members); later the server uses
the most it has sent (never fewer than 4). Dedicated and hosted relays should
set `-relay-upload-kbps` to their real upload from a speed test (round down).
Up to 4 relays share a room: four 100 Mbit/s relays carry about 60 people.

## How to tell it's working

- The line under the People table: *Relays: Group relay relays for 6 people; Sam
  relays for 4 people; Alex is standing by.* Without a relay: *Everyone sends their
  video to everyone (full mesh).*
- *People > their name > Network path* says *via* the relay's name.
- A dedicated relay logs `volunteered as relay`, `plan ... relays=[...]` and
  per member `upKbps` (what it sends them).

## Troubleshooting

- **Standing by, never relays:** the relays already picked have room for
  everyone.
- **Never picked, even alone:** a volunteer only reachable through TURN is
  never picked. Let it open its UDP port on the router (UPnP, NAT-PMP or PCP,
  on by default) or forward UDP 30471 (or your `-udp-port`) to it, and allow
  that port in its firewall (on a VPS, the provider's firewall too).
- **Two copies on one machine** (app and relay): give the relay its own port,
  e.g. `-udp-port 30480`, and forward that.
- **"Relay mode needs everyone in the room on Bellion Share build 31 or
  newer":** someone needs to update.
- **Exits with "video source":** add `-source none`.
- **Exits with "signaling":** wrong link, the room has ended, or the build is
  too old.
