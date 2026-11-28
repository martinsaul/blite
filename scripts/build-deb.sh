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
# Licenses: the app's and every Go module's, plus where to get the source.
# GStreamer comes from the distribution's packages.
lic=$(mktemp -d)
trap 'rm -rf "$pkg" "$lic"' EXIT
cp LICENSE "$lic/LICENSE.txt"
GOOS=linux GOARCH=amd64 CGO_ENABLED=1 python3 -I scripts/third-party-notices.py "$lic/GO-MODULES.txt" ./cmd/blite
scripts/source-notice.sh "$BUILD" linux > "$lic/README.txt"
doc=$pkg/usr/share/doc/blite
install -Dm644 -t "$doc" "$lic/LICENSE.txt" "$lic/GO-MODULES.txt" "$lic/README.txt"
cat > "$doc/copyright" <<COPY
Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/
Upstream-Name: Bellion Share
Source: https://github.com/martinsaul/blite

Files: *
Copyright: 2026 Martin Saul
License: AGPL-3+
 The full text is in /usr/share/doc/blite/LICENSE.txt.

Files: usr/bin/blite
Copyright: 2026 Martin Saul, and the authors of the Go modules built into it
License: AGPL-3+ and others
 The Go modules' licenses are in /usr/share/doc/blite/GO-MODULES.txt.
COPY
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
# Reproducible builds: one mtime for every file (dpkg-deb also reads SOURCE_DATE_EPOCH).
[ -z "${SOURCE_DATE_EPOCH:-}" ] || find "$pkg" -exec touch -h -d "@$SOURCE_DATE_EPOCH" {} +
dpkg-deb --root-owner-group --build "$pkg" "$deb" >/dev/null
ls -l "$deb"

# The per-user copy (update package blite-home): the same binary, which
# /usr/bin/blite starts from ~/.local/share/bellion/app and molt-swap updates
# without root.
home=$(mktemp -d)
trap 'rm -rf "$pkg" "$lic" "$home"' EXIT
install -Dm755 "$out/bin/blite" "$home/bin/blite"
echo "$BUILD" > "$home/build"
cp -r "$lic" "$home/licenses"
zip="$(realpath "$out")/blite-linux-home_${APP_VERSION}-${BUILD}.zip"
rm -f "$zip"
python3 - "$home" "$zip" <<'PY'
import os, sys, zipfile
src, dst = sys.argv[1], sys.argv[2]
with zipfile.ZipFile(dst, "w", zipfile.ZIP_DEFLATED) as z:
    names = sorted(os.path.relpath(os.path.join(d, f), src) for d, _, fs in os.walk(src) for f in fs)
    for name in names:
        mode = 0o755 if name == "bin/blite" else 0o644
        info = zipfile.ZipInfo(name, (1980, 1, 1, 0, 0, 0))
        info.external_attr = (0o100000 | mode) << 16  # a regular file; keeps the exec bit
        info.compress_type = zipfile.ZIP_DEFLATED
        z.writestr(info, open(f"{src}/{name}", "rb").read())
PY
ls -l "$zip"
