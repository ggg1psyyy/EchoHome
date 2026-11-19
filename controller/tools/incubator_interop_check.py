#!/usr/bin/env python3
"""
Check em_incubator_proto against the Voice Incubator app's own protocol code.

tests/test_incubator_proto.py holds the module to OUR reading of PROTOCOL.md.
This holds it to the app that will actually connect: voice_incubator/protocol.py
from OHF-Voice/voice-incubator, at a pinned commit whose file hash is pinned
too, so the check is the same tomorrow and an upstream change is a decision to
re-pin rather than a silent difference.

    message numbers   every message the app knows has our number and name
    constants         protocol 2, 4096-byte payloads, 16 kHz 16-bit mono
    service type      the app browses for the one we advertise
    our HELLO         read by the app's own Hello.parse: protocol 2, one
                      capture, no wake_format, listen, nothing streamed
                      outside a call
    framing           the app's frames read by our reader, ours by its header

Stdlib only, and the downloaded file is imported on its own (it imports
nothing of the app), so this runs on the controller's Python with no app
installed:

    python tools/incubator_interop_check.py [--file path/to/protocol.py]

Exit 0 when everything agrees, 1 with each difference listed when not.
"""
from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import os
import sys
import tempfile
import urllib.request

HERE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, HERE)

import em_incubator_proto as ours  # noqa: E402

COMMIT = "6f0ab821ad1d9e232cc4efb20d64ee9464034957"  # main, 2026-11-15
SHA256 = "bbcb4f1bc1b971a4a5ae4fbedb3079d093237c00352ba4a9a1246fe648689624"
URL = f"https://raw.githubusercontent.com/OHF-Voice/voice-incubator/{COMMIT}/voice_incubator/protocol.py"


def fetch(path: str | None) -> bytes:
    if path:
        with open(path, "rb") as f:
            return f.read()
    with urllib.request.urlopen(URL, timeout=30) as resp:  # noqa: S310 (pinned https URL)
        data = resp.read()
    digest = hashlib.sha256(data).hexdigest()
    if digest != SHA256:
        sys.exit(f"protocol.py at {COMMIT[:12]} hashes {digest}, pinned {SHA256}: refusing to run it")
    return data


def load(source: bytes):
    with tempfile.TemporaryDirectory() as tmp:
        path = os.path.join(tmp, "incubator_protocol.py")
        with open(path, "wb") as f:
            f.write(source)
        spec = importlib.util.spec_from_file_location("incubator_protocol", path)
        module = importlib.util.module_from_spec(spec)
        # Registered before it runs: its dataclasses look their own module up
        # in sys.modules while being defined.
        sys.modules[spec.name] = module
        spec.loader.exec_module(module)
        return module


def check(theirs) -> list[str]:
    problems: list[str] = []

    def expect(what: str, got, want) -> None:
        if got != want:
            problems.append(f"{what}: app has {got!r}, we have {want!r}")

    # Message numbers: everything the app knows, we know the same way.
    for msg in theirs.Message:
        if msg.name not in ours.Message.__members__:
            problems.append(f"the app knows {msg.name} (0x{int(msg):02X}); we do not")
        else:
            expect(f"Message.{msg.name}", int(msg), int(ours.Message[msg.name]))
    for msg in ours.Message:
        if msg.name not in theirs.Message.__members__:
            problems.append(f"we know {msg.name} (0x{int(msg):02X}); the app does not")

    expect("PROTOCOL", theirs.PROTOCOL, ours.PROTOCOL)
    expect("MAX_PAYLOAD", theirs.MAX_PAYLOAD, ours.MAX_PAYLOAD)
    expect("SAMPLE_RATE", theirs.SAMPLE_RATE, ours.SAMPLE_RATE)
    expect("BYTES_PER_SAMPLE", theirs.BYTES_PER_SAMPLE, ours.BYTES_PER_SAMPLE)
    expect("HEADER", theirs.HEADER.format, ours.HEADER.format)
    if ours.SERVICE_TYPE not in theirs.SERVICE_TYPES:
        problems.append(f"the app browses {theirs.SERVICE_TYPES}, not {ours.SERVICE_TYPE}")

    # Our HELLO, read by the app.
    (frame,) = ours.FrameReader().feed(ours.hello(
        name="interop-echo", mac="02:ec:00:00:00:01", model="echohome-biscuit",
        firmware="v0.0.0-interop", preroll_ms=260, output_buffer_bytes=128000))
    hello = theirs.Hello.parse(frame.json())
    expect("Hello.protocol", hello.protocol, 2)
    expect("Hello.name", hello.name, "interop-echo")
    expect("Hello.mac", hello.mac, "02:ec:00:00:00:01")
    expect("Hello.model", hello.model, "echohome-biscuit")
    expect("Hello.firmware", hello.firmware, "v0.0.0-interop")
    expect("Hello.aec", hello.aec, False)
    expect("Hello.wake_format", hello.wake_format, None)
    expect("Hello.listen", hello.listen, True)
    for flag in ("monitor", "intercom", "stop_word", "monitor_aec"):
        expect(f"Hello.{flag}", getattr(hello, flag), False)

    # Framing, both ways.
    samples = [
        (theirs.Message.AUDIO_OUT, bytes(range(256)) * 16),
        (theirs.Message.AUDIO_OUT_END, b""),
        (theirs.Message.EVENT, json.dumps({"type": "listening", "data": {"stage": "listening"}}).encode()),
        (theirs.Message.HANGUP, json.dumps({"reason": "done", "listen": True}).encode()),
    ]
    stream = b"".join(theirs.encode(t, pl) for t, pl in samples)
    got = [(f.type, f.payload) for f in ours.FrameReader().feed(stream)]
    expect("our reader on the app's frames", got, [(int(t), pl) for t, pl in samples])
    hangup = ours.Hangup.parse(ours.Frame(int(theirs.Message.HANGUP), samples[3][1]).json())
    expect("HANGUP listen", hangup.listen, True)

    for built in (ours.call_start("hey_jarvis", 48000, probability=0.6), ours.call_end(ours.CANCELLED),
                  ours.playback_drained(), ours.device_event(ours.STOP_TIMER)):
        msg_type, length = theirs.HEADER.unpack(built[:theirs.HEADER.size])
        body = built[theirs.HEADER.size:]
        if length != len(body):
            problems.append(f"0x{msg_type:02X}: header says {length} bytes, frame has {len(body)}")
        elif not isinstance(json.loads(body), dict):
            problems.append(f"0x{msg_type:02X}: payload is not a JSON object")

    return problems


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[1])
    parser.add_argument("--file", help="a local copy of voice_incubator/protocol.py instead of the pinned download")
    args = parser.parse_args()
    theirs = load(fetch(args.file))
    problems = check(theirs)
    where = args.file or f"voice-incubator@{COMMIT[:12]}"
    if problems:
        print(f"em_incubator_proto disagrees with {where}:")
        for problem in problems:
            print(f"  - {problem}")
        return 1
    print(f"em_incubator_proto agrees with {where}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
