"""
The Voice Incubator satellite protocol (em_incubator_proto), held to its spec:
OHF-Voice/firmware-voice-incubator PROTOCOL.md, protocol 2.

Written from the spec's text and its edges -- the frame header's byte layout,
a payload of 0, 1, 4096 and 4097 bytes, a frame cut anywhere, a payload that
is not a JSON object, unknown types and fields -- not from the module's own
reading of it. tools/incubator_interop_check.py runs the same module against
the app's real voice_incubator/protocol.py.
"""

import asyncio
import json
import struct

import pytest

import em_incubator_proto as p


# The spec's message table, transcribed: type, name, direction.
SPEC_TABLE = [
    (0x01, "HELLO", "satellite"),
    (0x02, "CALL_START", "satellite"),
    (0x03, "AUDIO_IN", "satellite"),
    (0x04, "CALL_END", "satellite"),
    (0x05, "STATUS", "satellite"),
    (0x06, "PLAYBACK_DRAINED", "satellite"),
    (0x07, "MODEL_RESULT", "satellite"),
    (0x08, "AUDIO_IN_AEC", "satellite"),
    (0x09, "DEVICE_EVENT", "satellite"),
    (0x0A, "AUDIO_MONITOR", "satellite"),
    (0x0B, "WAKE_REPORT", "satellite"),
    (0x0C, "AUDIO_MONITOR_AEC", "satellite"),
    (0x10, "AUDIO_OUT", "app"),
    (0x11, "AUDIO_OUT_END", "app"),
    (0x12, "EVENT", "app"),
    (0x13, "HANGUP", "app"),
    (0x14, "MONITOR", "app"),
    (0x20, "MODEL_START", "app"),
    (0x21, "MODEL_DATA", "app"),
    (0x22, "MODEL_END", "app"),
    (0x23, "MODEL_CLEAR", "app"),
]


# ------------------------------------------------------------ the table

def test_every_message_has_the_spec_number_and_direction():
    assert {m.name: int(m) for m in p.Message} == {name: num for num, name, _ in SPEC_TABLE}
    for num, name, side in SPEC_TABLE:
        msg = p.Message(num)
        assert (msg in p.FROM_SATELLITE) == (side == "satellite"), name
        assert (msg in p.FROM_APP) == (side == "app"), name


def test_the_audio_messages_are_the_pcm_ones():
    assert p.AUDIO == {
        p.Message.AUDIO_IN, p.Message.AUDIO_IN_AEC, p.Message.AUDIO_MONITOR,
        p.Message.AUDIO_MONITOR_AEC, p.Message.AUDIO_OUT,
    }


def test_the_audio_format_is_the_only_one():
    # "16 kHz, 16-bit signed little-endian, mono, in both directions"
    assert (p.SAMPLE_RATE, p.CHANNELS, p.BITS_PER_SAMPLE) == (16000, 1, 16)
    assert p.BYTE_RATE == 32000


# ------------------------------------------------------------ the frame

def test_the_header_is_type_then_big_endian_length():
    frame = p.encode(0x13, b"ab")
    assert frame == bytes([0x13, 0x00, 0x02]) + b"ab"
    assert p.encode(0x03, b"\x00" * 0x0102)[:3] == bytes([0x03, 0x01, 0x02])


@pytest.mark.parametrize("size", [0, 1, 2, 4095, 4096])
def test_payloads_up_to_4096_bytes_are_frames(size):
    payload = bytes(range(256)) * 17
    payload = payload[:size]
    frame = p.encode(p.Message.AUDIO_OUT, payload)
    assert len(frame) == 3 + size
    (got,) = p.FrameReader().feed(frame)
    assert got == p.Frame(0x10, payload)


def test_a_payload_of_4097_bytes_is_not_sent():
    with pytest.raises(ValueError):
        p.encode(p.Message.AUDIO_OUT, b"\x00" * 4097)


def test_a_type_that_does_not_fit_a_byte_is_not_sent():
    with pytest.raises(ValueError):
        p.encode(0x100)
    with pytest.raises(ValueError):
        p.encode(-1)


