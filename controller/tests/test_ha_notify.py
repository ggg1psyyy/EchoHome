"""The Home Assistant notification while an Echo has an update (em_ha_notify)."""

import asyncio

import aiohttp
from aiohttp import web
from aiohttp.test_utils import TestServer

import em_ha_notify as n


def echo(label="Kitchen", fw="v3.0.0-ea.3", emos="emos-v0.12", approved=1, base_os="emos",
         device_id="G090XX0000000AB1"):
    return {"label": label, "firmware_ver": fw, "emos_version": emos,
            "approved": approved, "base_os": base_os, "device_id": device_id}


FW, EMOS = "v3.0.0-ea.4", "emos-v0.13"


def test_nothing_to_offer_is_no_notification():
    assert n.decide([echo(fw=FW, emos=EMOS)], FW, EMOS) is None
    assert n.decide([], FW, EMOS) is None
    assert n.decide([echo()], None, None) is None


def test_firmware_and_emos_name_their_echoes():
    sig, title, msg = n.decide([echo("Kitchen"), echo("Office", fw=FW)], FW, EMOS)
    assert "Firmware v3.0.0-ea.4** for Kitchen" in msg
    assert "emOS emos-v0.13** for Kitchen, Office" in msg
    assert sig == f"firmware={FW};emos={EMOS}"


def test_unapproved_fireos_and_unknown_versions_are_left_out():
    rows = [echo("A", approved=0), echo("B", base_os="fireos"), echo("C", fw=None, emos=None)]
    assert n.decide(rows, FW, EMOS) is None


def test_the_signature_is_the_versions_not_the_echoes():
    one = n.decide([echo("A")], FW, EMOS)[0]
    two = n.decide([echo("A"), echo("B")], FW, EMOS)[0]
    assert one == two


def test_a_label_is_plain_text_in_markdown():
    _, _, msg = n.decide([echo("Living [Room]*")], FW, EMOS)
    assert "Living \\[Room\\]\\*" in msg


class FakeSupervisor:
    def __init__(self):
        self.calls = []

    def app(self):
        app = web.Application()
        app.router.add_get("/addons/self/info", self.info)
        app.router.add_post("/core/api/services/persistent_notification/{verb}", self.service)
        return app

    async def info(self, request):
        self.calls.append(("info", request.headers.get("Authorization"), None))
        return web.json_response({"result": "ok", "data": {"slug": "abcd1234_controller-ea"}})

    async def service(self, request):
        self.calls.append((request.match_info["verb"], request.headers.get("Authorization"),
                           await request.json()))
        return web.json_response([])


def run_syncs(monkeypatch, rows_by_pass, stored="", firmware=FW, emos=EMOS):
    """sync() once per entry of rows_by_pass against a fake Supervisor."""
    config = {"ha_notify_sig": stored}
    monkeypatch.setattr(n.db, "get_config", lambda k, d=None: config.get(k, d))
    monkeypatch.setattr(n.db, "set_config", lambda k, v: config.__setitem__(k, v))
    monkeypatch.setattr(n.em_releases, "firmware_version", lambda: firmware)
    monkeypatch.setattr(n.em_releases, "emos_latest_version", lambda: emos)
    sup = FakeSupervisor()

    async def go():
        server = TestServer(sup.app())
        await server.start_server()
        try:
            async with aiohttp.ClientSession() as s:
                notifier = n.Notifier(s, "tok", base=str(server.make_url("")).rstrip("/"))
                for rows in rows_by_pass:
                    monkeypatch.setattr(n.db, "get_all_devices", lambda rows=rows: rows)
                    await notifier.sync()
        finally:
            await server.close()

    asyncio.run(go())
    return sup.calls, config


def test_an_update_raises_one_notification_with_the_dashboard_link(monkeypatch):
    calls, config = run_syncs(monkeypatch, [[echo()]])
    creates = [c for c in calls if c[0] == "create"]
    assert len(creates) == 1
    _, auth, body = creates[0]
    assert auth == "Bearer tok"
    assert body["notification_id"] == n.NOTIFICATION_ID
    assert "[Open the EchoHome dashboard](/abcd1234_controller-ea)" in body["message"]
    assert config["ha_notify_sig"] == f"firmware={FW};emos={EMOS}"


def test_the_same_offer_is_not_raised_again(monkeypatch):
    # Every pass and every restart: a dismissed notification stays dismissed.
    calls, _ = run_syncs(monkeypatch, [[echo()], [echo()]],
                         stored=f"firmware={FW};emos={EMOS}")
    assert [c for c in calls if c[0] in ("create", "dismiss")] == []


def test_a_new_release_raises_it_again(monkeypatch):
    calls, _ = run_syncs(monkeypatch, [[echo()]], stored="firmware=v3.0.0-ea.3;emos=")
    assert [c[0] for c in calls if c[0] in ("create", "dismiss")] == ["create"]


def test_once_all_are_updated_it_is_cleared(monkeypatch):
    calls, config = run_syncs(monkeypatch, [[echo()], [echo(fw=FW, emos=EMOS)]])
    assert [c[0] for c in calls if c[0] in ("create", "dismiss")] == ["create", "dismiss"]
    assert calls[-1][2] == {"notification_id": n.NOTIFICATION_ID}
    assert config["ha_notify_sig"] == ""


def test_an_unknown_latest_version_decides_nothing(monkeypatch):
    # Just after a start the firmware cache is empty until the first poll:
    # that must neither clear the notification nor forget it was dismissed.
    sig = f"firmware={FW};emos={EMOS}"
    for firmware, emos in ((None, EMOS), (FW, "")):
        calls, config = run_syncs(monkeypatch, [[echo()]], stored=sig,
                                  firmware=firmware, emos=emos)
        assert [c for c in calls if c[0] in ("create", "dismiss")] == []
        assert config["ha_notify_sig"] == sig


def test_without_a_supervisor_it_does_nothing(monkeypatch):
    monkeypatch.delenv("SUPERVISOR_TOKEN", raising=False)
    asyncio.run(asyncio.wait_for(n.run(), 2))  # returns at once
