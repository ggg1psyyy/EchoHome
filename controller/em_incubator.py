"""
A Voice Incubator satellite per Echo: OHF-Voice's Voice Incubator app talks to
the controller as it would to one of its own satellites.

The Incubator app (OHF-Voice/voice-incubator) finds satellites over mDNS
(`_voice-incubator._tcp`), connects to each over TCP and does everything but
the microphone and the speaker: it decides when a command ended, recognises
it, acts and answers. Its satellites are normally its own ESPHome firmware;
here the controller is the satellite, one per Echo, the way em_esphome is one
ESPHome device per Echo. The wire format is em_incubator_proto's.

An Echo uses this when its effective `voiceBackend` is `incubator`; anything
else (the default `ha`, or a value this controller does not know) leaves it on
Home Assistant's own voice pipeline, and nothing here runs for it. Nothing
falls back from one backend to the other: an Echo set to the Incubator whose
app is not connected has no voice assistant, and says so.

Lifecycle follows em_ble_proxy: `reconcile(device_id)` is the single entry
point (config and approval, from the routes and from the control connection),
`device_connected`/`device_disconnected` bring the listener up and down with
the Echo, idempotent, per-device locked, failures logged and swallowed. The
listener is the ESPHome voice port + PORT_OFFSET, so it needs no allocator and
no column: the app connects to the port mDNS gives, keyed on the `mac` TXT
record, so the number itself may move.

A call is one of em_controller's turns. `trigger_voice_turn` has
em_esphome.trigger_voice_turn's contract (em_voice picks between them), so
the turn loop around it -- arbitration, the voice lock, the ring, the barge
watcher, the follow-up loop, the private session's routing into voice_queue
-- is the same code whichever backend answers. Inside a call:

    CALL_START   wake_word: `echomuse-<model>` for a wake, `manual` for the
                 button, `follow_up` for the answer the app asked for
    AUDIO_IN     voice_queue as it comes, untouched (no speech gate, no
                 denoiser, no pre-roll discard: the app wants the audio)
    on_thinking  when the app says the command ended (`processing`, or any
                 later stage), which is where em_controller closes a private
                 session -- so the Echo streams until end of speech, as it
                 does for Home Assistant, not until the HANGUP after the reply
    AUDIO_OUT    16 kHz, upsampled to the speaker's 48 kHz (em_resample) and
                 handed to post_turn_play as one stream; the socket is read
                 no faster than that queue empties (the spec's backpressure)
    PLAYBACK_DRAINED  once the Echo reports it played (post_turn_play returns)
    HANGUP       `listen` asks for a follow-up: True to the turn loop, which
                 opens the microphone for it (the device refuses while muted)

A barge or the button cancels a call with CALL_END `cancelled`; AUDIO_OUT
still arriving for it is dropped until the app ends that stream.

The wake word is named so that the app does NOT check it: the app re-checks a
wake word it knows (`hey_jarvis`, ...) on the pre-roll, which must hold the
whole word (1.5 s); an Echo's private session carries 260 ms from before the
crossing, so a checked word would be refused. A name the app cannot resolve
stands as detected (voice_incubator/server.py _verify_wake: "A wake word with
no verifier ... stands as detected").
"""

from __future__ import annotations

import asyncio
import logging
import os
import socket
import time
from typing import Optional

from zeroconf import ServiceInfo

import em_db as db
import em_incubator_proto as proto
import em_oww_models
import em_resample
import em_state
import em_tasks
import em_turn_record
import em_turnclock
from esphome.frame_protocol import _enable_tcp_keepalive

log = logging.getLogger("echomuse.incubator")

CONFIG_KEY = "voiceBackend"
BACKEND_HA = "ha"
BACKEND_INCUBATOR = "incubator"
BACKENDS = (BACKEND_HA, BACKEND_INCUBATOR)

PORT_OFFSET = 2000
"""Voice port + 2000: 18001 beside 16001 (voice) and 17001 (BLE proxy,
em_db.BLE_PORT_OFFSET). The channels' 100-port spacing carries over: GA
18001+, EA 18101+, a dev add-on 18201+."""

