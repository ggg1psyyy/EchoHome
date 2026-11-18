"""
EchoHome setup tool — the device link
=====================================

The WebSocket server Echoes dial, found by mDNS (_emcontroller._tcp, the
name older firmware looks for), and the Device each connection becomes. The
HTTP API and dashboard are registered by em_api on API_PORT.

Each Echo opens three planes (docs/device-controller-interface.md is the
contract):
    /control — JSON: register/ack, config, stats, state reports, WiFi, the
               satellite's enable (incubator_satellite), shell control
    /data    — binary, idle: the controller sends no audio and takes none;
               an older Echo's frames are dropped
    /shell   — raw binary, dialled by the Echo on shell_open (OTA transfers,
               the dashboard's shell)

Since EchoHome there is no voice path here: an Echo's wake word, calls and
replies are its own Voice Incubator satellite's, and it works with this
process stopped.
"""

import asyncio
import contextlib
import hmac
import json
import logging
import os
import signal
import socket
import time

from aiohttp import web
from zeroconf.asyncio import AsyncZeroconf
from zeroconf import ServiceInfo
import websockets
from websockets.asyncio.server import ServerConnection as WebSocketServerProtocol

import em_db as db
import em_events
import em_credentials
import em_device_view
import em_process_stats
import em_reconcile
from version import VERSION as CONTROLLER_VERSION
import em_releases
import em_state
import em_auth as auth
import em_api as api
import em_pki
import em_hostip
import em_linkauth
import em_pairing
import em_config_types
import em_dbwriter
import em_tasks
import em_platform
import em_boards
import em_tcp
import em_wsclose
import em_rttlog
import em_mbc
import em_scenes
import em_health
import em_listen
import em_esphome_mdns
import em_incubator
import em_volume
import em_loglevel

_LOG_FORMAT = "%(asctime)s [%(levelname)s] %(name)s — %(message)s"

# Any non-empty string is truthy in Python, so `os.environ.get("DEBUG")`
# alone turned debug logging ON for DEBUG=0 — and em_start.py renders a
# false add-on option as exactly that string, so an untouched "Debug
# logging" toggle would have shipped every add-on install at DEBUG level.
# Same `== "1"` convention as REQUIRE_DEVICE_TLS, widened to the word
# spellings because DEBUG went undocumented for long enough that a
# container user's .env may already say `true`.
DEBUG = os.environ.get("DEBUG", "0").strip().lower() in {"1", "true", "yes", "on"}

logging.basicConfig(
    level=logging.DEBUG if DEBUG else logging.INFO,
    format=_LOG_FORMAT,
)
log = logging.getLogger("echomuse")

# Keep the last few hundred lines in memory so a support bundle can carry the
# controller's own log, not just the relayed per-device one.
em_process_stats.install_log_ring(_LOG_FORMAT)

# websockets' own logger stays quiet — its per-connection chatter is not
# useful — but that ALSO silenced its account of why a connection closed,
# and the handlers below discarded the exception carrying the close frames.
# em_wsclose renders that ourselves, so the reason survives the silence.
logging.getLogger("websockets.server").setLevel(logging.CRITICAL)

# aiohttp's per-request access log is the dashboard polling itself — 65% of
# measured log volume (em_support.py) — and drowns out actual events at INFO.
# Respect DEBUG rather than silencing unconditionally, same as the rest of
# this module's log level.
logging.getLogger("aiohttp.access").setLevel(logging.INFO if DEBUG else logging.WARNING)

# LOG_LEVELS="echomuse.esphome=DEBUG,aiohttp.access=INFO" — per-logger
# overrides on top of the global level, applied after the two defaults above
# so a specific setting can supersede them. `.get(..., "")` rather than a
# truthiness test for the reason DEBUG needed one: every non-empty string is
# truthy in Python, and em_start.py writes a false add-on option as exactly
# the string "0". A bad pair warns and is skipped (em_loglevel), never fatal
# — a typo in a diagnostics setting must not refuse to boot a controller.
_overrides = em_loglevel.apply(os.environ.get("LOG_LEVELS", ""))
for _problem in _overrides.problems:
    log.warning("LOG_LEVELS: %s", _problem)
if _overrides.levels:
    log.info("Log levels: " + ", ".join(
        f"{name}={logging.getLevelName(level)}"
        for name, level in _overrides.levels.items()))


# ─── Config ───────────────────────────────────────────────────────────────────

SERVER_HOST  = os.environ.get("SERVER_HOST", "0.0.0.0")
SERVER_PORT  = int(os.environ.get("SERVER_PORT", "8767"))
# Device-link TLS listener (wss) — same three WS planes as SERVER_PORT,
# wrapped in TLS with the em_pki-generated cert. 0 disables. Devices pick
# it up from the tls_port mDNS TXT property and dial wss iff they hold the
# pushed CA file (see device/internal/client/tlscreds.go).
SERVER_TLS_PORT = int(os.environ.get("SERVER_TLS_PORT", "8770"))
# Enforcing posture: reject device connections that are not TLS + a valid
# per-device token. Leave 0 until the whole fleet shows tls=true —
# a plain, tokenless connection is the legacy default and must keep
# working during the rollout.
REQUIRE_DEVICE_TLS = os.environ.get("REQUIRE_DEVICE_TLS", "0") == "1"
API_PORT     = int(os.environ.get("API_PORT", "8768"))
# The address devices are told to dial. Detected from the routing table when
# unset — never a literal, which used to send every unconfigured deployment
# to a developer's own machine. See em_hostip.
SERVER_IP    = em_hostip.server_ip(os.environ.get("SERVER_IP"))
MDNS_NAME    = os.environ.get("MDNS_NAME", "echomuse")
DB_PATH      = os.environ.get("DB_PATH", "echomuse.db")

# Device approval mode — overridden by system_config after db.init()
DEVICE_APPROVAL = os.environ.get("DEVICE_APPROVAL", "strict")

# NOTE: VOICE_PREROLL_DISCARD lives in em_esphome.py (esphome.VOICE_PREROLL_DISCARD)
# — it's used there in _stream_mic_audio, and _run_voice_locked below reads it
# via that single source of truth rather than keeping a second copy here that
# could drift out of sync (a duplicate here was previously dead code — see
# v2.6.3 changelog — resist the temptation to reintroduce it).


# Speaker — must match PcmSpeaker constants in Go. The wire carries MONO
# 48kHz (the device duplicates to stereo at the ALSA write — shipping two
# identical channels to a mono speaker doubled TTS bandwidth for nothing,
# and halving it matters on marginal 2.4GHz links).
SPEAKER_RATE   = 48000
SPEAKER_PERIOD = 2048
SPEAKER_BYTES  = SPEAKER_PERIOD * 2       # 4096 bytes/period (mono S16)
SPEAKER_PERIOD_SECONDS = SPEAKER_BYTES / (SPEAKER_RATE * 2)   # ~42.7ms

# How far ahead of realtime a VOICE response may be queued.
#
# The voice path used to send every period as fast as the socket would take
# it, with TCP backpressure as the only brake. That is what cut long
# responses off mid-sentence, and the chain is mechanical:
#
#   1. controller pushes ~1.7x realtime, bounded only by the link
#   2. the device's WS read goroutine calls PumpPeriod INLINE per 0x02 frame
#   3. pump() ends in a BLOCKING channel send, on a channel 128 periods
#      (~5.5s) deep
#   4. once full, the read goroutine blocks inside PumpPeriod and stops
#      calling ReadMessage
#   5. gorilla fires the pong handler only INSIDE ReadMessage, so a blocked
#      device cannot answer a ping
#   6. we ping every 20s and close after 10s without a pong
#   7. the buffer drains at realtime, so the block outlasts the timeout
#   8. 1011 keepalive ping timeout, mid-response
#
# Measured on Test Echo 1, 2026-08-30: 3.4MB (35.4s of audio) sent in 21.3s,
# 9.8s of that blocked in socket writes, connection closed 5s later. Short
# responses never reproduce it because they never fill 5.5s.
#
# SENDING FASTER THAN THIS BUYS NOTHING. The device can hold 5.5s and no
# more; everything beyond that sits in TCP buffers, which are lost on a
# reconnect exactly like un-sent audio is. So the excess never improved
# stall resilience — it only bought the block. Pacing to just under the
# device's own depth keeps the same protection with none of the cost.
#
# 4.0s to match em_player.LEAD_S, which reached the same number from the
# same constraint for the music plane: ~1.4s of headroom under audioChanDepth
# so the feed can never outrun the device's channel. Voice had no equivalent,
# and that asymmetry was the bug.
VOICE_LEAD_S = 4.0

# The device holds playback until ~this much audio is buffered (or EOS
# arrives) — primePeriods in pcm_speaker.go. The post-playback drain sleep
# must allow for the delayed start.
SPEAKER_PRIME_SECONDS = 1.1


# How long a freshly established data connection gets to deliver its first
# mic frame before the wake watchdog stops treating the silence as
# "device still warming up" and starts the zombie-stream ladder (#299,
# review on PR #311: a boolean guard made the Office-style zombie — deaf
# for hours — unrecoverable, because no frame ever arrives on the fresh
# connection and only the escalation repairs it).
FRESH_CONN_GRACE_S = 30.0


PING_INTERVAL_SEC = 5.0

