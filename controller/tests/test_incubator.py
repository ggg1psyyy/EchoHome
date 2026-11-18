"""
em_incubator: one Voice Incubator satellite per Echo set to the Incubator,
over a real socket, with the app played by a client written from
PROTOCOL.md (connect, read HELLO, send frames).

The database and zeroconf are stand-ins; the listener, the framing and the
connection handling are the real ones.
"""

import asyncio

import pytest

import em_incubator as inc
import em_incubator_proto as proto

DEV = "G090LF1234567890"


@pytest.fixture
def world(monkeypatch):
    state = {
        "config": {"voiceBackend": "incubator"},
        "approved": 1,
        "registered": [],
        "unregistered": [],
    }

    class FakeZeroconf:
        async def async_register_service(self, info, allow_name_change=False):
            state["registered"].append(info)

        async def async_unregister_service(self, info):
            state["unregistered"].append(info)

    monkeypatch.setattr(inc, "_azc", lambda: FakeZeroconf())
    monkeypatch.setattr(inc, "_backends", {})  # no order between tests
    monkeypatch.setattr(inc, "_host", "127.0.0.1")
    monkeypatch.setattr(inc, "PORT_OFFSET", 0)
    monkeypatch.setattr(inc.db, "get_device", lambda _i: (
        {"approved": state["approved"], "firmware_ver": "v2.28.0", "label": "Kitchen"}))
    monkeypatch.setattr(inc.db, "get_effective_device_config", lambda _i: dict(state["config"]))
    monkeypatch.setattr(inc.db, "get_esphome_port", lambda _i: 0)  # ephemeral
    monkeypatch.setattr(inc.db, "get_esphome_mac", lambda _i: "02:EC:12:34:56:78")
    monkeypatch.setitem(inc.em_state.boards, DEV, "biscuit")
    yield state
    asyncio.run(inc.stop_all())
    inc._online.clear()
    inc._locks.clear()


# ------------------------------------------------------- the backend

def test_every_echo_is_its_own_satellite_whatever_the_setting():
    """EchoHome has one voice path. The stored value is "ha" on every
    install that had the old default, and must not stop a satellite."""
    for config in ({}, {"voiceBackend": "ha"}, {"voiceBackend": "incubator"},
                   {"voiceBackend": "incubator_echo"}, {"voiceBackend": None}):
        assert inc.backend(config) == inc.BACKEND_INCUBATOR, config
        assert inc.wants_on_echo(config), config


def test_the_port_sits_beside_voice_and_bluetooth():
    import em_db
    assert inc.PORT_OFFSET == 2 * em_db.BLE_PORT_OFFSET


# ------------------------------------------------------- lifecycle


def test_a_reconcile_failure_is_swallowed(world, monkeypatch):
    def boom(_i):
        raise RuntimeError("database gone")
    monkeypatch.setattr(inc.db, "get_device", boom)
    asyncio.run(inc.device_connected(DEV))  # must not raise


# ------------------------------------------------------- the connection


# ------------------------------------------------- what Home Assistant sees


def test_backends_are_known_at_start_before_any_echo_connects(monkeypatch):
    monkeypatch.setattr(inc, "_backends", {})
    rows = [{"device_id": "A", "approved": 1}, {"device_id": "B", "approved": 1},
            {"device_id": "C", "approved": 0}]
    cfgs = {"A": {"voiceBackend": "incubator"}, "B": {}}
    monkeypatch.setattr(inc.db, "get_all_devices", lambda: rows)
    monkeypatch.setattr(inc.db, "get_effective_device_config", lambda i: cfgs[i])
    asyncio.run(inc.start("127.0.0.1"))
    assert inc._backends == {"A": "incubator", "B": "incubator"}


# ------------------------------------------------- the dashboard's row

def test_the_dashboard_sees_nothing_for_a_home_assistant_echo(monkeypatch):
    monkeypatch.setattr(inc, "_backends", {DEV: inc.BACKEND_HA})
    assert inc.status_view(DEV) is None


def test_the_dashboard_sees_an_incubator_echo_offline_as_not_listening(monkeypatch):
    monkeypatch.setattr(inc, "_backends", {DEV: inc.BACKEND_INCUBATOR})
    assert inc.status_view(DEV) == {"port": None, "listening": False,
                                    "appConnected": False, "inCall": False}


def test_the_device_record_and_the_panel_carry_it():
    import _sources
    view = _sources.code_only(_sources.def_source("merge_device"))
    assert "'voiceIncubator': em_incubator.status_view(device_id)" in view
    jsx = (_sources.CONTROLLER / "static" / "dashboard.jsx").read_text()
    assert "const vi = device.voiceIncubator;" in jsx
    assert "Voice Incubator connected" in jsx and "Waiting for the Voice Incubator app" in jsx


# ------------------------------------------------------- the Echo's own satellite