WAKE_LEAD_MS = 260
"""What a private session carries from before the wake word's crossing
(device/internal/listen WakeLead): HELLO's `preroll_ms`, and a wake call's
pre-roll."""

OUTPUT_BUFFER_BYTES = 4 * proto.BYTE_RATE
"""The controller holds a reply 4 s ahead of the Echo (em_player.LEAD_S =
VOICE_LEAD_S, test-pinned), so that is the buffer it declares, in the app's
16 kHz bytes."""

PLAYBACK_QUEUE_FRAMES = 32
"""AUDIO_OUT frames held between the socket and the speaker: 32 x 4096 bytes
is 4 s at 16 kHz, the same lead. Past it the reader waits, the socket stops
being read, and TCP pushes back on the app."""

CALL_MAX_S = 30.0
"""How long a command may stream before the controller gives up waiting for
the app to end it: the Echo's own session limit (listen maxOpen). The app's
own limit is shorter (15 s)."""

HANGUP_WAIT_S = 10.0
"""After the reply has played, how long the app has to hang up before the
call is ended here (CALL_END `timeout`, as the spec has a satellite do)."""

TTS_WAIT_S = em_turnclock.tts_wait_seconds(os.environ.get("EM_TTS_WAIT_TIMEOUT"))
"""How long the app has, once the command ended, to begin its reply: the
same add-on option (`tts_wait_timeout`) and bounds as Home Assistant's
(em_esphome.TTS_WAIT_TIMEOUT), read the same way, once at import."""

SILENCE_FRAME = b"\x00\x00" * 1280  # 80 ms at 16 kHz

COMMAND_ENDED = frozenset({
    "processing", "recognized", "not_recognized", "responding", "result",
})
"""EVENTs that mean the app has stopped listening to the command."""


def backend(config: dict) -> str:
    """The Echo's voice backend. An unknown value is Home Assistant's: an
    Echo is never moved off the pipeline it has by a value nothing here can
    serve."""
    return BACKEND_INCUBATOR if config.get(CONFIG_KEY) == BACKEND_INCUBATOR else BACKEND_HA


def satellite_model(board: str | None) -> str:
    """HELLO's `model`: what the satellite is, as the spec's examples are
    (`voice-pe`, `box-3`, `linux`)."""
    return f"echomuse-{board}" if board else "echomuse"


def node_name(device_id: str) -> str:
    """The Echo's ESPHome node name (em_esphome's DeviceInfo `name`): HELLO's
    `name` must be it, because it is how Home Assistant knows the device and
    how the app finds its media player."""
    return f"echomuse-{device_id[-12:].lower()}"


def wake_word_name(model: str) -> str:
    """CALL_START's `wake_word` for a wake: the model's key, prefixed so the
    app's verifier cannot resolve it (module docstring)."""
    return f"echomuse-{em_oww_models.prediction_key(model)}"


def call_kind(trigger_label: str, oww_model: str, session: int | None) -> tuple[str, int]:
    """(wake_word, preroll_bytes) for a turn. A wake from a private session
    starts WAKE_LEAD_MS before the crossing; one the controller scored, and
    every other turn, starts at the detection or later."""
    if trigger_label.startswith("wakeword") or trigger_label.startswith("barge-in"):
        lead = WAKE_LEAD_MS * proto.BYTE_RATE // 1000 if session else 0
        return wake_word_name(oww_model), lead
    if trigger_label == "continuation":
        return proto.FOLLOW_UP, 0
    return proto.MANUAL, 0


