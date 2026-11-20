#!/usr/bin/env bash
# Prints the build stamp: version name (VERSION file), build number and build
# date. The build number is the commit count of HEAD, which only grows on dev;
# a BUILD file overrides it, and a git archive carries it in BUILDINFO. Releases follow
# beta by default; BLITE_DEFAULT_CHANNEL=stable for a stable release.
#
# Distribution settings (all optional) come from the environment or from the
# file named by BLITE_BUILD_ENV:
#   BLITE_DEFAULT_SERVER  signaling URL the app uses without -server
#   BLITE_DEFAULT_SERVER_ALIASES  other host names of that server (comma-separated)
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
# The build number: a BUILD file (releases), else the commit count from git,
# else BUILDINFO as stamped by git archive (CI deploys; needs the build-root
# tag and full history where the archive is made).
stamped() {
  local d
  d=$(sed -n 's/^describe=//p' BUILDINFO 2>/dev/null)
  case "$d" in
    build-root) echo 1 ;;
    build-root-*-g*) d=${d#build-root-}; echo $((${d%%-g*} + 1)) ;;
    *) return 1 ;;
  esac
}
build=$(cat BUILD 2>/dev/null || git rev-list --count HEAD 2>/dev/null || stamped) || {
  echo "version.sh: no build number (no BUILD file, no git, BUILDINFO unstamped)" >&2
  exit 1
}
date=$(date -u '+%Y-%m-%d %H:%M')
ld="-s -w -X main.version=$app -X main.build=$build -X \"main.buildDate=$date\" -X main.defaultChannel=${BLITE_DEFAULT_CHANNEL:-beta}"
[ -z "${BLITE_DEFAULT_SERVER:-}" ] || ld="$ld -X main.defaultServer=$BLITE_DEFAULT_SERVER"
[ -z "${BLITE_DEFAULT_SERVER_ALIASES:-}" ] || ld="$ld -X main.defaultServerAliases=$BLITE_DEFAULT_SERVER_ALIASES"
[ -z "${BLITE_UPDATE_URL:-}" ] || ld="$ld -X blite/internal/update.serverURL=$BLITE_UPDATE_URL"
[ -z "${BLITE_UPDATE_KEYS:-}" ] || ld="$ld -X blite/internal/update.trustedKeys=$BLITE_UPDATE_KEYS"
echo "APP_VERSION='$app' BUILD='$build' BUILD_DATE='$date'"
echo "LDFLAGS='$ld'"
[ -z "${BLITE_MAINTAINER:-}" ] || echo "export BLITE_MAINTAINER=$(printf %q "$BLITE_MAINTAINER")"
