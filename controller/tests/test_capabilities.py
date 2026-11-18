"""
Device/controller compatibility is negotiated by CAPABILITY, not version.

The two halves of this project ship on independent version schemes (device
`v*`, controller `controller-v*`), so any given moment can pair new firmware
with an old controller or the reverse. Version comparison would mean encoding
release history into the controller and getting it wrong the first time
someone runs a dev build; a capability is the device stating what it
implements.

That only works if both sides spell the capability identically. A typo makes
the feature permanently unavailable and looks exactly like a device that does
not support it — silent, and the sort of thing you debug from the wrong end.
So the strings are asserted to match across the two languages, the same way
CONFIG_SECTIONS is mirrored between Python and dashboard.jsx.
"""

import _sources
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent.parent
CONTROL_GO = ROOT / "device" / "internal" / "client" / "control.go"
CONTROLLER = ROOT / "controller" / "em_controller.py"


def _device_record() -> str:
    """merge_device, which builds each device's record for /api/devices."""
    return _sources.def_source("merge_device")


def device_capabilities() -> list[str]:
    """
    Every capability the firmware can announce.

    Read from the whole capabilities() function rather than a single literal:
    the list is no longer fixed — "ambient_light" is appended only when the
    hardware actually has a readable sensor, because the controller advertises
    an HA entity off the back of it. A parser that only understood one literal
    would silently stop covering the conditional ones, which is the direction
    that hides a typo rather than surfacing it.
    """
    src = CONTROL_GO.read_text()
    m = re.search(r'func capabilities\(\) \[\]string \{(.*?)\n\}', src, re.S)
    assert m, "could not find func capabilities() in control.go"
    return re.findall(r'"([a-z_]+)"', m.group(1))


def test_device_announces_expected_capabilities():
    caps = device_capabilities()
    for expected in ("mic", "speaker", "leds", "led_anim", "buttons", "oww_shadow"):
        assert expected in caps, f"firmware no longer announces {expected!r}"


def test_every_capability_the_controller_checks_is_one_the_device_sends():
    """
    A controller checking for a capability string the device never sends is a
    feature that is silently off forever. This catches the typo direction that
    the device-side test cannot.
    """
    caps = set(device_capabilities())
    # The idiom scanned: `"<cap>" in (self.capabilities or [])` on Device in
    # em_controller. A typo is a feature that never turns on, with no error
    # anywhere — exactly what this test exists to catch.
    checked = set(re.findall(r'"([a-z_]+)"\s+in\s+\(self\.capabilities',
                             CONTROLLER.read_text()))
    assert checked, "no capability checks found — has the idiom changed?"
    unknown = checked - caps
    assert not unknown, (
        f"controller checks capabilities the firmware never announces: {sorted(unknown)}. "
        f"Device sends: {sorted(caps)}"
    )


def test_shadow_capability_is_surfaced_to_the_dashboard():
    """
    The dashboard must be able to tell "cannot" from "off", or it offers a
    toggle that silently does nothing on older firmware — which reads as a
    broken feature rather than an unsupported one.
    """
    assert "oww_shadow_capable" in CONTROLLER.read_text(), \
        "em_controller must expose the shadow capability as a property"
    assert "owwShadowCapable" in _device_record(), \
        "/api/devices must surface the shadow capability"
    jsx = (ROOT / "controller" / "static" / "dashboard.jsx").read_text()
    assert "owwShadowCapable" in jsx, \
        "the dashboard must gate the on-device toggle on the capability"


def test_wake_mic_capability_is_surfaced_to_the_dashboard():
    """Older firmware ignores `wakeMic` (#705), so the control must not be
    offered there as a switch that does nothing."""
    assert "wake_mic" in device_capabilities(), "firmware no longer announces wake_mic"
    assert "wake_mic_capable" in CONTROLLER.read_text()
    assert "wakeMicCapable" in _device_record()
    jsx = (ROOT / "controller" / "static" / "dashboard.jsx").read_text()
    assert "disabled={!wakeMicCapable}" in jsx


