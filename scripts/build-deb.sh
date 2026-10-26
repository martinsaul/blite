#!/usr/bin/env bash
# Builds blite_<ver>_amd64.deb for Ubuntu 24.04 / Linux Mint 22 inside an
# ubuntu:24.04 container (GStreamer 1.24). Run where Docker is available:
#   scripts/build-deb.sh [out-dir]
set -euo pipefail
cd "$(dirname "$0")/.."
out=${1:-dist/linux}
eval "$(scripts/version.sh)"
ver="$APP_VERSION+$BUILD"
mkdir -p "$out"
docker build -q -f Dockerfile.client-ubuntu2404 --build-arg LDFLAGS="$LDFLAGS" --output "type=local,dest=$out/bin" . >/dev/null

pkg=$(mktemp -d)
trap 'rm -rf "$pkg"' EXIT
install -Dm755 "$out/bin/blite" "$pkg/usr/bin/blite"
install -d "$pkg/usr/share/applications" "$pkg/DEBIAN"
cat > "$pkg/usr/share/applications/blite.desktop" <<DESK
[Desktop Entry]
Type=Application
Name=Blite
Comment=Peer-to-peer screen sharing
Exec=/usr/bin/blite %u
Icon=video-display
Terminal=false
Categories=Network;
MimeType=x-scheme-handler/blite;
DESK
cat > "$pkg/DEBIAN/control" <<CTRL
Package: blite
Version: $ver
Architecture: amd64
Maintainer: ${BLITE_MAINTAINER:-Blite <blite@localhost>}
Depends: gstreamer1.0-plugins-base, gstreamer1.0-plugins-good, gstreamer1.0-plugins-bad, gstreamer1.0-plugins-ugly, gstreamer1.0-libav, gstreamer1.0-x, gstreamer1.0-pipewire, pipewire-bin, pulseaudio-utils | pipewire-pulse, xdg-utils
Recommends: gstreamer1.0-vaapi
Section: net
Priority: optional
Description: Blite - peer-to-peer screen sharing (POC)
 Share your screen and chosen apps' audio with up to 4 people,
 directly peer to peer. Runs as a tray app with a local web UI.
CTRL
cat > "$pkg/DEBIAN/postinst" <<'POST'
#!/bin/sh
set -e
command -v update-desktop-database >/dev/null && update-desktop-database -q /usr/share/applications || true
POST
chmod 755 "$pkg/DEBIAN/postinst"
deb="$out/blite_${APP_VERSION}-${BUILD}_amd64.deb"
dpkg-deb --root-owner-group --build "$pkg" "$deb" >/dev/null
ls -l "$deb"
