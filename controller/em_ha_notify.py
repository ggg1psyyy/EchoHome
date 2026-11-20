"""
em_ha_notify.py — a Home Assistant notification while an Echo has an update.

While the setup tool runs as the add-on, an Echo with firmware or emOS older
than the newest release it found raises one persistent notification in Home
Assistant (the bell's badge), naming the Echoes and linking the dashboard.
It is cleared once none has an update. Stopped, the setup tool says nothing:
the Echoes never look for updates themselves (they need no internet).

What counts as an update is what the dashboard offers, from the same caches
and predicates: firmware whose version differs from the newest release
(em_releases.firmware_version), emOS for which em_emos_update.update_available
holds. Approved Echoes only, offline ones included (the database, not the
live set), FireOS ones never (they get no update, em_platform.unsupported).
The setup tool's own update is Home Assistant's to show, not this.

A notification someone dismissed stays dismissed: it is raised again only
when the versions on offer change (the signature, kept in system_config so a
restart does not raise it again). With release checks off (interval 0)
nothing is evaluated or sent.

Add-on only: it goes through the Supervisor's proxy to Home Assistant's API
(`homeassistant_api: true`, SUPERVISOR_TOKEN). The container has no
Supervisor, and this is skipped there, like OIDC sign-in the other way round.
"""

from __future__ import annotations

import asyncio
import logging
import os

import aiohttp

import em_db as db
import em_emos_update
import em_platform
import em_releases

log = logging.getLogger("echohome.ha_notify")

SUPERVISOR = "http://supervisor"
NOTIFICATION_ID = "echohome_updates"
EVERY_S = 60
_SIG_KEY = "ha_notify_sig"
_TIMEOUT = aiohttp.ClientTimeout(total=10)


def _md(text: str) -> str:
    """A label as plain text in Home Assistant's Markdown."""
    return "".join("\\" + c if c in "\\`*_[]()<>#|" else c for c in text)


def pending(rows, firmware_latest, emos_latest) -> tuple[list[str], list[str]]:
    """The labels of the Echoes with a firmware update and with an emOS one."""
    firmware, emos = [], []
    for row in rows:
        if not row["approved"] or em_platform.unsupported(row["base_os"]):
            continue
        name = _md(row["label"] or row["device_id"][-4:])
        running = row["firmware_ver"]
        if firmware_latest and running and running != firmware_latest:
            firmware.append(name)
        if em_emos_update.update_available(row["emos_version"], emos_latest):
            emos.append(name)
    return firmware, emos


def decide(rows, firmware_latest, emos_latest) -> tuple[str, str, str] | None:
    """(signature, title, message) of the notification, or None for none.

    The signature is the versions on offer, not the Echoes: a second Echo
    falling behind on the same release raises nothing new, and one of two
    updated leaves both named until the offer changes or all are current."""
    firmware, emos = pending(rows, firmware_latest, emos_latest)
    if not firmware and not emos:
        return None
    sig = f"firmware={firmware_latest if firmware else ''};emos={emos_latest if emos else ''}"
    lines = []
    if firmware:
        lines.append(f"- **Firmware {firmware_latest}** for {', '.join(firmware)}")
    if emos:
        lines.append(f"- **emOS {emos_latest}** for {', '.join(emos)}")
    return sig, "EchoHome: updates for your Echoes", "\n".join(lines)


def _known():
    """(rows, firmware latest, emOS latest, signature sent), or None while
    either latest version is unknown. Database reads: off the event loop."""
    firmware_latest = em_releases.firmware_version()
    emos_latest = em_releases.emos_latest_version()
    if not firmware_latest or not emos_latest:
        return None
    return (db.get_all_devices(), firmware_latest, emos_latest,
            db.get_config(_SIG_KEY, "") or "")


class Notifier:
    """Sends what decide() says, through the Supervisor."""

    def __init__(self, session: aiohttp.ClientSession, token: str, base: str = SUPERVISOR):
        self.session, self.base = session, base
        self.headers = {"Authorization": f"Bearer {token}"}
        self.panel: str | None = None

    async def _post(self, path: str, payload: dict) -> bool:
        async with self.session.post(self.base + path, json=payload,
                                     headers=self.headers, timeout=_TIMEOUT) as r:
            if r.status >= 300:
                log.warning(f"[ha_notify] {path}: HTTP {r.status}")
            return r.status < 300

    async def _panel_path(self) -> str | None:
        """The dashboard's sidebar panel, /<this add-on's slug>, as Home
        Assistant registers it (hassio/addon_panel.py)."""
        if self.panel is None:
            async with self.session.get(self.base + "/addons/self/info",
                                        headers=self.headers, timeout=_TIMEOUT) as r:
                if r.status < 300:
                    slug = ((await r.json()).get("data") or {}).get("slug")
                    if slug:
                        self.panel = f"/{slug}"
        return self.panel

    async def show(self, title: str, message: str) -> bool:
        panel = await self._panel_path()
        if panel:
            message += f"\n\n[Open the EchoHome dashboard]({panel})"
        return await self._post("/core/api/services/persistent_notification/create",
                                {"notification_id": NOTIFICATION_ID,
                                 "title": title, "message": message})

    async def clear(self) -> bool:
        return await self._post("/core/api/services/persistent_notification/dismiss",
                                {"notification_id": NOTIFICATION_ID})

    async def sync(self) -> None:
        """One pass: raise, replace or clear the notification if what is on
        offer changed since the last one sent.

        A latest version not known yet (just after a start, before the first
        release poll fills the firmware cache) decides nothing: read as
        "nothing on offer" it would clear the notification, and raise a
        dismissed one again once the poll came."""
        loop = asyncio.get_running_loop()
        known = await loop.run_in_executor(None, _known)
        if known is None:
            return
        rows, firmware_latest, emos_latest, sent = known
        what = decide(rows, firmware_latest, emos_latest)
        sig = what[0] if what else ""
        if sig == sent:
            return
        ok = await self.show(what[1], what[2]) if what else await self.clear()
        if ok:
            await loop.run_in_executor(None, db.set_config, _SIG_KEY, sig)
            log.info(f"[ha_notify] {'raised: ' + sig if sig else 'cleared'}")


async def run() -> None:
    """Every EVERY_S while the setup tool runs; nothing without a Supervisor."""
    token = os.environ.get("SUPERVISOR_TOKEN") or ""
    if not token:
        log.info("[ha_notify] no Supervisor (not the add-on): no update notifications")
        return
    async with aiohttp.ClientSession() as session:
        notifier = Notifier(session, token)
        while True:
            if em_releases.check_interval() > 0:
                try:
                    await notifier.sync()
                except Exception as e:  # noqa: BLE001 - a notification must never stop the tool
                    log.warning(f"[ha_notify] {e!r}")
            await asyncio.sleep(EVERY_S)
