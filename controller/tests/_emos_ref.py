"""emOS's own files, for the tests where EchoHome's code must agree with emOS.

emOS is EchoMuse's (https://github.com/wilbowes/EchoMuse, emos/); its source
is not kept in EchoHome. tools/fetch_emos_ref.py fetches the files named in
tools/emos_ref.json, at the commit pinned there, into .emos-ref/. A missing
or changed file FAILS the test, naming the command: a skip would drop the
proof without anyone noticing.
"""
from __future__ import annotations

import hashlib
import json
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
MANIFEST = ROOT / "tools" / "emos_ref.json"
REF = ROOT / ".emos-ref"
FETCH = "python3 tools/fetch_emos_ref.py"


def pin() -> dict:
    return json.loads(MANIFEST.read_text(encoding="utf-8"))


def emos_file(rel: str) -> Path:
    """The pinned copy of emOS's `rel` (e.g. "emos/init/init.c")."""
    files = pin()["files"]
    if rel not in files:
        pytest.fail(f"{rel} is not pinned: add it to tools/emos_ref.json and run "
                    f"{FETCH} --update")
    p = REF / rel
    if not p.is_file():
        pytest.fail(f"emOS's {rel} is not fetched: run {FETCH}")
    if hashlib.sha256(p.read_bytes()).hexdigest() != files[rel]:
        pytest.fail(f"emOS's {rel} differs from the pinned commit: run {FETCH}")
    return p
