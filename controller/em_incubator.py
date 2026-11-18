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
import socket
from typing import Optional

from zeroconf import ServiceInfo

import em_db as db
import em_events
import em_incubator_proto as proto
import em_oww_models
import em_state

log = logging.getLogger("echomuse.incubator")

CONFIG_KEY = "voiceBackend"
BACKEND_HA = "ha"
BACKEND_INCUBATOR = "incubator"
ON_ECHO = "incubator_echo"
"""The setting's third value: the Voice Incubator answers this Echo, and the
Echo's own firmware is the satellite when it announces FIRMWARE_CAPABILITY.
Until it does (older firmware, or offline at the decision) the controller's
satellite serves it, as for "incubator". Either way the Echo's backend is the
Incubator: Home Assistant offers no voice assistant for it, and nothing
falls back to Home Assistant."""
BACKENDS = (BACKEND_HA, BACKEND_INCUBATOR)
SETTINGS = (BACKEND_HA, BACKEND_INCUBATOR, ON_ECHO)

FIRMWARE_CAPABILITY = "incubator_satellite"
MONITOR_KEY = "incubatorMonitor"
"""Per Echo, off by default: may the app listen live (Test page, MONITOR: a stream
while it listens). Enforced on the Echo, which offers it only when told."""
MONITOR_CAPABILITY = "incubator_monitor"
MDNS_CAPABILITY = "incubator_mdns"  # the Echo also announces itself (no stand-down)
STANDALONE_KEY, STANDALONE_CAPABILITY = "incubatorStandalone", "incubator_standalone"  # outlives us
"""Firmware whose satellite takes button calls: the controller passes a voice
press on (incubator_press) and the Echo decides what it means there -- stop
a ringing timer, cancel its call, or start one -- as only it knows which."""
ECHO_PORT = 6055
"""Where the Echo's own satellite listens (the protocol's default port)."""

PORT_OFFSET = 2000
"""Voice port + 2000: 18001 beside 16001 (voice) and 17001 (BLE proxy,
em_db.BLE_PORT_OFFSET). The channels' 100-port spacing carries over: GA
18001+, EA 18101+, a dev add-on 18201+."""

WAKE_LEAD_MS = 260
"""A private session's audio from before the crossing (listen WakeLead): the pre-roll."""



READER_LIMIT = 8192







COMMAND_ENDED = frozenset({
    "processing", "recognized", "not_recognized", "responding", "result",
})
"""EVENTs that mean the app has stopped listening to the command."""



EVENT_LOG_KEYS = ("stage", "ok", "active", "reason", "timers", "shown", "on")
"""The only EVENT fields logged. The rest can be what someone said or was
told (`recognized` text, `responding` text, `not_recognized` heard) or who
they are (`processing` speaker), and the log tail goes into support bundles,
which drop transcript lines by pattern, not by guessing."""


def _identifier(value) -> str:
    """The app's word for something (a hang-up reason, an event's stage),
    kept only while it looks like one: short, lowercase, letters and
    underscores. Anything else -- which no part of the app sends today --
    becomes `other` before it reaches the log, the turn record or a bundle."""
    text = str(value)
    if 0 < len(text) <= 32 and all(c.islower() or c == "_" for c in text) and text.isascii():
        return text
    return "other"


def _event_summary(event: proto.Event) -> dict:
    return {k: (_identifier(event.data[k]) if isinstance(event.data[k], str) else event.data[k])
            for k in EVENT_LOG_KEYS if k in event.data}


def backend(config: dict) -> str:
    """The Echo's voice backend. An unknown value is Home Assistant's: an
    Echo is never moved off the pipeline it has by a value nothing here can
    serve."""
    return BACKEND_INCUBATOR if config.get(CONFIG_KEY) in (BACKEND_INCUBATOR, ON_ECHO) else BACKEND_HA


def wants_on_echo(config: dict) -> bool:
    """Whether the setting asks for the Echo's own satellite (ON_ECHO)."""
    return config.get(CONFIG_KEY) == ON_ECHO


def echo_capable(device_id: str) -> bool:
    """Whether the connected Echo's firmware can be the satellite itself."""
    dev = em_state.devices.get(device_id)
    return dev is not None and FIRMWARE_CAPABILITY in (getattr(dev, "capabilities", None) or [])


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










_online: set[str] = set()