def test_a_declared_length_over_4096_is_a_protocol_error():
    # The header can say up to 65535; the spec allows 4096.
    with pytest.raises(p.ProtocolError):
        p.FrameReader().feed(struct.pack(">BH", 0x10, 4097))
    with pytest.raises(p.ProtocolError):
        p.FrameReader().feed(struct.pack(">BH", 0x12, 0xFFFF))


def test_a_frame_cut_anywhere_comes_out_whole():
    stream = (
        p.encode_json(p.Message.EVENT, {"type": "listening", "data": {}})
        + p.encode(p.Message.AUDIO_OUT, b"\x01\x02" * 700)
        + p.encode(p.Message.AUDIO_OUT_END)
        + p.encode_json(p.Message.HANGUP, {"reason": "done"})
    )
    whole = p.FrameReader().feed(stream)
    assert [f.type for f in whole] == [0x12, 0x10, 0x11, 0x13]
    for cut in range(len(stream) + 1):
        reader = p.FrameReader()
        got = reader.feed(stream[:cut]) + reader.feed(stream[cut:])
        assert got == whole, cut
        assert reader.buffered == 0
    reader = p.FrameReader()
    one_at_a_time = [f for b in stream for f in reader.feed(bytes([b]))]
    assert one_at_a_time == whole


def test_an_empty_payload_frame_is_three_bytes():
    assert p.encode(p.Message.AUDIO_OUT_END) == bytes([0x11, 0, 0])
    (got,) = p.FrameReader().feed(bytes([0x11, 0, 0]))
    assert got.payload == b"" and got.json() == {}


def test_an_unknown_type_is_a_frame_with_no_message():
    # "An unknown message type is logged and ignored": the frame still
    # parses, so the stream stays in step, and the caller ignores it.
    stream = p.encode(0x7F, b"whatever") + p.encode(p.Message.AUDIO_OUT_END)
    unknown, known = p.FrameReader().feed(stream)
    assert unknown.message is None and unknown.payload == b"whatever"
    assert known.message is p.Message.AUDIO_OUT_END


# ------------------------------------------------------------- JSON

def test_unknown_json_fields_are_ignored():
    frame = p.Frame(0x13, json.dumps({"reason": "done", "listen": True, "from_the_future": [1]}).encode())
    assert p.Hangup.parse(frame.json()) == p.Hangup("done", True)


def test_a_control_payload_must_be_a_json_object():
    for payload in (b"[1, 2]", b'"text"', b"3", b"null"):
        with pytest.raises(p.ProtocolError):
            p.Frame(0x13, payload).json()


def test_a_control_payload_that_is_not_json_or_not_utf8_is_refused():
    for payload in (b"{", b"not json", b"\xff\xfe{}"):
        with pytest.raises(p.ProtocolError):
            p.Frame(0x12, payload).json()


def test_control_json_is_utf8():
    frame = p.encode_json(p.Message.HELLO, {"name": "Küche"})
    (got,) = p.FrameReader().feed(frame)
    assert got.json()["name"] == "Küche"


def test_only_an_object_is_encoded_as_control():
    with pytest.raises(TypeError):
        p.encode_json(p.Message.EVENT, ["listening"])


# ------------------------------------------------------------- audio

def test_audio_is_cut_into_frames_without_splitting_a_sample():
    pcm = bytes(range(256)) * 40  # 10240 bytes
    frames = p.audio_frames(p.Message.AUDIO_IN, pcm)
    payloads = [f.payload for f in p.FrameReader().feed(b"".join(frames))]
    assert [len(x) for x in payloads] == [4096, 4096, 2048]
    assert all(len(x) % 2 == 0 for x in payloads)
    assert b"".join(payloads) == pcm
    assert p.audio_frames(p.Message.AUDIO_IN, b"") == []


def test_odd_length_audio_is_not_16_bit_pcm():
    with pytest.raises(ValueError):
        p.audio_frames(p.Message.AUDIO_IN, b"\x00\x00\x00")


