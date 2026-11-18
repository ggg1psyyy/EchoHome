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
    monkeypatch.setattr(inc, "_server_ip", lambda: "127.0.0.1")
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
    inc._satellites.clear()
    inc._online.clear()
    inc._locks.clear()


async def _connect(port):
    reader, writer = await asyncio.open_connection("127.0.0.1", port)
    return reader, writer


async def _next(reader, timeout=2.0):
    return await asyncio.wait_for(proto.read_frame(reader), timeout)


def _port():
    return inc._satellites[DEV].port


# ------------------------------------------------------- the backend

def test_only_incubator_selects_the_incubator():
    assert inc.backend({"voiceBackend": "incubator"}) == "incubator"
    for config in ({}, {"voiceBackend": "ha"}, {"voiceBackend": "Incubator"},
                   {"voiceBackend": None}, {"voiceBackend": 1}):
        assert inc.backend(config) == "ha", config


def test_the_default_is_home_assistant():
    import em_db
    assert em_db.DEFAULT_DEVICE_CONFIG[inc.CONFIG_KEY] == inc.BACKEND_HA


def test_the_declared_buffer_is_the_controllers_lead():
    import em_player
    assert inc.OUTPUT_BUFFER_BYTES == em_player.LEAD_S * proto.BYTE_RATE


def test_the_name_is_the_esphome_node_name():
    # The app finds the Echo's Home Assistant device (and its media player)
    # by it; em_esphome advertises this exact name in DeviceInfo.
    import _sources
    assert inc.node_name(DEV) == "echomuse-lf1234567890"
    assert 'server_name=f"echomuse-{device_id[-12:].lower()}"' in _sources.source(
        _sources.CONTROLLER / "em_esphome.py")


def test_the_port_sits_beside_voice_and_bluetooth():
    import em_db
    assert inc.PORT_OFFSET == 2 * em_db.BLE_PORT_OFFSET


# ------------------------------------------------------- lifecycle

def test_an_incubator_echo_listens_advertises_and_says_hello(world):
    async def run():
        await inc.device_connected(DEV)
        (info,) = world["registered"]
        assert info.type == proto.SERVICE_TYPE
        assert info.port == _port()
        assert info.properties == {b"mac": b"02:ec:12:34:56:78", b"protocol": b"2"}
        reader, writer = await _connect(_port())
        frame = await _next(reader)
        assert frame.message is proto.Message.HELLO
        hello = frame.json()
        assert hello["protocol"] == 2
        assert hello["name"] == inc.node_name(DEV)
        assert hello["mac"] == "02:ec:12:34:56:78"
        assert hello["model"] == "echomuse-biscuit"
        assert hello["firmware"] == "v2.28.0"
        assert hello["preroll_ms"] == inc.WAKE_LEAD_MS
        assert "wake_format" not in hello and hello["aec"] is False
        status = inc.get_status(DEV)
        assert status["listening"] and status["appConnected"]
        writer.close()
    asyncio.run(run())


def test_an_echo_on_home_assistant_gets_nothing(world):
    world["config"] = {"voiceBackend": "ha"}
    asyncio.run(inc.device_connected(DEV))
    assert inc.get_status(DEV) is None and world["registered"] == []


def test_an_unapproved_echo_gets_nothing(world):
    world["approved"] = 0
    asyncio.run(inc.device_connected(DEV))
    assert inc.get_status(DEV) is None


def test_an_offline_echo_is_not_served(world):
    # Configured, but the Echo is not connected: nothing listens for it,
    # so the app never reaches a satellite with no microphone behind it.
    asyncio.run(inc.reconcile(DEV))
    assert inc.get_status(DEV) is None


def test_switching_back_to_home_assistant_tears_it_down(world):
    async def run():
        await inc.device_connected(DEV)
        port = _port()
        world["config"] = {"voiceBackend": "ha"}
        await inc.reconcile(DEV)
        assert inc.get_status(DEV) is None
        assert len(world["unregistered"]) == 1
        with pytest.raises(OSError):
            await _connect(port)
    asyncio.run(run())


