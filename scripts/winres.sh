#!/bin/sh
# Writes the Windows resources (icon and version info) of blite.exe and
# blite-guard.exe as .syso files the Go build links in. Usage:
#   scripts/winres.sh <version> <build>   (from build-windows.sh)
# remove them afterwards with: scripts/winres.sh clean
set -eu
cd "$(dirname "$0")/.."
targets="cmd/blite cmd/blite-guard"
if [ "${1:-}" = clean ]; then
	for d in $targets; do rm -f "$d/rsrc_windows_amd64.syso"; done
	exit 0
fi
version=$1 build=$2
IFS=. read -r major minor patch <<V
$version
V
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cp docs/brand/bellion.ico "$tmp/app.ico"
for d in $targets; do
	name=$(basename "$d")
	cat > "$tmp/$name.rc" <<RC
1 ICON "app.ico"
1 VERSIONINFO
FILEVERSION $major,$minor,${patch:-0},$build
PRODUCTVERSION $major,$minor,${patch:-0},$build
FILEOS 0x40004
FILETYPE 0x1
BEGIN
  BLOCK "StringFileInfo"
  BEGIN
    BLOCK "040904B0"
    BEGIN
      VALUE "CompanyName", "Bellion Share"
      VALUE "FileDescription", "Bellion Share"
      VALUE "FileVersion", "$version ($build)"
      VALUE "InternalName", "$name"
      VALUE "OriginalFilename", "$name.exe"
      VALUE "ProductName", "Bellion Share"
      VALUE "ProductVersion", "$version ($build)"
      VALUE "LegalCopyright", "AGPL-3.0-or-later"
    END
  END
  BLOCK "VarFileInfo"
  BEGIN
    VALUE "Translation", 0x409, 1200
  END
END
RC
	(cd "$tmp" && x86_64-w64-mingw32-windres -O coff -i "$name.rc" -o "rsrc.syso")
	mv "$tmp/rsrc.syso" "$d/rsrc_windows_amd64.syso"
done
