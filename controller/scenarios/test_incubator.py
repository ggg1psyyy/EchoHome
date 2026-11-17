"""
An Echo answered by OHF-Voice's Voice Incubator (#1207), end to end: the real
controller (its turn loop, wake gates, button handler and playback, not
stand-ins), an Echo speaking the wire contract, the app speaking the
satellite protocol as PROTOCOL.md writes it, and Home Assistant as its own
client library reading the Echo's ESPHome device.
"""

import asyncio
import contextlib
import json

import harness
from harness import FakeEcho, FakeHA, FakeIncubatorApp as App, wait_for
from test_voice_turn import SPEECH, _connect

PORT_OFFSET = 2000  # em_incubator.PORT_OFFSET: the satellite beside the voice port


@contextlib.asynccontextmanager
async def incubator_room(ctl):
    """One Echo set to the Voice Incubator, and the app connected to it."""
    # The fleet's backend, as the dashboard's Advanced section saves it.
    ctl.execute("INSERT OR REPLACE INTO system_config (key, value) VALUES ('global_device_config', ?)",
                json.dumps({"voiceBackend": "incubator"}))
    echo = FakeEcho(ctl)
    await echo.connect()
    assert "listen_session" in echo.features, "private listening not negotiated"
    port = await wait_for(lambda: ctl.esphome_port(echo.device_id), 10, "the Echo's satellite port")
    app = App(port + PORT_OFFSET)
    await app.connect()
    try:
        yield echo, app, port
    finally:
        await app.close()
        await echo.close()


def _turns(ctl, echo):
    return ctl.turns(echo.device_id)


def _duck_released(echo) -> bool:
    """Music let up again: the last duck the Echo was told is off (the
    owner count em_player keeps came back to zero)."""
    ducks = echo.sent("duck")
    return bool(ducks) and ducks[-1].get("on") is False


def test_a_wake_call_through_the_voice_incubator(scenario):
    async def body(ctl):
        async with incubator_room(ctl) as (echo, app, _port):
            assert app.hello["protocol"] == 2
            assert app.hello["name"] == f"echomuse-{echo.device_id[-12:].lower()}"
            assert "wake_format" not in app.hello

            sid = await echo.wake(SPEECH)
            start = await app.until(App.CALL_START)
            assert start["wake_word"].startswith("echomuse-")
            await wait_for(lambda: any(m.get("session") == sid for m in echo.sent("listen_ack")),
                           5, "the wake to be acknowledged")
            await wait_for(lambda: len(app.audio_in) >= len(SPEECH) // 2, 10,
                           "the command to reach the app")

            app.event("listening", stage="listening")
            app.event("processing")
            # End of speech closes the Echo's session, before any reply: the
            # Echo streams until the command ended, not until the call does
            # (docs/listening.md). Asserted before the reply exists, so no
            # later close (playback, the turn's cleanup) can stand in for it.
            await wait_for(lambda: echo.closed(sid), 5, "the session to close at end of speech")
            reply = harness.tone(1.0, harness.MIC_RATE, hz=330)
            app.say(reply)
            await app.until(App.PLAYBACK_DRAINED)
            app.send_json(App.HANGUP, {"reason": "done"})

            # The reply played on the Echo at its 48 kHz, whole, once, and
            # ended: three times its 16 kHz bytes, plus the upsampler's tail
            # (72 bytes) and at most one period of padding.
            assert echo.voice_eos >= 1
            assert 3 * len(reply) <= len(echo.voice) <= 3 * len(reply) + 72 + 4096
            row = (await wait_for(lambda: _turns(ctl, echo), 10, "the turn row"))[-1]
            assert row["outcome"] == "ok"
    scenario(body)


