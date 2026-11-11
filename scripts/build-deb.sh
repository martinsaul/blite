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
Name=Bellion Share
Comment=Peer-to-peer screen sharing
Exec=/usr/bin/blite %u
Icon=video-display
Terminal=false
Categories=Network;
MimeType=x-scheme-handler/bshare;x-scheme-handler/blite;
DESK
cat > "$pkg/DEBIAN/control" <<CTRL
Package: blite
Version: $ver
Architecture: amd64
Maintainer: ${BLITE_MAINTAINER:-Bellion Share <blite@localhost>}
Depends: gstreamer1.0-plugins-base, gstreamer1.0-plugins-good, gstreamer1.0-plugins-bad, gstreamer1.0-plugins-ugly, gstreamer1.0-libav, gstreamer1.0-x, gstreamer1.0-pipewire, pipewire-bin, pulseaudio-utils | pipewire-pulse, xdg-utils, libgl1, libx11-6, libxcursor1, libxrandr2, libxinerama1, libxi6, libxxf86vm1, libwayland-client0, libwayland-cursor0, libwayland-egl1, libxkbcommon0
Recommends: gstreamer1.0-vaapi
Section: net
Priority: optional
Description: Bellion Share - peer-to-peer screen sharing with friends
 Share your screen, a window and app audio with friends, up to 4K60,
 directly peer to peer and end-to-end encrypted.
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
