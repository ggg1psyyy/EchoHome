"""
em_listen.py — private listening: sessions, per-Echo state, capture time
=========================================================================

The controller half of docs/listening.md. An Echo listening privately scores
its own wake word and sends nothing until it fires; a wake opens a numbered
SESSION whose audio arrives on /data as 0x07 frames tagged with that number,
and the controller closes it at end of speech. Everything here is pure, so it
is tested without aiohttp, openwakeword or a socket (tests/test_listen.py).

Three pieces:

- `resolve` — what an Echo is actually doing, from what was configured, what
  the firmware can do and what the Echo last reported. The dashboard's privacy
  statement is built from this and nothing else.
- `SessionRouter` — which session audio may reach a turn. Control and data are
  separate sockets, so a session's first frames can beat its `oww_wake`, and a
  late frame of a closed session can arrive after the controller moved on.
  Untagged audio was routed by a flag, and a frame on the wrong side of a flag
  flip became the start of the next person's command.
- `heard_at` / `CaptureClock` / `DeviceClock` — capture time in the controller's clock,
  so arbitration picks the Echo that heard first rather than the one whose
  message arrived first.
"""

from __future__ import annotations

from collections import deque
from dataclasses import dataclass, field

# The controller's feature, announced on the ack. The device listens privately
# only when it sees this: an older controller acts on a device wake only when
# wake-stream frames are arriving, so a device that went quiet on it is deaf.
FEATURE = "listen_session"

# The device's capability: it CAN listen privately. Whether it IS comes from
# its listen_state report.
CAPABILITY = "oww_local_only"


# How long frames for a session nobody has announced are held, waiting for its
# oww_wake. Covers the control plane lagging the data plane by a retransmit or
# two (#139 measured RTO at 500-800ms) with room to spare; beyond it the wake
# is not coming, and the device's own ack timeout (3s) has closed the session.
PENDING_MAX_S = 3.0
# At most this many unannounced sessions held at once. More means something is
# wrong, and the oldest is dropped rather than growing without bound.
PENDING_MAX_SESSIONS = 4
# Closed ids remembered, so their stragglers are dropped rather than held.
CLOSED_MEMORY = 32

# How long past its window a winning claim is held, so a claim for the same
# utterance that spent seconds in a retransmit still finds it and cedes. A
# fixed hold, not one sized from recent RTTs: those lag the very stall that
# delays a claim. Holding costs nothing, because a claim cedes only if it was
# HEARD within the window of the winner's — a separate wake in another room is
# told apart by its capture time, not by when it arrives. 3s matches the
# Echo's ack timeout, after which a late private wake's session is gone.
MAX_ARB_SLACK_S = 3.0


DETECT_DEVICE     = "device"
DETECT_CONTROLLER = "controller"
DETECT_UNKNOWN    = "unknown"


# ── State ────────────────────────────────────────────────────────────────────

STATE_LOCAL      = "local"
STATE_CONTROLLER = "controller"
STATE_DIAGNOSTIC = "diagnostic"
STATE_LEGACY     = "legacy"
STATE_DEGRADED   = "degraded"
STATE_UNKNOWN    = "unknown"


@dataclass(frozen=True)
class ListenView:
    """What an Echo is doing with its microphone, for the dashboard and logs.

    `streams` is True/False, or None when it is not yet known — which the
    dashboard must render as unknown, never as private.
    """
    state:   str
    streams: bool | None
    reason:  str = ""

    @property
    def private(self) -> bool:
        """Session-driven: no wake stream, turns start from device wakes."""
        return self.state in (STATE_LOCAL, STATE_DEGRADED)


# The stored owwOnDevice and what it means against a device's capability,
# moved here from em_shadow (removed with the controller's wake word): it
# still reads a "stream" report from firmware from before EchoHome.
MODE_OFF = "off"
MODE_SHADOW = "shadow"
MODE_ON = "on"
MODES = (MODE_OFF, MODE_SHADOW, MODE_ON)


def normalise_mode(v) -> str:
    """
    Map a stored or pushed owwOnDevice value onto a known mode.

    Unknown values become "off" rather than being guessed at, for the reason
    the device gives: the two plausible readings of an unrecognised mode are
    "score silently" and "start triggering turns", and one of those is a live
    behaviour change nobody asked for.
    """
    s = str(v or "").strip().lower()
    return s if s in MODES else MODE_OFF

# How far apart the device's crossing and the controller's detection may be and
# still be considered the same utterance.
#
# 2s is deliberately loose. Both detectors see the same frames, but not in the
# same detector STATE: the controller drops wake frames while a turn or TTS is
# in flight, so its feature ring can lag the device's by more than a frame or
# two. Too tight and real agreements are recorded as misses — the more
# misleading of the two possible errors, because a false "miss" argues against
# a feature that is actually working.
MATCH_WINDOW_S = 2.0

