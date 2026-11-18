"""em_listen — private listening (docs/listening.md)."""

import em_listen as L


# ── resolve ──────────────────────────────────────────────────────────────────

NEW = {"mic", "oww_shadow", "oww_trigger", L.CAPABILITY}
OLD = {"mic", "oww_shadow", "oww_trigger"}


def test_controller_mode_streams_while_the_echo_says_it_does():
    v = L.resolve("off", NEW, "stream")
    assert v.state == L.STATE_CONTROLLER and v.streams is True


def test_shadow_is_a_streaming_diagnostic():
    v = L.resolve("shadow", NEW, "stream")
    assert v.state == L.STATE_DIAGNOSTIC and v.streams is True


def test_the_echos_report_outranks_a_stored_mode():
    """EchoHome: nothing to configure, and nothing goes to the controller.
    A stored "off" or "shadow" from before must not read as streaming."""
    for mode in ("off", "shadow", "on"):
        v = L.resolve(mode, NEW, "local")
        assert v.state == L.STATE_LOCAL and v.streams is False, mode
        assert L.resolve(mode, NEW, "degraded", "no microphone").streams is False, mode
        assert L.resolve(mode, NEW, None).streams is None, mode


def test_private_only_on_the_devices_word():
    """Configuration alone never makes an Echo private: only its report."""
    assert L.resolve("on", NEW, "local").streams is False
    assert L.resolve("on", NEW, None).streams is None
    assert L.resolve("on", NEW, "stream").streams is True


def test_degraded_does_not_stream_and_says_why():
    v = L.resolve("on", NEW, "degraded", "model missing")
    assert v.state == L.STATE_DEGRADED and v.streams is False
    assert "model missing" in v.reason
    assert v.private


def test_an_echo_with_no_mic_is_degraded_in_every_mode():
    """A board whose capture the firmware cannot read announces no "mic".
    It is not streaming in controller mode, whatever the configuration says."""
    caps = NEW - {"mic"}
    for mode in ("off", "shadow", "on"):
        v = L.resolve(mode, caps, "degraded", "no microphone this firmware can read on this board")
        assert v.state == L.STATE_DEGRADED and v.streams is False, mode
        assert "microphone" in v.reason
    assert "microphone" in L.resolve("off", caps, None).reason
    # Not knowing the capabilities yet is not the same as having none.
    assert L.resolve("off", set(), None).state == L.STATE_CONTROLLER


def test_old_firmware_is_shown_streaming():
    v = L.resolve("on", OLD, "local")   # a stray report must not override
    assert v.state == L.STATE_LEGACY and v.streams is True
    assert "firmware" in v.reason
    assert not v.private


# ── routing ──────────────────────────────────────────────────────────────────

def test_audio_that_beats_its_wake_is_held_then_delivered_in_order():
    r = L.SessionRouter()
    assert r.frame(1, b"a", 0.0) == []
    assert r.frame(1, b"b", 0.1) == []
    assert r.open(1, 0.2) == [b"a", b"b"]
    assert r.frame(1, b"c", 0.3) == [b"c"]


def test_stragglers_of_a_closed_session_never_reach_the_next():
    r = L.SessionRouter()
    r.open(1, 0.0)
    r.close(1)
    assert r.frame(1, b"late", 0.1) == []
    r.open(2, 0.2)
    assert r.frame(1, b"later", 0.3) == []
    assert r.frame(2, b"x", 0.3) == [b"x"]


def test_opening_a_new_session_closes_the_old():
    r = L.SessionRouter()
    r.open(1, 0.0)
    r.open(2, 0.1)
    assert r.frame(1, b"old", 0.2) == []


def test_unannounced_audio_expires():
    r = L.SessionRouter()
    r.frame(9, b"a", 0.0)
    r.frame(8, b"b", L.PENDING_MAX_S + 0.5)    # triggers expiry of 9
    assert r.open(9, L.PENDING_MAX_S + 0.6) == []
    assert r.dropped >= 1


def test_pending_sessions_are_bounded():
    r = L.SessionRouter()
    for s in range(1, L.PENDING_MAX_SESSIONS + 3):
        r.frame(s, b"x", 0.01 * s)
    assert len(r._pending) == L.PENDING_MAX_SESSIONS
    assert r.open(1, 0.1) == []     # the oldest went