class FakeEcho:
    """A connected Echo, as em_state.devices holds it: what the controller
    sends it is kept."""

    def __init__(self, capabilities, ip="192.0.2.10"):
        self.device_id = DEV
        self.ip = ip
        self.capabilities = capabilities
        self.sent = []

    async def send_control(self, msg):
        self.sent.append(msg)


@pytest.fixture
def on_echo(world, monkeypatch):
    world["config"] = {"voiceBackend": "incubator_echo"}
    echo = FakeEcho(["mic", "speaker", "incubator_satellite"])
    monkeypatch.setitem(inc.em_state.devices, DEV, echo)
    pushed = []

    async def push(ev):
        pushed.append(ev)
    monkeypatch.setattr(inc.em_events, "push", push)
    world["echo"], world["pushed"] = echo, pushed
    yield world
    inc._on_echo.clear()


def _state(echo, **st):
    return inc.on_incubator_state(echo, {"type": "incubator_state", "state": st})


def test_a_capable_echo_is_asked_to_listen_and_the_controller_stands_down(on_echo):
    async def run():
        await inc.device_connected(DEV)
        (msg,) = on_echo["echo"].sent
        assert msg == {"type": "incubator_satellite", "enabled": True, "name": inc.node_name(DEV),
                       "mac": "02:ec:12:34:56:78", "port": inc.ECHO_PORT, "monitor": False,
                       "standalone": False}
        # Not advertised until the Echo says it listens.
        assert on_echo["registered"] == []
        view = inc.status_view(DEV)
        assert view["onEcho"] and not view["listening"]
    asyncio.run(run())


def test_the_echo_is_advertised_at_its_own_address_while_it_listens(on_echo):
    async def run():
        await inc.device_connected(DEV)
        echo = on_echo["echo"]
        await _state(echo, listening=True, port=6055, appConnected=False)
        (info,) = on_echo["registered"]
        assert info.type == proto.SERVICE_TYPE
        assert info.name == f"{inc.node_name(DEV)}.{proto.SERVICE_TYPE}"
        assert info.parsed_addresses() == ["192.0.2.10"] and info.port == 6055
        assert info.properties == {b"mac": b"02:ec:12:34:56:78", b"protocol": b"2"}
        await _state(echo, listening=True, port=6055, appConnected=True)
        assert len(on_echo["registered"]) == 1, "advertised twice"
        assert inc.status_view(DEV)["appConnected"]
        await _state(echo, listening=False, port=6055)
        assert len(on_echo["unregistered"]) == 1
        assert on_echo["pushed"][-1]["state"]["voiceIncubator"]["listening"] is False
    asyncio.run(run())


def test_a_stored_home_assistant_backend_still_gets_the_echos_satellite(on_echo):
    on_echo["config"] = {"voiceBackend": "ha"}
    async def run():
        await inc.device_connected(DEV)
        assert on_echo["echo"].sent[-1]["enabled"] is True
        assert inc.status_view(DEV)["onEcho"]
    asyncio.run(run())


def test_firmware_that_cannot_be_the_satellite_is_told_to_stop(on_echo):
    """Older firmware (0SE7's v2.19.0): nothing here can serve it now."""
    on_echo["echo"].capabilities = ["mic", "speaker"]
    async def run():
        await inc.device_connected(DEV)
        assert DEV not in inc._on_echo and on_echo["registered"] == []
    asyncio.run(run())


def test_an_unapproved_echo_gets_nothing(on_echo):
    on_echo["approved"] = 0
    async def run():
        await inc.device_connected(DEV)
        assert on_echo["echo"].sent == [] and DEV not in inc._on_echo
    asyncio.run(run())


def test_a_disconnect_withdraws_the_record_and_sends_nothing(on_echo):
    async def run():
        await inc.device_connected(DEV)
        await _state(on_echo["echo"], listening=True, port=6055)
        sent = len(on_echo["echo"].sent)
        await inc.device_disconnected(DEV)
        assert len(on_echo["unregistered"]) == 1
        assert len(on_echo["echo"].sent) == sent, "a message to an Echo that is gone"
    asyncio.run(run())


def test_every_reconcile_resends_and_advertises_once(on_echo):
    async def run():
        await inc.device_connected(DEV)
        await _state(on_echo["echo"], listening=True, port=6055)
        await asyncio.gather(inc.reconcile(DEV), inc.reconcile(DEV))
        assert [m["enabled"] for m in on_echo["echo"].sent] == [True, True, True]
        assert len(on_echo["registered"]) == 1
    asyncio.run(run())


def test_a_new_address_is_advertised_anew(on_echo):
    async def run():
        await inc.device_connected(DEV)
        await _state(on_echo["echo"], listening=True, port=6055)
        on_echo["echo"].ip = "192.0.2.77"
        await inc.reconcile(DEV)
        assert len(on_echo["unregistered"]) == 1
        await _state(on_echo["echo"], listening=True, port=6055)
        assert on_echo["registered"][-1].parsed_addresses() == ["192.0.2.77"]
    asyncio.run(run())


