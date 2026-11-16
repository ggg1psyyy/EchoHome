"""
A call through em_incubator, end to end over a real socket: the turn loop's
side (trigger_voice_turn with em_controller's callbacks) and the app's side
(a client written from PROTOCOL.md) in one event loop.

The Device is a stand-in carrying what the call path reads (voice_queue,
cancel_event, muted, oww_model, listen_session, last_wake); the listener,
the framing, the resampling and the call itself are the real ones.
"""

import asyncio

import pytest

import em_incubator as inc
import em_incubator_proto as proto
import em_resample
import em_voice

DEV = "G090LF00000000C1"
FRAME = b"\x01\x00" * 1280  # 80 ms of 16 kHz audio, as voice_queue holds it


class FakeDevice:
    def __init__(self):
        self.device_id = DEV
        self.voice_queue = asyncio.Queue()
        self.cancel_event = asyncio.Event()
        self.muted = False
        self.oww_model = "hey_jarvis_v0.1"
        self.listen_session = 7
        self.last_wake = {"model": "hey_jarvis", "score": 0.6, "threshold": 0.5, "noise_floor": 0.001}
        self.turn_history = []
        self.last_turn_outcome = None


@pytest.fixture
def world(monkeypatch):
    recorded = []

    async def persist(device, record):
        recorded.append(record)
        device.last_turn_outcome = record["outcome"]

    class NoZeroconf:
        async def async_register_service(self, info, allow_name_change=False):
            pass

        async def async_unregister_service(self, info):
            pass

    monkeypatch.setattr(inc, "_azc", lambda: NoZeroconf())
    monkeypatch.setattr(inc, "_server_ip", lambda: "127.0.0.1")
    monkeypatch.setattr(inc, "_host", "127.0.0.1")
    monkeypatch.setattr(inc, "PORT_OFFSET", 0)
    monkeypatch.setattr(inc.db, "get_device", lambda _i: {"approved": 1, "firmware_ver": "v2.28.0"})
    monkeypatch.setattr(inc.db, "get_effective_device_config", lambda _i: {"voiceBackend": "incubator"})
    monkeypatch.setattr(inc.db, "get_esphome_port", lambda _i: 0)
    monkeypatch.setattr(inc.db, "get_esphome_mac", lambda _i: "02:EC:00:00:00:C1")
    monkeypatch.setattr(inc.em_turn_record, "persist_turn", persist)
    monkeypatch.setattr(inc, "TTS_WAIT_S", 2.0)
    monkeypatch.setattr(inc, "HANGUP_WAIT_S", 1.0)
    yield recorded
    asyncio.run(inc.stop_all())
    inc._satellites.clear()
    inc._online.clear()
    inc._locks.clear()


class App:
    """The Incubator app, as PROTOCOL.md describes it, from the other end."""

    def __init__(self, reader, writer):
        self.reader, self.writer = reader, writer
        self.audio_in = bytearray()

    @classmethod
    async def connect(cls):
        await inc.device_connected(DEV)
        reader, writer = await asyncio.open_connection("127.0.0.1", inc._satellites[DEV].port)
        app = cls(reader, writer)
        hello = await app.next()
        assert hello.message is proto.Message.HELLO
        return app

    async def next(self, timeout=3.0):
        frame = await asyncio.wait_for(proto.read_frame(self.reader), timeout)
        if frame is not None and frame.message is proto.Message.AUDIO_IN:
            self.audio_in += frame.payload
        return frame

    async def until(self, message, timeout=3.0):
        while True:
            frame = await self.next(timeout)
            assert frame is not None, f"closed waiting for {message.name}"
            if frame.message is message:
                return frame

    def send(self, data: bytes):
        self.writer.write(data)

    def send_json(self, message, obj):
        self.send(proto.encode_json(message, obj))

    def say(self, pcm: bytes):
        for frame in proto.audio_frames(proto.Message.AUDIO_OUT, pcm):
            self.send(frame)
        self.send(proto.encode(proto.Message.AUDIO_OUT_END))


class Turn:
    """em_controller's callbacks, recorded."""

    def __init__(self, device):
        self.device = device
        self.thinking = 0
        self.played = bytearray()
        self.play_calls = 0
        self.delay = 0.0  # seconds per chunk: a speaker slower than the socket

    async def on_thinking(self):
        self.thinking += 1

    async def post_turn_play(self, chunks):
        self.play_calls += 1
        async for chunk in chunks:
            self.played += chunk
            if self.delay:
                await asyncio.sleep(self.delay)
            if self.device.cancel_event.is_set():
                break
        return len(self.played)

    def run(self, label="wakeword-dev(0.600)"):
        return asyncio.ensure_future(inc.trigger_voice_turn(
            self.device, on_thinking=self.on_thinking,
            post_turn_play=self.post_turn_play, trigger_label=label, preroll_discard=3))