def test_volume_cue_capability_is_surfaced_to_the_dashboard():
    """Old firmware must not be offered a switch it cannot honour."""
    caps = device_capabilities()
    assert "volume_cue" in caps, "firmware no longer announces volume_cue"
    assert "volume_cue_capable" in CONTROLLER.read_text(), \
        "em_controller must expose the volume cue capability as a property"
    assert "volumeCueCapable" in _device_record(), \
        "/api/devices must surface the volume cue capability"
    jsx = (ROOT / "controller" / "static" / "dashboard.jsx").read_text()
    assert "volumeCueCapable" in jsx, \
        "the dashboard must gate the volume button sound on the capability"


def test_speaker_protection_hides_on_stock_sound():
    assert "stock_sound" in device_capabilities()
    assert "stock_sound_capable" in CONTROLLER.read_text()
    assert "stockSoundCapable" in _device_record()
    jsx = (ROOT / "controller" / "static" / "dashboard.jsx").read_text()
    assert re.search(r"\{!stockSoundCapable && \(\s*<StageAdvanced", jsx)


def test_response_level_is_gated_on_its_capability():
    assert "response_level" in device_capabilities()
    assert "response_level_capable" in CONTROLLER.read_text()
    assert "responseLevelCapable" in _device_record()
    jsx = (ROOT / "controller" / "static" / "dashboard.jsx").read_text()
    assert "disabled={!responseLevelCapable}" in jsx
    select = re.search(r"function Select\(\{(.*?)\n\}", jsx, re.S)
    assert select and "disabled" in select.group(1)


def test_sendspin_is_gated_on_its_capability_and_its_token_stays_private():
    """
    Sendspin (#89) is off on firmware without a player, so the toggle must be
    disabled with the reason there. The pairing token carries the device's
    pairing key: it is fetched on request and must never ride the stats relay
    or a dashboard event, which land in support bundles and every open tab.
    """
    assert "sendspin" in device_capabilities()
    assert "sendspinCapable" in _device_record()
    jsx = (ROOT / "controller" / "static" / "dashboard.jsx").read_text()
    assert "disabled={!sendspinCapable}" in jsx
    handler = _sources.def_source("_on_sendspin_token")
    assert "log." not in handler and "em_events.push" not in handler, \
        "the sendspin_token handler must hand the token to its waiter and nothing else"


def test_the_echos_esphome_key_stays_private():
    """The key of the Echo's own ESPHome server is made and kept on the Echo
    and asked of it for one admin request: the handler, the route and the
    firmware's answer never log it, and nothing here stores it."""
    assert "esphome_noise" in device_capabilities()
    handler = _sources.def_source("_on_esphome_key")
    assert "log." not in handler and "em_events.push" not in handler
    route = _sources.def_source("_get_esphome_key")
    assert "log." not in route and "db." not in route
    assert "@auth.require_admin\nasync def _get_esphome_key" in (
        ROOT / "controller" / "em_routes_devices.py").read_text()
    go = (ROOT / "device" / "cmd" / "esphome.go").read_text()
    body = go[go.index("func esphomeKeyReply"):]
    body = body[:body.index("\n}\n")]
    assert "log.Printf" not in body.replace('log.Printf("[esphome] no key: %v", err)', "")


def test_triggering_is_a_separate_capability_from_scoring():
    """
    Shadow shipped first, so there is firmware in the field that scores the
    wake word and reports it while having no code to act on it. Gating "on"
    behind oww_shadow alone would offer those devices a mode that leaves them
    scoring perfectly and never answering — the "I enabled it and nothing
    happened" the capability rule exists to prevent.
    """
    caps = device_capabilities()
    assert "oww_trigger" in caps, "firmware no longer announces oww_trigger"
    assert "oww_shadow" in caps, \
        "oww_trigger must not replace oww_shadow — shadow is still a mode"
    assert "oww_trigger_capable" in CONTROLLER.read_text(), \
        "em_controller must expose the trigger capability as a property"
    assert "owwTriggerCapable" in _device_record(), \
        "/api/devices must surface the trigger capability"
    assert "owwTriggerCapable" in (ROOT / "controller" / "static" / "dashboard.jsx").read_text(), \
        "the dashboard must gate the 'On device' option on the capability"




