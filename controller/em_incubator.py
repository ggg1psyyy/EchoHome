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
import em_player
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

RECEIVE_BUFFER_BYTES = 16384
"""SO_RCVBUF for the app's connections (set on the listening socket; Linux
doubles it and accepted sockets inherit it), with READER_LIMIT for asyncio's
own buffer. The spec's pacing is TCP backpressure: the app writes until the
satellite stops reading. Measured on loopback, a test app bursting a 60 s
reply at a speaker playing at its own pace had 0.55-0.9 MB of it off its
hands within a second (17-28 s of audio) with the kernel left to autotune;
sized, ~0.37 MB (~11 s). Both figures include that test app's own 64 KB send
buffer and the second already played, so what this side takes in is about
the reply queue (~4 s) plus a little: room against the app's 30 s drain wait
that up to 28 s held unplayed did not leave. (The real app's send buffer is
not ours to size.)"""

READER_LIMIT = 8192

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

BACKGROUND_IDLE_S = 5.0
"""A stream outside a call that gets no frame for this long was abandoned by
the app (its ring or announcement task cancelled mid-send, so no
AUDIO_OUT_END will come): what came is played and the stream let go,
unanswered. The app paces its ring and announcements in real time, a frame
every ~128 ms, so a gap this long is never a stream still in progress."""

COMMAND_ENDED = frozenset({
    "processing", "recognized", "not_recognized", "responding", "result",
})
"""EVENTs that mean the app has stopped listening to the command."""

CALL_EVENTS = COMMAND_ENDED | {"listening", "wake_rejected", "wake_overturned"}
"""EVENTs about the call itself, as opposed to ones the app sends at any time
(timer_progress, night): the first of them marks where the call's own
traffic begins (Call.stale)."""


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