def test_a_wake_call_streams_hears_the_reply_and_hangs_up(world):
    async def run():
        app = await App.connect()
        dev = FakeDevice()
        for _ in range(3):
            dev.voice_queue.put_nowait(FRAME)
        turn = Turn(dev)
        task = turn.run()
        start = (await app.until(proto.Message.CALL_START)).json()
        assert start["wake_word"] == "echomuse-hey_jarvis_v0.1"
        assert start["preroll_bytes"] == inc.WAKE_LEAD_MS * proto.BYTE_RATE // 1000
        assert start["probability"] == 153 and start["cutoff"] == 128
        while len(app.audio_in) < 3 * len(FRAME):
            await app.next()
        assert bytes(app.audio_in[:len(FRAME)]) == FRAME  # untouched
        app.send_json(proto.Message.EVENT, {"type": "processing", "data": {}})
        reply = b"\x10\x00" * 8000  # 0.5 s at 16 kHz
        app.say(reply)
        await app.until(proto.Message.PLAYBACK_DRAINED)
        app.send_json(proto.Message.HANGUP, {"reason": "done"})
        assert await task is False
        assert turn.thinking == 1 and turn.play_calls == 1
        up = em_resample.Upsampler()
        assert bytes(turn.played) == up.process(reply) + up.flush()
        assert world[-1]["outcome"] == "ok"
        assert world[-1]["trigger"] == "wakeword-dev(0.600)"
        app.writer.close()
    asyncio.run(run())


def test_listen_on_hangup_asks_for_a_follow_up_unless_muted(world):
    async def call(muted):
        app = await App.connect()
        dev = FakeDevice()
        dev.muted = muted
        task = Turn(dev).run()
        await app.until(proto.Message.CALL_START)
        app.send_json(proto.Message.EVENT, {"type": "responding", "data": {"text": "which room?"}})
        app.say(b"\x00\x00" * 1600)
        await app.until(proto.Message.PLAYBACK_DRAINED)
        app.send_json(proto.Message.HANGUP, {"reason": "asked", "listen": True})
        result = await task
        app.writer.close()
        await inc.device_disconnected(DEV)
        return result
    assert asyncio.run(call(False)) is True
    assert asyncio.run(call(True)) is False


def test_a_rejected_wake_ends_quietly(world):
    async def run():
        app = await App.connect()
        dev = FakeDevice()
        turn = Turn(dev)
        task = turn.run()
        await app.until(proto.Message.CALL_START)
        app.send_json(proto.Message.EVENT, {"type": "wake_rejected", "data": {"reason": "confusable"}})
        app.send_json(proto.Message.HANGUP, {"reason": "wake_rejected"})
        assert await task is False
        assert turn.thinking == 0 and turn.play_calls == 0
        assert world[-1]["outcome"] == "wake_rejected"
        app.writer.close()
    asyncio.run(run())


def test_a_barge_cancels_the_call_and_the_rest_of_the_reply_is_dropped(world):
    async def run():
        app = await App.connect()
        dev = FakeDevice()
        turn = Turn(dev)
        task = turn.run()
        await app.until(proto.Message.CALL_START)
        app.send_json(proto.Message.EVENT, {"type": "processing", "data": {}})
        for frame in proto.audio_frames(proto.Message.AUDIO_OUT, b"\x00\x01" * 2048):
            app.send(frame)
        while not turn.played:
            await asyncio.sleep(0.01)
        # The turn loop's barge: mark, set cancel_event, abort the run.
        em_voice.note_barged(DEV)
        dev.cancel_event.set()
        em_voice.abort_ha_run(DEV)
        end = await app.until(proto.Message.CALL_END)
        assert end.json() == {"reason": "cancelled"}
        played = len(turn.played)
        app.say(b"\x00\x01" * 8000)  # still arriving for the cancelled call
        assert await task is False
        await asyncio.sleep(0.1)
        assert len(turn.played) == played
        assert world[-1]["outcome"] == "barged"
        # The connection is still in step: the next stream is answered.
        app.send(proto.encode(proto.Message.AUDIO_OUT_END))
        assert (await app.until(proto.Message.PLAYBACK_DRAINED)).message is proto.Message.PLAYBACK_DRAINED
        app.writer.close()
    asyncio.run(run())


def test_every_stream_in_a_call_is_played_and_answered(world):
    # The app's call sounds can be a stream of their own, ended before the
    # reply's (its Cues); it waits for a PLAYBACK_DRAINED for each.
    async def run():
        app = await App.connect()
        dev = FakeDevice()
        turn = Turn(dev)
        task = turn.run()
        await app.until(proto.Message.CALL_START)
        app.send_json(proto.Message.EVENT, {"type": "processing", "data": {}})
        heard = b"\x05\x00" * 3200
        app.say(heard)                      # the "heard" sound, its own stream
        await app.until(proto.Message.PLAYBACK_DRAINED)
        reply = b"\x07\x00" * 8000
        app.say(reply)                      # then the answer
        await app.until(proto.Message.PLAYBACK_DRAINED)
        app.send(proto.encode(proto.Message.AUDIO_OUT_END))  # an empty stream
        await app.until(proto.Message.PLAYBACK_DRAINED)
        app.send_json(proto.Message.HANGUP, {"reason": "done"})
        assert await task is False
        assert turn.play_calls == 2
        def at48(pcm):
            up = em_resample.Upsampler()
            return up.process(pcm) + up.flush()
        assert bytes(turn.played) == at48(heard) + at48(reply)
        assert world[-1]["outcome"] == "ok"
        app.writer.close()
    asyncio.run(run())