# WebSocket keepalive on every device plane. The timeout is how long a ping may
# go unanswered before the link is declared dead, and 10s was shorter than the
# loss bursts this link actually has: with the BLE scan running, a Dot loses
# enough frames that TCP's retransmits alone can outlast it, and the overnight
# `1011 keepalive ping timeout` closes were exactly that (2026-09-23) — a live
# device torn down, its HA entities flapping, to rediscover it 5s later. 30s
# rides those out and still finds a dead Echo within ~50s; the device waits 45s
# for its own pongs (wsPongWait), so the two ends now disagree less.
WS_PING_INTERVAL_S = 20
WS_PING_TIMEOUT_S = 30
# A sample at or above this counts as an excursion. 200ms is well clear of a
# healthy hop (Office measures 264ms median for a whole audio round trip
# including frame batching) while catching the ~1s tail under investigation.
RTT_EXCURSION_MS = 200

# Outstanding pings older than this are abandoned — a reply that late is not
# a latency measurement, it is a lost packet, and keeping them would grow
# ping_sent without bound across a long disconnect.
PING_TIMEOUT_SEC = 60.0

# LEDs
NUM_LEDS = 12


# mDNS re-registration interval — keeps IGMP membership alive on the LAN
MDNS_REFRESH_INTERVAL = 120


# BLE advertisement batches from the device's passive scanner, moved off the
# control plane (#404). The control WebSocket is where liveness is measured —
# the RTT echo and the keepalive pong take the same mutex and the same TCP
# stream — so bulk telemetry there head-of-line-blocks the channel a device's
# health is judged on. Proven by crossover on 2026-09-01: the proxy moved
# between two Dots on one desk and took the excursions with it.
#
# Payload is the same JSON body the control message carried, so nothing about
# what an advert IS changed; only which socket it arrives on.
BLE_ADVERTS_TYPE   = 0x06

# What this controller can do, announced to the device on the ack. The mirror
# of the device's own `capabilities` in its register message, and it exists for
# the same reason: negotiate by capability, never by version string.
#
# `ble_adverts_data` says this controller reads BLE_ADVERTS_TYPE off the data
# plane. A device must not send those frames unnegotiated — an older controller
# ignores unknown frame types, so the adverts would vanish in silence, which is
# a worse fault than the one being fixed. The control-plane `ble_adverts`
# message stays handled forever, for firmware that predates this.
#
# `listen_session` says this controller understands private listening
# (docs/listening.md): listen_state, session-tagged oww_wake, 0x07 session
# audio and the listen_* replies. A device goes quiet only when it sees this —
# an older controller acts on a device wake only when stream frames arrive.
#
# Since EchoHome both stay only for firmware from before it (0SE7's
# v2.19.0 until it is updated): ble_adverts_data so an Echo with its
# Bluetooth proxy on sends its batches on the data plane, where they are
# dropped, and never on the control plane, the liveness channel; and
# listen_session so it listens privately rather than streaming. EchoHome
# firmware reads neither for its adverts (its own ESPHome proxy) and runs
# its output chain always (no "output_chain", which once handed the chain
# over from a controller that processed the audio itself).
CONTROLLER_FEATURES = ["ble_adverts_data", "listen_session"]

_device_level_to_ha = em_volume.device_level_to_ha

# ─── Device registry ──────────────────────────────────────────────────────────

# #315: the control plane gets the same treatment the data plane has had —
# a device whose control connection drops during a controller restart, an
# add-on update or a link excursion keeps its ESPHome entities, BLE proxy
# and media session for this long before they are released. The device's
# own reconnect (endpoint pinned, per #106) lands well inside the window.
CONTROL_RECONNECT_GRACE_S = 5.0


