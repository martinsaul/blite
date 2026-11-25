#!/usr/bin/env bash
# Unzips a Windows portable build zip, signs every .exe inside (including
# bin/blite.exe and blite-guard.exe), and rezips next to the original as
# *-signed.zip with the identical directory layout.
#
# Usage:
#   scripts/sign-zip.sh TOKEN_FILE ZIP
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 TOKEN_FILE ZIP" >&2
  exit 2
fi

token_file=$1
zip_file=$2

if [ ! -f "$token_file" ] || [ ! -r "$token_file" ]; then
  echo "sign-zip: token file not found or not readable: $token_file" >&2
  exit 1
fi

if [ ! -f "$zip_file" ] || [ ! -r "$zip_file" ]; then
  echo "sign-zip: zip file not found or not readable: $zip_file" >&2
  exit 1
fi

case "$zip_file" in
  *-signed.[zZ][iI][pP])
    echo "sign-zip: refusing input that already ends in -signed.zip: $zip_file" >&2
    exit 1
    ;;
  *.[zZ][iI][pP])
    out_zip="${zip_file%.[zZ][iI][pP]}-signed.zip"
    ;;
  *)
    out_zip="${zip_file}-signed.zip"
    ;;
esac

script_dir=$(cd "$(dirname "$0")" && pwd)
token_abs=$(realpath -s "$token_file")
zip_abs=$(realpath -s "$zip_file")
out_abs=$(realpath -s "$out_zip")

tmp=$(mktemp -d)
out_tmp=""
cleanup() {
  rm -rf "$tmp"
  if [ -n "$out_tmp" ]; then
    rm -f "$out_tmp"
  fi
}
trap cleanup EXIT

# Reject archives containing absolute paths or '..' traversal entries.
python3 -I - "$zip_abs" <<'PY'
import os, sys, zipfile
with zipfile.ZipFile(sys.argv[1], "r") as zf:
    for info in zf.infolist():
        norm = os.path.normpath(info.filename)
        if os.path.isabs(info.filename) or norm == ".." or norm.startswith("../"):
            sys.exit(f"sign-zip: unsafe archive entry: {info.filename}")
PY

mkdir -p "$tmp/work"
if command -v unzip >/dev/null 2>&1; then
  unzip -q "$zip_abs" -d "$tmp/work"
else
  python3 -I -m zipfile -e "$zip_abs" "$tmp/work"
fi

mapfile -d '' -t found_exes < <(cd "$tmp/work" && find . -type f -iname '*.exe' -print0 | LC_ALL=C sort -z)
if [ "${#found_exes[@]}" -eq 0 ]; then
  echo "sign-zip: no .exe files found in $zip_file" >&2
  exit 1
fi

rel_exes=()
printf 'Executables in %s (%d):\n' "$zip_file" "${#found_exes[@]}"
for exe in "${found_exes[@]}"; do
  rel="${exe#./}"
  rel_exes+=("$rel")
  printf '  %s\n' "$rel"
done

(cd "$tmp/work" && "$script_dir/sign-windows.sh" "$token_abs" "${rel_exes[@]}")

out_tmp="${out_abs}.tmp.$$"
rm -f "$out_tmp"
python3 -I "$script_dir/detzip.py" "$out_tmp" "$tmp/work" .
mv -f "$out_tmp" "$out_zip"
out_tmp=""

sha256sum "$out_zip"
