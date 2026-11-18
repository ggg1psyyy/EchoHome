"""em_esphome_mdns: the Echo's ESPHome record, re-announced on this network.
Its limits are the specs' (RFC 1035 labels, RFC 6763 TXT strings) and Home
Assistant's (a discovery without "mac" aborts)."""

import asyncio
import socket

import em_esphome_mdns as M
import em_state

MSG = {"type": "esphome_mdns", "name": "echohome-11743300lq", "port": 6053,
       "txt": ["friendly_name=Kitchen", "mac=02ec1a2b3c4d", "board=biscuit",
               "network=wifi", "api_encryption=Noise_NNpsk0_25519_ChaChaPoly_SHA256"]}


def test_a_record_from_the_echo_is_taken_as_it_is():
    name, port, props = M.parse(MSG)
    assert (name, port) == ("echohome-11743300lq", 6053)
    assert props["mac"] == "02ec1a2b3c4d" and props["friendly_name"] == "Kitchen"


def test_the_name_must_be_a_dns_label():
    for bad in ("", "-echo", "echo-", "Echo", "echo.home", "e" * 64, "echo home", 5, None):
        assert M.parse({**MSG, "name": bad}) is None, bad
    assert M.parse({**MSG, "name": "e" * 63}) is not None
    assert M.parse({**MSG, "name": "a"}) is not None


def test_the_port_must_be_a_port():
    for bad in (0, 65536, -1, "6053", True, None, 6053.0):
        assert M.parse({**MSG, "port": bad}) is None, bad
    assert M.parse({**MSG, "port": 1}) and M.parse({**MSG, "port": 65535})


def test_txt_strings_keep_rfc_6763s_limits():
    base = MSG["txt"]
    # 255 bytes is the most one string may be (§6.1); counted in bytes, not characters.
    assert M.parse({**MSG, "txt": base + ["project_name=" + "x" * (255 - 13)]}) is not None
    assert M.parse({**MSG, "txt": base + ["project_name=" + "x" * (256 - 13)]}) is None
    assert M.parse({**MSG, "txt": base + ["project_name=" + "é" * 122]}) is None
    for bad in ("noequals", "=value", "k\x01=v", "kéy=v", "mac=02ec00000000"):
        assert M.parse({**MSG, "txt": base + [bad]}) is None, bad  # the last: a key twice
    assert M.parse({**MSG, "txt": "mac=1"}) is None
    assert M.parse({**MSG, "txt": base + ["x%d=1" % i for i in range(M.MAX_TXT)]}) is None
    assert M.parse({**MSG, "txt": base + [5]}) is None


def test_without_a_mac_there_is_nothing_home_assistant_could_take():
    assert M.parse({**MSG, "txt": [r for r in MSG["txt"] if not r.startswith("mac=")]}) is None
    assert M.parse({**MSG, "txt": ["mac="]}) is None


class _Zc:
    def __init__(self):
        self.registered, self.unregistered = [], []

    async def async_register_service(self, info, allow_name_change=False):
        self.registered.append(info)

    async def async_unregister_service(self, info):
        self.unregistered.append(info)


class _Dev:
    device_id = "dev1"
    ip = "10.0.20.5"


def test_it_is_announced_at_the_echos_address_and_withdrawn_when_it_goes(monkeypatch):
    zc = _Zc()
    monkeypatch.setattr(em_state, "zeroconf", zc, raising=False)
    M._announced.clear()

    async def run():
        await M.on_esphome_mdns(_Dev(), MSG)
        assert len(zc.registered) == 1
        info = zc.registered[0]
        assert info.type == "_esphomelib._tcp.local."
        assert info.name == "echohome-11743300lq._esphomelib._tcp.local."
        assert info.server == "echohome-11743300lq.local."  # HA's expected device name
        assert info.port == 6053
        assert info.addresses == [socket.inet_aton("10.0.20.5")]
        assert info.properties[b"mac"] == b"02ec1a2b3c4d"
        # The same record again changes nothing; a rename re-announces.
        await M.on_esphome_mdns(_Dev(), MSG)
        assert len(zc.registered) == 1
        renamed = {**MSG, "txt": ["friendly_name=Lounge"] + MSG["txt"][1:]}
        await M.on_esphome_mdns(_Dev(), renamed)
        assert len(zc.registered) == 2 and len(zc.unregistered) == 1
        await M.device_disconnected("dev1")
        assert len(zc.unregistered) == 2 and "dev1" not in M._announced
        await M.on_esphome_mdns(_Dev(), MSG)
        await M.device_deleted("dev1")
        assert len(zc.unregistered) == 3
        # A record that is not one is refused and leaves nothing behind.
        await M.on_esphome_mdns(_Dev(), {**MSG, "port": 0})
        assert "dev1" not in M._announced
    asyncio.run(run())


def test_an_echo_with_no_ipv4_address_is_not_announced(monkeypatch):
    zc = _Zc()
    monkeypatch.setattr(em_state, "zeroconf", zc, raising=False)
    M._announced.clear()

    class V6(_Dev):
        ip = "fe80::1"

    class NoIP(_Dev):
        ip = None

    async def run():
        await M.on_esphome_mdns(V6(), MSG)
        await M.on_esphome_mdns(NoIP(), MSG)
        assert zc.registered == []
    asyncio.run(run())