class Device:
    def __init__(
        self,
        device_id: str,
        ip: str,
        capabilities: list,
        control_ws: WebSocketServerProtocol,
    ):
        self.device_id    = device_id
        self.ip           = ip
        self.capabilities = capabilities
        self.control_ws   = control_ws
        # The Sendspin player's status (#89), from sendspin_status and the
        # stats tick; None while the player is off or on firmware without it.
        self.sendspin: dict | None = None
        # An outstanding sendspin_token_request, answered by sendspin_token.
        self.sendspin_token_waiter: asyncio.Future | None = None
        self.esphome_key_waiter: asyncio.Future | None = None
        # Set from the register message; None on firmware that predates it.
        self.ambient_light_status: dict | None = None
        # Which userspace the device booted, from its register message.
        # None until a device registers, and None forever for firmware too
        # old to say — which em_platform resolves to Android, leaving the
        # existing fleet exactly as it was.
        self._base_os: str | None = None
        self.kernel_arch: str | None = None
        # From the register message (schema v28): see em_health.
        self.boot_reason: str | None = None
        self.boot_id: str | None = None
        # The last eMMC wear row written, as (day, values), so a reading that
        # has not changed since is not rewritten every stats tick.
        self.wear_written: tuple | None = None
        self.kernel_release: str | None = None

        self.data_ws: WebSocketServerProtocol | None = None


        # Transient state — read by em_device_view.merge_device()
        self.speaking  = False
        self.muted     = False
        # HA's wake word picker (#286); seeded from the DB at connect.
        self.listening = False
        self.thinking  = False

        # Volume as HA float (0.0–1.0). Initialised from stored config in
        # handle_control() after config is read; updated on volume_state
        # messages from the device and persisted back to config.
        # Default matches DEFAULT_DEVICE_CONFIG startupVolume=85.
        self.volume: float = _device_level_to_ha(85)

        self.data_ready = asyncio.Event()

        self.bass_guard_db:      float = em_mbc.DEFAULT_BASS_GUARD_DB
        # LED ring scene — render-ready palette/spinner from em_scenes,
        # refreshed on connect and on any config push carrying led* keys.
        self.led_scene:     dict  = em_scenes.resolve({})
        self.stats:         dict | None = None
        # In-flight wifi_scan awaiter (set by the API handler). Change
        # pending/result state lives in em_device_view._wifi_states instead — this
        # Device object dies with the connection when the network switches.
        self.wifi_scan_future: asyncio.Future | None = None

        # owwOnDevice, normalised. "on" hands the wake DECISION to the device:
        # its crossing lands in pending_wake and the wake listener acts on it
        # instead of on its own score. Default off, and set from config on
        # every push, so a device whose config has never arrived behaves
        # exactly as it always did.
        self.oww_on_device: str = em_listen.MODE_OFF

        # Private listening (docs/listening.md). What the device last said it
        # is doing with its wake stream — "local", "stream" or "degraded" —
        # and why. None until it reports on this connection: the dashboard
        # must show that as unknown, never as private. `listen_view` combines
        # it with the configured mode and the firmware's capability.
        self.listen_reported: str | None = None
        self.listen_reason: str = ""

        # Per-room noise floor estimate (normalized RMS, 0..1), tracked from
        # the continuous wake stream in wake_word_listener. Measurement only —
        # never applied to the audio (see 2026-07-06 architecture discussion:
        # adaptation as measurement, not signal modification). Consumers:
        # em_esphome._stream_mic_audio's SNR-relative no-speech detection,
        # and diagnostics (near-miss logs). Asymmetric tracker: follows drops
        # quickly, rises slowly, so speech doesn't drag the floor up.
        self.noise_floor: float = 0.0
        # (MICPGA, digital gain): the gain a wake's level reading still
        # contains. Echoes that differ here are not compared by loudness.

        # Clear while a barge's claim is undecided; see _await_barge_decision.
        self.barge_decided    = asyncio.Event()
        self.barge_decided.set()


        # ── Control-plane RTT ────────────────────────────────────────────
        # End-to-end latency is the one thing the RF layer cannot tell us on
        # this hardware: the MTK driver leaves retry/discard/missed-beacon
        # at zero in /proc/net/wireless and reports NOISE=9999, and there is
        # no `iw` binary. So the tx/rx/error counters cannot distinguish a
        # healthy link from a struggling one — while RTT measures the thing
        # that actually degrades the experience, needs no driver support,
        # and discriminates between the live hypotheses: contention makes
        # latency track LOAD, whereas WiFi power-save makes it spike when
        # IDLE, quantised to the beacon interval.
        #
        # Accumulated in memory and flushed on the device's existing ~30s
        # stats report, so this costs no write the loop wasn't making.
        self.ping_seq   = 0
        self.ping_sent: dict[int, float] = {}   # seq -> monotonic send time
        self.ping_busy: dict[int, bool]  = {}   # seq -> device busy at send
        self.rtt_last_ms: int | None = None
        self.rtt_sum_ms  = 0
        self.rtt_count   = 0
        self.rtt_min_ms: int | None = None
        self.rtt_max_ms: int | None = None
        self.rtt_excursions      = 0   # samples over RTT_EXCURSION_MS
        self.rtt_excursions_idle = 0   # ...of which the device was idle
        # Denominator for the above. Without it, "every excursion happened
        # while idle" is vacuous: almost every SAMPLE is idle, because
        # devices spend most of their life not in a turn. The discriminator
        # is the excursion RATE per state, not the raw count.
        self.rtt_samples_idle    = 0
        # Log-line coalescing only — the counters above are the measurement.
        self.rtt_log = em_rttlog.ExcursionLog(self.device_id)
        # Downlink loss from the kernel's counters on our own sockets to this
        # device (em_tcp); drained with the RTT window on each stats report.
        self.tcp_loss = em_tcp.LossWindow()
        # Loss per minute for the last 30, for the Status tab's link quality
        # — the hourly rollup is too coarse to show a link recovering.
        self.tcp_minutes = em_tcp.MinuteStrip()

    def is_busy(self) -> bool:
        """Whether this device was doing anything when a ping went out. The
        controller runs nothing on an Echo since EchoHome, so it never is;
        kept because every ping still records it."""
        return False


    def record_rtt(self, rtt_ms: int, was_busy: bool) -> None:
        self.rtt_last_ms = rtt_ms
        self.rtt_sum_ms += rtt_ms
        self.rtt_count  += 1
        if not was_busy:
            self.rtt_samples_idle += 1
        if self.rtt_min_ms is None or rtt_ms < self.rtt_min_ms:
            self.rtt_min_ms = rtt_ms
        if self.rtt_max_ms is None or rtt_ms > self.rtt_max_ms:
            self.rtt_max_ms = rtt_ms
        if rtt_ms >= RTT_EXCURSION_MS:
            self.rtt_excursions += 1
            if not was_busy:
                self.rtt_excursions_idle += 1

    def drain_rtt(self) -> dict:
        """Take the accumulated window and reset. Empty dict if no samples."""
        if not self.rtt_count:
            return {}
        out = {
            "rttSumMs":         self.rtt_sum_ms,
            "rttSamples":       self.rtt_count,
            "rttMinMs":         self.rtt_min_ms,
            "rttMaxMs":         self.rtt_max_ms,
            "rttExcursions":    self.rtt_excursions,
            "rttExcursionsIdle": self.rtt_excursions_idle,
            "rttSamplesIdle":   self.rtt_samples_idle,
        }
        self.rtt_sum_ms = self.rtt_count = 0
        self.rtt_min_ms = self.rtt_max_ms = None
        self.rtt_excursions = self.rtt_excursions_idle = 0
        self.rtt_samples_idle = 0
        return out

    def drain_tcp(self) -> dict:
        """Downlink segments and retransmits since the last report, summed over
        the control and data planes (em_tcp.LossWindow). Empty when neither
        socket could be read, so it stores as NULL rather than a clean link."""
        snaps = {}
        for ws in (self.control_ws, self.data_ws):
            transport = getattr(ws, "transport", None)
            if transport is not None:
                snaps[id(ws)] = em_tcp.read_info(transport.get_extra_info("socket"))
        return self.tcp_loss.drain(snaps)

    async def send_control(self, msg: dict):
        try:
            await self.control_ws.send(json.dumps(msg))
        except Exception as e:
            log.warning(f"[{self.device_id}] Control send failed: {e}")


    async def set_leds(self, leds: list, listening: bool | None = None):
        # The optional listening flag tells the device explicitly that this
        # frame is the listening ring (enables its direction overlay).
        # Pre-scene firmware inferred it from the ring being all-green —
        # that heuristic breaks for every non-green scene, so newer
        # firmware trusts this flag when present and old firmware just
        # ignores the extra key.
        msg = {"type": "leds", "leds": leds}
        if listening is not None:
            msg["listening"] = listening
        await self.send_control(msg)

    @property
    def led_anim_capable(self) -> bool:
        return "led_anim" in (self.capabilities or [])


    @property
    def pairing_capable(self) -> bool:
        """
        Whether this firmware asks to pair itself (action button held, then a
        pair_request or a plain dial with `pairing`). Without it an admin
        starts pairing from the dashboard, since the device cannot ask.
        """
        return "pairing" in (self.capabilities or [])

    @property
    def sendspin_capable(self) -> bool:
        """
        Whether this firmware can be a Sendspin player (#89). The dashboard
        shows the section disabled with the reason without it.
        """
        return "sendspin" in (self.capabilities or [])


    @property
    def wake_mic_capable(self) -> bool:
        """Whether firmware can listen for the wake word on a chosen mic (#705)."""
        return "wake_mic" in (self.capabilities or [])

    @property
    def identify_ring_capable(self) -> bool:
        """Whether firmware can take the whole ring on `identify` (#860)."""
        return "identify_ring" in (self.capabilities or [])

    @property
    def volume_cue_capable(self) -> bool:
        """Whether physical volume changes can play an idle preview tone."""
        return "volume_cue" in (self.capabilities or [])


    @property
    def response_level_capable(self) -> bool:
        """Whether firmware can apply the relative voice-response gain."""
        return "response_level" in (self.capabilities or [])

    @property
    def stock_sound_capable(self) -> bool:
        """Whether firmware voices its speaker and holds the bass guard on."""
        return "stock_sound" in (self.capabilities or [])

    @property
    def oww_trigger_capable(self) -> bool:
        """
        Whether this firmware can ACT on its own wake detection.

        Firmware from before EchoHome only (EchoHome announces no on-device
        modes): some of it scores the wake word and reports it, and cannot
        start a turn from it. Offering those owwOnDevice="on" produces a
        device that scores perfectly, never answers, and looks broken — the
        exact "I enabled it and nothing happened" the capability rule exists to
        prevent, so "on" is gated on this and shown disabled with the reason
        otherwise.
        """
        return "oww_trigger" in (self.capabilities or [])

    @property
    def oww_local_capable(self) -> bool:
        """Whether this firmware can listen privately (docs/listening.md).
        "Could it" — whether it IS comes from listen_view."""
        return "oww_local_only" in (self.capabilities or [])

    @property
    def listen_view(self) -> "em_listen.ListenView":
        """What this Echo is actually doing with its microphone."""
        return em_listen.resolve(
            em_listen.normalise_mode(self.oww_on_device),
            self.capabilities, self.listen_reported, self.listen_reason,
        )


    @property
    def aec_hw_ref_capable(self) -> bool:
        """
        Whether this firmware can take the AEC far-end reference from a
        hardware playback loopback in the mic capture, falling back to the
        software tap at the ALSA write when the board has none.

        This answers "could it" — `aec_ref` below answers "is it, right
        now", the usual "could it" against "is it" split.
        Both are needed and neither substitutes for the other: proving a
        channel is a loopback requires the speaker to have played, which has
        not happened at registration, so a capability alone can never say
        the reference was found.

        What it gates is the AEC delay control. That slider compensates
        write-to-ear latency for the software tap; on a frame-aligned
        hardware reference there is no latency to compensate, so a live
        slider there is a knob that does nothing.
        """
        return "aec_hw_ref" in (self.capabilities or [])

    @property
    def aec_ref(self) -> str | None:
        """
        The far-end reference cancellation is actually running on: "hw",
        "sw", "off", or None from firmware too old to report it.

        None is NOT "sw". Every device predating this reports nothing, and
        reading that as the software tap would tell the dashboard a fact
        about a device that never stated one — the same reason a missing
        playback_stats stores NULL rather than zero.
        """
        return (self.stats or {}).get("aecRef")

    @property
    def base_os(self):
        """
        The userspace the device booted: "emos", "fireos", or None.

        None means the firmware is too old to report it, NOT FireOS. The
        distinction is the whole point of the field: `android_userspace`
        below acts on it, and reading absence as Android would keep pushing
        the debloat payload at an emOS device that has no package manager.

        Set from the REGISTER message, not the stats tick. It rode the stats
        report for exactly one commit, which put it 30 seconds too late for
        its only consumer — the payload reconcile runs on connect, so it read
        None, pushed the Magisk service.d script at an emOS device, and each
        attempt sat out the full 120s transfer timeout waiting for a
        TRANSFER_OK that could never come.
        """
        return self._base_os

    @property
    def android_userspace(self) -> bool:
        """
        Whether Android-only payloads mean anything on this device.

        The rule and its asymmetry live in em_platform, where a test can
        reach them without aiohttp.
        """
        return em_platform.android_userspace(self.base_os)

    async def send_led_anim(self, anim: dict):
        """
        Hand the ring to the device's local animation engine (led_anim
        capability, v2.9+ firmware). The device renders frames on its own
        ticker until a newer led_anim/leds message replaces the spec or
        its ttlSec dead-man expires — so a controller stall or WiFi jitter
        can no longer make the spinner judder, and a dead controller can't
        leave the ring lit.
        """
        await self.send_control({"type": "led_anim", "anim": anim})

    async def ping(self):
        await self.send_control({"type": "ping"})




    async def identify(self):
        """Ask the Echo to take its whole ring for 30s (identify_ring, #860)."""
        await self.send_control({"type": "identify"})


# openWakeWord's AudioFeatures keeps ~10s of raw audio (160k Python ints) and
# converts the whole deque to a list on every predict() to take the last
# ~1.7k samples. Nothing else reads it, and trimming it changes no score
# (tools/trim_check.py: 1200 frames, 0 differing, max delta 0). This is the
# idle-CPU hot path the wake loop hits 12.5 times a second per device.
_OWW_RAW_BUFFER_KEEP = 8 * 1280 + 480  # 8 chunks + the mel tail


def get_device(device_id: str) -> Device | None:
    return em_state.devices.get(device_id)


async def _push_device_state(device: Device) -> None:
    """Push current transient device state to dashboard clients."""
    await em_events.push({
        "type":      "device_update",
        "device_id": device.device_id,
        "state": {
            "connected": True,
            "speaking":  device.speaking,
            "muted":     device.muted,
            "listening": device.listening,
            "thinking":  device.thinking,
        },
    })


# ─── LED helpers ──────────────────────────────────────────────────────────────

def _make_leds(r, g, b):
    return [{"id": i, "r": r, "g": g, "b": b} for i in range(NUM_LEDS)]


async def leds_off(device: Device):
    if device.led_anim_capable:
        await device.send_led_anim({"pattern": "off"})
    else:
        await device.set_leds(_make_leds(0, 0, 0))


