"""The LED Ring light and the start_call action through aioesphomeapi, as
Home Assistant uses them: listed (an RGB light; a service with no arguments),
a light command (on, a colour, a brightness) and its state back, the action
executed.

    python lightaction.py [port]
"""
import asyncio
import sys

from aioesphomeapi import APIClient
from aioesphomeapi.model import LightInfo, LightState, UserService


async def main(port):
    cli = APIClient("127.0.0.1", port, password="")
    await cli.connect(login=True)
    entities, services = await cli.list_entities_services()
    light = next(e for e in entities if isinstance(e, LightInfo))
    assert light.object_id == "led_ring" and light.supported_color_modes, light
    svc = next(s for s in services if isinstance(s, UserService) and s.name == "start_call")
    print("listed:", light.name, light.supported_color_modes, "service", svc.name, svc.args)
    got = []
    ev = asyncio.Event()

    def on_state(st):
        if isinstance(st, LightState):
            got.append(st)
            ev.set()

    cli.subscribe_states(on_state)
    await asyncio.wait_for(ev.wait(), 5)
    cli.light_command(light.key, state=True, rgb=(1.0, 0.5, 0.0), brightness=0.4)
    while not (got[-1].state and abs(got[-1].brightness - 0.4) < 1e-6):
        ev.clear()
        await asyncio.wait_for(ev.wait(), 5)
    print("light:", got[-1].state, got[-1].brightness, got[-1].red, got[-1].green, got[-1].blue)
    await cli.execute_service(svc, {})
    await asyncio.sleep(0.3)
    await cli.disconnect()
    print("ok")


asyncio.run(main(int(sys.argv[1]) if len(sys.argv) > 1 else 16053))
