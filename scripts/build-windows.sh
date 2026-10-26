#!/usr/bin/env bash
# Cross-builds the Windows client and a portable zip from Linux.
#   scripts/build-windows.sh [out-dir]
# Needs: gcc-mingw-w64-x86-64 and the GStreamer 1.28 MSVC SDK unpacked under
# $BLITE_WINSDK (default ~/.cache/blite-winsdk/msvc128). 1.28 ships only an
# Inno Setup .exe; unpack it with Wine (32-bit wine needed, e.g. in a
# debian:13 container with wine32:i386):
#   wine gstreamer-1.0-msvc-x86_64-1.28.8.exe /VERYSILENT /CURRENTUSER /TYPE=full /DIR=Z:\<dir>\msvc_x86_64
# 1.28+ because 1.26 wasapi2 process loopback fails to initialize
# (E_INVALIDARG, upstream 4257cd85) and stalls after Reset (8eaf77e7).
# MSVC (not MinGW) GStreamer because only it ships wasapi2 (process loopback).
# The MSVC SDK includes MinGW import libs, so mingw-gcc links fine.
set -euo pipefail
cd "$(dirname "$0")/.."
out=${1:-dist/windows}
sdk=${BLITE_WINSDK:-$HOME/.cache/blite-winsdk/msvc128}
gst=$(find "$sdk" -type d -name msvc_x86_64 | head -1)
[ -n "$gst" ] || { echo "GStreamer MSVC SDK not found under $sdk" >&2; exit 1; }

# A separate build cache: cgo objects from a MinGW-GStreamer build would
# otherwise be reused silently and link the wrong DLL names.
export GOCACHE=${GOCACHE_WINDOWS:-$HOME/.cache/blite-gocache-windows-128}
export GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc
export PKG_CONFIG_LIBDIR=$gst/lib/pkgconfig PKG_CONFIG_PATH=$gst/lib/pkgconfig

rm -rf "$out" && mkdir -p "$out/blite/bin" "$out/blite/lib/gstreamer-1.0"
eval "$(scripts/version.sh)"
go build -trimpath -ldflags "$LDFLAGS" -o "$out/blite/bin/blite.exe" ./cmd/blite
# The crash guard is pure Go and has no console window.
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -H windowsgui" -o "$out/blite/blite-guard.exe" ./cmd/blite-guard

plugins="coreelements app videoconvertscale videorate videotestsrc audiotestsrc audioconvert
audioresample audiomixer opus x264 openh264 videoparsersbad d3d11 nvcodec qsv amfcodec
mediafoundation wasapi2 playback typefindfunctions autodetect libav volume pango inter compositor"
for p in $plugins; do cp "$gst/lib/gstreamer-1.0/gst$p.dll" "$out/blite/lib/gstreamer-1.0/"; done

# Copy every DLL the exe and plugins import (transitively) from the runtime bin.
declare -A seen
queue=("$out/blite/bin/blite.exe" "$out"/blite/lib/gstreamer-1.0/*.dll)
while [ ${#queue[@]} -gt 0 ]; do
  f=${queue[0]}; queue=("${queue[@]:1}")
  for dll in $(x86_64-w64-mingw32-objdump -p "$f" | awk '/DLL Name:/ {print $3}'); do
    [ -n "${seen[$dll]:-}" ] && continue
    seen[$dll]=1
    if [ -f "$gst/bin/$dll" ]; then
      cp "$gst/bin/$dll" "$out/blite/bin/"
      queue+=("$gst/bin/$dll")
    fi
  done
done

cat > "$out/blite/README.txt" <<TXT
Blite $APP_VERSION ($BUILD) (Windows, POC)

Run blite-guard.exe. A tray icon appears and Blite opens in your browser.
To have Blite start when you sign in, tick "Start Blite when I sign in" in its window.
Share links open in Blite once it has run once.
Per-app audio needs Windows 11 (or Windows 10 build 20348+).
Updates: Blite checks the ${BLITE_DEFAULT_CHANNEL:-beta} channel and offers new builds in its window.
Logs: %LOCALAPPDATA%\\Blite\\logs. Crash dumps: %LOCALAPPDATA%\\Blite\\crash.
TXT
(cd "$out" && rm -f blite-windows.zip && python3 -I -m zipfile -c blite-windows.zip blite)
du -sh "$out/blite" "$out/blite-windows.zip"