# How long the listening ring is held before the no_ha cue replaces it. The
# wake word was heard, and an orange fault flashed on its own reads as "the
# device did nothing"; the ack has to land first. Costs the wake listener the
# same delay before it restarts, which is the price of the ring saying
# anything at all on a turn that lasted milliseconds.
NO_HA_HOLD_S = 0.6


# ─── Control plane handler ────────────────────────────────────────────────────

async def _link_auth_ok(
    ws: WebSocketServerProtocol, device_id: str, secure: bool, plane: str
) -> bool:
    """
    Device-link auth gate, applied to all three WS planes once the
    device_id is known.

    Rules (rollout-safe by construction):
      - a presented token that MISMATCHES a stored one always rejects;
      - a stored token with NO token presented is allowed unless
        REQUIRE_DEVICE_TLS — the DB row is minted before the files land
        on the device, and rejecting in that window would cut off the
        shell plane that the credential push itself rides on;
      - a presented token for a device with NOTHING on record is ignored,
        not rejected (see below);
      - REQUIRE_DEVICE_TLS=1 requires TLS + a matching token, full stop.

    That third rule used to be a rejection, and it made deleting a device a
    one-way door. Delete removes the row, the token is a column on it, and the
    device re-reads its credential file on every dial — so it presented a token
    nothing recognised and was refused on all three planes, INCLUDING the shell
    plane the controller would otherwise push fresh credentials over. The
    device retried forever behind a pulsing orange ring, and the dashboard had
    nothing to show because as far as it was concerned the device did not
    exist.

    Rejecting there also never bought anything. The rule immediately above
    admits a connection presenting no token at all, so an unrecognised token
    was being treated as worse than none while anyone could simply omit the
    header. A device with a stale credential now comes back as pending and
    waits for approval, which is the decision a human should be making anyway.

    `REQUIRE_DEVICE_TLS=1` installs are unaffected: the last rule still
    demands a token that matches a stored one, so a deleted device is refused
    there regardless and re-provisioning over USB stays the intended path.
    """
    presented = None
    try:
        presented = ws.request.headers.get("X-EM-Token")
    except AttributeError:
        pass

    loop = asyncio.get_event_loop()
    expected, confirmed = await loop.run_in_executor(
        None, db.get_device_link_auth, device_id)

    verdict = em_linkauth.decide(
        presented=presented,
        expected=expected,
        confirmed=confirmed,
        secure=secure,
        require_tls=REQUIRE_DEVICE_TLS,
    )
    if not verdict.ok:
        log.warning(f"[{plane}] {device_id}: {verdict.reason} — rejecting")
        # Only for a device with a row and a token, so ids nobody issued
        # cannot grow the map. The dashboard shows it on the Link row.
        if expected:
            em_device_view.note_link_refused(device_id, verdict.reason)
        return False
    if presented and expected and not confirmed:
        # First sight of the device holding its token: from now on it must.
        if await loop.run_in_executor(
                None, db.confirm_device_token, device_id, presented):
            log.info(f"[{plane}] {device_id}: link token confirmed — "
                     f"a connection without it will be refused from now on")
    if plane == "control":
        em_device_view.clear_link_refused(device_id)
    if verdict.stale_token:
        # Allowed, but worth seeing in the log: almost always a device that was
        # deleted and has come back carrying the credential from its previous
        # life, which is the answer to "why is this in pending again".
        log.warning(
            f"[{plane}] {device_id}: {verdict.reason}. "
            f"Treating as an unregistered device; it will need approval."
        )
    return True


async def _presented_its_token(ws, device_id: str) -> bool:
    """Whether this connection's X-EM-Token is the device's stored token."""
    try:
        presented = ws.request.headers.get("X-EM-Token")
    except AttributeError:
        return False
    expected = await asyncio.get_event_loop().run_in_executor(
        None, db.get_device_token, device_id)
    return bool(presented and expected and hmac.compare_digest(presented, expected))


def _peer_ip(ws) -> str | None:
    try:
        return ws.remote_address[0]
    except (AttributeError, TypeError, IndexError):
        return None


def _not_from_control(device: "Device", ws, secure: bool) -> str | None:
    """Why a /data or /shell connection is not the device's own, or None."""
    return em_linkauth.follows_control(
        control_peer=_peer_ip(device.control_ws),
        control_secure=bool(getattr(device, "secure", False)),
        peer=_peer_ip(ws),
        secure=secure,
    )


# ─── Control-plane messages ─────────────────────────────────────────────────
#
# One handler per message type a registered device sends on /control, looked up
# in CONTROL_HANDLERS by handle_control's message loop (#288). Each takes the
# device and the decoded message and nothing else, so each can be called on its
# own. `register` is not here: it runs before the loop and is fatal on failure.


async def _on_ambient_light(device: Device, msg: dict) -> None:
    device_id = device.device_id
    # A step change in room light, sent by the device the
    # moment it happens rather than waiting up to 30s for the
    # stats tick — the timing IS the signal ("someone turned
    # a light on"). The steady-state value still rides stats.
    _lux = msg.get("lux")
    if isinstance(_lux, int):
        # None until the first stats report; that report carries the
        # reading too, so there is nothing to hold until then.
        if device.stats is not None:
            device.stats["ambientLux"] = _lux
        log.info(f"[{device_id}] Ambient light → {_lux} lux")


async def _on_listen_state(device: Device, msg: dict) -> None:
    """What the Echo is DOING with its microphone, as it reports it: the
    only source the privacy line may use (docs/listening.md, em_listen.
    resolve). Kept through EchoHome's strip, which removed the controller's
    voice: a status shown from configuration instead would be the one lie
    that module exists to prevent."""
    device_id = device.device_id
    before = device.listen_view
    device.listen_reported = str(msg.get("state") or "") or None
    device.listen_reason   = str(msg.get("reason") or "")
    after = device.listen_view
    if after != before:
        log.info(
            f"[{device_id}] listening: {after.state}"
            f"{' (streams continuously)' if after.streams else ''}"
            f"{f' — {after.reason}' if after.reason else ''}"
        )
        if after.state == em_listen.STATE_DEGRADED:
            em_dbwriter.submit(db.log_device, device_id, "warning", "device",
                          f"Wake word unavailable, button only: {after.reason}")


async def _on_mute_state(device: Device, msg: dict) -> None:
    device_id = device.device_id
    device.muted = msg.get("muted", False)
    await em_events.push({
        "type":      "device_update",
        "device_id": device_id,
        "state":     {"muted": device.muted},
    })


async def _on_volume_state(device: Device, msg: dict) -> None:
    device_id = device.device_id
    loop = asyncio.get_event_loop()
    # Device reports its current volume level (raw tinymix index).
    # Convert to HA float, update in-memory state, persist to
    # config so the value survives controller and device restarts.
    raw_level = int(msg.get("level", 85))
    if raw_level > 0:  # silent is never kept as the startup volume
        device.volume = _device_level_to_ha(raw_level)
        log.debug(
            f"[{device_id}] volume_state: level={raw_level} "
            f"→ {device.volume:.3f}"
        )
        # Persist — read-modify-write to avoid stomping other fields
        stored_config = await loop.run_in_executor(
            None, db.get_device_config, device_id
        )
        stored_config["startupVolume"] = raw_level
        await loop.run_in_executor(
            None, db.set_device_config, device_id, stored_config
        )


async def _on_stats(device: Device, msg: dict) -> None:
    device_id = device.device_id
    loop = asyncio.get_event_loop()
    if "sendspin" in msg:
        device.sendspin = msg.get("sendspin")
    # eMMC wear, re-read on the device every few hours;
    # one row per day (schema v28), written on change.
    _note_wear(device, msg.get("emmc"))
    device.stats = {
        "cpuPct":        msg.get("cpuPct"),
        "memUsedMb":     msg.get("memUsedMb"),
        "memTotalMb":    msg.get("memTotalMb"),
        "storageUsedMb": msg.get("storageUsedMb"),
        "storageTotalMb":msg.get("storageTotalMb"),
        "wifiRssi":      msg.get("wifiRssi"),
        "wifiSsid":      msg.get("wifiSsid"),
        # v7 link telemetry (firmware >= v2.9.6). This dict
        # is an explicit allowlist, so any new device stat
        # must be added HERE as well as in DeviceStats and
        # record_device_stats — all three, or the field is
        # silently dropped in the relay (2026-07-20).
        "linkSpeedMbps": msg.get("linkSpeedMbps"),
        "wifiFreqMhz":   msg.get("wifiFreqMhz"),
        "wifiBssid":     msg.get("wifiBssid"),
        "txBytes":       msg.get("txBytes"),
        "rxBytes":       msg.get("rxBytes"),
        "txErrors":      msg.get("txErrors"),
        "txDropped":     msg.get("txDropped"),
        "rxCrcErrors":   msg.get("rxCrcErrors"),
        # Uplink loss: the device's own TCP retransmits since
        # its last report (tcpUpSegs only where the kernel
        # counts segments; FireOS 5's does not). Downlink is
        # measured here, in Device.drain_tcp.
        "tcpUpRetrans":  msg.get("tcpUpRetrans"),
        "tcpUpSegs":     msg.get("tcpUpSegs"),
        "ble":           msg.get("ble"),
        # Thermals + CPU topology. coresOnline is not optional
        # context: cpuPct is a share of ONLINE capacity, so the
        # same work halves its percentage when hotplug adds a
        # core. thermalCoreLimit < coresTotal means the thermal
        # governor is capping capacity.
        "cpuTempC":         msg.get("cpuTempC"),
        "maxTempC":         msg.get("maxTempC"),
        "coresOnline":      msg.get("coresOnline"),
        "coresTotal":       msg.get("coresTotal"),
        "thermalCoreLimit": msg.get("thermalCoreLimit"),
        # Ambient light (TSL2540). None on a device without
        # the sensor — 0 lux is a real reading (covered), so
        # the two must not collapse into each other.
        "ambientLux":       msg.get("ambientLux"),
        # v13 on-device shadow window summary, absent when the
        # device is not scoring (see the allowlist note above:
        # DeviceStats, here, and the consumer below).
        "owwShadow":     msg.get("owwShadow"),
        # Which far-end reference the AEC is on: "hw",
        # "sw", "off", or absent from firmware that
        # cannot say. Deliberately NOT added to
        # record_device_stats, unlike the rest of this
        # allowlist: it is a state, not a metric, and
        # the hourly rollup averages numbers.
        "aecRef":        msg.get("aecRef"),
    }
    # Fold into the persistent hourly rollup (CPU/RAM/storage/
    # RSSI trends) — one cheap upsert per ~30s report. The
    # last_seen refresh rides the same executor hop: a stats
    # report IS proof of life, and without it last_seen only
    # ever recorded the last *connect*.
    # RTT is controller-measured, not relayed from the
    # device message, so it is merged in here rather than
    # coming through the allowlist above. drain_rtt() takes
    # and resets the window accumulated since the last
    # report, so no sample is counted twice.
    _tcp = device.drain_tcp()
    device.tcp_minutes.add(time.time(), _tcp.get("tcpDownSegs"),
                           _tcp.get("tcpDownRetrans"))
    _metrics = {**device.stats, **device.drain_rtt(), **_tcp}
    def _persist_stats(_id=device_id, _s=_metrics):
        db.record_device_stats(_id, _s)
        db.touch_device_seen(_id)
    await loop.run_in_executor(None, _persist_stats)
    await em_events.push({
        "type":      "device_update",
        "device_id": device_id,
        "state":     {
            "stats": device.stats,
            "sendspin": device.sendspin,
        },
    })