# Ceiling on a reported crossing age, so a stale or malformed report cannot be
# projected backwards onto an unrelated wake.
MAX_AGE_S = 10.0

# Crossings are rare — the device applies a refractory period, so one per
# utterance — and only the most recent few can ever fall inside a match window.
# A bounded deque therefore needs no sweeper.
RING = 32


def effective_mode(configured, trigger_capable: bool) -> str:
    """
    The mode actually in force, given what the device can do.

    "on" against firmware that cannot trigger degrades to "shadow", never to
    "on": the controller would otherwise stop acting on its own detections
    while waiting for device wakes that the firmware has no code to send, and
    the device would be deaf. That is the wrong answer rather than the old
    behaviour, and the whole point of gating on capability is to not produce
    one. Shadow keeps the device scoring, which is what the user asked for as
    far as this firmware can deliver it.

    The dashboard refuses to offer "on" to such a device in the first place, so
    reaching this normally means firmware was rolled back under a config that
    was valid when it was set.

    **A missing wake word MODEL never changes the mode** (Wil, 2026-09-22). It
    used to drop "on" to "off" so the controller would trigger instead; for
    private-listening firmware that was already forbidden (docs/listening.md —
    it would score a stream the operator chose not to send), and for older
    firmware it was a fallback nobody chose. A device missing its model keeps
    its mode, answers the button, and the connect reconcile installs the model
    straight away — every device carries the full asset set.
    """
    mode = normalise_mode(configured)
    if mode == MODE_ON and not trigger_capable:
        return MODE_SHADOW
    return mode


def resolve(configured: str, capabilities, reported: str | None,
            reported_reason: str = "") -> ListenView:
    """
    The state in force for one Echo.

    `configured` is the stored owwOnDevice ("on"/"off"/"shadow", already
    normalised). `reported` is the device's last listen_state, None if it has
    not sent one on this connection.

    For firmware that can listen privately (CAPABILITY), its REPORT decides
    and the stored mode only reads a "stream" report: EchoHome firmware has
    no mode to configure (its wake word is the Voice Incubator satellite's,
    on the Echo, and nothing of the room ever goes to the controller), so a
    stored "off" or "shadow" from before must not make it read as streaming.
    Firmware from before it reports "stream" in its streaming modes, which
    keep their labels.
    """
    caps = set(capabilities or ())
    if caps and "mic" not in caps:
        # Every firmware has announced "mic"; one that does not has no
        # microphone it can read (a board whose capture is not supported
        # yet). It neither streams nor listens in any mode, and saying it
        # streams because the mode is "off" would be untrue.
        return ListenView(STATE_DEGRADED, False,
                          reported_reason or "no microphone it can read — speaker only")
    if CAPABILITY in caps:
        if reported == "local":
            return ListenView(STATE_LOCAL, False)
        if reported == "degraded":
            return ListenView(STATE_DEGRADED, False,
                              reported_reason or "cannot run its wake word — button only")
        if reported == "stream":
            if configured == "off":
                return ListenView(STATE_CONTROLLER, True)
            if configured == "shadow":
                return ListenView(STATE_DIAGNOSTIC, True,
                                  "diagnostic: both detectors score a continuous stream")
            # The device has not switched yet (a config push in flight). It
            # is streaming now, and saying otherwise would be the one lie
            # this module exists to prevent.
            return ListenView(STATE_UNKNOWN, True, "switching to private listening")
        return ListenView(STATE_UNKNOWN, None, "waiting for the Echo to report")
    # Firmware from before private listening streams in every mode.
    if configured == "off":
        return ListenView(STATE_CONTROLLER, True)
    if configured == "shadow":
        return ListenView(STATE_DIAGNOSTIC, True,
                          "diagnostic: both detectors score a continuous stream")
    return ListenView(STATE_LEGACY, True, "update the firmware for private listening")


# ── Frames ───────────────────────────────────────────────────────────────────


# ── Routing ──────────────────────────────────────────────────────────────────

@dataclass
class _Pending:
    first_seen: float
    frames: list = field(default_factory=list)


