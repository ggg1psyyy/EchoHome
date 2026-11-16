"""
The Voice Incubator satellite protocol, protocol 2: the satellite's half.

Spec: OHF-Voice/firmware-voice-incubator PROTOCOL.md (protocol 2). The app
(OHF-Voice/voice-incubator) connects to a satellite over TCP and the satellite
says HELLO first; every message is one frame

    uint8 type | uint16 big-endian payload length (at most 4096) | payload

where audio payloads are raw PCM, 16 kHz, 16-bit signed little-endian, mono,
in both directions, and every other payload is one JSON object (UTF-8). An
unknown message type is logged and ignored; unknown JSON fields are ignored.

The controller is the satellite here, one per Echo, the way em_esphome is one
ESPHome device per Echo: the Echo stays the microphone and speaker, and the
controller speaks this on its behalf. This module is the wire format only --
frames, the satellite's messages and the app's -- with no sockets, no Device
and no openwakeword, so the suite tests it from the spec's edges and an
interop check (tools/incubator_interop_check.py) runs it against the app's own
voice_incubator/protocol.py.

What a satellite does NOT declare is as much part of the contract as what it
does. HELLO's absent fields read as false, so this satellite never says
`wake_format` (it runs its own wake words; the app pushes none), `aec` (one
capture: the Echo cancels its echo before the audio leaves it), `monitor`,
`intercom` or `stop_word` (streaming outside a call is not something an Echo
does unasked: docs/listening.md). And it always says `protocol: 2`: the app
reads an absent `protocol` as 1, and a protocol 1 satellite is taken to be a
Voice PE with two captures and mixednet wake words.
"""

from __future__ import annotations

import asyncio
import json
import struct
from dataclasses import dataclass, field
from enum import IntEnum
from typing import Any

PROTOCOL = 2

SERVICE_TYPE = "_voice-incubator._tcp.local."
"""What the app browses for first. Protocol 1's `_voice-labs._tcp` is only
for apps from before the rename, and an Echo was never a Voice Labs device,
so it is not advertised."""

DEFAULT_PORT = 6055
"""The spec's default. A controller serving several Echoes on one address
cannot use it for all of them; the app connects to the port mDNS gives
(voice_incubator/discover.py), so each Echo gets its own."""

HEADER = struct.Struct(">BH")
MAX_PAYLOAD = 4096

SAMPLE_RATE = 16000
CHANNELS = 1
BITS_PER_SAMPLE = 16
BYTES_PER_SAMPLE = BITS_PER_SAMPLE // 8
BYTE_RATE = SAMPLE_RATE * BYTES_PER_SAMPLE


class Message(IntEnum):
    # Satellite to app.
    HELLO = 0x01
    CALL_START = 0x02
    AUDIO_IN = 0x03
    CALL_END = 0x04
    STATUS = 0x05
    PLAYBACK_DRAINED = 0x06
    MODEL_RESULT = 0x07
    AUDIO_IN_AEC = 0x08
    DEVICE_EVENT = 0x09
    AUDIO_MONITOR = 0x0A
    WAKE_REPORT = 0x0B
    AUDIO_MONITOR_AEC = 0x0C
    # App to satellite.
    AUDIO_OUT = 0x10
    AUDIO_OUT_END = 0x11
    EVENT = 0x12
    HANGUP = 0x13
    MONITOR = 0x14
    MODEL_START = 0x20
    MODEL_DATA = 0x21
    MODEL_END = 0x22
    MODEL_CLEAR = 0x23


FROM_SATELLITE = frozenset({
    Message.HELLO, Message.CALL_START, Message.AUDIO_IN, Message.CALL_END,
    Message.STATUS, Message.PLAYBACK_DRAINED, Message.MODEL_RESULT,
    Message.AUDIO_IN_AEC, Message.DEVICE_EVENT, Message.AUDIO_MONITOR,
    Message.WAKE_REPORT, Message.AUDIO_MONITOR_AEC,
})
FROM_APP = frozenset(Message) - FROM_SATELLITE

