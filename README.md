# Blite

Peer-to-peer group screen sharing. Up to 5 desktops (Windows, Linux) share
screens and app audio in a WebRTC full mesh, or through one member acting as
relay. The server only introduces peers; it never carries media.

- `cmd/blite`: the desktop app (tray, local web UI, GStreamer + Pion media).
- `cmd/blite-signal`: the signaling server (Go, no cgo). Rooms by code + password.
- `cmd/blite-guard`: Windows crash guard that runs the app and keeps crash dumps.
- `docs/PROTOCOL.md`: signaling wire protocol.
- `docs/SELF_HOSTING.md`: running your own server (TURN, hosted relay, client builds).

## Build and test

```bash
go test -race ./...
go build -o bin/blite-signal ./cmd/blite-signal
BLITE_ADDR=:8080 ./bin/blite-signal
go build -o bin/blite ./cmd/blite   # needs GStreamer 1.20+ development files
./bin/blite -server ws://localhost:8080/ws
```

Release packages: `scripts/build-deb.sh` (Ubuntu 24.04 / Mint 22, in Docker)
and `scripts/build-windows.sh` (cross-built with MinGW GStreamer). Server
image: `docker build -f Dockerfile.signal -t blite-signal .`

## License

Copyright (C) 2026 Martin Saul

Blite is free software: you can redistribute it and/or modify it under the
terms of the GNU Affero General Public License as published by the Free
Software Foundation, either version 3 of the License, or (at your option) any
later version. See [LICENSE](LICENSE).

The Windows release bundles GStreamer and its dependencies (LGPL and other
licenses), which keep their own terms.
