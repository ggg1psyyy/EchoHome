"""Interop vectors for internal/incubator against the Voice Incubator app's
own protocol module (voice_incubator/protocol.py), at the commit
tools/mixednet_ref/build.sh checks out and sha256-checks.

    python3 -I gen.py <voice-incubator checkout> <go_frames.bin> <testdata dir>

Reads the frames the Go codec wrote (go_frames.bin), parses each the way the
app does (HEADER, Message, json, Hello.parse) and writes what the app made of
them to interop_app_view.json; and writes interop_app_frames.bin, every
app-to-satellite message encoded by the app's own encode/encode_json.
"""
import dataclasses
import hashlib
import json
import sys
from pathlib import Path

app, go_frames, out = Path(sys.argv[1]), Path(sys.argv[2]), Path(sys.argv[3])
proto_path = app / "voice_incubator" / "protocol.py"
SHA = "bbcb4f1bc1b971a4a5ae4fbedb3079d093237c00352ba4a9a1246fe648689624"  # the pinned protocol.py
got = hashlib.sha256(proto_path.read_bytes()).hexdigest()
if got != SHA:
    sys.exit(f"protocol.py is not the pinned one: {got}")
sys.path.insert(0, str(app))
from voice_incubator import protocol as p  # noqa: E402

view = []
data = go_frames.read_bytes()
pos = 0
while pos < len(data):
    t, n = p.HEADER.unpack_from(data, pos)
    payload = data[pos + p.HEADER.size : pos + p.HEADER.size + n]
    pos += p.HEADER.size + n
    entry = {"type": p.Message(t).name, "length": n}
    if p.Message(t) in (p.Message.AUDIO_IN, p.Message.AUDIO_IN_AEC,
                        p.Message.AUDIO_MONITOR, p.Message.AUDIO_MONITOR_AEC):
        entry["sha256"] = hashlib.sha256(payload).hexdigest()
    elif n:
        obj = json.loads(payload)
        entry["json"] = obj
        if p.Message(t) == p.Message.HELLO:
            entry["hello"] = dataclasses.asdict(p.Hello.parse(obj))
    view.append(entry)
(out / "interop_app_view.json").write_text(json.dumps(view, indent=1, sort_keys=True) + "\n")

pcm = bytes(range(256)) * 16  # 4096 bytes
frames = b"".join([
    p.encode(p.Message.AUDIO_OUT, pcm),
    p.encode(p.Message.AUDIO_OUT, pcm[:2]),
    p.encode(p.Message.AUDIO_OUT_END),
    p.encode_json(p.Message.EVENT, {"type": "listening", "data": {"stage": "listening"}}),
    p.encode_json(p.Message.EVENT, {"type": "timer_ringing", "data": {"timers": 2, "name": "Pasta"}}),
    p.encode_json(p.Message.EVENT, {"type": "processing"}),
    p.encode_json(p.Message.HANGUP, {"reason": "done", "listen": True}),
    p.encode_json(p.Message.HANGUP, {"reason": "not_recognized"}),
    p.encode_json(p.Message.MONITOR, {"active": True, "intercom": False}),
    p.encode_json(p.Message.MONITOR, {"active": False}),
    p.encode(p.Message.MODEL_CLEAR),
    p.encode_json(p.Message.MODEL_START, {
        "format": "mixednet", "slot": 0, "id": "okay_nabu", "wake_word": "Okay Nabu", "kind": "wake",
        "size": 46880, "sha256": "ab" * 32, "probability_cutoff": 0.85, "sliding_window_size": 5,
        "feature_step_size": 10}),
    p.encode(p.Message.MODEL_DATA, pcm),
    p.encode_json(p.Message.MODEL_END, {"id": "okay_nabu"}),
    p.encode_json(p.Message.EVENT, {"type": "night", "data": {"on": True}, "unknown_field": 1}),
])
(out / "interop_app_frames.bin").write_bytes(frames)
print(f"{len(view)} Go frames read by the app; app frames {len(frames)} bytes")