class SessionRouter:
    """
    Decides which session audio reaches a turn, for one Echo.

    - A frame for the ACTIVE session is delivered.
    - A frame for a session this router has closed is dropped.
    - A frame for a session not yet announced is held (bounded) until its
      wake arrives, then delivered in order ahead of anything newer.

    One active session at a time, because an Echo holds at most one open.
    """

    def __init__(self) -> None:
        self.active: int | None = None
        self._pending: dict[int, _Pending] = {}
        self._closed: list[int] = []
        self.dropped = 0          # frames discarded: closed or expired

    def frame(self, session: int, pcm: bytes, now: float) -> list[bytes]:
        """Route one frame; returns what to deliver now (0 or 1 chunks)."""
        self._expire(now)
        if session == self.active:
            return [pcm]
        if session in self._closed:
            self.dropped += 1
            return []
        p = self._pending.get(session)
        if p is None:
            if len(self._pending) >= PENDING_MAX_SESSIONS:
                oldest = min(self._pending, key=lambda s: self._pending[s].first_seen)
                self.dropped += len(self._pending.pop(oldest).frames)
            p = self._pending[session] = _Pending(first_seen=now)
        p.frames.append(pcm)
        return []

    def open(self, session: int, now: float) -> list[bytes]:
        """Make `session` active; returns its held frames, oldest first.

        Any previously active session is closed: the Echo has moved on, so
        its stragglers belong to nobody.
        """
        self._expire(now)
        if self.active is not None and self.active != session:
            self._remember_closed(self.active)
        self.active = session
        p = self._pending.pop(session, None)
        return p.frames if p else []

    def close(self, session: int | None = None) -> int | None:
        """Close `session` (or the active one). Returns what was closed."""
        target = self.active if session is None else session
        if target is None:
            return None
        if target == self.active:
            self.active = None
        p = self._pending.pop(target, None)
        if p:
            self.dropped += len(p.frames)
        self._remember_closed(target)
        return target

    def is_closed(self, session: int) -> bool:
        """True if `session` has ended, here or on the Echo."""
        return session in self._closed

    def reset(self) -> None:
        """A new connection: nothing from the old one is routable."""
        self.__init__()

    def _remember_closed(self, session: int) -> None:
        if session in self._closed:
            return
        self._closed.append(session)
        if len(self._closed) > CLOSED_MEMORY:
            del self._closed[0]

    def _expire(self, now: float) -> None:
        for s in [s for s, p in self._pending.items()
                  if now - p.first_seen > PENDING_MAX_S]:
            self.dropped += len(self._pending.pop(s).frames)
            self._remember_closed(s)


# ── Capture time ─────────────────────────────────────────────────────────────


class Frame(bytes):
    """
    Stream mic audio stamped with when it arrived and when it was captured,
    both in the loop's clock.

    A controller-scored wake is heard when its frame was CAPTURED — not when
    inference on it finished, and not when it arrived. The first put every
    frame's wait in mic_queue (up to 5s) and the executor into arbitration
    against Echoes that report their own capture age; the second put the
    link's retransmits there. A bytes subclass, so every other reader of the
    queues is unchanged.
    """

    def __new__(cls, data: bytes, arrived: float, captured: float | None = None):
        f = super().__new__(cls, data)
        f.arrived = arrived
        f.captured = arrived if captured is None else captured
        return f


def captured(payload, default: float) -> float:
    """When `payload` was captured; `default` for audio never stamped."""
    return getattr(payload, "captured", default)


class DeviceClock:
    """
    An Echo's monotonic clock mapped onto ours, from the pings we already send.

    Each reply carries the Echo's monotonic time (`mono`, ms) when it answered,
    which lies between our send and our receipt; the midpoint is off by at
    most half that round trip. So the sample with the SMALLEST round trip in a
    sliding window is the best one, the rule NTP uses: a retransmit only ever
    lengthens a round trip, so it is never chosen. Two minutes of 5s pings is
    24 chances for one clean exchange, and short enough that the two clocks
    drifting apart costs milliseconds.
    """

    WINDOW_S = 120.0

    def __init__(self) -> None:
        self._best: deque[tuple[float, float, float]] = deque()   # (received, rtt, offset), rtt increasing

    def add(self, sent: float, received: float, device_ms) -> None:
        try:
            dev = float(device_ms) / 1000.0
        except (TypeError, ValueError):
            return
        rtt = received - sent
        if rtt < 0:
            return
        offset = (sent + received) / 2 - dev
        while self._best and self._best[-1][1] >= rtt:
            self._best.pop()
        self._best.append((received, rtt, offset))
        while received - self._best[0][0] > self.WINDOW_S:
            self._best.popleft()

    def to_local(self, device_ms, arrived: float) -> float | None:
        """When device time `device_ms` was, in our clock; None if unknown.
        Never after `arrived`, and never more than the hold before it."""
        if not self._best or device_ms is None:
            return None
        try:
            t = float(device_ms) / 1000.0 + self._best[0][2]
        except (TypeError, ValueError):
            return None
        return max(arrived - MAX_ARB_SLACK_S, min(arrived, t))


class RttEstimator:
    """TCP's smoothed RTT and variance (RFC 6298), per Echo, in ms."""

    def __init__(self) -> None:
        self.srtt: float | None = None
        self.rttvar: float = 0.0

    def add(self, rtt_ms: float) -> None:
        r = float(rtt_ms)
        if self.srtt is None:
            self.srtt, self.rttvar = r, r / 2
            return
        self.rttvar = 0.75 * self.rttvar + 0.25 * abs(self.srtt - r)
        self.srtt = 0.875 * self.srtt + 0.125 * r

    @property
    def rto(self) -> float | None:
        if self.srtt is None:
            return None
        return self.srtt + 4 * self.rttvar


# ── Wakes ────────────────────────────────────────────────────────────────────