def test_a_disconnect_stops_the_listener_and_a_reconnect_restores_it(world):
    async def run():
        await inc.device_connected(DEV)
        await inc.device_disconnected(DEV)
        assert inc.get_status(DEV) is None
        await inc.device_connected(DEV)
        assert inc.get_status(DEV)["listening"]
    asyncio.run(run())


def test_a_delete_removes_everything(world):
    async def run():
        await inc.device_connected(DEV)
        await inc.device_deleted(DEV)
        assert inc.get_status(DEV) is None and DEV not in inc._online
        assert len(world["unregistered"]) == 1
    asyncio.run(run())


def test_reconcile_is_idempotent(world):
    async def run():
        await inc.device_connected(DEV)
        await asyncio.gather(inc.reconcile(DEV), inc.reconcile(DEV))
        assert len(world["registered"]) == 1
        assert inc.get_status(DEV)["listening"]
    asyncio.run(run())


def test_a_reconcile_failure_is_swallowed(world, monkeypatch):
    def boom(_i):
        raise RuntimeError("database gone")
    monkeypatch.setattr(inc.db, "get_device", boom)
    asyncio.run(inc.device_connected(DEV))  # must not raise


# ------------------------------------------------------- the connection

def test_a_second_app_is_refused_while_one_is_connected(world):
    async def run():
        await inc.device_connected(DEV)
        r1, w1 = await _connect(_port())
        assert (await _next(r1)).message is proto.Message.HELLO
        r2, w2 = await _connect(_port())
        assert await _next(r2) is None  # closed, no HELLO
        # The first is unaffected.
        w1.write(proto.encode(proto.Message.AUDIO_OUT_END))
        assert (await _next(r1)).message is proto.Message.PLAYBACK_DRAINED
        w1.close()
        w2.close()
    asyncio.run(run())


def test_the_app_can_reconnect_after_it_goes(world):
    async def run():
        await inc.device_connected(DEV)
        r1, w1 = await _connect(_port())
        await _next(r1)
        w1.close()
        await w1.wait_closed()
        for _ in range(50):
            if not inc.get_status(DEV)["appConnected"]:
                break
            await asyncio.sleep(0.01)
        r2, w2 = await _connect(_port())
        assert (await _next(r2)).message is proto.Message.HELLO
        w2.close()
    asyncio.run(run())


def test_a_pushed_model_is_refused_by_id(world):
    async def run():
        await inc.device_connected(DEV)
        r, w = await _connect(_port())
        await _next(r)
        w.write(proto.encode(proto.Message.MODEL_CLEAR)
                + proto.encode_json(proto.Message.MODEL_START, {"format": "mixednet", "id": "okay_nabu", "size": 4})
                + proto.encode(proto.Message.MODEL_DATA, b"\x00\x01\x02\x03")
                + proto.encode_json(proto.Message.MODEL_END, {"id": "okay_nabu"}))
        frame = await _next(r)
        assert frame.message is proto.Message.MODEL_RESULT
        assert frame.json()["ok"] is False and frame.json()["id"] == "okay_nabu"
        w.close()
    asyncio.run(run())


def test_unknown_and_misdirected_messages_are_ignored(world):
    async def run():
        await inc.device_connected(DEV)
        r, w = await _connect(_port())
        await _next(r)
        w.write(proto.encode(0x7E, b"future")
                + proto.encode_json(proto.Message.CALL_START, {"wake_word": "x"})  # a satellite's
                + proto.encode_json(proto.Message.EVENT, {"type": "something_new"})
                + proto.encode_json(proto.Message.MONITOR, {"active": True})
                + proto.encode(proto.Message.AUDIO_OUT, b"\x00\x00" * 100)
                + proto.encode(proto.Message.AUDIO_OUT_END))
        # Still in step: the stream end is answered.
        assert (await _next(r)).message is proto.Message.PLAYBACK_DRAINED
        w.close()
    asyncio.run(run())