def test_the_toggle_control_actually_honours_disabled():
    """
    "Disabled WITH the reason, never a control that silently does nothing" is
    the rule every capability gate above is enforced by — and Toggle did not
    accept a `disabled` prop at all; only Slider did. So the rule could not be
    expressed on a switch, and the one call site that needed it ("Tap sends an
    event", gated on button_hold) had to fake it by neutering `value` and
    `onChange` by hand — which leaves the control looking live: full-contrast
    label, pointer cursor, a switch that animates when clicked and stores
    nothing. Any caller passing `disabled` in good faith got worse: a switch
    greyed with its reason that WROTE THE OPPOSITE VALUE on click, so the
    stored setting silently disagreed with what the control showed.

    Asserted against the component rather than the call sites, because the
    call sites already looked correct while the bug was live.
    """
    jsx = (ROOT / "controller" / "static" / "dashboard.jsx").read_text()
    m = re.search(r'function Toggle\(\{(.*?)\}\)', jsx, re.S)
    assert m, "dashboard.jsx must still define a Toggle component"
    assert "disabled" in m.group(1), \
        "Toggle must accept a `disabled` prop — every caller that passes one " \
        "is relying on it to refuse the write, not merely to grey the switch"

    body = jsx[m.end():jsx.index("\n}", m.end())]
    assert re.search(r'if\s*\(!disabled\)|disabled\s*\?\s*undefined|disabled\s*\|\|', body), \
        "Toggle's click handler must check `disabled` before calling onChange — " \
        "styling it grey while still writing the value is the bug this pins"








def test_the_api_can_tell_no_sensor_from_no_reading():
    """
    0 lux is a real reading from a covered sensor, so absence cannot be
    expressed as a value: whether the device HAS the sensor has to be a
    separate field from what it read.

    Asserted on the API rather than the dashboard deliberately. A first
    attempt put this on the Status tab, which pushed the panel past its
    height and gave the page a scrollbar — what the device panel should show
    is its own question, tracked separately. The API field stands on its own
    merits regardless of who renders it.
    """
    assert "ambientLightCapable" in _device_record(), \
        "/api/devices must report whether the device found its ALS"


def test_hardware_echo_reference_is_a_capability_and_a_runtime_state():
    """
    Two facts, and neither substitutes for the other.

    `aec_hw_ref` says the FIRMWARE knows how to take the AEC far-end
    reference from a playback loopback in the mic capture. `aecRef` on the
    stats report says whether the board turned out to have one. They must
    stay separate because the proof is only available at runtime: confirming
    a channel is a loopback requires it to be bit-exact silent at idle AND
    to carry audio while the speaker plays, and nothing has played at
    registration time. A controller that read the capability as "it is using
    a hardware reference" would disable the AEC delay slider on every device
    the moment it connected, including the ones that fall back to the
    software tap and need that slider.
    """
    caps = device_capabilities()
    assert "aec_hw_ref" in caps, "firmware no longer announces aec_hw_ref"

    ctl = CONTROLLER.read_text()
    assert "aec_hw_ref_capable" in ctl, \
        "em_controller must expose the hardware-reference capability"
    assert '"aecRef"' in ctl or "'aecRef'" in ctl, \
        "the stats allowlist must carry aecRef, or it is dropped in the relay"

    api = _device_record()
    for field in ("aecHwRefCapable", "aecRef"):
        assert field in api, f"/api/devices must surface {field}"

    jsx = (ROOT / "controller" / "static" / "dashboard.jsx").read_text()
    assert "hwEchoRef" in jsx, \
        "the dashboard must gate the AEC delay control on the live reference"