class Stream:
    """One AUDIO_OUT stream: frames until AUDIO_OUT_END, played as one
    post_turn_play and answered with one PLAYBACK_DRAINED. A call can hold
    several -- the app's call sounds ("heard", "asking") are a stream it may
    end on its own before the reply's (voice_incubator/server.py Cues) -- and
    the app waits for each one's PLAYBACK_DRAINED."""

    def __init__(self, number: int) -> None:
        self.number = number
        self.audio: asyncio.Queue = asyncio.Queue(maxsize=PLAYBACK_QUEUE_FRAMES)
        self.ended = False      # nothing more is queued for the speaker
        self.app_ended = False  # the app sent its AUDIO_OUT_END
        self.cut = False        # stopped on purpose: no PLAYBACK_DRAINED for it
        self.broken = False     # the speaker side stopped first: drop the rest
        self.abandoned = False  # no frame for BACKGROUND_IDLE_S: no END coming
        self.bytes = 0
        self.last_frame = time.monotonic()

    async def put(self, pcm: bytes) -> None:
        if self.ended:
            return
        self.bytes += len(pcm)
        await self.audio.put(pcm)

    def end(self) -> None:
        if not self.ended:
            self.ended = True
            try:
                self.audio.put_nowait(None)
            except asyncio.QueueFull:
                # The speaker side is behind; it stops at `ended`.
                pass

    def discard(self) -> None:
        """Nothing more of it is played. Emptied, not just ended: a long reply
        fills the queue, the socket reader then waits in put(), and with the
        speaker side gone nothing would ever make room -- every later EVENT and
        HANGUP would sit unread behind it."""
        while True:
            try:
                self.audio.get_nowait()
            except asyncio.QueueEmpty:
                break
        self.end()

    async def pcm_chunks(self, stopped, idle: float | None = None):
        """The stream at 48 kHz, as post_turn_play takes it. `stopped()`
        says the call was cancelled: no more, and no filter tail. With
        `idle`, a stream that gets no frame for that long is abandoned."""
        up = em_resample.Upsampler()
        while True:
            if idle is None:
                item = await self.audio.get()
            else:
                try:
                    item = await asyncio.wait_for(self.audio.get(), idle)
                except asyncio.TimeoutError:
                    self.abandoned = True
                    item = None
            if item is None or stopped():
                break
            out = up.process(item)
            if out:
                yield out
            if self.ended and self.audio.empty():
                break
        if not stopped():
            tail = up.flush()
            if tail:
                yield tail


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
        self.streams: asyncio.Queue = asyncio.Queue()
        self.current: Stream | None = None
        self._count = 0
        self.last_event: str | None = None
        self.stale = False
        """Set for a call that started over the app's own playback (a ring, an
        announcement): AUDIO_OUT still in flight belongs to that, not to this
        call, until the app's first EVENT or HANGUP for the call -- TCP order
        makes that the boundary. Without it the tail of a ring would read as
        this call's reply and end the command before anyone spoke."""

    def stopped(self) -> bool:
        return self.cancelled or self.app_gone

    def on_event(self, event: proto.Event) -> None:
        self.last_event = event.type
        if event.type in COMMAND_ENDED:
            self.command_ended.set()

    def on_hangup(self, hangup: proto.Hangup) -> None:
        self.hangup = hangup
        self.command_ended.set()
        self.finished.set()
        if self.current is not None:
            self.current.end()

    async def on_audio(self, pcm: bytes) -> None:
        if self.stopped():
            return
        self.command_ended.set()
        self.play_started.set()
        if self.current is None or self.current.ended:
            self._count += 1
            self.current = Stream(self._count)
            self.streams.put_nowait(self.current)
        await self.current.put(pcm)

    def on_audio_end(self) -> Stream | None:
        """The stream that just ended, to be answered once it has played; None
        when there was none (an empty stream: answered at once)."""
        stream = self.current
        if stream is None or stream.ended:
            return None
        stream.end()
        return stream

    def lost(self) -> None:
        self.app_gone = True
        self.command_ended.set()
        self.finished.set()
        self.discard()

    def cancel(self) -> None:
        self.cancelled = True
        self.command_ended.set()
        self.finished.set()
        self.discard()

    def discard(self) -> None:
        """Nothing more of this call is played (Stream.discard)."""
        if self.current is not None:
            self.current.discard()
        while True:
            try:
                self.streams.get_nowait().discard()
            except asyncio.QueueEmpty:
                break


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
        self._server = await asyncio.start_server(self._accept, host, self.port,
                                                  limit=READER_LIMIT)
        for sock in self._server.sockets:
            try:
                sock.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, RECEIVE_BUFFER_BYTES)
            except OSError as e:
                log.warning(f"[{self._log}] Could not size the receive buffer: {e}")
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
        self.background: Stream | None = None
        self._background_task: asyncio.Task | None = None
        self._background_count = 0
        self.timer_ringing = False
        self._holds: set[str] = set()
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
        await self.cut_background("the app went away")
        await self._release()
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

    # ── Playback outside a call: the app's ring, its announcements ──────

    @property
    def playing_outside_call(self) -> bool:
        task = self._background_task
        return task is not None and not task.done()

    async def _background_audio(self, pcm: bytes) -> None:
        stream = self.background
        now = time.monotonic()
        if stream is not None and stream.broken and not stream.app_ended:
            if now - stream.last_frame <= BACKGROUND_IDLE_S:
                stream.last_frame = now
                return  # the rest of a stream whose playback stopped early
            # No AUDIO_OUT_END came for it and the app went quiet: it gave
            # that stream up. This frame starts the next one.
            log.info(f"[{self._log}] Broken stream {stream.number} never ended — letting it go")
            self.background = stream = None
        if stream is None or stream.ended:
            self._background_count += 1
            stream = self.background = Stream(self._background_count)
            self._background_task = em_tasks.spawn(
                self._play_background(stream), name=f"{self._log}-playback")
            log.info(f"[{self._log}] Playback outside a call: stream {stream.number}"
                     f"{' (timer ringing)' if self.timer_ringing else ''}")
        stream.last_frame = now
        await stream.put(pcm)

    def _background_end(self) -> bool:
        """AUDIO_OUT_END outside a call. True when it ends a stream still
        playing (answered once played); False when it is answered now."""
        stream = self.background
        if stream is None or stream.app_ended:
            return False
        stream.app_ended = True
        if stream.broken:
            # Its playback stopped early and the rest was dropped; the app is
            # still owed its answer, now.
            self.background = None
            return False
        stream.end()
        log.info(f"[{self._log}] AUDIO_OUT_END: playback stream {stream.number} outside a call, "
                 f"{stream.bytes} bytes ({stream.bytes * 1000 // proto.BYTE_RATE} ms)")
        return True

    async def _play_background(self, stream: Stream) -> None:
        device_id = self.satellite.device_id
        player = _player
        try:
            if player is None:
                log.warning(f"[{self._log}] No player registered — app playback not played")
            else:
                await player(device_id, stream.pcm_chunks(lambda: stream.cut, idle=BACKGROUND_IDLE_S))
        except asyncio.CancelledError:
            raise
        except Exception as e:
            log.warning(f"[{self._log}] App playback failed: {e}")
        finally:
            if not stream.cut and not stream.app_ended and not stream.abandoned:
                # Stopped before the app finished sending (the player failed,
                # the Echo dropped): the rest is dropped, not replayed as new
                # streams each answered as played.
                stream.broken = True
            stream.discard()  # whatever is left must not hold the reader
        if stream.abandoned:
            log.info(f"[{self._log}] Playback stream {stream.number} abandoned by the app "
                     f"(no AUDIO_OUT_END) — played what came, unanswered")
            if self.background is stream:
                self.background = None
            return
        if stream.cut or stream.broken or self._closed:
            return
        if self.background is stream:
            self.background = None  # played and answered: nothing left to cut
        await self.send_quietly(proto.playback_drained())

    def _detach_background(self) -> tuple[Stream | None, asyncio.Task | None]:
        """Take the background stream and its playback, synchronously, so
        nothing the app sends next is queued behind a stream being cut. A
        second caller finds nothing, rather than cancelling the task again
        inside its cleanup (which would skip resume_interrupted)."""
        stream, task = self.background, self._background_task
        self.background, self._background_task = None, None
        if stream is not None:
            stream.cut = True
            stream.discard()
        return stream, task

    async def _stop_background(self, stream: Stream | None, task: asyncio.Task | None,
                               why: str) -> bool:
        """The Echo's buffer flushed first, then the playback ended
        (em_player's order). Flushed whenever a stream was detached, even if
        its playback task already returned: ending the stream lets up to the
        4 s lead already sent play out on the Echo. Never through
        cancel_event: a call starting over the ring races that same event,
        and would be cancelled at birth."""
        alive = task is not None and not task.done()
        if stream is None and not alive:
            return False
        device = em_state.devices.get(self.satellite.device_id)
        if device is not None:
            try:
                await device.send_control({"type": "speaker_flush"})
            except Exception as e:
                log.info(f"[{self._log}] speaker_flush failed: {e}")
        if alive:
            task.cancel()
            try:
                await task
            except asyncio.CancelledError:
                pass
            except Exception:
                pass
        log.info(f"[{self._log}] App playback cut: {why}")
        return True

    async def cut_background(self, why: str) -> bool:
        """Stop what the app is playing outside a call, at once. Returns
        whether anything was playing."""
        stream, task = self._detach_background()
        return await self._stop_background(stream, task, why)

    # The ring and an announcement hold the music down from start to end: one
    # owner for the whole of it, not one per stream (each ring is its own
    # stream ~1 s apart, an announcement is a chime stream then a speech
    # stream), or music pumps up in every gap. One em_player owner for any
    # number of reasons ("ring", "announcement"), taken on the first, given
    # back on the last.

    async def _hold(self, reason: str) -> None:
        first = not self._holds
        self._holds.add(reason)
        if first:
            try:
                await em_player.interrupt(self.satellite.device_id)
            except Exception as e:  # never the app connection's problem
                log.warning(f"[{self._log}] Holding music ({reason}) failed: {e}")

    async def _release(self, reason: str | None = None) -> None:
        """Give back `reason`'s hold, or every hold (None)."""
        if not self._holds:
            return
        if reason is None:
            self._holds.clear()
        else:
            self._holds.discard(reason)
            if self._holds:
                return
        try:
            await em_player.resume_interrupted(self.satellite.device_id)
        except Exception as e:
            log.warning(f"[{self._log}] Releasing music failed: {e}")

    async def _hold_for_ring(self) -> None:
        await self._hold("ring")

    async def _release_ring(self) -> None:
        await self._release("ring")

    async def stop_timer(self) -> None:
        """The button while the app's timer rings: silence it here, and tell
        the app (DEVICE_EVENT stop_timer); it stops the ring everywhere."""
        await self.cut_background("button: stop the timer")
        self.timer_ringing = False
        await self._release_ring()
        await self.send_quietly(proto.device_event(proto.STOP_TIMER))

    async def _on_app_event(self, event: proto.Event) -> None:
        """EVENTs that matter outside a call."""
        if event.type == "timer_ringing":
            self.timer_ringing = True
            await self._hold_for_ring()
        elif event.type == "timer_stopped":
            self.timer_ringing = False
            # Detached now, so a stream that follows starts afresh; the flush
            # and the end of the old playback run behind it.
            stream, task = self._detach_background()
            em_tasks.spawn(self._stop_background(stream, task, "the app stopped its timer"),
                           name=f"{self._log}-cut")
            await self._release_ring()
        elif event.type == "announcement" and event.data.get("active") is True:
            await self._hold("announcement")
        elif event.type == "announcement" and event.data.get("active") is False:
            # Normally the stream ended already; if the app gave up on it
            # mid-send, no AUDIO_OUT_END follows -- let what came play out,
            # unanswered, and the next stream start afresh (as an idle one).
            stream = self.background
            if stream is not None and stream.broken:
                self.background = None  # nothing more of it is coming
            elif stream is not None and not stream.app_ended:
                stream.abandoned = True
                stream.end()
            await self._release("announcement")

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
            if call is not None and call.stale:
                return  # the ring or announcement this call cut
            if call is not None:
                await call.on_audio(frame.payload)
            else:
                await self._background_audio(frame.payload)
            return
        if msg is proto.Message.AUDIO_OUT_END:
            if self._discard_out:
                self._discard_out = False
                log.info(f"[{self._log}] AUDIO_OUT_END of a cancelled call's stream — dropped")
                return
            if call is not None and call.stale:
                log.info(f"[{self._log}] AUDIO_OUT_END of the playback a call cut — dropped")
                return
            if call is None and self._background_end():
                return  # PLAYBACK_DRAINED once it has played (_play_background)
            stream = call.on_audio_end() if call is not None else None
            if stream is not None:
                # PLAYBACK_DRAINED once it has played (trigger_voice_turn).
                log.info(f"[{self._log}] AUDIO_OUT_END: stream {stream.number} of the call, "
                         f"{stream.bytes} bytes ({stream.bytes * 1000 // proto.BYTE_RATE} ms)")
            else:
                log.info(f"[{self._log}] AUDIO_OUT_END with nothing playing"
                         f"{' in the call' if call is not None else ' (outside a call)'} — drained")
                await self.send(proto.playback_drained())
            return
        if msg is proto.Message.MODEL_DATA:
            return
        info = frame.json()
        if msg is proto.Message.EVENT:
            event = proto.Event.parse(info)
            log.debug(f"[{self._log}] EVENT {event.type} {event.data}")
            await self._on_app_event(event)
            if call is not None:
                if event.type in CALL_EVENTS:
                    call.stale = False
                call.on_event(event)
        elif msg is proto.Message.HANGUP:
            hangup = proto.Hangup.parse(info)
            log.info(f"[{self._log}] HANGUP reason={hangup.reason!r} listen={hangup.listen}")
            if call is not None:
                call.stale = False
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
_backends: dict[str, str] = {}
"""Each approved Echo's voice backend as last read from its config: filled at
start for every one (before any connects, so Home Assistant's first read of
an Echo's ESPHome device already has it) and kept by reconcile. em_esphome
reads it to leave the voice assistant out of an Incubator Echo's device."""