def test_a_broken_frame_closes_the_connection(world):
    async def run():
        await inc.device_connected(DEV)
        r, w = await _connect(_port())
        await _next(r)
        w.write(bytes([0x12, 0x20, 0x00]))  # declares 8192 bytes
        assert await _next(r) is None
        w.close()
    asyncio.run(run())


def test_a_control_payload_that_is_not_json_closes_the_connection(world):
    async def run():
        await inc.device_connected(DEV)
        r, w = await _connect(_port())
        await _next(r)
        w.write(proto.encode(proto.Message.HANGUP, b"not json"))
        assert await _next(r) is None
        w.close()
    asyncio.run(run())


def test_stopping_closes_the_app_connection(world):
    async def run():
        await inc.device_connected(DEV)
        r, w = await _connect(_port())
        await _next(r)
        await inc.device_disconnected(DEV)
        assert await _next(r) is None
        w.close()
    asyncio.run(run())


# ------------------------------------------------- what Home Assistant sees

@pytest.fixture
def esphome_module():
    """em_esphome imported over the openwakeword stand-in, which is removed
    again afterwards: left in sys.modules it would stop the suite's
    importorskip("openwakeword") tests from skipping (_oww_stub)."""
    import _oww_stub
    _oww_stub.install()
    import em_esphome
    yield em_esphome
    _oww_stub.uninstall()


def _flags_for(device_id, caps=("mic",)):
    import em_esphome

    class Fake:
        pass

    fake = Fake()
    fake.device_id = device_id
    fake._device_has = lambda cap: cap in caps
    return em_esphome.EchoMuseSatellite._voice_assistant_flags(fake), em_esphome


def test_an_incubator_echo_offers_home_assistant_no_voice_assistant(monkeypatch, esphome_module):
    monkeypatch.setitem(inc._backends, DEV, inc.BACKEND_INCUBATOR)
    flags, _ = _flags_for(DEV)
    assert flags == 0  # no Assist satellite: the media player and the rest stay


def test_a_home_assistant_echo_keeps_its_voice_assistant(monkeypatch, esphome_module):
    monkeypatch.setitem(inc._backends, DEV, inc.BACKEND_HA)
    flags, em_esphome = _flags_for(DEV)
    assert flags == em_esphome.VOICE_ASSISTANT_FLAGS | em_esphome.VOICE_ASSISTANT_CONVERSATION_FLAG
    inc._backends.pop(DEV)
    assert _flags_for(DEV)[0] == flags  # unknown reads as Home Assistant


def test_home_assistant_is_bounced_only_on_a_real_change(monkeypatch, esphome_module):
    em_esphome = esphome_module
    bounced = []
    monkeypatch.setattr(em_esphome, "voice_backend_changed", lambda i: bounced.append(i))
    monkeypatch.setattr(inc, "_backends", {})
    inc._note_backend(DEV, inc.BACKEND_HA)          # unknown -> ha: nothing changed
    inc._note_backend(DEV, inc.BACKEND_HA)
    assert bounced == []
    inc._note_backend(DEV, inc.BACKEND_INCUBATOR)   # a real change
    inc._note_backend(DEV, inc.BACKEND_INCUBATOR)
    inc._note_backend(DEV, inc.BACKEND_HA)
    assert bounced == [DEV, DEV]


def test_the_bounce_disconnects_only_a_connected_home_assistant(monkeypatch, esphome_module):
    em_esphome = esphome_module
    gone = []

    class Sat:
        def disconnect(self):
            gone.append(True)

    class Server:
        def __init__(self, sat):
            self.sat = sat

        def get_satellite(self):
            return self.sat

    monkeypatch.setitem(em_esphome._servers, DEV, Server(Sat()))
    em_esphome.voice_backend_changed(DEV)
    assert gone == [True]
    monkeypatch.setitem(em_esphome._servers, DEV, Server(None))
    em_esphome.voice_backend_changed(DEV)  # HA not connected: nothing to bounce
    em_esphome.voice_backend_changed("NOBODY")
    assert gone == [True]