def test_close_with_no_active_session_is_harmless():
    r = L.SessionRouter()
    assert r.close() is None


# ── capture time ─────────────────────────────────────────────────────────────


def test_rtt_estimator_follows_rfc6298():
    e = L.RttEstimator()
    assert e.rto is None
    e.add(100)
    assert e.srtt == 100 and e.rto == 100 + 4 * 50
    e.add(200)
    assert e.srtt == 0.875 * 100 + 0.125 * 200


def test_a_session_the_echo_closed_is_known_closed():
    r = L.SessionRouter()
    assert not r.is_closed(8)
    r.close(8)                     # listen_end before its wake was acted on
    assert r.is_closed(8)
    assert r.frame(8, b"\0\0", 0.0) == []


# ── CaptureClock: dating stream frames through a lossy link ─────────────────

def _feed(clock, frames):
    """frames: [(seq, arrived)] -> [captured]"""
    return [clock.observe(s, a) for s, a in frames]


# ── DeviceClock: an Echo's monotonic clock in ours ──────────────────────────

def _pings(clock, skew, samples):
    """samples: [(sent, out_delay, back_delay)]; the Echo's clock = ours - skew."""
    for sent, out, back in samples:
        clock.add(sent, sent + out + back, (sent + out - skew) * 1000)


def test_device_clock_maps_through_the_cleanest_exchange():
    c = L.DeviceClock()
    skew = 1234.5
    # Mostly retransmit-delayed, asymmetric exchanges; one clean one.
    _pings(c, skew, [(0, 0.9, 0.002), (5, 0.002, 1.4), (10, 0.0015, 0.0015),
                     (15, 0.4, 0.3), (20, 2.1, 0.002)])
    t = c.to_local((30.0 - skew) * 1000, arrived=30.5)
    assert abs(t - 30.0) < 0.002


def test_a_wake_held_three_seconds_in_flight_is_dated_to_its_capture():
    """Bench, 14:59:35: VVV's wake arrived 3.1s after capture. Age plus half
    an RTT would put it at arrival; the Echo's clock puts it where it was."""
    c = L.DeviceClock()
    skew = 500.0
    _pings(c, skew, [(t, 0.002, 0.002) for t in range(0, 60, 5)])
    captured_dev_ms = (100.0 - skew) * 1000
    assert abs(c.to_local(captured_dev_ms, arrived=102.9) - 100.0) < 0.003
    # Never before the hold: past it, the session is gone anyway.
    assert c.to_local(captured_dev_ms, arrived=110.0) == 110.0 - L.MAX_ARB_SLACK_S
    # And never after arrival.
    assert c.to_local(captured_dev_ms, arrived=99.0) == 99.0


def test_device_clock_without_samples_or_field_says_unknown():
    c = L.DeviceClock()
    assert c.to_local(1000, arrived=5.0) is None
    c.add(0.0, 0.004, None)          # a pong from firmware that sends no mono
    assert c.to_local(1000, arrived=5.0) is None
    c.add(0.0, 0.004, 2.0)
    assert c.to_local(None, arrived=5.0) is None


def test_old_samples_leave_the_window():
    """An exchange older than the window no longer counts, however clean —
    otherwise the clocks drifting apart would never be followed."""
    c = L.DeviceClock()
    c.add(0.0, 0.001, 0.0)                    # superb, offset 0, but old
    c.add(200.0, 200.010, (200.005 - 7.0) * 1000)   # offset 7s now
    assert abs(c.to_local((250.0 - 7.0) * 1000, arrived=250.5) - 250.0) < 1e-6


# ── detection path and the mixed-fleet hold (em_arbiter.contest) ─────────────


# ── skip_backlog (#946) ──────────────────────────────────────────────────────

import asyncio  # noqa: E402

import _sources  # noqa: E402


# ── awaiting_report: nothing starts before a private Echo says it is private ──
#
# 41C8, 2026-11-15: after a reboot the Echo reported `stream` (its boot state)
# for 0.9s, and the controller's streaming path sent mic_start once its model
# had loaded. Only a 2.9s load kept that mic_start out of the window.


