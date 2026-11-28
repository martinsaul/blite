#!/usr/bin/env python3
# Writes the license texts of every Go module linked into the given packages,
# plus the Go standard library's, to one notices file.
#   GOOS=windows CGO_ENABLED=1 python3 -I scripts/third-party-notices.py out.txt ./cmd/blite ./cmd/blite-guard
# GOOS/GOARCH/CGO_ENABLED come from the environment so the list matches the
# build. Fails if a module has no license file, so a new dependency can't ship
# without one.
import json, os, subprocess, sys

NAMES = ("license", "licence", "copying", "notice", "patents", "unlicense")


def license_files(d):
    out = []
    for name in sorted(os.listdir(d), key=str.lower):
        p = os.path.join(d, name)
        if not name.lower().startswith(NAMES):
            continue
        if os.path.isfile(p):
            out.append(p)
        elif os.path.isdir(p):  # REUSE-style LICENSES/ directory
            out += [os.path.join(p, f) for f in sorted(os.listdir(p)) if os.path.isfile(os.path.join(p, f))]
    return out


def main():
    dst, pkgs = sys.argv[1], sys.argv[2:]
    raw = subprocess.run(["go", "list", "-deps", "-json=Module", *pkgs], check=True, capture_output=True, text=True).stdout
    mods = {}
    for obj in split(raw):
        m = obj.get("Module")
        # Skip the main module and modules replaced by a directory in this repo:
        # both are covered by the app's own LICENSE.
        rep = m.get("Replace") if m else None
        if not m or m.get("Main") or (rep and not rep.get("Version")):
            continue
        mods[m["Path"]] = m
    goroot = subprocess.run(["go", "env", "GOROOT"], check=True, capture_output=True, text=True).stdout.strip()
    gover = subprocess.run(["go", "env", "GOVERSION"], check=True, capture_output=True, text=True).stdout.strip()
    sections = [("Go standard library " + gover, [os.path.join(goroot, "LICENSE"), os.path.join(goroot, "PATENTS")])]
    missing = []
    for path in sorted(mods, key=str.lower):
        m = mods[path]
        files = license_files(m["Dir"])
        if not files:
            missing.append(path)
        sections.append((f"{path} {m.get('Version', '')}".strip(), files))
    if missing:
        sys.exit("no license file found for: " + ", ".join(missing))
    with open(dst, "w", encoding="utf-8", newline="\n") as f:
        f.write("Bellion Share includes the following Go modules. Their licenses follow.\n\n")
        for title, _ in sections:
            f.write(f"  {title}\n")
        for title, files in sections:
            for p in files:
                if not os.path.isfile(p):
                    continue
                f.write("\n" + "=" * 78 + "\n")
                f.write(f"{title}: {os.path.basename(p)}\n")
                f.write("=" * 78 + "\n\n")
                with open(p, encoding="utf-8", errors="replace") as lf:
                    f.write(lf.read().rstrip() + "\n")


def split(raw):
    # go list -json prints concatenated JSON objects.
    dec, i = json.JSONDecoder(), 0
    while True:
        while i < len(raw) and raw[i].isspace():
            i += 1
        if i >= len(raw):
            return
        obj, i = dec.raw_decode(raw, i)
        yield obj


if __name__ == "__main__":
    main()