class EchoSatellite:
    """An Echo whose own firmware is the satellite: the controller tells it to
    listen (incubator_satellite) under the identity the app already knows it
    by, and advertises it on the app's network -- the Echo's address, its port
    -- while the Echo reports that it listens (incubator_state). The app then
    connects to the Echo directly; nothing of the call crosses the controller."""

    def __init__(self, device_id: str, mac: str, port: int = ECHO_PORT):
        self.device_id = device_id
        self.name = node_name(device_id)
        self.mac = proto.normalise_mac(mac)
        self.port = port
        self.monitor = self.standalone = False  # MONITOR_KEY, STANDALONE_KEY
        self.address: Optional[str] = None
        self.state: dict = {}
        self._mdns_info: Optional[ServiceInfo] = None

    def message(self, enabled: bool) -> dict:
        return {"type": "incubator_satellite", "enabled": enabled,
                "name": self.name, "mac": self.mac, "port": self.port,
                "monitor": enabled and self.monitor, "standalone": enabled and self.standalone}


_on_echo: dict[str, EchoSatellite] = {}
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


_locks: dict[str, asyncio.Lock] = {}
_host = "0.0.0.0"


def _device_lock(device_id: str) -> asyncio.Lock:
    return _locks.setdefault(device_id, asyncio.Lock())


def _azc():
    # The controller's own mDNS responder (em_state.zeroconf, set by
    # em_controller.main): one per controller, bound to SERVER_IP's
    # interface. None until then, and the announcement waits for it.
    return em_state.zeroconf




def _server_ip() -> str:
    return em_state.link["ip"]


async def reconcile(device_id: str) -> None:
    """Bring this Echo's Incubator satellite in line with its effective
    config: created, advertised and listening while the Echo is approved,
    online and set to the Incubator; torn down otherwise. Idempotent."""
    try:
        async with _device_lock(device_id):
            loop = asyncio.get_running_loop()
            row = await loop.run_in_executor(None, db.get_device, device_id)
            wanted = on_echo = False
            if row is not None and row["approved"]:
                cfg = await loop.run_in_executor(None, db.get_effective_device_config, device_id)
                wanted = backend(cfg) == BACKEND_INCUBATOR
                on_echo = wanted and wants_on_echo(cfg) and echo_capable(device_id)
            _note_backend(device_id, BACKEND_INCUBATOR if wanted else BACKEND_HA)
            online, rec = device_id in _online, _on_echo.get(device_id)
            if wanted and not online and rec and rec.standalone and wants_on_echo(cfg):
                # The Echo keeps its satellite (always, since EchoHome) and
                # answers without us; our record still finds it across VLANs.
                return
            if not wanted or not online or not on_echo:
                # Off the Echo's own satellite: tell a connected Echo to stop.
                await _end_on_echo(device_id, tell=online)
                if wanted and online:
                    log.warning(f"[{device_id}] set to the Voice Incubator, but the controller no "
                                f"longer hosts satellites (EchoHome): set it to 'Voice Incubator, "
                                f"on the Echo' (needs EchoHome firmware)")
                return
            # Standalone wherever the firmware can be: the Echo keeps its
            # satellite whatever the switch says (EchoHome firmware), so it is
            # announced for while offline whenever it can answer.
            dev = em_state.devices.get(device_id)
            await _start_on_echo(device_id, loop, monitor_allowed(cfg, device_id),
                                 STANDALONE_CAPABILITY in (getattr(dev, "capabilities", None) or []))
    except Exception as e:
        log.exception(f"[{device_id}] Incubator reconcile failed: {e}")


def monitor_allowed(config: dict, device_id: str, key: str = MONITOR_KEY,
                    capability: str = MONITOR_CAPABILITY) -> bool:
    """An Echo's switch (live listening, standalone) on, and firmware that can: only True."""
    dev = em_state.devices.get(device_id)
    return config.get(key) is True and capability in (getattr(dev, "capabilities", None) or [])


def monitored(device_id: str) -> bool:
    """Whether the Echo REPORTS streaming its microphone to the app outside a
    call (incubator_state monitored): the privacy line's source, never the
    switch."""
    rec = _on_echo.get(device_id)
    return rec is not None and bool(rec.state.get("monitored"))


async def _start_on_echo(device_id: str, loop, monitor: bool = False, standalone: bool = False) -> None:
    rec = _on_echo.get(device_id)
    if rec is None:
        mac = await loop.run_in_executor(None, db.get_esphome_mac, device_id)
        rec = _on_echo[device_id] = EchoSatellite(device_id, mac)
    # In the message the Echo compares: a change restarts it (the app reads a new HELLO).
    rec.monitor, rec.standalone = monitor, standalone
    dev = em_state.devices.get(device_id)
    address = getattr(dev, "ip", None)
    if rec.address != address:
        await _unadvertise(rec)  # a new address: the record must say it
        rec.address = address
    # Sent on every reconcile (each connect): the firmware keeps a running
    # satellite and its app on an identical message, and answers with its
    # state either way.
    await dev.send_control(rec.message(True))
    log.info(f"[{device_id}] Incubator satellite on the Echo asked to listen as {rec.name}")


