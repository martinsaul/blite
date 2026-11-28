#!/usr/bin/env bash
# Prints the README for a package's licenses folder: what is included, under
# which licenses, and where to get the source.
#   scripts/source-notice.sh BUILD windows GST_VERSION
#   scripts/source-notice.sh BUILD linux
set -euo pipefail
build=$1 target=$2
cat <<TXT
Bellion Share build $build: licenses and source code

Bellion Share is free software under the GNU Affero General Public License,
version 3 or later (LICENSE.txt). The source code of this build is at
https://github.com/martinsaul/blite (tag b$build). "Building from source" in
its README explains how to build it.

GO-MODULES.txt holds the licenses of the Go modules built into Bellion Share.
TXT
case $target in
windows)
  gstver=$3
  cat <<TXT

The bin and lib\\gstreamer-1.0 folders hold DLLs from the official GStreamer
$gstver MSVC x86_64 runtime, unmodified. GStreamer and most of its libraries
are under the GNU Lesser General Public License (LGPL); x264 and a few others
are under the GNU General Public License (GPL) version 2 or later. The
gstreamer folder has the license texts from that runtime, one folder per
project; README-LICENSE-INFO.txt in each names the license.

Source code for GStreamer and the libraries it ships:
  https://gstreamer.freedesktop.org/src/ (GStreamer $gstver)
  https://gitlab.freedesktop.org/gstreamer/cerbero (tag $gstver): the recipes
  that build the runtime, with each library's version and source URL.

The DLLs are loaded at run time. To use a modified build of one, build it for
Windows x86_64 against GStreamer $gstver and put it in place of the DLL with
the same name. scripts/build-windows.sh in the source builds this zip.
TXT
  ;;
linux)
  cat <<TXT

GStreamer and the other libraries Bellion Share uses come from your
distribution's packages, each with its own license and source.
scripts/build-deb.sh in the source builds this package.
TXT
  ;;
*) echo "unknown target $target" >&2; exit 1 ;;
esac
