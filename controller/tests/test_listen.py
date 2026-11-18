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


def test_a_stored_mode_is_normalised_and_unknown_reads_off():
    for raw, want in (("on", L.MODE_ON), ("ON", L.MODE_ON), (" shadow ", L.MODE_SHADOW),
                      ("off", L.MODE_OFF), ("", L.MODE_OFF), (None, L.MODE_OFF),
                      ("sometimes", L.MODE_OFF)):
        assert L.normalise_mode(raw) == want, raw


def test_on_degrades_to_shadow_for_firmware_that_cannot_trigger():
    assert L.effective_mode("on", trigger_capable=True) == L.MODE_ON
    assert L.effective_mode("on", trigger_capable=False) == L.MODE_SHADOW
    assert L.effective_mode("off", trigger_capable=False) == L.MODE_OFF


def test_firmware_without_private_listening_keeps_its_old_labels():
    assert L.resolve("off", OLD, None).state == L.STATE_CONTROLLER
    assert L.resolve("shadow", OLD, None).state == L.STATE_DIAGNOSTIC