def test_audio_frames_are_only_for_audio_messages():
    with pytest.raises(ValueError):
        p.audio_frames(p.Message.EVENT, b"\x00\x00")


# --------------------------------------------------- reading a stream

def _read_all(data: bytes, eof: bool = True):
    async def run():
        reader = asyncio.StreamReader()
        reader.feed_data(data)
        if eof:
            reader.feed_eof()
        out = []
        while True:
            frame = await p.read_frame(reader)
            if frame is None:
                return out
            out.append(frame)
    return asyncio.run(run())


def test_a_close_between_frames_ends_the_stream():
    data = p.encode(p.Message.AUDIO_OUT_END) + p.encode_json(p.Message.HANGUP, {})
    assert [f.type for f in _read_all(data)] == [0x11, 0x13]
    assert _read_all(b"") == []


@pytest.mark.parametrize("cut", [1, 2, 3, 5])
def test_a_close_inside_a_frame_is_a_protocol_error(cut):
    data = p.encode_json(p.Message.HANGUP, {"reason": "x"})
    with pytest.raises(p.ProtocolError):
        _read_all(data[:cut])


def test_reading_refuses_an_oversize_declared_length_before_reading_it():
    with pytest.raises(p.ProtocolError):
        _read_all(struct.pack(">BH", 0x10, 5000) + b"\x00" * 5000)


# ------------------------------------------------------------- HELLO

def _hello(**over):
    args = dict(name="kitchen-echo", mac="02:EC:12:34:56:78", model="echomuse-biscuit",
                firmware="v2.28.0", preroll_ms=260, output_buffer_bytes=128000)
    args.update(over)
    (frame,) = p.FrameReader().feed(p.hello(**args))
    assert frame.message is p.Message.HELLO
    return frame.json()


def test_hello_always_says_protocol_2():
    # Absent, the app reads protocol 1: a Voice PE with two captures and
    # mixednet wake words -- everything this satellite is not.
    info = _hello()
    assert info["protocol"] == 2 and isinstance(info["protocol"], int)


def test_hello_states_the_only_audio_format():
    info = _hello()
    assert (info["sample_rate"], info["channels"], info["bits_per_sample"]) == (16000, 1, 16)


def test_hello_declares_nothing_this_satellite_does_not_do():
    info = _hello()
    assert "wake_format" not in info  # absent: the app pushes no models
    assert "model_slots" not in info and "wake_models" not in info
    for key in ("aec", "monitor", "intercom", "stop_word", "monitor_aec"):
        assert not info.get(key), key


def test_hello_says_it_listens_for_follow_ups_only_when_asked():
    assert _hello()["listen"] is True
    assert _hello(listen=False)["listen"] is False


def test_hello_carries_name_mac_model_firmware_and_sizes():
    info = _hello()
    assert info["name"] == "kitchen-echo"
    assert info["mac"] == "02:ec:12:34:56:78"
    assert info["model"] == "echomuse-biscuit"
    assert info["firmware"] == "v2.28.0"
    assert info["preroll_ms"] == 260 and info["output_buffer_bytes"] == 128000
    assert "firmware" not in _hello(firmware=None)


def test_hello_needs_a_name_and_sane_sizes():
    for bad in (dict(name=""), dict(name="  "), dict(preroll_ms=-1), dict(output_buffer_bytes=0)):
        with pytest.raises(ValueError):
            _hello(**bad)


@pytest.mark.parametrize("mac", ["02:ec:12:34:56:78", "02EC12345678", "02-EC-12-34-56-78"])
def test_mac_spellings_normalise(mac):
    assert p.normalise_mac(mac) == "02:ec:12:34:56:78"


@pytest.mark.parametrize("mac", ["", "02:ec:12:34:56", "02:ec:12:34:56:78:9a", "zz:ec:12:34:56:78"])
def test_a_mac_that_is_not_one_is_refused(mac):
    with pytest.raises(ValueError):
        p.normalise_mac(mac)


def test_txt_records_are_mac_and_protocol():
    assert p.txt_records("02EC12345678") == {"mac": "02:ec:12:34:56:78", "protocol": "2"}