def test_a_wake_over_the_apps_ring_cuts_it_and_starts_a_call(scenario):
    async def body(ctl):
        async with incubator_room(ctl) as (echo, app, _port):
            app.event("timer_ringing", timers=1, name="pasta")
            stop = asyncio.Event()
            ring = asyncio.create_task(app.paced(harness.tone(1.0, harness.MIC_RATE, hz=880), stop))
            await wait_for(lambda: len(echo.voice) > 0, 10, "the ring to play on the Echo")
            flushes = len(echo.sent("speaker_flush"))

            # Over its own playback. This Echo listens privately, so it is
            # _private_listen's gate that must let the wake through
            # (_stream_listen's is held by a source guard, not run here).
            await echo.wake(SPEECH)
            await app.until(App.CALL_START)
            stop.set()
            await ring
            assert len(echo.sent("speaker_flush")) > flushes, "the ring was not cut"

            app.event("listening", stage="listening")
            app.send_json(App.HANGUP, {"reason": "timer_stopped"})
            app.event("timer_stopped")
            await wait_for(lambda: _turns(ctl, echo), 10, "the turn row")
            await wait_for(lambda: _duck_released(echo), 10, "music to be let up again")
    scenario(body)


def test_the_button_stops_the_apps_ring_and_starts_no_call(scenario):
    async def body(ctl):
        async with incubator_room(ctl) as (echo, app, _port):
            app.event("timer_ringing", timers=1, name="pasta")
            stop = asyncio.Event()
            ring = asyncio.create_task(app.paced(harness.tone(1.0, harness.MIC_RATE, hz=880), stop))
            await wait_for(lambda: len(echo.voice) > 0, 10, "the ring to play on the Echo")
            flushes = len(echo.sent("speaker_flush"))

            await echo.send({"type": "button", "clickType": 138, "down": False,
                             "heldMs": 80, "muted": False})
            event = await app.until(App.DEVICE_EVENT)
            stop.set()
            await ring
            assert event == {"type": "stop_timer"}
            assert len(echo.sent("speaker_flush")) > flushes, "the ring was not cut"
            await asyncio.sleep(0.5)
            assert not app.got(App.CALL_START), "the button started a call"
            app.event("timer_stopped")
            await wait_for(lambda: _duck_released(echo), 10, "music to be let up again")
    scenario(body)


def test_home_assistant_sees_no_voice_assistant_on_an_incubator_echo(scenario):
    async def body(ctl):
        async with incubator_room(ctl) as (echo, _app, port):
            ha = FakeHA(port)
            await _connect(ha)
            try:
                # HA's own client, reading the Echo's ESPHome device: no
                # Assist satellite, and the media player still there.
                assert ha.device_info.voice_assistant_feature_flags == 0
                kinds = {type(e).__name__ for e in ha.entities}
                assert "MediaPlayerInfo" in kinds, kinds
            finally:
                await ha.disconnect()
    scenario(body)


def test_no_app_means_the_echo_stands_down_never_to_home_assistant(scenario):
    async def body(ctl):
        async with incubator_room(ctl) as (echo, app, port):
            ha = FakeHA(port)
            await _connect(ha)
            await app.close()
            await wait_for(lambda: "App disconnected" in ctl.log(), 10,
                           "the controller to see the app go")
            sid = await echo.wake(SPEECH)
            await wait_for(lambda: echo.closed(sid), 10, "the wake to be declined")
            await asyncio.sleep(0.5)
            assert not ha.starts, "the wake went to Home Assistant"
            row = (await wait_for(lambda: _turns(ctl, echo), 10, "the turn row"))[-1]
            assert row["outcome"] == "no_ha"
            await ha.disconnect()
    scenario(body)