async def _on_sendspin_status(device: Device, msg: dict) -> None:
    device_id = device.device_id
    # The player's state as it changes (pairing, a stream
    # starting); the stats tick carries it too.
    device.sendspin = msg.get("status")
    await em_events.push({
        "type": "device_update", "device_id": device_id,
        "state": {"sendspin": device.sendspin},
    })


async def _on_sendspin_token(device: Device, msg: dict) -> None:
    # The answer to sendspin_token_request. A secret: it
    # goes to the one waiting request and nowhere else,
    # never a log line or an event.
    waiter = device.sendspin_token_waiter
    if waiter is not None and not waiter.done():
        waiter.set_result(msg)


async def _on_esphome_key(device: Device, msg: dict) -> None:
    # The answer to esphome_key_request: the key of the Echo's own ESPHome
    # server. A secret, handled as the Sendspin token is.
    waiter = device.esphome_key_waiter
    if waiter is not None and not waiter.done():
        waiter.set_result(msg)


async def _on_wifi_result(device: Device, msg: dict) -> None:
    device_id = device.device_id
    # Outcome of a wifi_change. The device re-sends this
    # until it sees a wifi_commit ack (a single send can
    # vanish into a half-open TCP connection killed by the
    # network switch), so: ALWAYS ack — on success the ack
    # also finalises the change (deletes rollback backup +
    # pending marker; a failed change already removed both,
    # so the ack is a no-op there) — and log/record only
    # the first arrival.
    ok    = bool(msg.get("ok"))
    ssid  = msg.get("ssid", "")
    error = msg.get("error") or ""
    st, duplicate = em_device_view.wifi_record_result(device_id, ok, ssid, error)
    await device.send_control({"type": "wifi_commit"})
    if not duplicate:
        if ok:
            log.info(f"[{device_id}] WiFi changed to \"{ssid}\" — committed")
            em_dbwriter.submit(db.log_device, device_id, "info", "device",
                          f'WiFi changed to "{ssid}"')
        else:
            log.warning(f"[{device_id}] WiFi change to \"{ssid}\" "
                        f"failed: {error}")
            em_dbwriter.submit(db.log_device, device_id, "warning", "device",
                          f'WiFi change to "{ssid}" failed: {error}')
        await em_events.push({
            "type":      "device_update",
            "device_id": device_id,
            "state":     {"wifi": st},
        })


async def _on_wifi_scan_result(device: Device, msg: dict) -> None:
    fut = device.wifi_scan_future
    if fut is not None and not fut.done():
        fut.set_result(msg)


async def _on_log(device: Device, msg: dict) -> None:
    device_id = device.device_id
    level   = msg.get("level", "info")
    message = msg.get("message", "")
    # em_events.push_log PERSISTS as well as pushing, so this must
    # not also call db.log_device — doing both wrote every
    # device log line twice, ~6ms apart, which is how half the
    # device_logs table came to be duplicates. It also matters
    # for the support bundle: thin_noise keeps the newest three
    # [mem] lines per device, so duplication halved the readings
    # a leak hunt actually gets.
    #
    # The removed call was a synchronous DB write on the event
    # loop; em_events.push_log does it in an executor.
    await em_events.push_log(device_id, level, "device", message)


async def _on_pair_request(device: Device, msg: dict) -> None:
    device_id = device.device_id
    # The owner held the action button on a connected
    # device; an admin issues credentials with Approve.
    await api.notify_pair_request(device_id, "link")


async def _on_pong(device: Device, msg: dict) -> None:
    loop = asyncio.get_event_loop()
    # Solicited pong (carries our sequence id) -> an RTT
    # sample. Unsolicited keepalive pongs have no id and are
    # ignored here; pairing one with whatever ping happened
    # to be outstanding would invent a measurement.
    _seq = msg.get("id")
    if _seq is not None:
        _sent = device.ping_sent.pop(_seq, None)
        _busy = device.ping_busy.pop(_seq, False)
        if _sent is not None:
            _now = loop.time()
            _rtt = int((_now - _sent) * 1000)
            device.record_rtt(_rtt, _busy)
            if _rtt >= RTT_EXCURSION_MS:
                # Busy excursions log one for one; idle
                # ones coalesce into a periodic summary.
                _line = device.rtt_log.record(
                    _rtt, _busy, loop.time())
                if _line:
                    log.info(_line)


CONTROL_HANDLERS = {
    "ambient_light":    _on_ambient_light,
    "listen_state":     _on_listen_state,
    "mute_state":       _on_mute_state,
    "volume_state":     _on_volume_state,
    "stats":            _on_stats,
    "sendspin_status":  _on_sendspin_status,
    "incubator_state":  em_incubator.on_incubator_state,
    "esphome_mdns":     em_esphome_mdns.on_esphome_mdns,
    "sendspin_token":   _on_sendspin_token,
    "esphome_key":      _on_esphome_key,
    "wifi_result":      _on_wifi_result,
    "wifi_scan_result": _on_wifi_scan_result,
    "log":              _on_log,
    "pair_request":     _on_pair_request,
    "pong":             _on_pong,
}