AUDIO = frozenset({
    Message.AUDIO_IN, Message.AUDIO_IN_AEC, Message.AUDIO_MONITOR,
    Message.AUDIO_MONITOR_AEC, Message.AUDIO_OUT,
})
"""Raw PCM payloads. MODEL_DATA is a binary blob too, but not audio; every
other known type carries JSON."""

BINARY = AUDIO | {Message.MODEL_DATA}

# CALL_START's wake_word when no wake word fired. Any other value is a wake
# word's id (or, for a satellite running its own, its name).
MANUAL = "manual"          # the button: press to talk
FOLLOW_UP = "follow_up"    # the answer HANGUP's `listen` asked for
STOP = "stop"              # "stop" said outside a call

# CALL_END reasons.
TIMEOUT = "timeout"
CANCELLED = "cancelled"

# DEVICE_EVENT types.
ANNOUNCING = "announcing"
ANNOUNCED = "announced"
STOP_TIMER = "stop_timer"
INTERCOM_END = "intercom_end"


class ProtocolError(Exception):
    """The peer broke the framing or sent a control payload that is not a
    JSON object. The connection cannot be trusted past it: close it, and the
    app reconnects."""


# --------------------------------------------------------------- framing

def encode(msg_type: int, payload: bytes = b"") -> bytes:
    """One frame. Refuses what the peer would have to refuse: a type that
    does not fit its byte, or a payload over MAX_PAYLOAD."""
    if not 0 <= int(msg_type) <= 0xFF:
        raise ValueError(f"message type {msg_type} does not fit a byte")
    if len(payload) > MAX_PAYLOAD:
        raise ValueError(f"payload of {len(payload)} bytes exceeds {MAX_PAYLOAD}")
    return HEADER.pack(int(msg_type), len(payload)) + bytes(payload)


def encode_json(msg_type: int, obj: dict[str, Any]) -> bytes:
    """A control frame. `obj` must be a JSON object: the spec has no other
    control payload."""
    if not isinstance(obj, dict):
        raise TypeError("a control payload is a JSON object")
    return encode(msg_type, json.dumps(obj, separators=(",", ":")).encode("utf-8"))


def audio_frames(msg_type: int, pcm: bytes) -> list[bytes]:
    """PCM as frames of at most MAX_PAYLOAD, never splitting a sample (4096
    is a whole number of 16-bit samples). Odd-length PCM is not 16-bit PCM."""
    if msg_type not in AUDIO:
        raise ValueError(f"0x{int(msg_type):02X} is not an audio message")
    if len(pcm) % BYTES_PER_SAMPLE:
        raise ValueError(f"{len(pcm)} bytes is not a whole number of 16-bit samples")
    return [encode(msg_type, pcm[i:i + MAX_PAYLOAD]) for i in range(0, len(pcm), MAX_PAYLOAD)]


@dataclass(frozen=True)
class Frame:
    type: int
    payload: bytes

    @property
    def message(self) -> Message | None:
        """The known message, or None: logged and ignored by the caller."""
        try:
            return Message(self.type)
        except ValueError:
            return None

    def json(self) -> dict[str, Any]:
        """The control payload as a JSON object. An empty payload is `{}`: a
        message that carries nothing (AUDIO_OUT_END, MODEL_CLEAR) may be sent
        with no payload at all."""
        if not self.payload:
            return {}
        try:
            obj = json.loads(self.payload.decode("utf-8"))
        except (UnicodeDecodeError, ValueError) as err:
            raise ProtocolError(f"0x{self.type:02X}: payload is not JSON: {err}") from None
        if not isinstance(obj, dict):
            raise ProtocolError(f"0x{self.type:02X}: payload is JSON but not an object")
        return obj


