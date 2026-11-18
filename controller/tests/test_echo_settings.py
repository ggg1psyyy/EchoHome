"""Settings Home Assistant changes on the Echo itself (em_echo_settings,
device/cmd/owned.go): stored as the Echo's own, never the fleet's; nothing
else in the section changes; only the keys the Echo owns are taken; the push
back is the Echo's acknowledgement."""
import asyncio
import re
from pathlib import Path

import pytest

import em_config_sections as cs
import em_db
import em_device_config
import em_echo_settings as es
import em_events
import em_incubator
import em_state

ROOT = Path(__file__).resolve().parents[2]


def _go(name):
    return (ROOT / "device" / "cmd" / name).read_text()


def test_the_keys_the_echo_owns_are_the_firmwares():
    go = _go("owned.go")
    keys = set(re.findall(r'owned\w+\s*=\s*"(\w+)"', go))
    assert keys == es.ECHO_OWNED
    for k in es.ECHO_OWNED:
        assert es.section_of(k), f"{k} is in no section"




def test_a_section_the_echo_follows_from_the_fleet_is_seeded_from_it():
    effective = {"muteRing": True, "remoteVolumeArc": True, "ledScene": "pride", "micGainDb": 30}
    sections, config = es.plan({"muteRing": False}, ["microphones"], {"micGainDb": 30}, effective)
    assert sections == cs.normalise(["microphones", "ring"])
    assert config["muteRing"] is False
    # The rest of the ring section keeps the fleet's values.
    assert config["ledScene"] == "pride" and config["remoteVolumeArc"] is True
    assert config["micGainDb"] == 30


@pytest.fixture
def device(tmp_path, monkeypatch):
    em_db.init(str(tmp_path / "t.db"))
    em_db.register_new_device("dev1", "10.0.0.9", "vtest")
    pushed, events = [], []

    async def apply_live(device_id, live, effective):
        pushed.append(effective)

    async def push(ev):
        events.append(ev)

    async def reconcile(device_id):
        pass

    monkeypatch.setattr(em_device_config, "apply_live_config", apply_live)
    monkeypatch.setattr(em_events, "push", push)
    monkeypatch.setattr(em_incubator, "reconcile", reconcile)
    monkeypatch.setitem(em_state.devices, "dev1", object())
    return pushed, events


def test_a_report_is_stored_as_the_echos_own_and_pushed_back(device):
    pushed, events = device
    fleet_before = em_db.get_global_device_config()["muteRing"]
    assert fleet_before is True
    assert asyncio.run(es.from_echo("dev1", {"muteRing": False})) is True
    eff = em_db.get_effective_device_config("dev1")
    assert eff["muteRing"] is False
    assert "ring" in em_db.get_device_config_sections("dev1")
    # The fleet is untouched.
    assert em_db.get_global_device_config()["muteRing"] is fleet_before
    assert pushed and pushed[-1]["muteRing"] is False, "no push back: the Echo never clears pending"
    assert events and events[-1]["type"] == "device_update"


def test_only_owned_and_well_typed_keys_are_taken(device):
    pushed, _ = device
    assert asyncio.run(es.from_echo("dev1", {"micGainDb": 99})) is False
    assert asyncio.run(es.from_echo("dev1", {"muteRing": "no"})) is False
    assert asyncio.run(es.from_echo("dev1", "muteRing")) is False
    assert asyncio.run(es.from_echo("nobody", {"muteRing": False})) is False
    assert pushed == []
    assert em_db.get_device_config_sections("dev1") == []