async def handle_control(ws: WebSocketServerProtocol, secure: bool = False):
    """
    Handle a /control WebSocket connection from a device.
    """
    device = None
    remote = ws.remote_address

    try:
        raw = await asyncio.wait_for(ws.recv(), timeout=10.0)
        msg = json.loads(raw)

        if msg.get("type") != "register":
            log.warning(
                f"[control] First message from {remote} was not register — closing"
            )
            await ws.close()
            return

        device_id    = msg["device_id"]
        # Sent by firmware whose owner held the action button (em_pairing).
        pairing      = bool(msg.get("pairing"))
        loop         = asyncio.get_event_loop()

        if pairing and em_pairing.approved(device_id):
            # An admin approved this pairing. Rotate first: with no token on
            # record the device is admitted by em_linkauth's existing rules,
            # and issue_credentials mints its new one below.
            await loop.run_in_executor(None, db.clear_device_token, device_id)
            log.info(f"[control] {device_id}: pairing approved — admitting to issue credentials")

        if not await _link_auth_ok(ws, device_id, secure, "control"):
            # Said before closing, so the device can show "hold the button to
            # pair" rather than "no controller" (firmware with `pairing`;
            # older firmware ignores it). A pairing device is told pending.
            reply = {"type": "refused"}
            if pairing:
                await api.notify_pair_request(device_id, "plain")
                reply = {"type": "pending", "pairing": True}
            try:
                await ws.send(json.dumps(reply))
            except Exception:
                pass
            await ws.close()
            return
        ip           = msg.get("ip", str(remote[0]))
        version      = msg.get("version")
        capabilities = msg.get("capabilities", [])

        loop         = asyncio.get_event_loop()
        approval_mode = await loop.run_in_executor(
            None, db.get_config, "device_approval", DEVICE_APPROVAL
        )
        row          = await loop.run_in_executor(None, db.get_device, device_id)

        if row is None:
            if approval_mode == "auto":
                label = f"Unknown {device_id[:8]}"
                await loop.run_in_executor(
                    None, db.register_new_device, device_id, ip, version
                )
                await loop.run_in_executor(
                    None, db.approve_device, device_id, label, None
                )
                log.info(
                    f"[control] Auto-approved new device: {device_id} "
                    f"label={label!r}"
                )
                row = await loop.run_in_executor(None, db.get_device, device_id)
            else:
                await loop.run_in_executor(
                    None, db.register_new_device, device_id, ip, version
                )
                await ws.send(json.dumps({"type": "pending"}))
                log.info(
                    f"[control] Unknown device held as pending: {device_id} "
                    f"from {ip}"
                )
                await api.notify_device_pending(device_id, ip)
                em_dbwriter.submit(db.log_device,
                    device_id, "info", "controller",
                    f"Device seen for first time — pending approval ({ip})"
                )
                await ws.close()
                return

        if not row["approved"]:
            await loop.run_in_executor(
                None, db.upsert_device_seen, device_id, ip, version
            )
            await ws.send(json.dumps({"type": "pending"}))
            log.info(
                f"[control] Device pending approval: {device_id} from {ip}"
            )
            await api.notify_device_pending(device_id, ip)
            await ws.close()
            return

        await loop.run_in_executor(
            None, db.upsert_device_seen, device_id, ip, version
        )

        device = Device(device_id, ip, capabilities, ws)
        # Why the device has no ambient light sensor, when it has none. The
        # capability list says only that it is absent; without the reason,
        # an unfitted chip and an unbound driver are indistinguishable
        # remotely, and #90 needed a shell session on the user's own hardware
        # to tell them apart. Absent on firmware that does not send it, which
        # reads as "not reported" rather than as a fault.
        device.ambient_light_status = msg.get("ambient_light_status")
        # Static property of the boot, so it arrives with registration
        # rather than on the stats tick — em_reconcile.on_connect asks for it
        # immediately and a stats-borne value is ~30s too late.
        device._base_os = msg.get(em_platform.REGISTER_KEY)
        # Persisted as well as held live (schema v21). The live value answers
        # "what is THIS device", which is all payload gating ever needs; the
        # stored one answers "what is the fleet", which is a question about
        # devices that are mostly offline. Written on every register, because a
        # device reflashed between FireOS and emOS is the case it has to track.
        if device._base_os:
            em_dbwriter.submit(db.set_device_base_os, device_id, device._base_os)
        # Which board, for the model HA shows; before the satellite exists.
        if boards := em_boards.record(em_state.boards, device_id, msg.get("board")):
            em_dbwriter.submit(db.set_config, em_boards.CONFIG_KEY, boards)
        # The running kernel, so emOS on FireOS 5's 64-bit kernel and FireOS
        # 6's 32-bit one can be told apart (schema v23). Absent on older
        # firmware; stored only when reported so a known value is not erased.
        device.kernel_arch = msg.get("kernel_arch") or None
        device.kernel_release = msg.get("kernel_release") or None
        if device.kernel_arch:
            em_dbwriter.submit(db.set_device_kernel, device_id, device.kernel_arch, device.kernel_release or "")
        # How this boot started and the eMMC's wear (schema v28), one row per
        # boot_id: a device re-registers on every redial. Held live too, so
        # the dashboard shows them without a query. Absent on older firmware.
        device.boot_reason = msg.get("boot_reason") or None
        device.boot_id = msg["boot_id"] if isinstance(msg.get("boot_id"), str) and msg["boot_id"] else None
        if device.boot_id:
            em_dbwriter.submit(db.record_boot, device_id, device.boot_id,
                               msg.get("version"), device.boot_reason)
        _note_wear(device, msg.get("emmc"))
        # Link-security telemetry for the dashboard: True when this control
        # connection arrived over the TLS listener.
        device.secure = secure
        em_state.devices[device_id] = device

        log.info(
            f"[control] Device connected: {device_id} v={version} "
            f"at {ip} caps={capabilities}"
        )
        em_dbwriter.submit(db.log_device,
            device_id, "info", "controller",
            f"Connected from {ip} version={version}"
        )

        await device.send_control({
            "type": "ack",
            "device_id": device_id,
            "features": CONTROLLER_FEATURES,
            # The device's wall clock. An Echo has no RTC that survives a
            # power cut and boots reading 2010; under emOS nothing ever
            # corrects it, because bionic resolves through Android's property
            # service so no bionic-linked binary there has DNS for an NTP
            # pool. Telling it over the link it already trusts costs one
            # integer on a message that already flows — against an NTP server
            # here, which would mean a listening socket, a second way for the
            # device to find us, and a daemon to supervise, for an accuracy
            # nothing in this system reads. Every measurement we take is
            # monotonic and the device never sends a timestamp.
            #
            # Not negotiated, and does not need to be: adding a field is safe
            # unnegotiated, older firmware ignores it, and a device that gets
            # no field leaves its clock alone.
            "time_ms": int(time.time() * 1000),
        })

        config = await loop.run_in_executor(
            None, db.get_effective_device_config, device_id
        )
        # A stored value of the wrong type fails the device's whole decode and
        # the conversions below; dropped, it reads as absent at both ends.
        config, bad_keys = em_config_types.drop_invalid(config)
        if bad_keys:
            log.warning(f"[control] {device_id}: stored config has values of "
                        f"the wrong type, not sent: {', '.join(bad_keys)}")
        # The Sendspin player advertises itself to Music Assistant under the
        # device's label, which the firmware does not otherwise know. Carried
        # on the same push, so the player starts under the right name rather
        # than restarting a moment later; a rename sends it alone.
        if device.sendspin_capable and row["label"]:
            config = {**config, "sendspinName": row["label"]}
        await device.send_control({"type": "config", **config})
        # Only firmware from before EchoHome acts on it; the privacy line
        # reads it against that firmware's "stream" report (em_listen).
        device.oww_on_device = em_listen.effective_mode(
            config.get("owwOnDevice"), device.oww_trigger_capable,
        )
        # Start script, controller address and debloat, reconciled against what
        # the device actually has — see em_reconcile.on_connect for why the
        # arrival is the trigger. Background: shell round trips and possibly a
        # multi-megabyte push, none of which the handshake should wait on.
        em_tasks.spawn(em_reconcile.on_connect(device_id, device))
        # An approval (a new device, or an approved pairing) issues link
        # credentials unless this connection already has working ones.
        if em_pairing.approved(device_id):
            if secure and await _presented_its_token(ws, device_id):
                em_pairing.done(device_id)
            else:
                em_tasks.spawn(em_credentials.issue_credentials(device_id))
        elif pairing:
            # Connected (no credentials yet, or plain) and asking: offer it.
            await api.notify_pair_request(device_id, "link")
        device.bass_guard_db      = float(config.get(
            "bassGuardDb", em_mbc.DEFAULT_BASS_GUARD_DB))
        device.led_scene     = em_scenes.resolve(config)
        # #263: hand the device its current listening animation so a wake it
        # detected ITSELF can light the ring immediately, instead of waiting
        # for leds_listening to make the round trip (measured +522ms before
        # the ring moved; control-plane tail reaches 2s, #139). The
        # controller's frame still lands within an RTT and takes over — the
        # device just stops being two hops behind its own event.
        if device.led_anim_capable and device.led_scene.get("listening_anim"):
            await device.send_control(em_scenes.ring_push(
                device.led_scene, device.capabilities))
        # Initialise volume from stored config — device will report its real
        # value via volume_state on connect, but this seeds a sane default
        # in the window before that first message arrives.
        device.volume = _device_level_to_ha(
            int(config.get("startupVolume", 85))
        )
        log.info(f"[control] Config pushed to {device_id} (volume={device.volume:.3f})")

        await leds_off(device)
        await api.notify_device_connected(device_id)
        await em_incubator.device_connected(device_id)

        # ── Main message loop ─────────────────────────────────────────────

        async def ping_loop():
            while True:
                await asyncio.sleep(PING_INTERVAL_SEC)
                now = loop.time()
                # Abandon replies that never came — a very late pong is a
                # lost packet, not a latency sample.
                stale = [q for q, t in device.ping_sent.items()
                         if now - t > PING_TIMEOUT_SEC]
                for q in stale:
                    device.ping_sent.pop(q, None)
                device.ping_seq += 1
                seq = device.ping_seq
                device.ping_sent[seq] = now
                # Record busyness at SEND time: the discriminator is whether
                # the device was doing anything when the probe went out, and
                # by the time the reply lands a turn may have started or
                # ended.
                device.ping_busy[seq] = device.is_busy()
                await device.send_control({"type": "ping", "id": seq})

        ping_task = asyncio.create_task(ping_loop())

        try:
            async for raw in ws:
                try:
                    msg = json.loads(raw)
                except Exception:
                    continue

                msg_type = msg.get("type")

                # One bad message must not cost the connection (#255):
                # the LEAST important message in the protocol used to be
                # able to disconnect the device - a missing attribute while
                # handling a playback_stats telemetry report took the
                # ESPHome satellite, the BLE proxy and the data plane down
                # with it, and reconnected on every barge-in. A handler
                # failure now logs message type + traceback and moves on.
                # Deliberate exceptions:
                #   - CancelledError propagates: teardown is not a message
                #     fault.
                #   - register runs BEFORE this loop and stays fatal -
                #     arriving into an unknown state is different from
                #     failing to record a statistic.
                try:
                    handler = CONTROL_HANDLERS.get(msg_type)
                    if handler is None:
                        log.debug(
                            f"[{device_id}] Unknown control message: {msg_type}"
                        )
                    else:
                        await handler(device, msg)

                except asyncio.CancelledError:
                    raise
                except Exception:
                    log.exception(
                        f"[{device_id}] Control handler failed for "
                        f"{msg_type!r} - message dropped, connection kept")
        finally:
            ping_task.cancel()

    except asyncio.TimeoutError:
        log.warning(f"[control] Registration timeout from {remote}")

    except websockets.exceptions.ConnectionClosed as e:
        _close_why = em_wsclose.from_exception(e)
        _msg = f"[control] Connection closed: {em_wsclose.describe(*_close_why)}"
        if em_wsclose.is_routine(_close_why[0], _close_why[2]):
            log.info(_msg)
        else:
            log.warning(_msg)

    except Exception as e:
        log.error(f"[control] Handler error: {e}")

    finally:
        if device:
            if em_state.devices.get(device.device_id) is not device:
                # A replacement connection has already registered for this
                # device_id — this socket is stale. Tearing down shared
                # per-device services here would rip them out from under the
                # live connection: on 2026-07-14 a stale close 4s after a
                # reconnect stopped Lounge's ESPHome listener, so HA's
                # redials hit connection-refused and every turn failed
                # no_ha for 11 hours until the next device bounce.
                log.info(
                    f"[control] Stale connection closed for "
                    f"{device.device_id} — replacement is active, keeping "
                    f"services up"
                )
            else:
                log.info(f"[control] Device disconnected: {device.device_id}")
                em_dbwriter.submit(db.log_device,
                    device.device_id, "info", "controller", "Disconnected"
                )
                # Stamp the moment it went away, so "last seen" is exact for
                # an offline device rather than up to one stats report stale.
                em_dbwriter.submit(db.touch_device_seen, device.device_id)
                em_state.devices.pop(device.device_id, None)
                # #315: the services stay up for a grace window instead of
                # being torn down immediately — a four-second link blip used
                # to deregister the HA entities, drop the BLE proxy and kill
                # the media session, then rebuild all of it when the device
                # returned on its own.
                em_tasks.spawn(_release_device_services(device))


