"""Drives tools/esphome_interop's media player with aioesphomeapi, as Home
Assistant's ESPHome media player does: play a URL (playing, then idle when
it has played out), pause and resume, an announcement (announcing, then
back), volume, stop. The player is the real internal/mediaplayer, on a
stand-in speaker running 20x fast.

    python media.py [port]      (WAV served on port+1)
"""
import asyncio
import sys

from aioesphomeapi import APIClient
from aioesphomeapi.model import MediaPlayerCommand, MediaPlayerEntityState, MediaPlayerState


async def main(port: int) -> None:
    base = f"http://127.0.0.1:{port + 1}"
    cli = APIClient("127.0.0.1", port, password="")
    await cli.connect(login=True)
    entities, _ = await cli.list_entities_services()
    mp = next(e for e in entities if e.object_id == "media_player")
    states = []
    changed = asyncio.Event()

    def on_state(st):
        if isinstance(st, MediaPlayerEntityState):
            states.append(st)
            changed.set()

    cli.subscribe_states(on_state)

    async def until(state, timeout=10):
        while not states or states[-1].state != state:
            changed.clear()
            await asyncio.wait_for(changed.wait(), timeout)

    await until(MediaPlayerState.IDLE)
    cli.media_player_command(mp.key, media_url=f"{base}/song.wav")
    await until(MediaPlayerState.PLAYING)
    cli.media_player_command(mp.key, command=MediaPlayerCommand.PAUSE)
    await until(MediaPlayerState.PAUSED)
    cli.media_player_command(mp.key, command=MediaPlayerCommand.PLAY)
    await until(MediaPlayerState.PLAYING)
    cli.media_player_command(mp.key, media_url=f"{base}/ann.wav", announcement=True)
    await until(MediaPlayerState.ANNOUNCING)
    await until(MediaPlayerState.PLAYING)
    cli.media_player_command(mp.key, volume=0.3)
    while abs(states[-1].volume - 0.3) > 1e-6:
        changed.clear()
        await asyncio.wait_for(changed.wait(), 5)
    await until(MediaPlayerState.IDLE, timeout=20)  # the song played out
    print("played out, idle")
    cli.media_player_command(mp.key, media_url=f"{base}/song.wav")
    await until(MediaPlayerState.PLAYING)
    cli.media_player_command(mp.key, command=MediaPlayerCommand.STOP)
    await until(MediaPlayerState.IDLE)
    print("states:", [s.state.name for s in states])
    await cli.disconnect()
    print("ok")


asyncio.run(main(int(sys.argv[1]) if len(sys.argv) > 1 else 16053))
