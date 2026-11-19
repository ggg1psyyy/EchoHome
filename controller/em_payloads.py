"""
Files the controller keeps in step on every device, pushed over the shell
plane: the start script and the controller address list (controller.json,
#166). (The FireOS debloat script and hide list went with FireOS support,
2026-11-19.)

Each sync compares the device's copy by md5 and pushes only on a difference,
and each tells a missing file from a device that did not answer
(em_shell.OK_MARK) before deciding anything. They run on connect
(em_reconcile.on_connect) and during an OTA; see "OTA update system" in
controller/CLAUDE.md.
"""

from __future__ import annotations

import asyncio
import hashlib
import logging
import posixpath
import re
from pathlib import Path

import em_db as db
import em_endpoints
import em_events
import em_shell
import em_state

log = logging.getLogger("echomuse.api")

# Device payloads — files the controller distributes to devices (provisioning
# wizard today; script/component OTA tomorrow). One canonical copy on disk,
# read per-request so edits ship without a restart. device/scripts/
# start_server.sh is a symlink into this directory.
PAYLOADS_DIR = Path(__file__).parent / "device_payloads"


async def sync_start_script(live, device_id: str) -> None:
    """
    OTA-time payload sync: heal /data/local/bin/start_server.sh drift.

    The startup script is installed at provisioning and — unlike the server
    binary — had no other update path, so fleet drift accumulates (found
    2026-07-11: Lounge was a script revision behind Office). Every OTA now
    compares the device's script against the canonical payload
    (controller/device_payloads/) and pushes it when they differ.

    Replacement is rename-based on purpose: the running script's shell keeps
    reading the OLD inode, so the update only takes effect at the next
    device reboot — safe to do while the script sits in its `wait` loop.
    Best-effort: a sync failure logs but never blocks the firmware update.
    """
    path = "/data/local/bin/start_server.sh"
    try:
        script = (PAYLOADS_DIR / "start_server.sh").read_bytes()
    except OSError as e:
        log.error(f"[api] start_server.sh payload unreadable — skipping sync: {e}")
        return
    want = hashlib.md5(script).hexdigest()

    # The trailing marker separates "the file differs" from "the device did not
    # answer". em_shell.run returns "" for both — it swallows the exception — and
    # an absent md5 was read as out-of-date, so a shell plane that is not up
    # yet produced a push attempt and a user-visible "out of date" event that
    # was not true. That was harmless while this only ran mid-OTA, with a shell
    # already proven; it is not, now that it also runs seconds after connect.
    out = await em_shell.run(live, f"busybox md5sum {path} 2>/dev/null; echo {em_shell.OK_MARK}")
    if em_shell.OK_MARK not in out:
        log.info(f"[api] [{device_id}] start_server.sh: no answer from the "
                 f"device — leaving it alone")
        return
    if want in out:
        return  # in sync — the common case
    await asyncio.sleep(1.0)  # let the md5 shell session close cleanly

    await em_events.push_log(device_id, "info", "controller",
                          "start_server.sh out of date — syncing canonical version")
    tmp = path + ".new"
    res = await em_shell.stream_file(live, script, tmp)
    if not res:
        await em_events.push_log(device_id, "warn", "controller",
                              f"start_server.sh sync failed: {res} — continuing OTA")
        return
    await asyncio.sleep(1.0)

    res = await em_shell.run(live,
        f'NEW=$(busybox md5sum {tmp} | busybox cut -d" " -f1); '
        f'if [ "$NEW" = "{want}" ]; then '
        f'mv {tmp} {path} && chmod 755 {path} && echo SCRIPT_SYNCED; '
        f'else rm -f {tmp}; echo SCRIPT_MD5_MISMATCH:$NEW; fi')
    if "SCRIPT_SYNCED" in res:
        await em_events.push_log(device_id, "info", "controller",
                              "start_server.sh synced — takes effect on next device reboot")
    else:
        await em_events.push_log(device_id, "warn", "controller",
                              f"start_server.sh sync failed ({res.strip() or 'no output'}) — continuing OTA")
    await asyncio.sleep(1.0)


def endpoints_file() -> bytes | None:
    cfg = db.get_global_device_config()
    return em_endpoints.file_bytes(cfg.get("controllerEndpoints") or [])


async def sync_endpoints(live, device_id: str) -> bool:
    """
    Make the device's controller.json match the fleet's controllerEndpoints.

    Takes effect at the device's next dial: firmware re-reads the file every
    attempt (#166), so nothing is bounced. An empty setting removes only a
    file this controller wrote (em_endpoints.MANAGED_KEY): #166 documented
    hand-writing one for devices that cannot use mDNS, and deleting it on
    upgrade would strand them. A set list replaces any file. Returns whether
    the device now matches.
    """
    path = em_endpoints.DEVICE_PATH
    want = endpoints_file()
    out = await em_shell.run(
        live, f"mkdir -p {posixpath.dirname(path)}; busybox md5sum {path} 2>/dev/null; "
              f"busybox grep -c '\"{em_endpoints.MANAGED_KEY}\"' {path} 2>/dev/null; "
              f"echo {em_shell.OK_MARK}")
    if em_shell.OK_MARK not in out:
        log.info(f"[api] [{device_id}] controller address: no answer from the "
                 f"device — leaving it alone")
        return False
    has_file = re.search(r"\b[0-9a-f]{32}\s", out) is not None
    managed = re.search(r"(?m)^[1-9]\d*$", out) is not None
    if want is None:
        if has_file and managed:
            await asyncio.sleep(1.0)
            res = await em_shell.run(live, f"rm -f {path}; [ -e {path} ] || echo {em_shell.OK_MARK}")
            if em_shell.OK_MARK in res:
                await em_events.push_log(device_id, "info", "controller",
                                      "Controller address list removed — mDNS only from the next reconnect")
            else:
                await em_events.push_log(device_id, "warn", "controller",
                                      "Controller address list not removed")
                return False
        return True
    if em_endpoints.md5(want) in out:
        return True
    if has_file and not managed:
        await em_events.push_log(device_id, "info", "controller",
                              "Replacing a hand-written controller.json with the fleet's address list")
    await asyncio.sleep(1.0)
    res = await em_shell.stream_file(live, want, path, mode="644")
    if res:
        await em_events.push_log(device_id, "info", "controller",
                              "Controller address list updated — used from the next reconnect")
        return True
    await em_events.push_log(device_id, "warn", "controller",
                          f"Controller address list not written: {res}")
    return False


# A push on save that fails is tried once more after this long, rather than
# leaving the device on its old list until it next reconnects.
ENDPOINTS_RETRY_S = 30.0


async def push_endpoints(live, device_id: str) -> None:
    if await sync_endpoints(live, device_id):
        return
    await asyncio.sleep(ENDPOINTS_RETRY_S)
    if em_state.devices.get(device_id) is live:
        await sync_endpoints(live, device_id)