async def _release_device_services(device) -> None:
    """
    Release a device's services CONTROL_RECONNECT_GRACE_S after its control
    connection closed — unless a replacement connection registered in the
    meantime (#315).

    esphome.device_connected() is idempotent (a still-listening port is
    kept and only the callbacks are refreshed), so holding services
    through a blip costs nothing on reconnect.
    """
    try:
        await asyncio.sleep(CONTROL_RECONNECT_GRACE_S)
        current = em_state.devices.get(device.device_id)
        if current is not None and current is not device:
            return  # replacement is live; it owns the services now
        log.info(
            f"[{device.device_id}] control connection not restored within "
            f"{CONTROL_RECONNECT_GRACE_S:.0f}s — releasing services"
        )
        await api.notify_device_disconnected(device.device_id)
        await em_incubator.device_disconnected(device.device_id)
        await em_esphome_mdns.device_disconnected(device.device_id)
    except asyncio.CancelledError:
        raise
    except Exception as e:
        log.error(f"[{device.device_id}] delayed service release failed: {e}")


def _note_wear(device, emmc) -> None:
    """Store the day's eMMC wear reading, only when the day or the reading changes."""
    values = em_health.wear_values(emmc)
    if values is None:
        return
    day = time.strftime("%Y-%m-%d", time.gmtime())
    if device.wear_written == (day, values):
        return
    device.wear_written = (day, values)
    em_dbwriter.submit(db.record_wear, device.device_id, day, values)


# ─── Data plane handler ───────────────────────────────────────────────────────

async def handle_data(ws: WebSocketServerProtocol, secure: bool = False):
    device = None
    remote = ws.remote_address
    # Initialised here, not in the handler below: the finally block reports
    # it on EVERY exit path, including the ones that never raise
    # ConnectionClosed at all.
    _close_why = (None, None, None, None)

    try:
        raw = await asyncio.wait_for(ws.recv(), timeout=10.0)
        msg = json.loads(raw)

        if msg.get("type") != "identify":
            log.warning(
                f"[data] First message from {remote} was not identify — closing"
            )
            await ws.close()
            return

        device_id = msg["device_id"]

        if not await _link_auth_ok(ws, device_id, secure, "data"):
            await ws.close()
            return

        for _ in range(20):
            device = em_state.devices.get(device_id)
            if device is not None:
                break
            await asyncio.sleep(0.1)

        if device is None:
            log.warning(f"[data] Unknown device_id: {device_id} — closing")
            await ws.close()
            return

        why = _not_from_control(device, ws, secure)
        if why:
            log.warning(f"[data] {device_id}: {why} — rejecting")
            device = None   # the finally must not treat the live device as ours
            await ws.close()
            return

        replaced, device.data_ws = device.data_ws, ws
        if replaced is not None and replaced is not ws:
            # #751: a redial means the old link is dead; left open it lasts
            # to the ping timeout. Playback carries on over this connection.
            em_tasks.spawn(replaced.close(code=1000, reason="replaced"))
        # #299: a fresh connection has by definition sent nothing yet — the
        # no-frames watchdog gives it FRESH_CONN_GRACE_S before treating
        # the silence as a zombie stream.
        # A session's audio rode the old connection, and the device closes
        # its session on a link drop; nothing from before is routable.
        device.data_ready.set()
        log.info(f"[data] Data connection established: {device_id}")

        # One bad frame must not close the data connection (#255), same
        # reading as the control plane. This loop runs per frame though
        # (~23/s), so a persistent fault would flood the log at frame
        # rate: one traceback per 5s window, suppressed count reported
        # when the next window opens.
        _frame_err_last: float = float("-inf")
        _frame_err_suppressed = 0
        async for raw in ws:
            try:
                if not isinstance(raw, bytes):
                    continue
                # Checked before the mic-frame guards below, which would
                # otherwise drop this on the header-length test.
                if raw and raw[0] == BLE_ADVERTS_TYPE:
                    continue  # the Echo's own ESPHome device is the proxy now
                # Audio (0x01 microphone, 0x07 session): the controller hears
                # none since EchoHome; the Echo's voice is its own satellite.
                continue
            except asyncio.CancelledError:
                raise
            except Exception:
                _now = asyncio.get_running_loop().time()
                if _now - _frame_err_last >= 5.0:
                    if _frame_err_suppressed:
                        log.error(
                            f"[{device.device_id}] {_frame_err_suppressed} data-frame errors suppressed")
                    log.exception(
                        f"[{device.device_id}] Data handler failed - frame dropped, connection kept")
                    _frame_err_last = _now
                    _frame_err_suppressed = 0
                else:
                    _frame_err_suppressed += 1
    except asyncio.TimeoutError:
        log.warning(f"[data] Identify timeout from {remote}")

    except websockets.exceptions.ConnectionClosed as e:
        # Keep the reason — see em_wsclose. Discarding it here is what made
        # three sessions of intermittent mid-response drops undiagnosable.
        _close_why = em_wsclose.from_exception(e)

    except Exception as e:
        log.error(f"[data] Handler error: {e}")

    finally:
        if device:
            if device.data_ws is ws:
                device.data_ws = None
                device.data_ready.clear()
            _msg = (f"[data] Data connection closed: {device.device_id} — "
                    f"{em_wsclose.describe(*_close_why)}")
            if em_wsclose.is_routine(_close_why[0], _close_why[2]):
                log.info(_msg)
            else:
                # A device dropping every few minutes on a keepalive timeout
                # is indistinguishable from one that never dropped, in a log
                # of INFO lines.
                log.warning(_msg)


# ─── Router ───────────────────────────────────────────────────────────────────

# ─── Shell plane handler ──────────────────────────────────────────────────────

async def handle_shell(ws: WebSocketServerProtocol, path: str, secure: bool = False):
    import aiohttp as _aiohttp

    # Path may carry a query: /shell/{device_id}?pty=1 signals that the
    # device actually established a PTY session (it may have been requested
    # but failed to allocate — the device falls back to a plain pipe and
    # omits the flag). The dashboard needs the established mode, not the
    # requested one, to pick its input framing.
    device_id, _, query = path.removeprefix("/shell/").partition("?")
    pty_mode = "pty=1" in query
    if not device_id:
        log.warning("[shell] Missing device_id in path")
        await ws.close()
        return

    if not await _link_auth_ok(ws, device_id, secure, "shell"):
        await ws.close()
        return

    live = em_state.devices.get(device_id)
    why = "device is not connected" if live is None else _not_from_control(live, ws, secure)
    if why:
        log.warning(f"[shell] {device_id}: {why} — rejecting")
        await ws.close()
        return

    log.info(f"[shell] Device connected: {device_id} (pty={pty_mode})")

    done_future  = em_state.shell_pending.get(device_id)
    dashboard_ws = em_state.shell_dashboard.get(device_id)

    if done_future is None or done_future.done():
        log.warning(f"[shell] No pending shell request for {device_id} — closing")
        await ws.close()
        return

    if dashboard_ws is None:
        log.info(f"[shell] Programmatic session: {device_id}")
        done_future.set_result(ws)
        try:
            await asyncio.wait_for(ws.wait_closed(), timeout=300.0)
        except (asyncio.TimeoutError, Exception):
            pass
        log.info(f"[shell] Programmatic session ended: {device_id}")
        return

    log.info(f"[shell] Proxying: {device_id}")

    # Tell the dashboard which mode the device established before any
    # shell bytes flow: PTY sessions use framed input (0x00 stdin /
    # 0x01 resize) and emit terminal escape sequences; pipe sessions
    # (pre-PTY firmware) are raw both ways.
    try:
        await dashboard_ws.send_str(json.dumps({"type": "shell_meta", "pty": pty_mode}))
    except Exception:
        pass

    async def device_to_dashboard():
        try:
            async for msg in ws:
                if isinstance(msg, bytes):
                    await dashboard_ws.send_bytes(msg)
                else:
                    await dashboard_ws.send_str(msg)
        except Exception:
            pass

    async def dashboard_to_device():
        try:
            async for msg in dashboard_ws:
                if msg.type == _aiohttp.WSMsgType.BINARY:
                    await ws.send(msg.data)
                elif msg.type == _aiohttp.WSMsgType.TEXT:
                    await ws.send(msg.data.encode())
                elif msg.type in (_aiohttp.WSMsgType.CLOSE,
                                  _aiohttp.WSMsgType.ERROR):
                    break
        except Exception:
            pass

    tasks = [
        asyncio.create_task(device_to_dashboard()),
        asyncio.create_task(dashboard_to_device()),
    ]
    try:
        await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
    finally:
        for task in tasks:
            task.cancel()
        log.info(f"[shell] Session ended: {device_id}")
        if not done_future.done():
            done_future.set_result(None)