class Call:
    """One call in flight: what the app has said about it so far."""

    def __init__(self, wake_word: str) -> None:
        self.wake_word = wake_word
        self.command_ended = asyncio.Event()
        self.play_started = asyncio.Event()
        self.finished = asyncio.Event()
        self.hangup: proto.Hangup | None = None
        self.cancelled = False
        self.barged = False
        self.app_gone = False
        self.audio: asyncio.Queue = asyncio.Queue(maxsize=PLAYBACK_QUEUE_FRAMES)
        self.audio_ended = False
        self.last_event: str | None = None

    def on_event(self, event: proto.Event) -> None:
        self.last_event = event.type
        if event.type in COMMAND_ENDED:
            self.command_ended.set()

    def on_hangup(self, hangup: proto.Hangup) -> None:
        self.hangup = hangup
        self.command_ended.set()
        self.finished.set()
        self._end_audio()

    async def on_audio(self, pcm: bytes) -> None:
        if self.cancelled or self.audio_ended:
            return
        self.command_ended.set()
        self.play_started.set()
        await self.audio.put(pcm)

    def on_audio_end(self) -> None:
        self._end_audio()

    def lost(self) -> None:
        self.app_gone = True
        self.command_ended.set()
        self.finished.set()
        self._end_audio()

    def cancel(self) -> None:
        self.cancelled = True
        self.command_ended.set()
        self.finished.set()
        self._end_audio()

    def _end_audio(self) -> None:
        if not self.audio_ended:
            self.audio_ended = True
            try:
                self.audio.put_nowait(None)
            except asyncio.QueueFull:
                # The speaker side is behind; it stops at `audio_ended`.
                pass

    async def pcm_chunks(self):
        """The reply at 48 kHz, as post_turn_play takes it."""
        up = em_resample.Upsampler()
        while True:
            item = await self.audio.get()
            if item is None or self.cancelled:
                break
            out = up.process(item)
            if out:
                yield out
            if self.audio_ended and self.audio.empty():
                break
        if not self.cancelled:
            tail = up.flush()
            if tail:
                yield tail