class FrameReader:
    """Frames out of a byte stream, however it is cut up: a frame split
    across reads, or several in one."""

    def __init__(self) -> None:
        self._buf = bytearray()

    @property
    def buffered(self) -> int:
        return len(self._buf)

    def feed(self, data: bytes) -> list[Frame]:
        self._buf += data
        frames: list[Frame] = []
        while len(self._buf) >= HEADER.size:
            msg_type, length = HEADER.unpack_from(self._buf)
            if length > MAX_PAYLOAD:
                raise ProtocolError(f"0x{msg_type:02X}: payload length {length} exceeds {MAX_PAYLOAD}")
            end = HEADER.size + length
            if len(self._buf) < end:
                break
            frames.append(Frame(msg_type, bytes(self._buf[HEADER.size:end])))
            del self._buf[:end]
        return frames


async def read_frame(reader: asyncio.StreamReader) -> Frame | None:
    """The next frame, or None when the peer closed cleanly between frames.
    A close inside a frame is a ProtocolError: half a frame is not one."""
    try:
        header = await reader.readexactly(HEADER.size)
    except asyncio.IncompleteReadError as err:
        if not err.partial:
            return None
        raise ProtocolError("connection closed inside a frame header") from None
    msg_type, length = HEADER.unpack(header)
    if length > MAX_PAYLOAD:
        raise ProtocolError(f"0x{msg_type:02X}: payload length {length} exceeds {MAX_PAYLOAD}")
    try:
        payload = await reader.readexactly(length) if length else b""
    except asyncio.IncompleteReadError:
        raise ProtocolError(f"0x{msg_type:02X}: connection closed inside its payload") from None
    return Frame(msg_type, payload)


# ------------------------------------------------- satellite to app

def normalise_mac(mac: str) -> str:
    """`aa:bb:cc:dd:ee:ff`. The app keys a satellite on its MAC (HELLO and
    the mDNS TXT record both carry it, and must agree); it strips colons and
    lowercases when it compares, but one spelling everywhere is simpler to
    read in its logs and ours."""
    hexdigits = "".join(c for c in str(mac) if c not in ":-.").lower()
    if len(hexdigits) != 12 or any(c not in "0123456789abcdef" for c in hexdigits):
        raise ValueError(f"not a MAC address: {mac!r}")
    return ":".join(hexdigits[i:i + 2] for i in range(0, 12, 2))


def txt_records(mac: str) -> dict[str, str]:
    """The mDNS TXT record the spec asks for: `mac` and `protocol`."""
    return {"mac": normalise_mac(mac), "protocol": str(PROTOCOL)}


def hello(
    *,
    name: str,
    mac: str,
    model: str,
    firmware: str | None,
    preroll_ms: int,
    output_buffer_bytes: int,
    listen: bool = True,
) -> bytes:
    """HELLO for an Echo. `name` is how Home Assistant knows the satellite
    (its ESPHome device's name) and `mac` what the app keys it on; both must
    be the ones the Echo's ESPHome device already uses, or the app files it
    as a different device from its media player."""
    if not name or not str(name).strip():
        raise ValueError("HELLO needs a name")
    if preroll_ms < 0:
        raise ValueError("preroll_ms is a duration")
    if output_buffer_bytes <= 0:
        raise ValueError("output_buffer_bytes is a size")
    info: dict[str, Any] = {
        "protocol": PROTOCOL,
        "name": str(name),
        "mac": normalise_mac(mac),
        "model": str(model),
        "sample_rate": SAMPLE_RATE,
        "channels": CHANNELS,
        "bits_per_sample": BITS_PER_SAMPLE,
        "preroll_ms": int(preroll_ms),
        "output_buffer_bytes": int(output_buffer_bytes),
        # Said, though absent reads the same: one capture, nothing pushed,
        # no streaming outside a call (module docstring).
        "aec": False,
        "monitor": False,
        "intercom": False,
        "stop_word": False,
        "listen": bool(listen),
    }
    if firmware:
        info["firmware"] = str(firmware)
    return encode_json(Message.HELLO, info)


def probability_byte(p: float) -> int:
    """A 0-1 score as the spec's 0-255."""
    p = float(p)
    if p != p:  # NaN
        raise ValueError("probability is NaN")
    return int(round(min(1.0, max(0.0, p)) * 255))