# ---------------------------------------------------------- calls

def _json(frame_bytes):
    (frame,) = p.FrameReader().feed(frame_bytes)
    return frame.message, frame.json()


def test_call_start_carries_the_wake_word_and_its_preroll():
    msg, info = _json(p.call_start("hey_jarvis", 48000, probability=0.6, max_probability=1.0, cutoff=0.5))
    assert msg is p.Message.CALL_START
    assert info == {"wake_word": "hey_jarvis", "preroll_bytes": 48000,
                    "probability": 153, "max_probability": 255, "cutoff": 128}


def test_a_detection_score_is_0_to_255():
    assert [p.probability_byte(x) for x in (0.0, 1.0, -0.5, 2.0)] == [0, 255, 0, 255]
    with pytest.raises(ValueError):
        p.probability_byte(float("nan"))


def test_manual_and_follow_up_calls_have_no_preroll():
    assert _json(p.call_start(p.MANUAL, 0))[1] == {"wake_word": "manual", "preroll_bytes": 0}
    assert _json(p.call_start(p.FOLLOW_UP, 0))[1] == {"wake_word": "follow_up", "preroll_bytes": 0}
    for word in (p.MANUAL, p.FOLLOW_UP):
        with pytest.raises(ValueError):
            p.call_start(word, 3200)


def test_a_preroll_is_whole_samples():
    for bad in (-2, 3):
        with pytest.raises(ValueError):
            p.call_start("hey_jarvis", bad)
    with pytest.raises(ValueError):
        p.call_start("", 0)


def test_call_end_reasons_are_the_specs():
    assert _json(p.call_end(p.TIMEOUT)) == (p.Message.CALL_END, {"reason": "timeout"})
    assert _json(p.call_end(p.CANCELLED)) == (p.Message.CALL_END, {"reason": "cancelled"})
    with pytest.raises(ValueError):
        p.call_end("bored")


def test_device_events_are_the_specs():
    for kind in ("announcing", "announced", "stop_timer", "intercom_end"):
        assert _json(p.device_event(kind)) == (p.Message.DEVICE_EVENT, {"type": kind})
    with pytest.raises(ValueError):
        p.device_event("exploded")


def test_playback_drained_is_an_empty_object():
    assert _json(p.playback_drained()) == (p.Message.PLAYBACK_DRAINED, {})


def test_status_carries_the_specs_fields():
    msg, info = _json(p.status(free_bytes=64000, queued_bytes=32000, speaker_busy=True, mic_dropped_bytes=5))
    assert msg is p.Message.STATUS
    assert set(info) == {"free_bytes", "queued_bytes", "queued_ms", "speaker_busy",
                         "mic_dropped_bytes", "aec_dropped_bytes"}
    assert info["queued_ms"] == 1000 and info["speaker_busy"] is True


def test_a_pushed_model_is_refused():
    msg, info = _json(p.model_refused("okay_nabu", "this satellite runs its own wake words"))
    assert msg is p.Message.MODEL_RESULT
    assert info["ok"] is False and info["id"] == "okay_nabu" and info["error"]


# ------------------------------------------------------ app messages

def test_hangup_listen_must_be_true_to_count():
    assert p.Hangup.parse({}) == p.Hangup("", False)
    assert p.Hangup.parse({"reason": "done", "listen": True}).listen is True
    assert p.Hangup.parse({"listen": "yes"}).listen is False


def test_an_event_of_any_type_parses_and_bad_data_is_empty():
    assert p.Event.parse({"type": "listening", "data": {"stage": "listening"}}) == p.Event(
        "listening", {"stage": "listening"})
    assert p.Event.parse({"type": "something_new"}) == p.Event("something_new", {})
    assert p.Event.parse({"type": "result", "data": [1]}).data == {}


def test_monitor_flags_must_be_true_to_count():
    assert p.Monitor.parse({"active": True}) == p.Monitor(True, False)
    assert p.Monitor.parse({"active": True, "intercom": True}) == p.Monitor(True, True)
    assert p.Monitor.parse({"active": 1}) == p.Monitor(False, False)