def backend_of(device_id: str) -> str:
    return _backends.get(device_id, BACKEND_HA)


def _note_backend(device_id: str, value: str) -> None:
    """A change Home Assistant must see: its ESPHome connection reads the
    device's features once, so the Echo's satellite is bounced -- only on an
    actual change, as every bounce flaps the device's entities."""
    before = _backends.get(device_id, BACKEND_HA)
    _backends[device_id] = value
    if value != before:
        log.info(f"[{device_id}] Voice backend {before} → {value}")
        from em_esphome import voice_backend_changed
        voice_backend_changed(device_id)
_player = None
"""em_controller's playback for the app's streams outside a call, set once at
start (set_player): async (device_id, pcm_chunks at 48 kHz) -> bytes played.
A function, not a Device: it looks the Echo up when it plays, so it survives
a reconnect."""


def set_player(player) -> None:
    global _player
    _player = player
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
            _note_backend(device_id, BACKEND_INCUBATOR if wanted else BACKEND_HA)
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
    """At controller start, before the device listeners open (em_controller
    main): every approved Echo's backend is read now, so Home Assistant's
    first read of an Incubator Echo's ESPHome device already leaves the voice
    assistant out, and no restart bounces it. Satellites come up as Echoes
    connect; zeroconf is looked up then (em_esphome's, started by then)."""
    global _host
    _host = host
    loop = asyncio.get_running_loop()
    try:
        rows = await loop.run_in_executor(None, db.get_all_devices)
        for row in rows:
            if row["approved"]:
                cfg = await loop.run_in_executor(None, db.get_effective_device_config, row["device_id"])
                _backends[row["device_id"]] = backend(cfg)
    except Exception as e:
        log.warning(f"Reading voice backends at start failed: {e}")


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
    _backends.pop(device_id, None)
    sat = _satellites.get(device_id)
    if sat is not None:
        async with _device_lock(device_id):
            await _teardown(device_id, sat)
    _locks.pop(device_id, None)


