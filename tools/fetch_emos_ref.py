#!/usr/bin/env python3
"""Fetch the emOS files EchoHome's tests check against, at a pinned commit.

emOS is EchoMuse's (https://github.com/wilbowes/EchoMuse, emos/, by Wil
Bowes and the EchoMuse contributors); EchoHome only installs and updates it,
so its source is not kept here. Where EchoHome's code must agree with emOS
(the boot image packer with mkboot.py, the console password hash with init's,
the update's paths and trial marks with init and trialcheck, a dashboard
field with what init parses), its tests read emOS's own files, fetched by
this script from the commit in tools/emos_ref.json and checked against the
sha256 recorded there.

    python3 tools/fetch_emos_ref.py           # fetch into .emos-ref/ (gitignored)
    python3 tools/fetch_emos_ref.py --update  # after moving "commit": re-hash

Standard library only, so CI and a bare checkout can run it before anything
is installed.
"""
from __future__ import annotations

import hashlib
import json
import sys
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
MANIFEST = ROOT / "tools" / "emos_ref.json"
OUT = ROOT / ".emos-ref"


def _sha(b: bytes) -> str:
    return hashlib.sha256(b).hexdigest()


def main(argv: list[str]) -> int:
    update = "--update" in argv
    m = json.loads(MANIFEST.read_text(encoding="utf-8"))
    repo, commit = m["repo"], m["commit"]
    if len(commit) != 40 or any(c not in "0123456789abcdef" for c in commit):
        print(f"emos_ref.json: commit must be a full 40-hex sha, not {commit!r}", file=sys.stderr)
        return 2
    bad = 0
    for rel in sorted(m["files"]):
        url = f"https://raw.githubusercontent.com/{repo}/{commit}/{rel}"
        try:
            with urllib.request.urlopen(url, timeout=60) as r:
                data = r.read()
        except Exception as e:
            print(f"{rel}: not fetched from {url}: {e}", file=sys.stderr)
            bad += 1
            continue
        got = _sha(data)
        if update:
            m["files"][rel] = got
        elif got != m["files"][rel]:
            print(f"{rel}: sha256 {got}, the manifest says {m['files'][rel]}", file=sys.stderr)
            bad += 1
            continue
        dest = OUT / rel
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_bytes(data)
    if update and not bad:
        MANIFEST.write_text(json.dumps(m, indent=2) + "\n", encoding="utf-8")
    (OUT / "COMMIT").write_text(f"{repo}@{commit}\n", encoding="utf-8")
    print(f"{len(m['files']) - bad} of {len(m['files'])} emOS files from {repo}@{commit[:12]} in {OUT}")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
