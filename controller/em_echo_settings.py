"""
Settings an Echo owns: the ones Home Assistant changes on the Echo itself,
as switches on its own ESPHome server (device/cmd/owned.go). The Echo keeps
the change and reports it (settings_changed) until a config push carries it
back; until then its value stands against the controller's. Here a report is
stored as this Echo's own setting, never the fleet's, and the effective
config pushed back, which is the acknowledgement.

Only ECHO_OWNED keys are taken, each checked by em_config_types as a
dashboard POST would be. A key whose section the Echo follows from the fleet
makes the Echo override that section, seeded with the fleet's values, so
nothing else in the section changes.
"""

from __future__ import annotations

import asyncio
import logging

import em_config_sections as sections_mod
import em_config_types
import em_db as db
import em_device_config
import em_events
import em_incubator
import em_state

log = logging.getLogger("echomuse.settings")

# What the Echo may set; device/cmd/owned.go's own keys (test-pinned).
ECHO_OWNED = frozenset({"muteRing", "remoteVolumeArc", "incubatorMonitor"})


def section_of(key: str) -> str | None:
    for sid, section in sections_mod.SECTIONS.items():
        if key in section["keys"]:
            return sid
    return None


def plan(values: dict, sections: list, stored: dict, effective: dict) -> tuple[list, dict]:
    """The sections and stored config after taking `values` (already
    checked) as this device's own. Pure."""
    missing = {section_of(k) for k in values} - set(sections) - {None}
    new_sections = sections_mod.normalise(list(sections) + sorted(missing))
    seed = {k: effective[k] for k in sections_mod.keys_for(missing) if k in effective}
    return new_sections, {**stored, **seed, **values}


async def from_echo(device_id: str, settings) -> bool:
    """Take a settings_changed report. False if nothing in it was taken."""
    if not isinstance(settings, dict):
        return False
    values = {k: v for k, v in settings.items() if k in ECHO_OWNED}
    ignored = sorted(set(settings) - ECHO_OWNED)
    if ignored:
        log.warning(f"[settings] {device_id} reported keys it does not own, ignored: {', '.join(map(str, ignored))}")
    if not values:
        return False
    loop = asyncio.get_running_loop()
    if await loop.run_in_executor(None, db.get_device, device_id) is None:
        return False
    effective = await loop.run_in_executor(None, db.get_effective_device_config, device_id)
    if (bad := em_config_types.problems(values, effective)):
        log.warning(f"[settings] {device_id} reported values refused: {'; '.join(bad)}")
        return False
    sections = await loop.run_in_executor(None, db.get_device_config_sections, device_id)
    stored = await loop.run_in_executor(None, db.get_device_config, device_id)
    new_sections, config = plan(values, sections, stored, effective)
    if new_sections != sections:
        await loop.run_in_executor(None, db.set_device_config_sections, device_id, new_sections)
    await loop.run_in_executor(None, db.set_device_config, device_id, config)
    log.info(f"[settings] {device_id} set on the Echo: "
             + ", ".join(f"{k}={v}" for k, v in sorted(values.items())))
    effective = await loop.run_in_executor(None, db.get_effective_device_config, device_id)
    live = em_state.devices.get(device_id)
    if live is not None:
        # The push is the Echo's acknowledgement.
        await em_device_config.apply_live_config(device_id, live, effective)
    await em_incubator.reconcile(device_id)
    await em_events.push({"type": "device_update", "device_id": device_id,
                          "state": {"config": effective,
                                    "config_sections": new_sections,
                                    "use_global_config": not new_sections}})
    return True