async def _route(ws: WebSocketServerProtocol, secure: bool):
    path = ws.request.path if hasattr(ws, "request") else getattr(ws, "path", "/")
    em_tcp.tune(ws.transport.get_extra_info("socket"))

    if path == "/control":
        await handle_control(ws, secure)
    elif path == "/data":
        await handle_data(ws, secure)
    elif path.startswith("/shell/"):
        await handle_shell(ws, path, secure)
    else:
        log.warning(f"Unknown WebSocket path: {path} from {ws.remote_address}")
        await ws.close()


async def router(ws: WebSocketServerProtocol):
    await _route(ws, secure=False)


async def router_tls(ws: WebSocketServerProtocol):
    await _route(ws, secure=True)


# ─── mDNS ─────────────────────────────────────────────────────────────────────

def _make_mdns_info(tls_active: bool) -> ServiceInfo:
    props = {"version": "1", "server": MDNS_NAME}
    if tls_active:
        # Devices holding the pushed CA dial wss://<addr>:<tls_port> instead
        # of the plain port. Absent property = pre-TLS controller → plain ws.
        props["tls_port"] = str(SERVER_TLS_PORT)
    return ServiceInfo(
        "_emcontroller._tcp.local.",
        f"{MDNS_NAME}._emcontroller._tcp.local.",
        addresses=[socket.inet_aton(SERVER_IP)],
        port=SERVER_PORT,
        properties=props,
        server=f"{MDNS_NAME}.local.",
    )


async def _mdns_refresh_loop(azc: AsyncZeroconf, info: ServiceInfo) -> None:
    while True:
        await asyncio.sleep(MDNS_REFRESH_INTERVAL)
        try:
            await azc.async_update_service(info)
            log.debug("mDNS registration refreshed")
        except Exception as e:
            log.warning(f"mDNS refresh failed: {e}")


# ─── Main ─────────────────────────────────────────────────────────────────────

async def event_loop_lag_monitor(interval: float = 1.0,
                                 warn_ms: float = 250.0) -> None:
    """
    Watch for asyncio event-loop stalls.

    Sleeps for a known interval and measures the overshoot: if the loop is
    blocked by synchronous work, the wake-up is late by roughly the length
    of that block. Anything blocking the loop also delays speaker frames
    reaching the socket, so this is the controller-side counterpart to the
    device's buffer-margin metric — it answers "were we the ones who were
    late?" without needing a profiler attached.

    Costs one wake-up per second and logs only when a threshold is crossed;
    the running peak is exposed on /api/system/status.
    """
    loop = asyncio.get_event_loop()
    next_cpu = 0.0
    while True:
        t0 = loop.time()
        await asyncio.sleep(interval)
        lag_ms = (loop.time() - t0 - interval) * 1000
        if lag_ms > em_state.loop_lag_peak_ms:
            em_state.loop_lag_peak_ms = lag_ms
        # Piggyback the controller's CPU sampling on this ticker rather than
        # starting a second one: it is one os.times() every 30s, and the
        # windowed CPU it feeds is only read when a support bundle is built.
        if loop.time() >= next_cpu:
            next_cpu = loop.time() + em_process_stats.CPU_SAMPLE_INTERVAL_S
            em_process_stats.sample_cpu()
        if lag_ms >= warn_ms:
            log.warning(
                f"[loop] event loop stalled {lag_ms:.0f}ms — "
                f"speaker sends and LED frames were delayed by this much"
            )


async def _until_stopped() -> None:
    """Return on SIGTERM or SIGINT, so main()'s teardown runs. In a container
    we are PID 1 (em_start execs us), and the kernel drops a signal PID 1 has
    no handler for: `docker stop` used to SIGKILL us after its grace period."""
    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGTERM, signal.SIGINT):
        loop.add_signal_handler(sig, stop.set)
    await stop.wait()
    log.info("Shutting down")
    for sig in (signal.SIGTERM, signal.SIGINT):
        loop.remove_signal_handler(sig)


def _close_db() -> None:
    em_dbwriter.drain()
    db.close()


async def main():
    log.info(f"EchoMuse Controller {CONTROLLER_VERSION}")
    db.init(DB_PATH)
    em_state.boards.update(em_boards.decode(await asyncio.get_running_loop()
                           .run_in_executor(None, db.get_config, em_boards.CONFIG_KEY)))
    auth.maybe_generate_bootstrap_token()

    runner = await api.create_runner()
    await runner.setup()
    site = web.TCPSite(runner, SERVER_HOST, API_PORT)
    await site.start()
    log.info(f"Dashboard + API listening on http://{SERVER_HOST}:{API_PORT}")

    release_task       = asyncio.create_task(em_releases.poll_loop())
    session_prune_task = asyncio.create_task(api.session_prune_loop())
    loop_lag_task      = asyncio.create_task(event_loop_lag_monitor())

    # Device-link TLS: generate/load the CA + server cert. Failure to set
    # up TLS (missing cryptography package, unwritable dir) must never take
    # the plain listener down with it — the fleet lives on that during
    # rollout.
    tls_ctx = None
    if SERVER_TLS_PORT:
        try:
            tls_dir = em_pki.ensure_pki(DB_PATH)
            if tls_dir:
                tls_ctx = em_pki.server_ssl_context(tls_dir)
                em_state.set_tls_dir(tls_dir)
        except Exception as e:
            log.error(f"Device-link TLS setup failed — wss listener disabled: {e}")
    em_state.set_link_ports(SERVER_IP, SERVER_PORT, SERVER_TLS_PORT if tls_ctx else 0)
    # Before any Echo can connect: an Incubator Echo's ESPHome device must not
    # offer HA a voice assistant even on HA's first read (em_incubator.start).
    await em_incubator.start(SERVER_HOST)

    # Bound to SERVER_IP's own interface, never auto-detected across all of
    # them. zeroconf's default enumerates every interface on the host and
    # opens a socket per one, and on a multi-homed box that includes whatever
    # else is attached — a monitoring VLAN in one deployment, something else
    # elsewhere. A send on any one of those sockets can fail on its own
    # (measured: `OSError: [Errno 19] No such device` from a `Monitor*`
    # interface that carries a real IPv4 address and gets auto-detected like
    # any other), and that exception surfaces inside the asyncio event loop
    # rather than being contained to the interface that caused it — stalling
    # the loop long enough to miss keepalive pings and disconnect the fleet.
    # SERVER_IP is already the one address every device is told to dial, so
    # advertising from anywhere else would be a second kind of wrong answer.
    azc  = em_hostip.bind_mdns(AsyncZeroconf, SERVER_IP)
    em_state.zeroconf = azc  # the Voice Incubator announcements share it
    info = _make_mdns_info(tls_active=tls_ctx is not None)
    await azc.async_register_service(info, allow_name_change=True)
    log.info(
        f"mDNS advertising {MDNS_NAME}._emcontroller._tcp.local "
        f"→ {SERVER_IP}:{SERVER_PORT}"
        + (f" (tls_port={SERVER_TLS_PORT})" if tls_ctx else "")
    )
    mdns_task = asyncio.create_task(_mdns_refresh_loop(azc, info))

    log.info(f"WebSocket server starting on {SERVER_HOST}:{SERVER_PORT}")

    try:
        async with contextlib.AsyncExitStack() as stack:
            await stack.enter_async_context(websockets.serve(
                router,
                SERVER_HOST,
                SERVER_PORT,
                ping_interval=WS_PING_INTERVAL_S,
                ping_timeout=WS_PING_TIMEOUT_S,
                max_size=10 * 1024 * 1024,
            ))
            if tls_ctx is not None:
                await stack.enter_async_context(websockets.serve(
                    router_tls,
                    SERVER_HOST,
                    SERVER_TLS_PORT,
                    ssl=tls_ctx,
                    ping_interval=WS_PING_INTERVAL_S,
                    ping_timeout=WS_PING_TIMEOUT_S,
                    max_size=10 * 1024 * 1024,
                ))
                log.info(f"Device-link TLS (wss) listening on {SERVER_HOST}:{SERVER_TLS_PORT}")
            if REQUIRE_DEVICE_TLS:
                log.info("REQUIRE_DEVICE_TLS=1 — plain/tokenless device connections will be rejected")

            log.info("EchoMuse Controller ready — waiting for devices")
            await _until_stopped()

    finally:
        await em_incubator.stop_all()
        release_task.cancel()
        session_prune_task.cancel()
        loop_lag_task.cancel()
        mdns_task.cancel()
        await azc.async_unregister_service(info)
        await azc.async_close()
        await runner.cleanup()
        await asyncio.get_running_loop().run_in_executor(None, _close_db)
        log.info("EchoMuse Controller stopped")


if __name__ == "__main__":
    asyncio.run(main())