def test_backends_are_known_at_start_before_any_echo_connects(monkeypatch):
    monkeypatch.setattr(inc, "_backends", {})
    rows = [{"device_id": "A", "approved": 1}, {"device_id": "B", "approved": 1},
            {"device_id": "C", "approved": 0}]
    cfgs = {"A": {"voiceBackend": "incubator"}, "B": {}}
    monkeypatch.setattr(inc.db, "get_all_devices", lambda: rows)
    monkeypatch.setattr(inc.db, "get_effective_device_config", lambda i: cfgs[i])
    asyncio.run(inc.start("127.0.0.1"))
    assert inc._backends == {"A": "incubator", "B": "ha"}


def test_switching_backend_bounces_home_assistant_once(world, monkeypatch, esphome_module):
    em_esphome = esphome_module
    bounced = []
    monkeypatch.setattr(em_esphome, "voice_backend_changed", lambda i: bounced.append(i))
    monkeypatch.setattr(inc, "_backends", {DEV: inc.BACKEND_HA})

    async def run():
        await inc.device_connected(DEV)       # set to incubator in `world`
        await inc.reconcile(DEV)              # same again: no second bounce
        world["config"] = {"voiceBackend": "ha"}
        await inc.reconcile(DEV)
    asyncio.run(run())
    assert bounced == [DEV, DEV]


# ------------------------------------------------- the dashboard's row

def test_the_dashboard_sees_nothing_for_a_home_assistant_echo(monkeypatch):
    monkeypatch.setattr(inc, "_backends", {DEV: inc.BACKEND_HA})
    assert inc.status_view(DEV) is None


def test_the_dashboard_sees_an_incubator_echo_offline_as_not_listening(monkeypatch):
    monkeypatch.setattr(inc, "_backends", {DEV: inc.BACKEND_INCUBATOR})
    assert inc.status_view(DEV) == {"port": None, "listening": False,
                                    "appConnected": False, "inCall": False}


def test_the_dashboard_row_reads_what_the_turn_path_reads(world):
    async def run():
        await inc.device_connected(DEV)
        view = inc.status_view(DEV)
        assert view["listening"] and not view["appConnected"]
        assert "appPeer" not in view  # no address on the panel
        reader, writer = await asyncio.open_connection("127.0.0.1", inc._satellites[DEV].port)
        await asyncio.wait_for(inc.proto.read_frame(reader), 2)
        assert inc.status_view(DEV)["appConnected"] is inc.can_serve_turn(DEV) is True
        writer.close()
    asyncio.run(run())


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


def test_the_setting_on_the_echo_is_the_incubator_backend():
    assert inc.backend({"voiceBackend": "incubator_echo"}) == "incubator"
    assert inc.wants_on_echo({"voiceBackend": "incubator_echo"})
    assert not inc.wants_on_echo({"voiceBackend": "incubator"})


def test_a_capable_echo_is_asked_to_listen_and_the_controller_stands_down(on_echo):
    async def run():
        await inc.device_connected(DEV)
        assert inc.get_status(DEV) is None, "the controller's satellite is up beside the Echo's"
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


def test_without_the_capability_the_controller_serves_it(on_echo):
    on_echo["echo"].capabilities = ["mic", "speaker"]
    async def run():
        await inc.device_connected(DEV)
        assert inc.get_status(DEV)["listening"], "no satellite for an Echo whose firmware cannot be one"
        assert on_echo["echo"].sent == []
    asyncio.run(run())


def test_switching_to_the_controllers_satellite_stops_the_echos(on_echo):
    async def run():
        await inc.device_connected(DEV)
        echo = on_echo["echo"]
        await _state(echo, listening=True, port=6055)
        on_echo["config"] = {"voiceBackend": "incubator"}
        await inc.reconcile(DEV)
        assert echo.sent[-1]["enabled"] is False
        assert len(on_echo["unregistered"]) == 1
        assert inc.get_status(DEV)["listening"]
        # One satellite to the app throughout: the same service name.
        assert on_echo["registered"][-1].name == on_echo["registered"][0].name
    asyncio.run(run())


