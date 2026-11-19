"""
Saving config: pushing a device's effective config to its live connection
and the controller-side copies of it, refusing a write that would silently
drop keys, and the emOS console password and idle timeout, which are stored
and shown differently from every other key. Shared by the device and fleet
config routes.
"""

from __future__ import annotations

import logging

import em_config_types
import em_console_pw
import em_listen
import em_scenes

log = logging.getLogger("echohome.api")


async def apply_live_config(device_id: str, live, effective: dict) -> None:
    """
    Push an effective config to a connected device and refresh the
    controller-side mirrors of it.

    Extracted because the per-device and fleet endpoints both did this
    inline, and a mirror added to one but not the other is a bug that reads
    as working — the same shape as the v7 stats-relay miss (PR #23). Take
    the EFFECTIVE config, never a request body: with per-section scoping a
    body is partial by design, and a device must always be sent the whole
    resolved picture.
    """
    effective = em_config_types.well_typed(device_id, effective)
    await live.send_control({"type": "config", **effective})
    if "owwOnDevice" in effective:
        # Only firmware from before EchoHome acts on it; the privacy line
        # reads it against that firmware's "stream" report (em_listen).
        live.oww_on_device = em_listen.effective_mode(
            effective["owwOnDevice"], live.oww_trigger_capable,
        )
    if "bassGuardDb" in effective:
        live.bass_guard_db = float(effective["bassGuardDb"])
    live.led_scene = em_scenes.resolve(effective)
    # #263: keep the device's cached listening animation in step when the
    # scene changes live, same push as at registration. Without it the ring
    # lit locally in the OLD scene's colours until the next reconnect.
    if live.led_anim_capable and live.led_scene.get("listening_anim"):
        try:
            await live.send_control(em_scenes.ring_push(
                live.led_scene, getattr(live, "capabilities", None)))
        except Exception:
            pass  # device offline — next connect re-sends it


# The console password record never leaves the controller. Hashing before
# storage is pointless if the result is then handed to every client that asks
# for the config, so reads see a sentinel and writes send it back untouched —
# see em_console_pw.for_display / resolve_write.
_CONSOLE_PW_KEY = "consolePassword"


def redact_console_pw(config: dict) -> dict:
    if not config or _CONSOLE_PW_KEY not in config:
        return config
    out = dict(config)
    out[_CONSOLE_PW_KEY] = em_console_pw.for_display(out[_CONSOLE_PW_KEY])
    return out


def resolve_console_pw(incoming: dict, stored: dict) -> None:
    """Turn whatever a client sent into the record to store, in place."""
    if _CONSOLE_PW_KEY not in incoming:
        return
    incoming[_CONSOLE_PW_KEY] = em_console_pw.resolve_write(
        incoming[_CONSOLE_PW_KEY], (stored or {}).get(_CONSOLE_PW_KEY)
    )


# The console idle timeout, in minutes: 0 for none, otherwise 1-90.
_CONSOLE_TMOUT_KEY = "consoleTimeoutMin"


_CONSOLE_TMOUT_MAX = 90


def validate_console_timeout(config: dict) -> str | None:
    """
    Return an error message when the timeout is out of range, else None.

    REFUSED rather than clamped, deliberately. A value of 600 is somebody who
    meant seconds, and silently giving them ten minutes is a console that logs
    them out all day from a setting that looked accepted. The device refuses
    it too — the controller validating first means a bad value reaching the
    firmware is a bug rather than a user, but neither end assumes the other is
    the careful one.

    Absence is fine: it means the body did not mention the key.
    """
    if _CONSOLE_TMOUT_KEY not in config:
        return None
    v = config[_CONSOLE_TMOUT_KEY]
    if isinstance(v, bool) or not isinstance(v, int):
        return (f"{_CONSOLE_TMOUT_KEY} must be a whole number of minutes, "
                f"got {v!r}")
    if v < 0 or v > _CONSOLE_TMOUT_MAX:
        return (f"{_CONSOLE_TMOUT_KEY} must be 0 (no timeout) or 1-"
                f"{_CONSOLE_TMOUT_MAX} minutes, got {v}")
    return None


def dropped_keys(incoming: dict, stored: dict) -> list[str]:
    """
    Keys present in the stored config that the incoming body would delete.

    Config POSTs REPLACE the stored dict — they do not merge. That is fine
    for the dashboard, which always submits the complete config, and a trap
    for anything that submits a partial one: on 2026-07-20 a POST carrying a
    single key silently reset all 26 fleet settings to defaults, taking the
    wake model from hey_mycroft back to hey_jarvis and dropping owwThreshold
    0.5 -> 0.3, which surfaced as devices false-waking on ordinary
    conversation. Callers that genuinely intend a destructive write pass
    replace=true; everything else is refused before anything is persisted.
    """
    return sorted(set(stored) - set(incoming))