def get_status(device_id: str) -> Optional[dict]:
    sat = _satellites.get(device_id)
    return sat.status() if sat is not None else None


def status_view(device_id: str) -> Optional[dict]:
    """For the dashboard (em_device_view): None for an Echo on Home
    Assistant, else what its Voice Incubator satellite is doing -- the same
    state the turn path refuses on (can_serve_turn), as em_esphome.get_status
    is for HA. No peer address: the panel needs none."""
    if backend_of(device_id) != BACKEND_INCUBATOR:
        return None
    sat = _satellites.get(device_id)
    if sat is None:
        return {"port": None, "listening": False, "appConnected": False, "inCall": False}
    st = sat.status()
    return {"port": st["port"], "listening": st["listening"],
            "appConnected": can_serve_turn(device_id), "inCall": st["inCall"]}


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


async def _next_stream(call: Call, device, timeout: float) -> Stream | None:
    """The call's next stream; None when the call finished with none left,
    the turn was cancelled, or `timeout` passed. A stream already waiting
    wins over a HANGUP that came after it."""
    get = asyncio.ensure_future(call.streams.get())
    others = [asyncio.ensure_future(call.finished.wait()),
              asyncio.ensure_future(device.cancel_event.wait())]
    try:
        await asyncio.wait([get, *others], timeout=timeout,
                           return_when=asyncio.FIRST_COMPLETED)
        if get.done() and not get.cancelled():
            return get.result()
        return None
    finally:
        get.cancel()
        for t in others:
            t.cancel()


