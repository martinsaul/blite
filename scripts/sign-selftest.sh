#!/usr/bin/env bash
# Self-test for the Windows signing scripts without real secrets.
# Builds a tiny Windows PE binary in /tmp, runs sign-windows.sh (and exercises
# SHA-256 verification, azure-token.sh, and sign-zip.sh) with dummy inputs,
# and verifies clean authentication failures without crashes or secret leaks.
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
scripts=(
  "$script_dir/azure-token.sh"
  "$script_dir/sign-windows.sh"
  "$script_dir/sign-zip.sh"
  "$script_dir/sign-selftest.sh"
)

echo "==> Syntax check (bash -n)"
bash -n "${scripts[@]}"
echo "bash -n: OK"

echo "==> ShellCheck"
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck "${scripts[@]}"
  echo "shellcheck: OK"
else
  echo "shellcheck: not installed (skipped)"
fi

tmp=$(mktemp -d /tmp/blite-sign-selftest.XXXXXX)
trap 'rm -rf "$tmp"' EXIT

echo "==> Building tiny Windows PE binary in $tmp"
mkdir -p "$tmp/hellosrc"
cat > "$tmp/hellosrc/go.mod" <<'GOMOD'
module selftest

go 1.21
GOMOD
cat > "$tmp/hellosrc/main.go" <<'GO'
package main

import "fmt"

func main() {
	fmt.Println("hello windows")
}
GO
(cd "$tmp/hellosrc" && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$tmp/hello.exe" .)
cp "$tmp/hello.exe" "$tmp/sample.exe"

token_canary="selftest-dummy-token-canary-not-a-secret"
secret_canary="selftest-dummy-secret-canary-not-a-secret"
umask_prev=$(umask)
umask 077
printf '%s\n' "$token_canary" > "$tmp/dummy-token"
printf '%s\n' "$secret_canary" > "$tmp/dummy-secret"
umask "$umask_prev"

echo "==> Checking jsign jar SHA-256 mismatch rejection"
mkdir -p "$tmp/badcache/jsign"
printf 'corrupted-jar-bytes\n' > "$tmp/badcache/jsign/jsign-7.6.jar"
set +e
bad_out=$(XDG_CACHE_HOME="$tmp/badcache" "$script_dir/sign-windows.sh" "$tmp/dummy-token" "$tmp/sample.exe" 2>&1)
bad_rc=$?
set -e
printf '%s\n' "$bad_out"
if [ "$bad_rc" -eq 0 ] || ! grep -q "SHA-256 mismatch" <<<"$bad_out"; then
  echo "FAIL: expected SHA-256 mismatch failure (rc=$bad_rc)" >&2
  exit 1
fi
echo "sha256 mismatch check: OK (exit $bad_rc)"

echo "==> Running sign-windows.sh with dummy token against PE copy"
set +e
win_out=$("$script_dir/sign-windows.sh" "$tmp/dummy-token" "$tmp/sample.exe" 2>&1)
win_rc=$?
set -e
printf '%s\n' "$win_out"

if [ "$win_rc" -eq 0 ] || [ "$win_rc" -ge 128 ]; then
  echo "FAIL: expected clean non-zero exit (<128), got $win_rc" >&2
  exit 1
fi
if [ "$(printf '%s\n' "$win_out" | wc -l)" -ne 1 ]; then
  echo "FAIL: expected exactly one output line per file" >&2
  exit 1
fi
if ! grep -q "signed FAILED" <<<"$win_out"; then
  echo "FAIL: output missing 'signed FAILED'" >&2
  exit 1
fi
if ! grep -Eq "401|403|Unauthorized|AuthenticationFailed|InvalidAuthenticationToken" <<<"$win_out"; then
  echo "FAIL: expected authentication error from Azure endpoint, not a crash" >&2
  exit 1
fi
if grep -Fq "$token_canary" <<<"$win_out"; then
  echo "FAIL: token canary leaked in output" >&2
  exit 1
fi
echo "sign-windows dummy auth failure check: OK (exit $win_rc)"

echo "==> Running sign-zip.sh with dummy token against synthetic Windows zip"
mkdir -p "$tmp/pkg/blite/bin"
cp "$tmp/hello.exe" "$tmp/pkg/blite/bin/blite.exe"
cp "$tmp/hello.exe" "$tmp/pkg/blite/blite-guard.exe"
printf 'readme\n' > "$tmp/pkg/blite/README.txt"
(cd "$tmp/pkg" && python3 -I -m zipfile -c "$tmp/blite-windows.zip" blite)
set +e
zip_out=$("$script_dir/sign-zip.sh" "$tmp/dummy-token" "$tmp/blite-windows.zip" 2>&1)
zip_rc=$?
set -e
printf '%s\n' "$zip_out"
if [ "$zip_rc" -eq 0 ] || [ "$zip_rc" -ge 128 ]; then
  echo "FAIL: sign-zip expected clean non-zero exit, got $zip_rc" >&2
  exit 1
fi
if [ -e "$tmp/blite-windows-signed.zip" ]; then
  echo "FAIL: sign-zip must not produce *-signed.zip when signing fails" >&2
  exit 1
fi
if ! grep -q "blite/bin/blite.exe" <<<"$zip_out" || ! grep -q "blite/blite-guard.exe" <<<"$zip_out"; then
  echo "FAIL: sign-zip did not list expected .exe entries" >&2
  exit 1
fi
if grep -Fq "$token_canary" <<<"$zip_out"; then
  echo "FAIL: token canary leaked in sign-zip output" >&2
  exit 1
fi
echo "sign-zip check: OK (exit $zip_rc)"

echo "==> Running azure-token.sh with dummy credentials"
set +e
tok_out=$("$script_dir/azure-token.sh" \
  "00000000-0000-0000-0000-000000000000" \
  "00000000-0000-0000-0000-000000000000" \
  "$tmp/dummy-secret" \
  "$tmp/out-token" 2>&1)
tok_rc=$?
set -e
printf '%s\n' "$tok_out"
if [ "$tok_rc" -eq 0 ] || [ -e "$tmp/out-token" ]; then
  echo "FAIL: azure-token should fail and not write OUT_FILE on invalid credentials" >&2
  exit 1
fi
if ! grep -Eq '^HTTP [45][0-9][0-9]$' <<<"$tok_out" || ! grep -q '^error:' <<<"$tok_out"; then
  echo "FAIL: azure-token error output missing HTTP status or JSON error field" >&2
  exit 1
fi
if grep -Fq "$secret_canary" <<<"$tok_out"; then
  echo "FAIL: secret canary leaked in azure-token output" >&2
  exit 1
fi
echo "azure-token dummy auth failure check: OK (exit $tok_rc)"

echo "sign-selftest: ALL CHECKS PASSED"