def test_a_report_nobody_asked_for_is_ignored(on_echo):
    on_echo["approved"] = 0
    async def run():
        await inc.device_connected(DEV)
        await _state(on_echo["echo"], listening=True, port=6055)
        assert on_echo["registered"] == [] and DEV not in inc._on_echo
    asyncio.run(run())


def test_a_malformed_report_is_cleaned(on_echo):
    async def run():
        await inc.device_connected(DEV)
        echo = on_echo["echo"]
        await inc.on_incubator_state(echo, {"type": "incubator_state", "state": "nonsense"})
        await _state(echo, listening="yes", port="6055", appConnected=1, error="x" * 5000)
        rec = inc._on_echo[DEV]
        assert rec.port == inc.ECHO_PORT and len(rec.state["error"]) == 200
        await _state(echo, listening=True, port=99999)
        assert rec.port == inc.ECHO_PORT and on_echo["registered"][-1].port == inc.ECHO_PORT
    asyncio.run(run())


def test_an_address_mdns_cannot_carry_is_logged_not_raised(on_echo):
    on_echo["echo"].ip = "2001:db8::10"
    async def run():
        await inc.device_connected(DEV)
        await _state(on_echo["echo"], listening=True, port=6055)  # must not raise
        assert on_echo["registered"] == []
    asyncio.run(run())


# ------------------------------------------------- button calls on the Echo

def _calls_echo(on_echo):
    echo = on_echo["echo"]
    echo.capabilities = ["mic", "speaker", "incubator_satellite", "incubator_calls"]
    echo.led_anim_capable = True
    echo.led_scene = {"error_anim": {"pattern": "pulse"}}
    echo.anims = []

    async def send_led_anim(anim):
        echo.anims.append(anim)
    echo.send_led_anim = send_led_anim
    return echo


def test_a_call_on_the_echo_shows_in_the_panel(on_echo):
    echo = _calls_echo(on_echo)
    async def run():
        await inc.device_connected(DEV)
        await _state(echo, listening=True, port=6055, appConnected=True, inCall=True)
        assert inc.status_view(DEV)["inCall"] is True
        await _state(echo, listening=True, port=6055, appConnected=True)
        assert inc.status_view(DEV)["inCall"] is False
    asyncio.run(run())


def test_the_wake_models_the_app_pushed_the_echo_show_in_the_panel(on_echo):
    """What the Echo reports, never inferred; a value that is not a count is
    none."""
    echo = _calls_echo(on_echo)
    async def run():
        await inc.device_connected(DEV)
        await _state(echo, listening=True, port=6055, appConnected=True, wakeModels=2)
        assert inc.status_view(DEV)["wakeModels"] == 2
        for bad in (-1, "2", True, 1.5, None):
            await _state(echo, listening=True, port=6055, appConnected=True, wakeModels=bad)
            assert inc.status_view(DEV)["wakeModels"] == 0, bad
    asyncio.run(run())


def test_an_echo_that_shows_its_satellite_on_the_ring_gets_the_whole_scene():
    import em_scenes
    import em_scenes
    scene = em_scenes.resolve({"ledScene": "custom", "ledThinkColor": "#112233"})
    plain = em_scenes.ring_push(scene, ["led_anim", "incubator_satellite"])
    assert plain == {"type": "config", "listeningAnim": scene["listening_anim"]}
    msg = em_scenes.ring_push(scene, ["led_anim", em_scenes.RING_CAPABILITY])
    ring = msg["incubatorRing"]
    assert msg["listeningAnim"] == scene["listening_anim"]
    assert ring == {"spin": scene["spin_anim"], "meter": scene["meter_anim"],
                    "done": scene["done_anim"], "error": scene["error_anim"],
                    "timer": em_scenes.TIMER_ANIM}
    assert ring["spin"]["colors"][0] == [0x11, 0x22, 0x33]  # the scene's, not a default
    assert em_scenes.RING_CAPABILITY == "incubator_ring"  # device/internal/client/control.go


def test_both_scene_pushes_use_the_one_builder():
    """At registration and on a live scene change: built apart, they drift
    (the #263 bug was the second one missing)."""
    import _sources
    code = {p.stem: _sources.code_only(_sources.source(p)) for p in _sources.MODULES}
    assert "em_scenes.ring_push(" in code["em_controller"]
    assert "em_scenes.ring_push(" in code["em_device_config"]
    built = [m for m, c in code.items() if "listening_anim\"]" in c and m != "em_scenes"
             and "listeningAnim" in c]
    assert built == [], f"a listeningAnim push built outside em_scenes.ring_push: {built}"