async def _stream_command(app: AppConnection, call: Call, device, trace,
                          pad_idle: bool = False) -> None:
    """voice_queue to AUDIO_IN until cancelled. The app decides when a
    command ended, and only from audio it receives, so a stream the Echo
    ended itself (its VAD, lock_mic turns) is padded with real-time silence;
    with `pad_idle` (those same wakeless turns, whose stream the Echo gates
    on its VAD and starts only once someone speaks), so is the wait before
    the first frame -- otherwise "nothing was said" can never be heard."""
    pad = False
    while True:
        if pad:
            await asyncio.sleep(0.08)
            await app.send(proto.encode(proto.Message.AUDIO_IN, SILENCE_FRAME))
            continue
        if pad_idle and trace.t_first_frame_ms == -1:
            try:
                item = await asyncio.wait_for(device.voice_queue.get(), 0.08)
            except asyncio.TimeoutError:
                await app.send(proto.encode(proto.Message.AUDIO_IN, SILENCE_FRAME))
                continue
        else:
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
    # A call cuts what the app was playing (as the reference firmware's
    # start_call resets its output), and its tail still in flight is not
    # this call's (Call.stale) -- marked BEFORE the call is published, so a
    # ring frame read while the cut is being awaited is dropped rather than
    # taken for the call's reply.
    call.stale = app.playing_outside_call or app.timer_ringing
    app.call = call
    await app.cut_background(f"a call ({trigger_label})")
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
        sender = em_tasks.spawn(_stream_command(app, call, device, trace,
                                                pad_idle=wake_word in (proto.MANUAL, proto.FOLLOW_UP)),
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

        # Every stream the app sends, each played and answered in turn: the
        # first within the reply wait, any later one within the hang-up wait.
        wait = TTS_WAIT_S
        while not call.stopped():
            stream = await _next_stream(call, device, wait)
            if device.cancel_event.is_set():
                app.cancel_call()
                break
            if stream is None:
                break
            if not played:
                trace.mark("t_playback_ms")
            trace.tts_bytes += (await post_turn_play(stream.pcm_chunks(call.stopped))) or 0
            played = True
            if device.cancel_event.is_set():
                app.cancel_call()
                break
            if not call.app_gone:
                await app.send_quietly(proto.playback_drained())
            wait = HANGUP_WAIT_S

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
        # Whatever the reply had left is not played now, and must not hold
        # the socket reader (Call.discard).
        call.discard()
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
    if reason == "timeout" and call.last_event not in COMMAND_ENDED:
        # Hung up for time before the command ever ended: nothing was said.
        # (`listening`, which the app sends on accepting the wake, does not
        # count -- it means the app was waiting to hear something.)
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


def wake_over_playback(device_id: str) -> bool:
    """Whether a wake heard over this Echo's own playback must still reach
    the turn path: the app's ring and announcements are stopped by a wake
    (it starts a call, which cuts them), as EchoMuse's own alarm is."""
    app = _app(device_id)
    # timer_ringing too: between two ring streams nothing plays for ~1 s,
    # and a wake there is still a wake over the ring.
    return app is not None and app.call is None and (app.playing_outside_call or app.timer_ringing)


def app_timer_ringing(device_id: str) -> bool:
    app = _app(device_id)
    return app is not None and app.timer_ringing


async def stop_app_timer(device_id: str) -> None:
    app = _app(device_id)
    if app is not None:
        await app.stop_timer()


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
