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

This first step serves the connection only: HELLO, the spec's "one app at a
time", model pushes refused, playback acknowledged. Calls come next.
"""

from __future__ import annotations

import asyncio
import logging
import socket
from typing import Optional

from zeroconf import ServiceInfo

import em_db as db
import em_incubator_proto as proto
import em_state
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
(device/internal/listen WakeLead): HELLO's `preroll_ms`."""

OUTPUT_BUFFER_BYTES = 4 * proto.BYTE_RATE
"""The controller holds a reply 4 s ahead of the Echo (em_player.LEAD_S =
VOICE_LEAD_S, test-pinned), so that is the buffer it declares, in the app's
16 kHz bytes."""


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
        self._write_lock = asyncio.Lock()
        self._model_id: str | None = None
        self._log = satellite._log

    async def send(self, frame: bytes) -> None:
        async with self._write_lock:
            self.writer.write(frame)
            await self.writer.drain()

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
        if not self.writer.is_closing():
            self.writer.close()
        try:
            await self.writer.wait_closed()
        except (ConnectionError, OSError):
            pass

    async def _on_frame(self, frame: proto.Frame) -> None:
        msg = frame.message
        if msg is None:
            log.info(f"[{self._log}] Ignoring unknown message 0x{frame.type:02X}")
            return
        if msg in proto.FROM_SATELLITE:
            log.info(f"[{self._log}] Ignoring {msg.name}: a satellite's message, sent by the app")
            return
        if msg is proto.Message.AUDIO_OUT:
            # Calls and playback are the next step; until then what arrives
            # is not played, and the stream is reported played at its end so
            # the app does not wait out its drain timeout.
            return
        if msg is proto.Message.AUDIO_OUT_END:
            await self.send(proto.playback_drained())
            return
        if msg is proto.Message.MODEL_DATA:
            return
        info = frame.json()
        if msg is proto.Message.MODEL_START:
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
            # EVENT, HANGUP, MODEL_CLEAR: nothing to act on without calls.
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