def test_switching_to_home_assistant_stops_the_echos(on_echo):
    async def run():
        await inc.device_connected(DEV)
        await _state(on_echo["echo"], listening=True, port=6055)
        on_echo["config"] = {"voiceBackend": "ha"}
        await inc.reconcile(DEV)
        assert on_echo["echo"].sent[-1]["enabled"] is False
        assert inc.status_view(DEV) is None and DEV not in inc._on_echo
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
    on_echo["config"] = {"voiceBackend": "ha"}
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


def test_switching_to_the_echos_satellite_closes_the_controllers_first(on_echo):
    on_echo["config"] = {"voiceBackend": "incubator"}
    async def run():
        await inc.device_connected(DEV)
        port = _port()
        assert inc.get_status(DEV)["listening"]
        on_echo["config"] = {"voiceBackend": "incubator_echo"}
        await inc.reconcile(DEV)
        # The controller's satellite is gone (listener closed, record
        # withdrawn) before the Echo is asked: never two under one MAC.
        assert inc.get_status(DEV) is None
        assert len(on_echo["unregistered"]) == 1
        with pytest.raises(OSError):
            await _connect(port)
        assert on_echo["echo"].sent[-1]["enabled"] is True
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


def test_a_voice_press_goes_to_the_echos_own_satellite(on_echo):
    echo = _calls_echo(on_echo)
    async def run():
        await inc.device_connected(DEV)
        await _state(echo, listening=True, port=6055, appConnected=True)
        echo.sent.clear()
        assert await inc.on_button_press(echo)
        assert echo.sent == [{"type": "incubator_press"}]
    asyncio.run(run())


def test_with_no_app_connected_a_press_shows_the_error_ring(on_echo):
    echo = _calls_echo(on_echo)
    async def run():
        await inc.device_connected(DEV)
        await _state(echo, listening=True, port=6055, appConnected=False)
        echo.sent.clear()
        assert await inc.on_button_press(echo), "the controller would start its own turn"
        assert echo.sent == [] and echo.anims == [{"pattern": "pulse"}]
    asyncio.run(run())


def test_presence_only_firmware_keeps_the_controllers_button(on_echo):
    echo = on_echo["echo"]  # incubator_satellite without incubator_calls
    async def run():
        await inc.device_connected(DEV)
        await _state(echo, listening=True, port=6055, appConnected=True)
        echo.sent.clear()
        assert not await inc.on_button_press(echo)
        assert echo.sent == []
    asyncio.run(run())


def test_an_echo_not_its_own_satellite_keeps_the_controllers_button(on_echo):
    echo = _calls_echo(on_echo)
    async def run():
        assert not await inc.on_button_press(echo)  # never asked to listen
        assert echo.sent == []
    asyncio.run(run())


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


def test_the_button_reaches_the_echo_only_as_a_voice_press():
    """Holds, tap events and a tap with no microphone are the controller's as
    for any Echo; what would have been a turn (or its cancel) goes to the
    Echo, before either is acted on here, and so does a muted tap, for the
    Echo to stop a ringing timer (it does nothing else muted)."""
    import _sources
    src = _sources.code_only(_sources.def_source("handle_button_event"))
    at = {k: src.index(k) for k in ("em_button.HOLD", "em_button.TAP_EVENT", "em_button.NO_MIC",
                                    "em_button.BLOCKED", "em_incubator.on_button_press",
                                    "em_button.CANCEL", "can_serve_turn")}
    assert at["em_button.HOLD"] < at["em_button.TAP_EVENT"] < at["em_button.NO_MIC"] \
        < at["em_button.BLOCKED"] < at["em_incubator.on_button_press"] \
        < at["em_button.CANCEL"] < at["can_serve_turn"]
    blocked = src[at["em_button.BLOCKED"]:src.index("em_button.CANCEL")]
    assert "em_incubator.on_button_press(device, muted=True)" in blocked