def test_an_announcement_from_the_app_plays_and_is_answered(scenario):
    async def body(ctl):
        async with incubator_room(ctl) as (echo, app, _port):
            app.event("announcement", active=True)
            chime = harness.tone(0.3, harness.MIC_RATE, hz=660)
            words = harness.tone(0.8, harness.MIC_RATE, hz=220)
            app.say(chime)
            await app.until(App.PLAYBACK_DRAINED)
            app.say(words)
            await app.until(App.PLAYBACK_DRAINED, after=1)
            app.event("announcement", active=False)
            # Both streams played on the Echo, each ended, and music (held
            # once for the whole announcement) was let up again after it.
            assert echo.voice_eos >= 2
            assert len(echo.voice) >= 3 * (len(chime) + len(words))
            await wait_for(lambda: _duck_released(echo), 10, "music to be let up again")
            ons = [m for m in echo.sent("duck") if m.get("on")]
            assert len(ons) == 1, f"music ducked {len(ons)} times for one announcement"
    scenario(body)


def test_a_button_turn_through_the_voice_incubator(scenario):
    async def body(ctl):
        async with incubator_room(ctl) as (echo, app, _port):
            echo.turn_speech.append(SPEECH)
            await echo.send({"type": "button", "clickType": 138, "down": False,
                             "heldMs": 80, "muted": False})
            start = await app.until(App.CALL_START)
            assert start == {"wake_word": "manual", "preroll_bytes": 0}
            # Silence while the Echo's VAD waits, then the speech itself.
            await wait_for(lambda: echo.turn_streams == 1, 5, "the Echo's turn stream")
            # The speech itself, untouched and in one piece -- not merely as
            # many bytes, which the silence padding alone would supply.
            await wait_for(lambda: SPEECH in bytes(app.audio_in), 10,
                           "the command to reach the app")
            app.event("listening", stage="listening")
            app.event("processing")
            reply = harness.tone(0.5, harness.MIC_RATE, hz=330)
            app.say(reply)
            await app.until(App.PLAYBACK_DRAINED)
            app.send_json(App.HANGUP, {"reason": "done"})
            assert len(echo.voice) >= 3 * len(reply)
            row = (await wait_for(lambda: _turns(ctl, echo), 10, "the turn row"))[-1]
            assert row["outcome"] == "ok" and row["trigger_type"] == "button"
    scenario(body)


def test_a_question_asked_back_is_answered_without_the_wake_word(scenario):
    async def body(ctl):
        async with incubator_room(ctl) as (echo, app, _port):
            echo.turn_speech.append(SPEECH)  # the answer, said after the question
            await echo.wake(SPEECH)
            await app.until(App.CALL_START)
            app.event("listening", stage="listening")
            app.event("processing")
            app.say(harness.tone(0.5, harness.MIC_RATE, hz=330))  # "which room?"
            await app.until(App.PLAYBACK_DRAINED)
            app.send_json(App.HANGUP, {"reason": "asked", "listen": True})

            # The Echo opens its microphone again, and the app hears the
            # answer as a call of its own, with no wake word and no pre-roll.
            follow = await app.until(App.CALL_START, after=1)
            assert follow == {"wake_word": "follow_up", "preroll_bytes": 0}
            before = len(app.audio_in)
            await wait_for(lambda: SPEECH in bytes(app.audio_in[before:]), 10,
                           "the answer to reach the app")
            app.event("listening", stage="listening")
            app.event("processing")
            app.say(harness.tone(0.3, harness.MIC_RATE, hz=440))
            await app.until(App.PLAYBACK_DRAINED, after=1)
            app.send_json(App.HANGUP, {"reason": "done"})
            rows = await wait_for(lambda: len(_turns(ctl, echo)) >= 2 and _turns(ctl, echo),
                                  10, "both turn rows")
            assert [r["outcome"] for r in rows[-2:]] == ["ok", "ok"]
            assert rows[-1]["trigger_type"] == "continuation"
    scenario(body)


