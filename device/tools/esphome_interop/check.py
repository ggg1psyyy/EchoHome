"""Drives tools/esphome_interop's server with aioesphomeapi, the library Home
Assistant's ESPHome integration uses, as HA would: connect (hello, login),
device info, list entities, subscribe states. Fails on any difference.

    python check.py [port] [base64 key]   (with the scenario venv's aioesphomeapi)

With a key, it also checks that a wrong key and no key are refused, as HA
reads the refusal (InvalidEncryptionKeyAPIError, RequiresEncryptionAPIError).
"""
import asyncio
import base64
import sys

from aioesphomeapi import APIClient
from aioesphomeapi.model import BinarySensorState, EventInfo, SensorState


async def refused(port: int, key) -> str:
    cli = APIClient("127.0.0.1", port, password="", noise_psk=key)
    try:
        await cli.connect(login=True)
    except Exception as e:  # the type is what HA reads
        return type(e).__name__
    await cli.disconnect()
    return "connected"


async def main(port: int, key) -> None:
    if key:
        wrong = base64.b64encode(bytes(32)).decode()
        assert await refused(port, wrong) == "InvalidEncryptionKeyAPIError"
        assert await refused(port, None) == "RequiresEncryptionAPIError"
        print("refused: a wrong key and no key, as HA expects")
    cli = APIClient("127.0.0.1", port, password="", noise_psk=key)
    await cli.connect(login=True)
    info = await cli.device_info()
    assert info.name == "echohome-interop", info
    assert info.mac_address == "02:EC:00:00:00:99", info
    assert info.project_name == "EchoHome.biscuit", info
    print("device info:", info.name, info.model, info.mac_address, info.project_name)
    entities, services = await cli.list_entities_services()
    by_id = {e.object_id: e for e in entities}
    assert set(by_id) == {"action_button", "ambient_light", "mic_muted"}, by_id
    assert isinstance(by_id["action_button"], EventInfo)
    assert by_id["action_button"].event_types == ["long", "single", "double", "triple"]
    assert by_id["ambient_light"].unit_of_measurement == "lx"
    print("entities:", sorted((e.object_id, e.key, type(e).__name__) for e in entities))
    got = []
    done = asyncio.Event()

    def on_state(st):
        got.append(st)
        if any(getattr(s, "event_type", None) == "long" for s in got):
            done.set()

    cli.subscribe_states(on_state)
    await asyncio.wait_for(done.wait(), 5)
    lux = [s for s in got if isinstance(s, SensorState)]
    mic = [s for s in got if isinstance(s, BinarySensorState)]
    assert lux[0].missing_state and not lux[-1].missing_state and lux[-1].state == 123, lux
    assert mic[-1].state is True, mic
    print("states:", [(type(s).__name__, getattr(s, "state", None), getattr(s, "missing_state", None),
                       getattr(s, "event_type", None)) for s in got])
    await cli.disconnect()
    print("ok")


asyncio.run(main(int(sys.argv[1]) if len(sys.argv) > 1 else 16053,
                 sys.argv[2] if len(sys.argv) > 2 else None))
