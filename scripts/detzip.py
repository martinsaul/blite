#!/usr/bin/env python3
"""Writes a reproducible zip of a directory: entries sorted by path, one fixed
timestamp (SOURCE_DATE_EPOCH, else 1980-01-01), fixed permissions (0644 files, 0755 dirs), deflate.
The same tree always gives the same bytes.
  python3 -I scripts/detzip.py OUT.zip DIR [PREFIX]
Entries are named PREFIX/<path below DIR> (default: the basename of DIR)."""
import os, sys, time, zipfile

def main():
    out, root = sys.argv[1], os.path.normpath(sys.argv[2])
    prefix = sys.argv[3] if len(sys.argv) > 3 else os.path.basename(root)
    epoch = int(os.environ.get("SOURCE_DATE_EPOCH", "315532800"))
    stamp = time.gmtime(max(epoch, 315532800))[:6]
    files, dirs = [], []
    for d, sub, names in os.walk(root):
        for n in sub:
            p = os.path.join(d, n)
            if os.path.islink(p):
                sys.exit(f"detzip: refusing symlink {p}")
            dirs.append(p)
        for n in names:
            p = os.path.join(d, n)
            if os.path.islink(p) or not os.path.isfile(p):
                sys.exit(f"detzip: refusing non-regular file {p}")
            files.append(p)
    def name(p):
        rel = os.path.relpath(p, root).replace(os.sep, "/")
        return rel if prefix in ("", ".") else prefix + "/" + rel
    entries = [(name(p) + "/", None) for p in dirs] + [(name(p), p) for p in files]
    if prefix not in ("", "."):
        entries.append((prefix + "/", None))
    entries.sort()
    tmp = out + ".tmp"
    with zipfile.ZipFile(tmp, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as zf:
        for arc, p in entries:
            info = zipfile.ZipInfo(arc, date_time=stamp)
            info.create_system = 3
            if p is None:
                info.external_attr = (0o40755 << 16) | 0x10
                zf.writestr(info, b"")
            else:
                info.external_attr = 0o100644 << 16
                info.compress_type = zipfile.ZIP_DEFLATED
                with open(p, "rb") as f:
                    zf.writestr(info, f.read(), compress_type=zipfile.ZIP_DEFLATED, compresslevel=9)
    os.replace(tmp, out)

main()