def test_a_tap_during_the_reply_cancels_the_call(scenario):
    async def body(ctl):
        async with incubator_room(ctl) as (echo, app, _port):
            sid = await echo.wake(SPEECH)
            await app.until(App.CALL_START)
            app.event("listening", stage="listening")
            app.event("processing")
            await wait_for(lambda: echo.closed(sid), 5, "the session to close at end of speech")
            stop = asyncio.Event()
            reply = asyncio.create_task(app.paced(harness.tone(1.0, harness.MIC_RATE, hz=330), stop))
            await wait_for(lambda: len(echo.voice) > 0, 10, "the reply to play on the Echo")
            flushes = len(echo.sent("speaker_flush"))

            # The real button handler's cancel branch, not em_voice called directly.
            await echo.send({"type": "button", "clickType": 138, "down": False,
                             "heldMs": 80, "muted": False})
            end = await app.until(App.CALL_END)
            stop.set()
            await reply
            assert end.get("reason") == "cancelled", end
            assert len(echo.sent("speaker_flush")) > flushes, "the reply was not cut"
            row = (await wait_for(lambda: _turns(ctl, echo), 10, "the turn row"))[-1]
            assert row["outcome"] == "cancelled", row["outcome"]
    scenario(body)


def test_a_tap_while_the_app_is_still_listening_cancels_the_call(scenario):
    async def body(ctl):
        async with incubator_room(ctl) as (echo, app, _port):
            await echo.wake(SPEECH)
            await app.until(App.CALL_START)
            app.event("listening", stage="listening")
            # No reply yet: the call is still streaming the command. The
            # button handler sets cancel_event (which the call loop watches)
            # and cancels through em_voice; either alone ends the call.
            await echo.send({"type": "button", "clickType": 138, "down": False,
                             "heldMs": 80, "muted": False})
            end = await app.until(App.CALL_END)
            assert end.get("reason") == "cancelled", end
            row = (await wait_for(lambda: _turns(ctl, echo), 10, "the turn row"))[-1]
            assert row["outcome"] == "cancelled", row["outcome"]
    scenario(body)


def test_an_echo_set_to_its_own_satellite_is_handed_over(scenario):
    """The real controller with firmware that announces incubator_satellite
    and is set to it: the controller's satellite stays down, the Echo is
    asked to listen under the identity the app already knows (the ESPHome
    node name and the stored MAC), and its reported state reaches the
    device view."""
    import aiohttp

    async def body(ctl):
        ctl.execute("INSERT OR REPLACE INTO system_config (key, value) VALUES ('global_device_config', ?)",
                    json.dumps({"voiceBackend": "incubator_echo"}))
        echo = FakeEcho(ctl)
        echo.capabilities = echo.capabilities + ["incubator_satellite"]
        await echo.connect()
        (msg,) = await wait_for(lambda: echo.sent("incubator_satellite"), 10, "the handover")
        mac = ctl.query("SELECT esphome_mac FROM devices WHERE device_id = ?", echo.device_id)[0][0]
        assert msg["enabled"] is True and msg["port"] == 6055
        assert msg["name"] == f"echomuse-{echo.device_id[-12:].lower()}"
        assert msg["mac"] == mac.lower()
        # Never two under one MAC: nothing listens on the controller's port.
        port = await wait_for(lambda: ctl.esphome_port(echo.device_id), 10, "the ESPHome port")
        try:
            _, w = await asyncio.open_connection("127.0.0.1", port + PORT_OFFSET)
            w.close()
            raise AssertionError("the controller's satellite is up beside the Echo's")
        except OSError:
            pass

        await echo.send({"type": "incubator_state",
                         "state": {"listening": True, "port": 6055, "appConnected": True}})
        headers = {"Authorization": f"Bearer {ctl.admin_token()}"}
        async with aiohttp.ClientSession(headers=headers) as s:
            async def view():
                async with s.get(f"http://127.0.0.1:{ctl.api_port}/api/devices") as r:
                    for d in await r.json():
                        if d["device_id"] == echo.device_id:
                            return d.get("voiceIncubator")
            vi = None
            for _ in range(50):
                vi = await view()
                if vi and vi.get("listening"):
                    break
                await asyncio.sleep(0.1)
        assert vi == {"port": 6055, "listening": True, "appConnected": True, "inCall": False, "onEcho": True}, vi
        await echo.close()
    scenario(body)
