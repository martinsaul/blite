#!/usr/bin/env bash
# Prints the build stamp: version name (VERSION file), build number and build
# date. The build number is the commit count of HEAD, which only grows on dev;
# a BUILD file overrides it for builds from a git archive. Releases follow
# beta by default; BLITE_DEFAULT_CHANNEL=stable for a stable release.
#
# Distribution settings (all optional) come from the environment or from the
# file named by BLITE_BUILD_ENV:
#   BLITE_DEFAULT_SERVER  signaling URL the app uses without -server
#   BLITE_UPDATE_URL      update server; with BLITE_UPDATE_KEYS (release
#                         signing keys, std base64 Ed25519, comma-separated)
#                         the app updates itself
#   BLITE_MAINTAINER      .deb Maintainer field
#
#   eval "$(scripts/version.sh)"   # sets APP_VERSION BUILD BUILD_DATE LDFLAGS
set -euo pipefail
cd "$(dirname "$0")/.."
if [ -n "${BLITE_BUILD_ENV:-}" ]; then
  set -a; . "$BLITE_BUILD_ENV"; set +a
fi
app=$(cat VERSION)
build=$(cat BUILD 2>/dev/null || git rev-list --count HEAD)
date=$(date -u '+%Y-%m-%d %H:%M')
ld="-s -w -X main.version=$app -X main.build=$build -X \"main.buildDate=$date\" -X main.defaultChannel=${BLITE_DEFAULT_CHANNEL:-beta}"
[ -z "${BLITE_DEFAULT_SERVER:-}" ] || ld="$ld -X main.defaultServer=$BLITE_DEFAULT_SERVER"
[ -z "${BLITE_UPDATE_URL:-}" ] || ld="$ld -X blite/internal/update.serverURL=$BLITE_UPDATE_URL"
[ -z "${BLITE_UPDATE_KEYS:-}" ] || ld="$ld -X blite/internal/update.trustedKeys=$BLITE_UPDATE_KEYS"
echo "APP_VERSION='$app' BUILD='$build' BUILD_DATE='$date'"
echo "LDFLAGS='$ld'"
[ -z "${BLITE_MAINTAINER:-}" ] || echo "export BLITE_MAINTAINER=$(printf %q "$BLITE_MAINTAINER")"
