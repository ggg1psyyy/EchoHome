"""
em_listen.py — what an Echo does with its microphone, for the privacy line
==========================================================================

The controller half of docs/listening.md: `resolve` says what an Echo is
actually doing, from what the firmware can do, what the Echo last reported
and, for firmware from before EchoHome, the stored owwOnDevice. The
dashboard's privacy statement is built from this and nothing else. Pure, so
it is tested without aiohttp or a socket (tests/test_listen.py).

EchoHome firmware listens on the Echo, through its Voice Incubator
satellite, and sends nothing of the room to the controller; the sessions,
the routing of their audio and the capture-time clocks that served the
controller's wake word went with it.
"""

from __future__ import annotations

from dataclasses import dataclass

# The controller's feature, announced on the ack. The device listens privately
# only when it sees this: an older controller acts on a device wake only when
# wake-stream frames are arriving, so a device that went quiet on it is deaf.
FEATURE = "listen_session"

# The device's capability: it CAN listen privately. Whether it IS comes from
# its listen_state report.
CAPABILITY = "oww_local_only"


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
