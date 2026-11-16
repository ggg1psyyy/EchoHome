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