def test_a_cancel_mid_long_reply_does_not_wedge_the_connection(world):
    # A reply longer than the queue (32 frames, ~4 s) fills it and parks the
    # socket reader in put(); the speaker side stops at the cancel. Unless
    # the call empties its queue, nothing reads the socket again and the
    # next call never hears its HANGUP.
    async def run():
        app = await App.connect()
        dev = FakeDevice()
        turn = Turn(dev)
        turn.delay = 0.05
        task = turn.run()
        await app.until(proto.Message.CALL_START)
        app.send_json(proto.Message.EVENT, {"type": "processing", "data": {}})
        long_reply = b"\x00\x01" * (proto.MAX_PAYLOAD // 2) * (inc.PLAYBACK_QUEUE_FRAMES * 3)
        for frame in proto.audio_frames(proto.Message.AUDIO_OUT, long_reply):
            app.send(frame)
        app.send(proto.encode(proto.Message.AUDIO_OUT_END))
        while not turn.played:
            await asyncio.sleep(0.01)
        await asyncio.sleep(0.2)  # let the queue fill behind the slow speaker
        dev.cancel_event.set()
        em_voice.cancel_voice_turn(DEV, abort_ha=True, reason="cancelled")
        assert await asyncio.wait_for(task, 3.0) is False
        await app.until(proto.Message.CALL_END)
        # A fresh call on the same connection is still heard.
        dev2 = FakeDevice()
        second = Turn(dev2).run()
        await app.until(proto.Message.CALL_START, timeout=3.0)
        app.send_json(proto.Message.HANGUP, {"reason": "done"})
        assert await asyncio.wait_for(second, 3.0) is False
        app.writer.close()
    asyncio.run(run())


def test_a_button_call_sends_silence_before_anyone_speaks(world):
    # The Echo's turn stream starts only once its VAD hears speech; the app
    # can only conclude "nothing was said" from audio it receives.
    async def run():
        app = await App.connect()
        dev = FakeDevice()
        task = Turn(dev).run(label="button")
        await app.until(proto.Message.CALL_START)
        while len(app.audio_in) < 3 * len(inc.SILENCE_FRAME):
            await app.next(timeout=1.0)
        assert set(app.audio_in) == {0}
        dev.voice_queue.put_nowait(FRAME)  # then the speech arrives as it is
        while FRAME not in bytes(app.audio_in):
            await app.next(timeout=1.0)
        app.send_json(proto.Message.HANGUP, {"reason": "done"})
        assert await task is False
        app.writer.close()
    asyncio.run(run())


def test_a_wake_call_is_not_padded_before_its_audio(world):
    async def run():
        app = await App.connect()
        dev = FakeDevice()
        task = Turn(dev).run()
        await app.until(proto.Message.CALL_START)
        await asyncio.sleep(0.4)
        dev.voice_queue.put_nowait(FRAME)
        while len(app.audio_in) < len(FRAME):
            await app.next(timeout=1.0)
        assert bytes(app.audio_in) == FRAME  # no silence invented ahead of it
        app.send_json(proto.Message.HANGUP, {"reason": "done"})
        assert await task is False
        app.writer.close()
    asyncio.run(run())


def test_the_button_cancels_a_call_while_the_app_thinks(world):
    async def run():
        app = await App.connect()
        dev = FakeDevice()
        task = Turn(dev).run(label="button")
        start = (await app.until(proto.Message.CALL_START)).json()
        assert start == {"wake_word": "manual", "preroll_bytes": 0, "probability": 153, "cutoff": 128}
        app.send_json(proto.Message.EVENT, {"type": "processing", "data": {}})
        await asyncio.sleep(0.05)
        dev.cancel_event.set()
        em_voice.cancel_voice_turn(DEV, abort_ha=True, reason="cancelled")
        assert (await app.until(proto.Message.CALL_END)).json()["reason"] == "cancelled"
        assert await task is False
        assert world[-1]["outcome"] == "cancelled"
        app.writer.close()
    asyncio.run(run())


def test_a_follow_up_call_names_itself_and_has_no_preroll(world):
    async def run():
        app = await App.connect()
        dev = FakeDevice()
        dev.last_wake = None
        task = Turn(dev).run(label="continuation")
        assert (await app.until(proto.Message.CALL_START)).json() == {
            "wake_word": "follow_up", "preroll_bytes": 0}
        app.send_json(proto.Message.HANGUP, {"reason": "no_answer"})
        assert await task is False
        app.writer.close()
    asyncio.run(run())


def test_a_stream_the_echo_ended_is_padded_with_silence_until_the_app_ends_it(world):
    async def run():
        app = await App.connect()
        dev = FakeDevice()
        dev.voice_queue.put_nowait(FRAME)
        dev.voice_queue.put_nowait("vad_end")
        task = Turn(dev).run(label="button")
        await app.until(proto.Message.CALL_START)
        app.send_json(proto.Message.EVENT, {"type": "listening", "data": {"stage": "listening"}})
        while len(app.audio_in) < len(FRAME) + 4 * len(inc.SILENCE_FRAME):
            await app.next()
        assert set(app.audio_in[len(FRAME):]) == {0}
        app.send_json(proto.Message.HANGUP, {"reason": "timeout"})
        assert await task is False
        assert world[-1]["outcome"] == "no_speech"
        app.writer.close()
    asyncio.run(run())


def test_a_command_the_app_never_ends_is_ended_here(world, monkeypatch):
    monkeypatch.setattr(inc, "CALL_MAX_S", 0.3)

    async def run():
        app = await App.connect()
        dev = FakeDevice()
        task = Turn(dev).run()
        await app.until(proto.Message.CALL_START)
        assert (await app.until(proto.Message.CALL_END)).json() == {"reason": "timeout"}
        assert await task is False
        assert world[-1]["outcome"] == "timeout"
        app.writer.close()
    asyncio.run(run())


def test_the_app_going_away_mid_call_ends_the_turn(world):
    async def run():
        app = await App.connect()
        dev = FakeDevice()
        task = Turn(dev).run()
        await app.until(proto.Message.CALL_START)
        app.writer.close()
        assert await asyncio.wait_for(task, 3.0) is False
        assert world[-1]["outcome"] == "app_gone"
    asyncio.run(run())


def test_no_app_means_no_call_and_a_dropped_wake(world, monkeypatch):
    dropped = []

    async def record_dropped_turn(device, label, info):
        dropped.append(label)

    monkeypatch.setattr(inc.em_turn_record, "record_dropped_turn", record_dropped_turn)

    async def run():
        await inc.device_connected(DEV)
        assert inc.serves(DEV) and not inc.can_serve_turn(DEV)
        assert await Turn(FakeDevice()).run() is False
    asyncio.run(run())
    assert dropped == ["wakeword-dev(0.600)"]


def test_a_second_turn_while_one_runs_is_refused(world, monkeypatch):
    monkeypatch.setattr(inc.em_turn_record, "record_dropped_turn",
                        lambda *a: asyncio.sleep(0))

    async def run():
        app = await App.connect()
        first = Turn(FakeDevice()).run()
        await app.until(proto.Message.CALL_START)
        assert await Turn(FakeDevice()).run() is False
        app.send_json(proto.Message.HANGUP, {"reason": "x"})
        assert await first is False
        app.writer.close()
    asyncio.run(run())


def test_the_reply_queue_is_bounded_so_the_socket_pushes_back():
    assert inc.Stream(1).audio.maxsize == inc.PLAYBACK_QUEUE_FRAMES
    # Full frames: the declared 4 s buffer, rounded up to whole frames.
    full = inc.PLAYBACK_QUEUE_FRAMES * proto.MAX_PAYLOAD
    assert full - proto.MAX_PAYLOAD < inc.OUTPUT_BUFFER_BYTES <= full


# ------------------------------------------------------------ dispatch

def test_dispatch_goes_to_the_incubator_only_for_its_echoes(monkeypatch):
    seen = []
    monkeypatch.setattr(em_voice.em_esphome, "can_serve_turn", lambda i: seen.append(("ha", i)) or True)
    monkeypatch.setattr(em_voice.em_incubator, "can_serve_turn", lambda i: seen.append(("inc", i)) or False)
    monkeypatch.setattr(em_voice.em_incubator, "serves", lambda i: i == "INC")
    assert em_voice.can_serve_turn("HA") is True
    assert em_voice.can_serve_turn("INC") is False
    assert seen == [("ha", "HA"), ("inc", "INC")]


@pytest.mark.parametrize("fn", ["cancel_voice_turn", "abort_ha_run", "note_barged"])
def test_every_turn_call_is_dispatched(monkeypatch, fn):
    seen = []
    monkeypatch.setattr(em_voice.em_esphome, fn, lambda i, **k: seen.append("ha"))
    monkeypatch.setattr(em_voice.em_incubator, fn, lambda i, **k: seen.append("inc"))
    monkeypatch.setattr(em_voice.em_incubator, "serves", lambda i: i == "INC")
    getattr(em_voice, fn)("HA")
    getattr(em_voice, fn)("INC")
    assert seen == ["ha", "inc"]


def test_trigger_voice_turn_is_dispatched(monkeypatch):
    seen = []

    async def ha(device, **kw):
        seen.append(("ha", kw["trigger_label"]))
        return True

    async def incubator(device, **kw):
        seen.append(("inc", kw["trigger_label"]))
        return False

    monkeypatch.setattr(em_voice.em_esphome, "trigger_voice_turn", ha)
    monkeypatch.setattr(em_voice.em_incubator, "trigger_voice_turn", incubator)
    monkeypatch.setattr(em_voice.em_incubator, "serves", lambda i: i == "INC")

    class D:
        def __init__(self, i):
            self.device_id = i

    async def run():
        assert await em_voice.trigger_voice_turn(device=D("HA"), trigger_label="button") is True
        assert await em_voice.trigger_voice_turn(device=D("INC"), trigger_label="button") is False
    asyncio.run(run())
    assert seen == [("ha", "button"), ("inc", "button")]


def test_em_controller_reaches_backends_only_through_em_voice():
    import _sources
    src = _sources.code_only(_sources.source(_sources.CONTROLLER / "em_controller.py"))
    for fn in ("can_serve_turn", "trigger_voice_turn", "cancel_voice_turn",
               "abort_ha_run", "note_barged", "record_dropped_wake"):
        assert f"esphome.{fn}(" not in src, fn
        assert f"voice.{fn}(" in src, fn


def test_the_reply_wait_is_home_assistants_option_read_the_same_way():
    # Read once at import from the add-on's tts_wait_timeout, with the same
    # bounds: never a call into em_turnclock per turn (which once took the
    # raw value and crashed a live call when called without it).
    import em_esphome
    import em_turnclock
    assert isinstance(inc.TTS_WAIT_S, float)
    assert inc.TTS_WAIT_S == em_esphome.TTS_WAIT_TIMEOUT
    assert 0 < inc.TTS_WAIT_S <= em_turnclock.TTS_WAIT_MAX


def test_call_kind_names_and_preroll():
    assert inc.call_kind("wakeword(0.512)", "alexa_v0.1", None) == ("echomuse-alexa_v0.1", 0)
    assert inc.call_kind("wakeword-dev(0.6)", "/data/oww_models/hey_kitchen.onnx", 3) == (
        "echomuse-hey_kitchen", 8320)
    assert inc.call_kind("barge-in", "hey_jarvis_v0.1", 3)[0] == "echomuse-hey_jarvis_v0.1"
    assert inc.call_kind("button", "x", None) == ("manual", 0)
    assert inc.call_kind("continuation", "x", 3) == ("follow_up", 0)
    assert inc.call_kind("conversation", "x", None) == ("manual", 0)


# ─── Playback outside a call: the app's ring and announcements ─────────────

class Player:
    """em_controller's _incubator_playback, recorded: plays (consumes) a
    stream, optionally slowly, and notes when it is released."""

    def __init__(self, delay=0.0):
        self.delay = delay
        self.played = bytearray()
        self.streams = 0
        self.released = 0

    async def __call__(self, device_id, chunks):
        self.streams += 1
        try:
            async for chunk in chunks:
                self.played += chunk
                if self.delay:
                    await asyncio.sleep(self.delay)
            return len(self.played)
        finally:
            self.released += 1


class ControlDevice(FakeDevice):
    def __init__(self):
        super().__init__()
        self.sent = []

    async def send_control(self, msg):
        self.sent.append(msg)


@pytest.fixture
def outside(world, monkeypatch):
    player = Player()
    dev = ControlDevice()
    monkeypatch.setattr(inc, "_player", player)
    monkeypatch.setitem(inc.em_state.devices, DEV, dev)
    return player, dev


def _ring(app, seconds=0.5, end=True):
    pcm = b"\x03\x00" * int(16000 * seconds)
    for frame in proto.audio_frames(proto.Message.AUDIO_OUT, pcm):
        app.send(frame)
    if end:
        app.send(proto.encode(proto.Message.AUDIO_OUT_END))
    return pcm


def test_an_announcement_outside_a_call_is_played_then_answered(world, outside):
    player, _dev = outside

    async def run():
        app = await App.connect()
        app.send_json(proto.Message.EVENT, {"type": "announcement", "data": {"active": True}})
        pcm = _ring(app, 0.3)
        await app.until(proto.Message.PLAYBACK_DRAINED)
        up = em_resample.Upsampler()
        assert bytes(player.played) == up.process(pcm) + up.flush()
        assert player.streams == 1 and player.released == 1
        app.writer.close()
    asyncio.run(run())


def test_a_wake_over_the_ring_cuts_it_and_its_tail_is_not_the_reply(world, outside):
    player, dev = outside
    player.delay = 0.05  # the ring plays at its own pace

    async def run():
        app = await App.connect()
        app.send_json(proto.Message.EVENT, {"type": "timer_ringing", "data": {"timers": 1, "name": "pasta"}})
        _ring(app, 2.0, end=False)  # the app paces its ring: still sending
        while not player.played:
            await asyncio.sleep(0.01)
        assert em_voice.wake_over_playback(DEV) is True  # the gate lets the wake through
        turn = Turn(dev)
        task = turn.run()
        await app.until(proto.Message.CALL_START)
        assert {"type": "speaker_flush"} in dev.sent
        assert player.released == 1  # the ring's playback is over
        assert em_voice.wake_over_playback(DEV) is False
        # The ring's tail, still in flight when the app saw CALL_START, then
        # its listening: the tail is neither played nor ends the command.
        _ring(app, 0.2)
        app.send_json(proto.Message.EVENT, {"type": "listening", "data": {"stage": "listening"}})
        await asyncio.sleep(0.2)
        assert turn.thinking == 0 and not turn.played
        app.send_json(proto.Message.EVENT, {"type": "processing", "data": {}})
        reply = b"\x09\x00" * 3200
        app.say(reply)
        await app.until(proto.Message.PLAYBACK_DRAINED)
        app.send_json(proto.Message.HANGUP, {"reason": "done"})
        assert await task is False
        up = em_resample.Upsampler()
        assert bytes(turn.played) == up.process(reply) + up.flush()
        assert turn.thinking == 1
        app.writer.close()
    asyncio.run(run())


def test_an_abandoned_stream_is_cut_when_the_app_stops_its_timer(world, outside):
    player, dev = outside

    async def run():
        app = await App.connect()
        app.send_json(proto.Message.EVENT, {"type": "timer_ringing", "data": {"timers": 1}})
        _ring(app, 0.3, end=False)  # the app's ring task was cancelled mid-send
        while not player.played:
            await asyncio.sleep(0.01)
        app.send_json(proto.Message.EVENT, {"type": "timer_stopped", "data": {}})
        for _ in range(100):
            if player.released and {"type": "speaker_flush"} in dev.sent:
                break
            await asyncio.sleep(0.01)
        # Flushed even though the playback ended at once: what was already
        # sent would otherwise ring on from the Echo's buffer.
        assert player.released == 1 and {"type": "speaker_flush"} in dev.sent
        assert not em_voice.app_timer_ringing(DEV)
        # A fresh stream afterwards plays and is answered.
        _ring(app, 0.1)
        assert (await app.until(proto.Message.PLAYBACK_DRAINED)).message is proto.Message.PLAYBACK_DRAINED
        assert player.streams == 2
        app.writer.close()
    asyncio.run(run())


def test_the_button_during_the_apps_ring_stops_it_and_starts_no_call(world, outside):
    player, dev = outside

    async def run():
        app = await App.connect()
        app.send_json(proto.Message.EVENT, {"type": "timer_ringing", "data": {"timers": 1}})
        _ring(app, 1.0, end=False)
        while not player.played:
            await asyncio.sleep(0.01)
        assert em_voice.app_timer_ringing(DEV)
        await em_voice.stop_app_timer(DEV)
        event = await app.until(proto.Message.DEVICE_EVENT)
        assert event.json() == {"type": "stop_timer"}
        assert player.released == 1 and {"type": "speaker_flush"} in dev.sent
        assert not em_voice.app_timer_ringing(DEV)
        # Nothing else was sent: no CALL_START, no PLAYBACK_DRAINED for the cut ring.
        app.send(proto.encode(proto.Message.AUDIO_OUT_END))  # the app's late end of it
        nxt = await app.next(timeout=1.0)
        assert nxt.message is proto.Message.PLAYBACK_DRAINED
        app.writer.close()
    asyncio.run(run())


def test_the_app_going_away_mid_ring_releases_the_speaker(world, outside):
    player, _dev = outside

    async def run():
        app = await App.connect()
        _ring(app, 1.0, end=False)
        while not player.played:
            await asyncio.sleep(0.01)
        app.writer.close()
        for _ in range(200):
            if player.released:
                break
            await asyncio.sleep(0.01)
        assert player.released == 1
    asyncio.run(run())


def test_ha_echoes_have_no_app_playback(monkeypatch):
    monkeypatch.setattr(em_voice.em_incubator, "serves", lambda i: False)
    assert em_voice.wake_over_playback("HA") is False
    assert em_voice.app_timer_ringing("HA") is False


def test_em_controller_plays_the_apps_streams_as_a_speaker_owner():
    import _sources
    body = _sources.code_only(_sources.def_source("em_controller._incubator_playback"))
    assert "em_player.interrupt(" in body and "em_player.resume_interrupted(" in body
    assert body.index("finally") < body.index("em_player.resume_interrupted(")
    assert "mic_stop" not in body  # a wake must still be heard over it
    assert "_run_streaming_post_turn_playback(" in body
    src = _sources.code_only(_sources.source(_sources.CONTROLLER / "em_controller.py"))
    assert "em_incubator.set_player(_incubator_playback)" in src
    # Both wake gates let a wake over the app's playback through.
    assert src.count("voice.wake_over_playback(device.device_id)") == 2
    assert "voice.app_timer_ringing(device.device_id)" in src


def test_ring_audio_arriving_during_the_cut_is_not_the_reply(world, outside):
    # The app paces its ring (a frame every ~128 ms); one read while the cut
    # is still being awaited (a slow speaker_flush here) must already count
    # as the ring's, not as this call's reply. A timer_progress meanwhile is
    # not the call's own traffic and does not end that.
    player, dev = outside
    player.delay = 0.05
    flushing = asyncio.Event()

    async def slow_send_control(msg):
        dev.sent.append(msg)
        if msg.get("type") == "speaker_flush":
            flushing.set()
            await asyncio.sleep(0.3)

    dev.send_control = slow_send_control

    async def run():
        app = await App.connect()
        app.send_json(proto.Message.EVENT, {"type": "timer_ringing", "data": {"timers": 1}})
        _ring(app, 2.0, end=False)
        while not player.played:
            await asyncio.sleep(0.01)
        turn = Turn(dev)
        task = turn.run()
        await flushing.wait()
        _ring(app, 0.3, end=False)  # read while the cut is in progress
        app.send_json(proto.Message.EVENT, {"type": "timer_progress", "data": {"shown": 0}})
        _ring(app, 0.2)
        await app.until(proto.Message.CALL_START)
        await asyncio.sleep(0.3)
        assert turn.thinking == 0 and not turn.played
        app.send_json(proto.Message.EVENT, {"type": "listening", "data": {"stage": "listening"}})
        app.send_json(proto.Message.EVENT, {"type": "processing", "data": {}})
        reply = b"\x0b\x00" * 1600
        app.say(reply)
        await app.until(proto.Message.PLAYBACK_DRAINED)
        app.send_json(proto.Message.HANGUP, {"reason": "done"})
        assert await task is False
        up = em_resample.Upsampler()
        assert bytes(turn.played) == up.process(reply) + up.flush()
        app.writer.close()
    asyncio.run(run())


def test_two_cuts_at_once_cut_once(world, outside):
    player, dev = outside

    async def run():
        app = await App.connect()
        _ring(app, 1.0, end=False)
        while not player.played:
            await asyncio.sleep(0.01)
        conn = inc._satellites[DEV].app
        results = await asyncio.gather(conn.cut_background("one"), conn.cut_background("two"))
        assert sorted(results) == [False, True]
        assert player.released == 1
        assert dev.sent.count({"type": "speaker_flush"}) == 1
        app.writer.close()
    asyncio.run(run())


@pytest.fixture
def owners(monkeypatch):
    """em_player's ownership calls, as the ring makes them."""
    calls = []

    async def interrupt(device_id):
        calls.append("interrupt")

    async def resume(device_id):
        calls.append("resume")

    monkeypatch.setattr(inc.em_player, "interrupt", interrupt)
    monkeypatch.setattr(inc.em_player, "resume_interrupted", resume)
    return calls


def test_the_ring_holds_the_music_down_from_first_ring_to_stop(world, outside, owners):
    # Each ring is its own stream, ~1 s apart; music must not pump up in the
    # gaps. One hold from timer_ringing to timer_stopped.
    player, _dev = outside

    async def run():
        app = await App.connect()
        app.send_json(proto.Message.EVENT, {"type": "timer_ringing", "data": {"timers": 1}})
        for _ in range(3):
            _ring(app, 0.1)
            await app.until(proto.Message.PLAYBACK_DRAINED)
        assert owners == ["interrupt"]
        app.send_json(proto.Message.EVENT, {"type": "timer_stopped", "data": {}})
        for _ in range(100):
            if owners == ["interrupt", "resume"]:
                break
            await asyncio.sleep(0.01)
        assert owners == ["interrupt", "resume"]
        assert player.streams == 3
        app.writer.close()
    asyncio.run(run())


def test_the_ring_hold_is_released_by_the_button_and_by_the_app_going(world, outside, owners):
    async def run():
        app = await App.connect()
        app.send_json(proto.Message.EVENT, {"type": "timer_ringing", "data": {"timers": 1}})
        _ring(app, 0.5, end=False)
        while not outside[0].played:
            await asyncio.sleep(0.01)
        await em_voice.stop_app_timer(DEV)
        assert owners == ["interrupt", "resume"]
        app.send_json(proto.Message.EVENT, {"type": "timer_ringing", "data": {"timers": 1}})
        while owners.count("interrupt") < 2:
            await asyncio.sleep(0.01)
        app.writer.close()
        for _ in range(200):
            if owners.count("resume") == 2:
                break
            await asyncio.sleep(0.01)
        assert owners == ["interrupt", "resume", "interrupt", "resume"]
    asyncio.run(run())


def test_a_wake_between_two_rings_is_still_a_wake_over_the_ring(world, outside, owners):
    player, dev = outside

    async def run():
        app = await App.connect()
        app.send_json(proto.Message.EVENT, {"type": "timer_ringing", "data": {"timers": 1}})
        _ring(app, 0.1)
        await app.until(proto.Message.PLAYBACK_DRAINED)
        conn = inc._satellites[DEV].app
        assert not conn.playing_outside_call  # the gap: nothing plays
        assert em_voice.wake_over_playback(DEV) is True
        turn = Turn(dev)
        task = turn.run()
        await app.until(proto.Message.CALL_START)
        _ring(app, 0.2)  # the next ring, sent before the app saw CALL_START
        app.send_json(proto.Message.EVENT, {"type": "listening", "data": {"stage": "listening"}})
        await asyncio.sleep(0.2)
        assert turn.thinking == 0 and not turn.played and player.streams == 1
        app.send_json(proto.Message.HANGUP, {"reason": "timer_stopped"})
        assert await task is False
        app.writer.close()
    asyncio.run(run())


def test_an_abandoned_stream_is_let_go_unanswered(world, outside, monkeypatch):
    player, _dev = outside
    monkeypatch.setattr(inc, "BACKGROUND_IDLE_S", 0.3)

    async def run():
        app = await App.connect()
        app.send_json(proto.Message.EVENT, {"type": "announcement", "data": {"active": True}})
        _ring(app, 0.2, end=False)  # the app gave up on it: no AUDIO_OUT_END
        for _ in range(100):
            if player.released:
                break
            await asyncio.sleep(0.02)
        assert player.released == 1
        # Unanswered: the next thing the app hears is the next stream's drain.
        _ring(app, 0.1)
        frame = await app.next(timeout=2.0)
        assert frame.message is proto.Message.PLAYBACK_DRAINED
        assert player.streams == 2
        app.writer.close()
    asyncio.run(run())


def test_a_stream_whose_playback_stopped_early_is_dropped_not_replayed(world, outside):
    # The player gives up after one chunk (the Echo dropped, a failure): the
    # rest of that stream must not come back as new streams, each "played".
    player, _dev = outside

    async def gives_up(device_id, chunks):
        player.streams += 1
        async for chunk in chunks:
            player.played += chunk
            break
        player.released += 1
        return len(player.played)

    inc._player = gives_up

    async def run():
        app = await App.connect()
        _ring(app, 1.5)  # ~12 frames, one stream, then its END
        frame = await app.until(proto.Message.PLAYBACK_DRAINED)
        assert frame.message is proto.Message.PLAYBACK_DRAINED
        assert player.streams == 1
        # Nothing more is owed for it, and a new stream is a new stream.
        _ring(app, 0.1)
        await app.until(proto.Message.PLAYBACK_DRAINED)
        assert player.streams == 2
        app.writer.close()
    asyncio.run(run())


def test_an_announcement_given_up_mid_send_does_not_swallow_the_next_stream(world, outside):
    player, _dev = outside

    async def run():
        app = await App.connect()
        app.send_json(proto.Message.EVENT, {"type": "announcement", "data": {"active": True}})
        _ring(app, 0.2, end=False)
        while not player.played:
            await asyncio.sleep(0.01)
        app.send_json(proto.Message.EVENT, {"type": "announcement", "data": {"active": False}})
        for _ in range(100):
            if player.released:
                break
            await asyncio.sleep(0.01)
        before = len(player.played)
        nxt = _ring(app, 0.3)  # a fresh stream: played in full, and answered
        await app.until(proto.Message.PLAYBACK_DRAINED)
        assert player.streams == 2
        up = em_resample.Upsampler()
        assert bytes(player.played[before:]) == up.process(nxt) + up.flush()
        app.writer.close()
    asyncio.run(run())


def test_a_finished_stream_leaves_nothing_to_cut(world, outside):
    # After an announcement has played and been answered, the next call cuts
    # nothing: no speaker_flush, no stale tail dropped.
    player, dev = outside

    async def run():
        app = await App.connect()
        _ring(app, 0.1)
        await app.until(proto.Message.PLAYBACK_DRAINED)
        task = Turn(dev).run()
        await app.until(proto.Message.CALL_START)
        assert {"type": "speaker_flush"} not in dev.sent
        app.send_json(proto.Message.HANGUP, {"reason": "done"})
        assert await task is False
        app.writer.close()
    asyncio.run(run())


def test_a_failing_player_owner_never_drops_the_app_connection(world, outside, monkeypatch):
    async def boom(device_id):
        raise RuntimeError("em_player broke")

    monkeypatch.setattr(inc.em_player, "interrupt", boom)
    monkeypatch.setattr(inc.em_player, "resume_interrupted", boom)

    async def run():
        app = await App.connect()
        app.send_json(proto.Message.EVENT, {"type": "timer_ringing", "data": {"timers": 1}})
        app.send_json(proto.Message.EVENT, {"type": "timer_stopped", "data": {}})
        app.send(proto.encode(proto.Message.AUDIO_OUT_END))
        assert (await app.until(proto.Message.PLAYBACK_DRAINED)).message is proto.Message.PLAYBACK_DRAINED
        app.writer.close()
    asyncio.run(run())