async def _end_on_echo(device_id: str, tell: bool) -> None:
    rec = _on_echo.pop(device_id, None)
    if rec is None:
        return
    await _unadvertise(rec)
    dev = em_state.devices.get(device_id)
    if tell and dev is not None:
        try:
            await dev.send_control(rec.message(False))
        except Exception as e:
            log.info(f"[{device_id}] incubator_satellite off not delivered: {e}")
    log.info(f"[{device_id}] Incubator satellite on the Echo released")


def _make_echo_mdns_info(rec: EchoSatellite) -> ServiceInfo:
    # The same service and host names the controller's satellite uses, so the
    # app sees one satellite throughout, now at the Echo's address.
    return ServiceInfo(
        proto.SERVICE_TYPE,
        f"{rec.name}.{proto.SERVICE_TYPE}",
        addresses=[socket.inet_aton(rec.address)],
        port=rec.port,
        properties=proto.txt_records(rec.mac),
        server=f"{rec.name}-vi.local.",
    )


async def _advertise(rec: EchoSatellite) -> None:
    azc = _azc()
    if rec._mdns_info is not None or azc is None or not rec.address:
        return
    try:
        # Inside the try: an address inet_aton refuses (IPv6, say) is logged,
        # never raised into the control loop.
        info = _make_echo_mdns_info(rec)
        await azc.async_register_service(info, allow_name_change=True)
        rec._mdns_info = info
        log.info(f"[{rec.device_id}] mDNS registered: {info.name} → the Echo, port {rec.port}")
    except Exception as e:
        log.warning(f"[{rec.device_id}] Incubator mDNS registration failed: {e}")


async def _unadvertise(rec: EchoSatellite) -> None:
    azc = _azc()
    if rec._mdns_info is not None and azc is not None:
        try:
            await azc.async_unregister_service(rec._mdns_info)
        except Exception:
            pass
    rec._mdns_info = None


async def on_incubator_state(device, msg: dict) -> None:
    """em_controller's handler for incubator_state: the Echo's own satellite,
    as it changes. Advertised while it listens, withdrawn otherwise; a report
    from an Echo not asked to listen is logged and ignored."""
    device_id = device.device_id
    st = msg.get("state") if isinstance(msg.get("state"), dict) else {}
    state = {"listening": bool(st.get("listening")), "appConnected": bool(st.get("appConnected")),
             "inCall": bool(st.get("inCall")),
             "monitored": st.get("monitored") is True,
             "wakeModels": st["wakeModels"] if type(st.get("wakeModels")) is int and st["wakeModels"] >= 0 else 0,
             "port": st.get("port") if isinstance(st.get("port"), int) else None,
             "error": str(st.get("error"))[:200] if st.get("error") else ""}
    async with _device_lock(device_id):
        rec = _on_echo.get(device_id)
        if rec is None:
            if state["listening"]:
                log.info(f"[{device_id}] incubator_state listening, not asked to: ignored")
            return
        rec.state = state
        if isinstance(state["port"], int) and 0 < state["port"] < 65536:
            if state["port"] != rec.port:
                await _unadvertise(rec)
            rec.port = state["port"]
        if state["listening"]:
            await _advertise(rec)  # beside the Echo's own: this one crosses VLANs
        else:
            await _unadvertise(rec)
        if state["error"]:
            log.warning(f"[{device_id}] Incubator satellite on the Echo: {state['error']}")
    await em_events.push({"type": "device_update", "device_id": device_id,
                          "state": {"voiceIncubator": status_view(device_id)}})






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
    for device_id in list(_on_echo):
        # The Echo stops its own when the controller link goes.
        await _end_on_echo(device_id, tell=False)
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
    async with _device_lock(device_id):
        await _end_on_echo(device_id, tell=False)
    _locks.pop(device_id, None)


def status_view(device_id: str) -> Optional[dict]:
    """For the dashboard (em_device_view): None for an Echo on Home
    Assistant, else what its Voice Incubator satellite is doing -- the same
    state the turn path refuses on (can_serve_turn), as em_esphome.get_status
    is for HA. No peer address: the panel needs none."""
    if backend_of(device_id) != BACKEND_INCUBATOR:
        return None
    rec = _on_echo.get(device_id)
    if rec is not None:
        st = rec.state
        return {"port": rec.port, "listening": bool(st.get("listening")),
                "appConnected": bool(st.get("appConnected")),
                "inCall": bool(st.get("inCall")), "wakeModels": st.get("wakeModels", 0),
                "monitored": bool(st.get("monitored")), "monitorAllowed": rec.monitor,
                "onEcho": True}
    # Set to the Voice Incubator, not (yet) on the Echo: nothing runs it.
    return {"port": None, "listening": False, "appConnected": False, "inCall": False}