def call_start(
    wake_word: str,
    preroll_bytes: int,
    *,
    probability: float | None = None,
    max_probability: float | None = None,
    cutoff: float | None = None,
) -> bytes:
    """CALL_START. `preroll_bytes` of the AUDIO_IN that follows come from
    before the detection, and the pre-roll ENDS at the detection: that is
    how the app finds where the wake word ended. `follow_up` and `manual`
    have none."""
    if not wake_word:
        raise ValueError("CALL_START needs a wake_word")
    if preroll_bytes < 0 or preroll_bytes % BYTES_PER_SAMPLE:
        raise ValueError(f"preroll_bytes {preroll_bytes} is not a whole number of samples")
    if wake_word in (MANUAL, FOLLOW_UP) and preroll_bytes:
        raise ValueError(f"a {wake_word} call has no pre-roll")
    info: dict[str, Any] = {"wake_word": wake_word, "preroll_bytes": int(preroll_bytes)}
    for key, value in (("probability", probability), ("max_probability", max_probability), ("cutoff", cutoff)):
        if value is not None:
            info[key] = probability_byte(value)
    return encode_json(Message.CALL_START, info)


def call_end(reason: str) -> bytes:
    if reason not in (TIMEOUT, CANCELLED):
        raise ValueError(f"CALL_END reason is {TIMEOUT} or {CANCELLED}, not {reason!r}")
    return encode_json(Message.CALL_END, {"reason": reason})


def playback_drained() -> bytes:
    return encode_json(Message.PLAYBACK_DRAINED, {})


def device_event(kind: str) -> bytes:
    if kind not in (ANNOUNCING, ANNOUNCED, STOP_TIMER, INTERCOM_END):
        raise ValueError(f"no DEVICE_EVENT {kind!r}")
    return encode_json(Message.DEVICE_EVENT, {"type": kind})


def status(
    *,
    free_bytes: int,
    queued_bytes: int,
    speaker_busy: bool,
    mic_dropped_bytes: int = 0,
) -> bytes:
    """STATUS, for diagnosis only (the spec: about every 200 ms in a call)."""
    return encode_json(Message.STATUS, {
        "free_bytes": max(0, int(free_bytes)),
        "queued_bytes": max(0, int(queued_bytes)),
        "queued_ms": max(0, int(queued_bytes)) * 1000 // BYTE_RATE,
        "speaker_busy": bool(speaker_busy),
        "mic_dropped_bytes": max(0, int(mic_dropped_bytes)),
        "aec_dropped_bytes": 0,
    })


def model_refused(model_id: str | None, error: str) -> bytes:
    """MODEL_RESULT `ok: false`. The app pushes models only to a satellite
    that said `wake_format`, which this one never does; this is what it
    gets if it pushes anyway."""
    info: dict[str, Any] = {"ok": False, "models": 0, "error": str(error)}
    if model_id:
        info["id"] = str(model_id)
    return encode_json(Message.MODEL_RESULT, info)


# ------------------------------------------------- app to satellite

@dataclass(frozen=True)
class Hangup:
    """End the call; with `listen`, open the microphone again for the answer
    (CALL_START `follow_up`), unless muted."""
    reason: str = ""
    listen: bool = False

    @classmethod
    def parse(cls, info: dict[str, Any]) -> Hangup:
        return cls(reason=str(info.get("reason") or ""), listen=info.get("listen") is True)


@dataclass(frozen=True)
class Event:
    """What is happening, to show on the ring. A satellite shows what it can
    and ignores the rest, so an unknown `type` is not an error."""
    type: str
    data: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def parse(cls, info: dict[str, Any]) -> Event:
        data = info.get("data")
        return cls(type=str(info.get("type") or ""), data=data if isinstance(data, dict) else {})


@dataclass(frozen=True)
class Monitor:
    active: bool = False
    intercom: bool = False

    @classmethod
    def parse(cls, info: dict[str, Any]) -> Monitor:
        return cls(active=info.get("active") is True, intercom=info.get("intercom") is True)