class Satellite:
    """One Echo's Incubator listener and the app connection it holds."""

    def __init__(self, device_id: str, port: int, mac: str, model: str,
                 firmware: str | None) -> None:
        self.device_id = device_id
        self.port = port
        self.mac = proto.normalise_mac(mac)
        self.model = model
        self.firmware = firmware
        self.name = node_name(device_id)
        self._server: Optional[asyncio.base_events.Server] = None
        self._app: Optional[AppConnection] = None
        self._mdns_info: Optional[ServiceInfo] = None
        self._log = f"incubator.{device_id[-8:]}"

    @property
    def listening(self) -> bool:
        return self._server is not None

    @property
    def app(self) -> Optional[AppConnection]:
        return self._app

    def hello(self) -> bytes:
        return proto.hello(
            name=self.name, mac=self.mac, model=self.model,
            firmware=self.firmware, preroll_ms=WAKE_LEAD_MS,
            output_buffer_bytes=OUTPUT_BUFFER_BYTES, listen=True,
        )

    async def start(self, host: str) -> None:
        if self._server is not None:
            return
        self._server = await asyncio.start_server(self._accept, host, self.port)
        if not self.port:  # tests bind port 0; mDNS must say the real one
            self.port = self._server.sockets[0].getsockname()[1]
        log.info(f"[{self._log}] Listening on {host}:{self.port}")

    async def stop(self) -> None:
        # Detached first, as em_esphome's server: a reconnect during the
        # awaits below starts a fresh listener rather than trusting this one.
        server, self._server = self._server, None
        app, self._app = self._app, None
        if app is not None:
            await app.close()
        if server is not None:
            server.close()
            await server.wait_closed()
            log.info(f"[{self._log}] Stopped")

    async def _accept(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        peer = writer.get_extra_info("peername")
        if self._app is not None or self._server is None:
            # "serves one app at a time: it refuses a second connection
            # while one is open."
            log.warning(f"[{self._log}] Refused a second app connection from {peer}")
            writer.close()
            return
        _enable_tcp_keepalive(writer.transport, self._log)
        app = AppConnection(self, reader, writer)
        self._app = app
        log.info(f"[{self._log}] App connected from {peer}")
        try:
            await app.run()
        finally:
            if self._app is app:
                self._app = None
            log.info(f"[{self._log}] App disconnected ({peer})")

    def status(self) -> dict:
        app = self._app
        return {
            "port": self.port,
            "listening": self.listening,
            "appConnected": app is not None,
            "appPeer": app.peer if app is not None else None,
            "inCall": app is not None and app.call is not None,
        }


class AppConnection:
    """One connection from the app: HELLO, then frames until either side
    closes. A broken frame closes it; the app reconnects."""

    def __init__(self, satellite: Satellite, reader: asyncio.StreamReader,
                 writer: asyncio.StreamWriter) -> None:
        self.satellite = satellite
        self.reader = reader
        self.writer = writer
        peer = writer.get_extra_info("peername")
        self.peer = f"{peer[0]}:{peer[1]}" if isinstance(peer, tuple) else str(peer)
        self.call: Call | None = None
        self._discard_out = False
        self._write_lock = asyncio.Lock()
        self._model_id: str | None = None
        self._closed = False
        self._log = satellite._log

    @property
    def closed(self) -> bool:
        return self._closed

    async def send(self, frame: bytes) -> None:
        if self._closed:
            return
        async with self._write_lock:
            self.writer.write(frame)
            await self.writer.drain()

    async def send_quietly(self, frame: bytes) -> bool:
        """A send that a connection going away cannot turn into an error in
        the turn that made it."""
        try:
            await self.send(frame)
            return True
        except (ConnectionError, OSError) as e:
            log.info(f"[{self._log}] Send failed, app connection going: {e}")
            return False

    async def run(self) -> None:
        try:
            await self.send(self.satellite.hello())
            while True:
                frame = await proto.read_frame(self.reader)
                if frame is None:
                    return
                await self._on_frame(frame)
        except proto.ProtocolError as e:
            log.warning(f"[{self._log}] Closing the app connection: {e}")
        except (ConnectionError, OSError) as e:
            log.info(f"[{self._log}] App connection lost: {e}")
        finally:
            await self.close()

    async def close(self) -> None:
        self._closed = True
        if self.call is not None:
            self.call.lost()
        if not self.writer.is_closing():
            self.writer.close()
        try:
            await self.writer.wait_closed()
        except (ConnectionError, OSError):
            pass

    def cancel_call(self) -> None:
        """The satellite ends the call: CALL_END `cancelled`, and what the app
        is still sending for it is not played (until AUDIO_OUT_END)."""
        call = self.call
        if call is None or call.cancelled:
            return
        call.cancel()
        self._discard_out = True
        em_tasks.spawn(self.send_quietly(proto.call_end(proto.CANCELLED)),
                       name=f"{self._log}-call-end")

    async def _on_frame(self, frame: proto.Frame) -> None:
        msg = frame.message
        if msg is None:
            log.info(f"[{self._log}] Ignoring unknown message 0x{frame.type:02X}")
            return
        if msg in proto.FROM_SATELLITE:
            log.info(f"[{self._log}] Ignoring {msg.name}: a satellite's message, sent by the app")
            return
        call = self.call
        if msg is proto.Message.AUDIO_OUT:
            if self._discard_out:
                return  # a cancelled call's reply
            if call is not None:
                await call.on_audio(frame.payload)
            # Outside a call (an app timer, an announcement): not played yet,
            # and acknowledged at its end below.
            return
        if msg is proto.Message.AUDIO_OUT_END:
            if self._discard_out:
                self._discard_out = False
                return
            if call is not None and call.play_started.is_set():
                call.on_audio_end()  # PLAYBACK_DRAINED once it has played
            else:
                await self.send(proto.playback_drained())
            return
        if msg is proto.Message.MODEL_DATA:
            return
        info = frame.json()
        if msg is proto.Message.EVENT:
            event = proto.Event.parse(info)
            log.debug(f"[{self._log}] EVENT {event.type} {event.data}")
            if call is not None:
                call.on_event(event)
        elif msg is proto.Message.HANGUP:
            hangup = proto.Hangup.parse(info)
            log.info(f"[{self._log}] HANGUP reason={hangup.reason!r} listen={hangup.listen}")
            if call is not None:
                call.on_hangup(hangup)
        elif msg is proto.Message.MODEL_START:
            self._model_id = str(info.get("id") or "") or None
        elif msg is proto.Message.MODEL_END:
            # HELLO says no wake_format, so the app pushes nothing; if it
            # does, each model is answered, as the spec asks.
            model_id = str(info.get("id") or "") or self._model_id
            self._model_id = None
            await self.send(proto.model_refused(
                model_id, "this satellite runs its own wake words (no wake_format)"))
        elif msg is proto.Message.MONITOR:
            log.info(f"[{self._log}] Ignoring MONITOR {info}: this satellite does not stream outside a call")
        else:
            log.debug(f"[{self._log}] {msg.name} {info}")


_satellites: dict[str, Satellite] = {}
_online: set[str] = set()
_locks: dict[str, asyncio.Lock] = {}
_host = "0.0.0.0"


def _device_lock(device_id: str) -> asyncio.Lock:
    return _locks.setdefault(device_id, asyncio.Lock())


def _azc():
    # The ESPHome servers' AsyncZeroconf, as em_ble_proxy uses it: one
    # responder per controller, bound to SERVER_IP's interface.
    from em_esphome import _azc as azc
    return azc


def _make_mdns_info(sat: Satellite, address: str) -> ServiceInfo:
    service = f"{sat.name}.{proto.SERVICE_TYPE}"
    return ServiceInfo(
        proto.SERVICE_TYPE,
        service,
        addresses=[socket.inet_aton(address)],
        port=sat.port,
        properties=proto.txt_records(sat.mac),
        server=f"{sat.name}-vi.local.",
    )


def _server_ip() -> str:
    from em_esphome import SERVER_IP
    return SERVER_IP


async def reconcile(device_id: str) -> None:
    """Bring this Echo's Incubator satellite in line with its effective
    config: created, advertised and listening while the Echo is approved,
    online and set to the Incubator; torn down otherwise. Idempotent."""
    try:
        async with _device_lock(device_id):
            loop = asyncio.get_running_loop()
            row = await loop.run_in_executor(None, db.get_device, device_id)
            wanted = False
            if row is not None and row["approved"]:
                cfg = await loop.run_in_executor(None, db.get_effective_device_config, device_id)
                wanted = backend(cfg) == BACKEND_INCUBATOR
            sat = _satellites.get(device_id)
            if not wanted or device_id not in _online:
                if sat is not None:
                    await _teardown(device_id, sat)
                return
            if sat is None:
                voice_port = await loop.run_in_executor(None, db.get_esphome_port, device_id)
                if voice_port is None:
                    log.warning(f"[{device_id}] Incubator backend set but no ESPHome port yet — deferring")
                    return
                mac = await loop.run_in_executor(None, db.get_esphome_mac, device_id)
                sat = Satellite(device_id, voice_port + PORT_OFFSET, mac,
                                satellite_model(em_state.boards.get(device_id)),
                                row["firmware_ver"])
                _satellites[device_id] = sat
            await sat.start(_host)
            azc = _azc()
            if sat._mdns_info is None and azc is not None:
                info = _make_mdns_info(sat, _server_ip())
                try:
                    await azc.async_register_service(info, allow_name_change=True)
                    sat._mdns_info = info
                    log.info(f"[{device_id}] mDNS registered: {info.name} → port {sat.port}")
                except Exception as e:
                    log.warning(f"[{device_id}] Incubator mDNS registration failed: {e}")
    except Exception as e:
        log.exception(f"[{device_id}] Incubator reconcile failed: {e}")


async def _teardown(device_id: str, sat: Satellite) -> None:
    _satellites.pop(device_id, None)
    azc = _azc()
    if sat._mdns_info is not None and azc is not None:
        try:
            await azc.async_unregister_service(sat._mdns_info)
        except Exception:
            pass
        sat._mdns_info = None
    await sat.stop()
    log.info(f"[{device_id}] Incubator satellite removed")


async def start(host: str = "0.0.0.0") -> None:
    """At controller start, after em_esphome.start_esphome_servers (its
    zeroconf). Echoes are not online yet: each comes up on connect."""
    global _host
    _host = host


async def stop_all() -> None:
    for device_id, sat in list(_satellites.items()):
        await _teardown(device_id, sat)
    _online.clear()


async def device_connected(device_id: str) -> None:
    _online.add(device_id)
    await reconcile(device_id)


async def device_disconnected(device_id: str) -> None:
    _online.discard(device_id)
    await reconcile(device_id)


async def device_deleted(device_id: str) -> None:
    _online.discard(device_id)
    sat = _satellites.get(device_id)
    if sat is not None:
        async with _device_lock(device_id):
            await _teardown(device_id, sat)
    _locks.pop(device_id, None)


def get_status(device_id: str) -> Optional[dict]:
    sat = _satellites.get(device_id)
    return sat.status() if sat is not None else None


# ─── The turn path (em_voice dispatches here) ───────────────────────────────

def serves(device_id: str) -> bool:
    """Whether this Echo's turns are the Incubator's: it has a satellite,
    which it has only while set to the Incubator and online."""
    return device_id in _satellites


def _app(device_id: str) -> Optional[AppConnection]:
    sat = _satellites.get(device_id)
    app = sat.app if sat is not None else None
    return app if app is not None and not app.closed else None


def can_serve_turn(device_id: str) -> bool:
    """Whether a call started now could reach the app: the same call
    trigger_voice_turn refuses on, as em_esphome.can_serve_turn is."""
    return _app(device_id) is not None


async def _wait_any(*aws, timeout: float | None = None) -> None:
    tasks = [asyncio.ensure_future(a) for a in aws]
    try:
        await asyncio.wait(tasks, timeout=timeout, return_when=asyncio.FIRST_COMPLETED)
    finally:
        for t in tasks:
            t.cancel()


async def _stream_command(app: AppConnection, call: Call, device, trace) -> None:
    """voice_queue to AUDIO_IN until cancelled. A turn stream the Echo ended
    itself (its VAD, lock_mic turns) is padded with real-time silence: the
    app decides when a command ended, and hears that it did."""
    pad = False
    while True:
        if pad:
            await asyncio.sleep(0.08)
            await app.send(proto.encode(proto.Message.AUDIO_IN, SILENCE_FRAME))
            continue
        item = await device.voice_queue.get()
        if item is None or isinstance(item, str):
            log.info(f"[{app._log}] Echo ended the stream ({item}) — silence until the app ends the command")
            if trace.t_vad_end_ms == -1:
                trace.mark("t_vad_end_ms")
            pad = True
            continue
        pcm = bytes(item)
        if not pcm:
            continue
        if trace.t_first_frame_ms == -1:
            trace.mark("t_first_frame_ms")
        for frame in proto.audio_frames(proto.Message.AUDIO_IN, pcm):
            await app.send(frame)
        trace.audio_frames += 1


async def trigger_voice_turn(
    device,
    on_thinking,
    post_turn_play,
    trigger_label: str = "unknown",
    preroll_discard: int = 0,  # noqa: ARG001 -- the app wants all the audio
) -> bool:
    """One call, em_esphome.trigger_voice_turn's contract: returns whether
    the app asked to hear an answer (HANGUP `listen`)."""
    wake_info, device.last_wake = device.last_wake, None
    app = _app(device.device_id)
    if app is None or app.call is not None:
        log.warning(f"[{device.device_id}] Incubator: no app connected — cannot start a call")
        await em_turn_record.record_dropped_turn(device, trigger_label, wake_info)
        return False

    wake_word, preroll = call_kind(trigger_label, device.oww_model, device.listen_session)
    trace = em_turn_record.TurnTrace(trigger=trigger_label, t0=time.monotonic(), wake_info=wake_info)
    call = Call(wake_word)
    app.call = call
    wi = wake_info or {}
    sender: asyncio.Task | None = None
    played = False
    try:
        await app.send(proto.call_start(
            wake_word, preroll,
            probability=wi.get("score"),
            cutoff=wi.get("threshold"),
        ))
        log.info(f"[{device.device_id}] Incubator call started: wake_word={wake_word} "
                 f"pre-roll={preroll * 1000 // proto.BYTE_RATE}ms ({trigger_label})")
        sender = em_tasks.spawn(_stream_command(app, call, device, trace),
                                name=f"{app._log}-audio-in")

        await _wait_any(call.command_ended.wait(), device.cancel_event.wait(),
                        timeout=CALL_MAX_S)
        if device.cancel_event.is_set():
            app.cancel_call()
        elif not call.command_ended.is_set():
            log.warning(f"[{device.device_id}] Incubator: the app did not end the command "
                        f"in {CALL_MAX_S:.0f}s — ending the call")
            trace.outcome = "timeout"
            await app.send_quietly(proto.call_end(proto.TIMEOUT))
            call.cancel()
        sender.cancel()

        if call.command_ended.is_set() and not call.cancelled and not call.app_gone \
                and not (call.finished.is_set() and not call.play_started.is_set()):
            if trace.t_vad_end_ms == -1:
                trace.mark("t_vad_end_ms")
            await on_thinking()

        if not call.finished.is_set() or call.play_started.is_set():
            await _wait_any(call.play_started.wait(), call.finished.wait(),
                            device.cancel_event.wait(),
                            timeout=TTS_WAIT_S)
        if device.cancel_event.is_set():
            app.cancel_call()

        if call.play_started.is_set() and not call.cancelled:
            trace.mark("t_playback_ms")
            trace.tts_bytes = await post_turn_play(call.pcm_chunks())
            played = True
            if device.cancel_event.is_set():
                app.cancel_call()
            elif not call.app_gone:
                await app.send_quietly(proto.playback_drained())

        if not call.finished.is_set():
            await _wait_any(call.finished.wait(), device.cancel_event.wait(),
                            timeout=HANGUP_WAIT_S)
            if device.cancel_event.is_set():
                app.cancel_call()
            elif not call.finished.is_set():
                log.warning(f"[{device.device_id}] Incubator: no HANGUP {HANGUP_WAIT_S:.0f}s "
                            f"after the reply — ending the call")
                await app.send_quietly(proto.call_end(proto.TIMEOUT))
                call.cancel()
    finally:
        if sender is not None:
            sender.cancel()
        if app.call is call:
            app.call = None
        trace.outcome = trace.outcome or _outcome(call, played)
        await _record(device, trace)

    return bool(call.hangup is not None and call.hangup.listen
                and not call.cancelled and not device.muted)


def _outcome(call: Call, played: bool) -> str:
    if call.barged:
        return "barged"
    if call.cancelled:
        return "cancelled"
    if call.app_gone:
        return "app_gone"
    reason = call.hangup.reason if call.hangup is not None else ""
    if played:
        return "ok"
    if reason in ("wake_rejected", "answered_elsewhere"):
        return reason
    if reason == "timeout" and call.last_event is None:
        return "no_speech"
    return reason or "no_tts"


async def _record(device, trace) -> None:
    trace.t_complete_ms = trace.elapsed_ms()
    trace.emit()
    wi = trace.wake_info or {}
    turn_record = {
        "ts":             time.time(),
        "trigger":        trace.trigger,
        "wake_model":     wi.get("model"),
        "wake_score":     wi.get("score"),
        "wake_threshold": wi.get("threshold"),
        "noise_floor":    wi.get("noise_floor"),
        "outcome":        trace.outcome,
        "total_ms":       trace.t_complete_ms,
        "vad_end_ms":     trace.t_vad_end_ms,
        "playback_ms":    trace.t_playback_ms,
        "audio_ms":       trace.audio_frames * 80,
        "tts_bytes":      trace.tts_bytes,
    }
    try:
        await em_turn_record.persist_turn(device, turn_record)
        device.turn_history.append(turn_record)
    except Exception as e:
        log.warning(f"[{device.device_id}] Incubator call not recorded: {e}")


def cancel_voice_turn(device_id: str, abort_ha: bool = False,  # noqa: ARG001
                      reason: str = "cancelled") -> None:
    """The button during a call, a barge while the app thinks, mute: the
    call ends here (CALL_END `cancelled`)."""
    app = _app(device_id)
    if app is not None and app.call is not None:
        if reason == "barged":
            app.call.barged = True
        app.cancel_call()


def abort_ha_run(device_id: str) -> None:
    """A barge over the reply: the same CALL_END `cancelled` (the app stops
    answering, and the interrupting wake is a call of its own)."""
    cancel_voice_turn(device_id, reason="barged")


def note_barged(device_id: str) -> None:
    app = _app(device_id)
    if app is not None and app.call is not None:
        app.call.barged = True