def test_a_muted_tap_reaches_the_echo_only_to_stop_a_timer(on_echo):
    """Passed on (the Echo stops a ringing timer and otherwise does nothing,
    its mute being its own); with no app connected, silent: no error ring for
    a muted tap."""
    echo = _calls_echo(on_echo)
    async def run():
        await inc.device_connected(DEV)
        await _state(echo, listening=True, port=6055, appConnected=True)
        echo.sent.clear()
        assert await inc.on_button_press(echo, muted=True) is True
        assert echo.sent == [{"type": "incubator_press"}]
        await _state(echo, listening=True, port=6055, appConnected=False)
        echo.sent.clear()
        echo.anims = []
        assert await inc.on_button_press(echo, muted=True) is True
        assert echo.sent == [] and echo.anims == []
    asyncio.run(run())


def test_an_echo_that_shows_its_satellite_on_the_ring_gets_the_whole_scene():
    import em_scenes
    import em_timers
    scene = em_scenes.resolve({"ledScene": "custom", "ledThinkColor": "#112233"})
    plain = em_scenes.ring_push(scene, ["led_anim", "incubator_satellite"])
    assert plain == {"type": "config", "listeningAnim": scene["listening_anim"]}
    msg = em_scenes.ring_push(scene, ["led_anim", em_scenes.RING_CAPABILITY])
    ring = msg["incubatorRing"]
    assert msg["listeningAnim"] == scene["listening_anim"]
    assert ring == {"spin": scene["spin_anim"], "meter": scene["meter_anim"],
                    "done": scene["done_anim"], "error": scene["error_anim"],
                    "timer": em_timers.TIMER_ANIM}
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


def test_the_app_may_listen_live_only_where_the_switch_and_the_firmware_allow():
    """incubatorMonitor true AND incubator_monitor announced: anything else is
    off, and off is what the Echo is told (it then offers no monitoring)."""
    import em_state
    class Dev:
        capabilities = ["incubator_satellite", inc.MONITOR_CAPABILITY]
    em_state.devices["X1"] = Dev()
    try:
        assert inc.monitor_allowed({inc.MONITOR_KEY: True}, "X1") is True
        for cfg in ({}, {inc.MONITOR_KEY: False}, {inc.MONITOR_KEY: "true"}, {inc.MONITOR_KEY: 1}):
            assert inc.monitor_allowed(cfg, "X1") is False, cfg
        Dev.capabilities = ["incubator_satellite"]
        assert inc.monitor_allowed({inc.MONITOR_KEY: True}, "X1") is False
    finally:
        em_state.devices.pop("X1", None)
    rec = inc.EchoSatellite("X1", "02:ec:12:34:56:78")
    rec.monitor = True
    assert rec.message(True)["monitor"] is True
    assert rec.message(False)["monitor"] is False  # never offered to a satellite told to stop


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


def test_the_monitor_switch_reaches_the_echo_through_reconcile(on_echo):
    """Turned on, the next incubator_satellite says monitor (the Echo then
    restarts its satellite and offers it); turned off, it says not; and never
    to firmware that does not announce incubator_monitor."""
    echo = on_echo["echo"]
    async def run():
        echo.capabilities = ["mic", "speaker", "incubator_satellite", inc.MONITOR_CAPABILITY]
        on_echo["config"][inc.MONITOR_KEY] = True
        await inc.device_connected(DEV)
        assert echo.sent[-1]["monitor"] is True
        on_echo["config"][inc.MONITOR_KEY] = False
        await inc.reconcile(DEV)
        assert echo.sent[-1]["monitor"] is False
        on_echo["config"][inc.MONITOR_KEY] = True
        echo.capabilities = ["mic", "speaker", "incubator_satellite"]
        await inc.reconcile(DEV)
        assert echo.sent[-1]["monitor"] is False
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


def test_the_standalone_switch_reaches_the_echo_through_reconcile(on_echo):
    echo = on_echo["echo"]
    async def run():
        echo.capabilities = ["mic", "speaker", "incubator_satellite", inc.STANDALONE_CAPABILITY]
        on_echo["config"][inc.STANDALONE_KEY] = True
        await inc.device_connected(DEV)
        assert echo.sent[-1]["standalone"] is True and echo.sent[-1]["monitor"] is False
        on_echo["config"][inc.STANDALONE_KEY] = False
        await inc.reconcile(DEV)
        assert echo.sent[-1]["standalone"] is False
        on_echo["config"][inc.STANDALONE_KEY] = True
        echo.capabilities = ["mic", "speaker", "incubator_satellite"]
        await inc.reconcile(DEV)
        assert echo.sent[-1]["standalone"] is False  # firmware that cannot
    asyncio.run(run())