def test_the_aec_delay_control_is_gated_on_the_live_reference_not_the_capability():
    """
    The delay slider compensates write-to-ear latency for the software tap.
    On the frame-aligned hardware reference there is nothing to compensate,
    so it must be shown disabled with the reason — but only for a device
    actually running on it.

    Gating on `aecHwRefCapable` instead would disable the slider on every
    device with current firmware, including one whose board has no loopback,
    leaving a real control permanently greyed out with a reason that is not
    true of it.
    """
    jsx = (ROOT / "controller" / "static" / "dashboard.jsx").read_text()
    m = re.search(r"hwEchoRef=\{([^}]*)\}", jsx)
    assert m, "dashboard must pass hwEchoRef to the config form"
    expr = m.group(1)
    assert "aecRef" in expr, \
        "hwEchoRef must come from the runtime aecRef, not from the capability"
    assert "aecHwRefCapable" not in expr, (
        "hwEchoRef must NOT be driven by the capability: a capable device on "
        "the software tap still needs its delay slider"
    )


def test_the_controller_announces_its_own_features_and_the_device_reads_them():
    """
    Negotiation runs both ways. The device announces `capabilities` in its
    register message; the controller announces `features` on the ack, and the
    two are read identically — absent means cannot.

    This half exists because `ble_adverts` MOVED rather than being added
    (#404): a device sending the new `0x06` data frame to a controller that
    cannot read it loses every advertisement in silence, since unknown frame
    types are ignored in both directions. The strings must therefore match
    across the two languages for the same reason every other capability does.
    """
    py = CONTROLLER.read_text()
    go = CONTROL_GO.read_text()

    block = py[py.index("CONTROLLER_FEATURES = ["):]
    block = block[:block.index("]") + 1]
    announced = set(re.findall(r'"([a-z_]+)"', block))
    assert announced, "CONTROLLER_FEATURES is empty — did it move?"

    # Every announced feature must be a constant the device actually tests
    # for. A typo here is permanently silent on both sides.
    consumed = set(re.findall(r'Feature\w+\s*=\s*"([a-z_]+)"', go))
    # Read only by firmware from before EchoHome, which still exists in the
    # field (em_controller.CONTROLLER_FEATURES says why it stays announced).
    consumed |= {"ble_adverts_data"}
    assert announced <= consumed, (
        f"controller announces {announced - consumed} which the device never "
        f"looks for — the feature would never be used and nothing would say so"
    )


def test_private_listening_is_negotiated_in_both_directions():
    """docs/listening.md: the device listens privately only against a
    controller announcing listen_session, and the controller treats a device
    as private-capable only on oww_local_only. The literals in em_controller
    are what the two tests above cross-check against the Go; em_listen's
    constants must be the same strings or the pure logic reads a different
    capability from the one negotiated."""
    import sys
    sys.path.insert(0, str(ROOT / "controller"))
    import em_listen
    py = CONTROLLER.read_text()
    assert f'"{em_listen.FEATURE}"' in py[py.index("CONTROLLER_FEATURES = ["):][:200]
    assert f'"{em_listen.CAPABILITY}" in (self.capabilities' in py
    assert em_listen.CAPABILITY in device_capabilities()
    assert re.search(r'FeatureListenSession\s*=\s*"listen_session"', CONTROL_GO.read_text())


def test_identify_ring_capability_is_surfaced_to_the_dashboard():
    """Identify (#860) has no fallback: old firmware gets the button disabled."""
    caps = device_capabilities()
    assert "identify_ring" in caps, "firmware no longer announces identify_ring"
    assert "identify_ring_capable" in CONTROLLER.read_text(), \
        "em_controller must expose the identify capability as a property"
    assert "identifyRingCapable" in _device_record(), \
        "/api/devices must surface the identify capability"
    jsx = (ROOT / "controller" / "static" / "dashboard.jsx").read_text()
    assert "!device.identifyRingCapable" in jsx, \
        "the dashboard must disable Identify when the firmware lacks the capability"
    assert "ledAnimCapable" not in jsx, "Identify must not depend on led_anim"