def test_the_privacy_line_says_the_echo_streams_while_it_reports_monitoring(on_echo):
    import em_device_view
    echo = _calls_echo(on_echo)
    echo.listen_view = type("V", (), {"state": "local", "streams": False, "reason": ""})()
    async def run():
        await inc.device_connected(DEV)
        await _state(echo, listening=True, port=6055, appConnected=True)
        assert em_device_view._listen_json(echo)["streams"] is False
        await _state(echo, listening=True, port=6055, appConnected=True, monitored=True)
        view = em_device_view._listen_json(echo)
        assert view["state"] == "monitored" and view["streams"] is True
        assert inc.status_view(DEV)["monitored"] is True
        await _state(echo, listening=True, port=6055, appConnected=True, monitored="yes")
        assert em_device_view._listen_json(echo)["streams"] is False  # only true is true
    asyncio.run(run())


def test_an_echo_that_announces_itself_is_still_announced_for(on_echo):
    """Firmware announcing incubator_mdns puts its own satellite on the
    network, but only on its own: where mDNS does not cross VLANs (the Echo
    on one, the app on another; 00LQ, 2026-11-18) the app hears only the
    controller's record, so the controller keeps announcing it."""
    echo = on_echo["echo"]
    echo.capabilities = ["mic", "speaker", "incubator_satellite", inc.MDNS_CAPABILITY]
    async def run():
        await inc.device_connected(DEV)
        await _state(echo, listening=True, port=6055, appConnected=False)
        (info,) = on_echo["registered"]
        assert info.parsed_addresses() == ["192.0.2.10"] and info.port == 6055
    asyncio.run(run())


def test_standalone_follows_the_firmware_not_the_switch(on_echo):
    """Since EchoHome the Echo keeps its satellite whatever the switch says,
    so standalone is what the firmware can do."""
    echo = on_echo["echo"]
    async def run():
        echo.capabilities = ["mic", "speaker", "incubator_satellite", inc.STANDALONE_CAPABILITY]
        on_echo["config"]["incubatorStandalone"] = True
        await inc.device_connected(DEV)
        assert echo.sent[-1]["standalone"] is True and echo.sent[-1]["monitor"] is False
        on_echo["config"]["incubatorStandalone"] = False
        await inc.reconcile(DEV)
        assert echo.sent[-1]["standalone"] is True
        on_echo["config"]["incubatorStandalone"] = True
        echo.capabilities = ["mic", "speaker", "incubator_satellite"]
        await inc.reconcile(DEV)
        assert echo.sent[-1]["standalone"] is False  # firmware that cannot
    asyncio.run(run())


def test_a_standalone_echo_that_goes_offline_stays_announced(on_echo):
    """Standalone, its satellite keeps answering without the controller link;
    where mDNS does not cross VLANs the app finds it only by the controller's
    record, so that stays while the Echo is offline. Since EchoHome the
    firmware is always standalone where it can be, so the switch no longer
    decides: the record stays either way."""
    echo = on_echo["echo"]
    echo.capabilities = ["mic", "speaker", "incubator_satellite", inc.STANDALONE_CAPABILITY]
    async def run():
        on_echo["config"]["incubatorStandalone"] = True
        await inc.device_connected(DEV)
        await _state(echo, listening=True, port=6055, appConnected=True)
        assert len(on_echo["registered"]) == 1
        await inc.device_disconnected(DEV)
        assert on_echo["unregistered"] == [] and DEV in inc._on_echo
        on_echo["config"]["incubatorStandalone"] = False
        await inc.reconcile(DEV)
        assert on_echo["unregistered"] == [] and DEV in inc._on_echo
    asyncio.run(run())




def test_the_controller_never_turns_live_listening_on(on_echo):
    """The Echo owns it (Home Assistant's switch on its own ESPHome device);
    the message's false is only a first value for an Echo that has none."""
    on_echo["echo"].capabilities = ["mic", "speaker", "incubator_satellite", inc.MONITOR_CAPABILITY]
    on_echo["config"]["incubatorMonitor"] = True  # a value stored before EchoHome
    async def run():
        await inc.device_connected(DEV)
        assert on_echo["echo"].sent[-1]["monitor"] is False
        assert "monitorAllowed" not in inc.status_view(DEV)
    asyncio.run(run())


def test_a_deleted_echo_loses_its_record_and_its_announcement(on_echo):
    async def run():
        await inc.device_connected(DEV)
        await _state(on_echo["echo"], listening=True, port=6055)
        sent = len(on_echo["echo"].sent)
        await inc.device_deleted(DEV)
        assert DEV not in inc._on_echo and len(on_echo["unregistered"]) == 1
        assert len(on_echo["echo"].sent) == sent, "nothing is sent to a deleted Echo"
        assert inc.backend_of(DEV) == inc.BACKEND_HA
    asyncio.run(run())
