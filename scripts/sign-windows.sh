#!/usr/bin/env bash
# Signs Windows PE binaries in place with Azure Trusted Signing (Artifact Signing)
# using a pinned release of jsign.
#
# Usage:
#   scripts/sign-windows.sh TOKEN_FILE FILE...
set -euo pipefail

JSIGN_VERSION="7.6"
JSIGN_SHA256="6cf58aa080e7ff82656f320cff8d4a3bff30408df8766806dcba3740dd839fa5"
JSIGN_URL="https://github.com/ebourg/jsign/releases/download/${JSIGN_VERSION}/jsign-${JSIGN_VERSION}.jar"

SIGN_ENDPOINT=${SIGN_ENDPOINT:-eus.codesigning.azure.net}
SIGN_ALIAS=${SIGN_ALIAS:-ZenithNetwork/ZenithNetwork}
SIGN_TSAURL=${SIGN_TSAURL:-http://timestamp.acs.microsoft.com}

if [ "$#" -lt 2 ]; then
  echo "usage: $0 TOKEN_FILE FILE..." >&2
  exit 2
fi

token_file=$1
shift

if [ ! -f "$token_file" ] || [ ! -r "$token_file" ]; then
  echo "sign-windows: token file not found or not readable: $token_file" >&2
  exit 1
fi

if ! grep -q '[^[:space:]]' "$token_file"; then
  echo "sign-windows: token file is empty: $token_file" >&2
  exit 1
fi

token_abs=$(realpath -s "$token_file")

cache_dir="${XDG_CACHE_HOME:-$HOME/.cache}/jsign"
jar="${cache_dir}/jsign-${JSIGN_VERSION}.jar"

tmp=$(mktemp -d)
jar_tmp=""
cleanup() {
  rm -rf "$tmp"
  if [ -n "$jar_tmp" ]; then
    rm -f "$jar_tmp"
  fi
}
trap cleanup EXIT

verify_jar() {
  local target_jar=$1
  local actual
  actual=$(sha256sum "$target_jar" | awk '{print $1}')
  if [ "$actual" != "$JSIGN_SHA256" ]; then
    printf 'sign-windows: SHA-256 mismatch for %s (expected %s, got %s)\n' \
      "$target_jar" "$JSIGN_SHA256" "$actual" >&2
    return 1
  fi
}

if [ ! -f "$jar" ]; then
  mkdir -p "$cache_dir"
  jar_tmp=$(mktemp "${cache_dir}/.jsign-${JSIGN_VERSION}.jar.XXXXXX")
  curl -fsSL --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 120 \
    -o "$jar_tmp" "$JSIGN_URL"
  verify_jar "$jar_tmp"
  chmod 644 "$jar_tmp"
  mv -f "$jar_tmp" "$jar"
  jar_tmp=""
else
  verify_jar "$jar"
fi

rc=0
for file in "$@"; do
  if [ ! -f "$file" ]; then
    printf '%s: signed FAILED (file not found)\n' "$file"
    rc=1
    continue
  fi

  # Prevent leading '-' (option) or '@' (jsign @filelist expansion) ambiguity.
  target=$file
  case "$target" in
    -*|@*) target="./$target" ;;
  esac

  if java -jar "$jar" \
      --quiet \
      --storetype TRUSTEDSIGNING \
      --keystore "$SIGN_ENDPOINT" \
      --storepass "file:$token_abs" \
      --alias "$SIGN_ALIAS" \
      --alg SHA-256 \
      --tsaurl "$SIGN_TSAURL" \
      --tsmode RFC3161 \
      --replace \
      "$target" >"$tmp/jout" 2>"$tmp/jerr"; then
    printf '%s: signed OK\n' "$file"
  else
    reason=$(awk '
      /^Caused by:/ {
        line = $0
        sub(/^Caused by: ([[:alnum:]_.$]+Exception|[[:alnum:]_.$]+Error): /, "", line)
        sub(/^Caused by: /, "", line)
        cause = line
      }
      /^jsign:/ && first == "" {
        line = $0
        sub(/^jsign: /, "", line)
        first = line
      }
      END {
        if (cause != "") print cause
        else if (first != "") print first
        else print "jsign exited non-zero"
      }
    ' "$tmp/jerr" | tr '\r\n' '  ' | sed 's/[[:space:]]*$//')
    if [ "${SIGN_VERBOSE:-0}" = "1" ]; then
      cat "$tmp/jerr" >&2
    fi
    printf '%s: signed FAILED (%s)\n' "$file" "$reason"
    rc=1
  fi
done

exit "$rc"
