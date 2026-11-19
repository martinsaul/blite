#!/usr/bin/env bash
# Fetches an Azure OAuth2 access token for Azure Trusted Signing (Artifact Signing)
# using the OAuth 2.0 v2.0 client-credentials flow.
#
# Usage:
#   scripts/azure-token.sh TENANT CLIENT SECRET_FILE OUT_FILE
#
# Never reads secrets onto the command line, environment, stdout, or stderr.
set -euo pipefail

if [ "$#" -ne 4 ]; then
  echo "usage: $0 TENANT CLIENT SECRET_FILE OUT_FILE" >&2
  exit 2
fi

tenant=$1
client=$2
secret_file=$3
out_file=$4

if [ -z "$tenant" ] || [ -z "$client" ] || [ -z "$secret_file" ] || [ -z "$out_file" ]; then
  echo "azure-token: all arguments must be non-empty" >&2
  exit 2
fi

case "$tenant" in
  *[/?#[:space:]]*)
    echo "azure-token: invalid tenant identifier" >&2
    exit 2
    ;;
esac

if [ ! -r "$secret_file" ] || [ -d "$secret_file" ]; then
  echo "azure-token: secret file not found or not readable: $secret_file" >&2
  exit 1
fi

umask 077
tmp=$(mktemp -d)
out_tmp=""
cleanup() {
  rm -rf "$tmp"
  if [ -n "$out_tmp" ]; then
    rm -f "$out_tmp"
  fi
}
trap cleanup EXIT

# curl's --data-urlencode name@file preserves trailing newlines as %0A.
# Strip CR/LF into a 0600 temp file so secrets saved with a trailing newline work.
secret_clean="$tmp/client_secret"
tr -d '\r\n' < "$secret_file" > "$secret_clean"
chmod 600 "$secret_clean"
if [ ! -s "$secret_clean" ]; then
  echo "azure-token: secret file is empty: $secret_file" >&2
  exit 1
fi

print_oauth_error() {
  local status=$1
  local body_file=$2
  printf 'HTTP %s\n' "$status" >&2
  if [ -s "$body_file" ]; then
    jq -r '
      if type == "object" then
        (if .error != null then "error: \(.error)" else empty end),
        (if .error_description != null then "error_description: \((.error_description | tostring | gsub("[\r\n]+"; " ")))" else empty end)
      else empty end
    ' "$body_file" >&2 2>/dev/null || true
  fi
}

curl_rc=0
http_code=$(curl -s \
  --proto '=https' --tlsv1.2 \
  --connect-timeout 15 --max-time 60 \
  -X POST \
  --data-urlencode "grant_type=client_credentials" \
  --data-urlencode "client_id=$client" \
  --data-urlencode "client_secret@$secret_clean" \
  --data-urlencode "scope=https://codesigning.azure.net/.default" \
  -o "$tmp/resp.json" \
  -w '%{http_code}' \
  "https://login.microsoftonline.com/$tenant/oauth2/v2.0/token") || curl_rc=$?

if [ "$curl_rc" -ne 0 ]; then
  printf 'HTTP %s\nerror: request_failed (curl exit %d)\n' "${http_code:-000}" "$curl_rc" >&2
  exit 1
fi

if [ "$http_code" != "200" ]; then
  print_oauth_error "$http_code" "$tmp/resp.json"
  exit 1
fi

out_dir=$(dirname "$out_file")
mkdir -p "$out_dir"
out_tmp=$(mktemp "$out_dir/.azure-token.XXXXXX")
chmod 600 "$out_tmp"

if ! jq -e -j '.access_token | select(type == "string" and length > 0)' "$tmp/resp.json" > "$out_tmp" 2>/dev/null; then
  print_oauth_error "$http_code" "$tmp/resp.json"
  echo "error: response missing non-empty access_token" >&2
  exit 1
fi

chmod 600 "$out_tmp"
mv -f "$out_tmp" "$out_file"
out_tmp=""
chmod 600 "$out_file"
