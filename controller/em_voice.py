"""
Which voice backend answers an Echo, for em_controller's turn path.

An Echo's turns go to Home Assistant through its ESPHome satellite
(em_esphome) unless it is set to the Voice Incubator (em_incubator, effective
`voiceBackend`), and never to both: these are the six calls the turn loop
makes into a backend, each sent to exactly one. The two share a contract --
em_incubator's functions are written to em_esphome's -- so everything around
them (arbitration, the voice lock, the ring, barge-in, follow-ups) is one
piece of code for either.

Looked up at call time, not bound at import: tests drive the turn loop by
patching em_esphome's functions, and an Echo can change backend while the
controller runs.
"""

from __future__ import annotations

import em_esphome
import em_incubator


def _incubator(device_id: str) -> bool:
    # By the Echo's configured backend, not by whether its satellite is up:
    # an Incubator Echo whose satellite failed to start offers HA no voice
    # assistant, so its turns must stand down here (no app: the no-assistant
    # cue), never be sent to an HA that cannot answer them.
    return em_incubator.backend_of(device_id) == em_incubator.BACKEND_INCUBATOR


def can_serve_turn(device_id: str) -> bool:
    if _incubator(device_id):
        return em_incubator.can_serve_turn(device_id)
    return em_esphome.can_serve_turn(device_id)


async def record_dropped_wake(device, trigger_label: str, wake_info) -> None:
    # Backend-neutral already: the record and the ring cue say no assistant
    # is behind the Echo, whichever it was meant to be.
    await em_esphome.record_dropped_wake(device, trigger_label, wake_info)


async def trigger_voice_turn(device, **kwargs) -> bool:
    if _incubator(device.device_id):
        return await em_incubator.trigger_voice_turn(device, **kwargs)
    return await em_esphome.trigger_voice_turn(device=device, **kwargs)


def cancel_voice_turn(device_id: str, abort_ha: bool = False,
                      reason: str = "cancelled") -> None:
    if _incubator(device_id):
        em_incubator.cancel_voice_turn(device_id, abort_ha=abort_ha, reason=reason)
    else:
        em_esphome.cancel_voice_turn(device_id, abort_ha=abort_ha, reason=reason)


def abort_ha_run(device_id: str) -> None:
    if _incubator(device_id):
        em_incubator.abort_ha_run(device_id)
    else:
        em_esphome.abort_ha_run(device_id)


def note_barged(device_id: str) -> None:
    if _incubator(device_id):
        em_incubator.note_barged(device_id)
    else:
        em_esphome.note_barged(device_id)


# ─── The Incubator app's own playback (Home Assistant has no equivalent) ────

def wake_over_playback(device_id: str) -> bool:
    """A wake heard over this Echo's playback still counts: the Incubator
    app's ring or announcement is playing, and a wake is how it is stopped."""
    return _incubator(device_id) and em_incubator.wake_over_playback(device_id)


def app_timer_ringing(device_id: str) -> bool:
    return _incubator(device_id) and em_incubator.app_timer_ringing(device_id)


async def stop_app_timer(device_id: str) -> None:
    await em_incubator.stop_app_timer(device_id)