def test_a_hold_is_a_hold_on_the_echo_as_here():
    """Standalone, the Echo tells a tap from a hold itself: by the same
    threshold as the controller's (em_esphome.BUTTON_HOLD_MS)."""
    import re
    import em_esphome
    from pathlib import Path
    go = (Path(__file__).resolve().parents[2] / "device" / "cmd" / "incubator.go").read_text()
    assert int(re.search(r"const incubatorHoldMs = (\d+)", go).group(1)) == em_esphome.BUTTON_HOLD_MS


def test_a_standalone_echo_that_goes_offline_stays_announced(on_echo):
    """Standalone, its satellite keeps answering without the controller link;
    where mDNS does not cross VLANs the app finds it only by the controller's
    record, so that stays while the Echo is offline. Not standalone, it goes."""
    echo = on_echo["echo"]
    echo.capabilities = ["mic", "speaker", "incubator_satellite", inc.STANDALONE_CAPABILITY]
    async def run():
        on_echo["config"][inc.STANDALONE_KEY] = True
        await inc.device_connected(DEV)
        await _state(echo, listening=True, port=6055, appConnected=True)
        assert len(on_echo["registered"]) == 1
        await inc.device_disconnected(DEV)
        assert on_echo["unregistered"] == [] and DEV in inc._on_echo
        on_echo["config"][inc.STANDALONE_KEY] = False
        await inc.reconcile(DEV)
        assert len(on_echo["unregistered"]) == 1 and DEV not in inc._on_echo
    asyncio.run(run())


def test_the_echos_own_esphome_server_keeps_the_controllers_entities():
    """The Echo's server (device/cmd/esphome.go) uses the controller's entity
    keys, object ids and button event types, so HA's entities survive the day
    it takes over; and its message ids (internal/esphome/api/msgids.go,
    generated) are the ones this controller decodes."""
    import re
    from pathlib import Path
    import em_esphome
    from esphome import message_registry as reg
    dev = Path(__file__).resolve().parents[2] / "device"
    go = (dev / "cmd" / "esphome.go").read_text()
    consts = dict(re.findall(r"esphome(\w+Key)\s*=\s*(\d+)", go))
    assert int(consts["EventKey"]) == em_esphome.EVENT_KEY
    assert int(consts["AmbientKey"]) == em_esphome.AMBIENT_LUX_KEY
    assert int(consts["MicMutedKey"]) == em_esphome.MIC_MUTED_KEY
    assert int(consts["CPUTempKey"]) == em_esphome.CPU_TEMP_KEY
    assert int(consts["MaxTempKey"]) == em_esphome.MAX_TEMP_KEY
    for object_id in ("action_button", "ambient_light", "cpu_temperature",
                      "hottest_zone_temperature", "mic_muted"):
        assert f'ObjectId: "{object_id}"' in go or f'temp("{object_id}"' in go, object_id
    types = re.search(r"esphomeButtonEventTypes = \[\]string\{([^}]*)\}", go).group(1)
    assert [t.strip().strip('"') for t in types.split(",")] == em_esphome.BUTTON_EVENT_TYPES
    ids = dict((n, int(i)) for n, i in re.findall(r'"(\w+)":\s*(\d+),',
                                                  (dev / "internal/esphome/api/msgids.go").read_text()))
    ours = {cls.__name__: t for t, cls in reg.MESSAGE_TYPE_TO_CLASS.items()}
    # The Echo's API is newer (aioesphomeapi 46.25.0, the controller's 45.3.1):
    # every message this controller knows has the same id there, and none of
    # the controller's is missing.
    assert {n: ids.get(n) for n in ours} == ours
