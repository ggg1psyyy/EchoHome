"""
em_esphome_mdns.py — each Echo's ESPHome record, announced on this network
=========================================================================

An EchoHome Echo announces its own ESPHome device (_esphomelib._tcp) so Home
Assistant discovers it, as it discovers an ESPHome node. Where the Echo and
Home Assistant are on different VLANs that multicast never arrives, so the
Echo also reports the record (esphome_mdns) and the controller announces the
same record, at the Echo's address, on its own network, as em_incubator does
for the satellite.

The record is the Echo's: name, port and TXT exactly as it sent them, checked
against the spec's limits, nothing derived here. Announced while the Echo is
connected, withdrawn when it goes or is deleted; Home Assistant needs it only
to discover the Echo, and keeps the device it added.
"""

from __future__ import annotations

import logging
import re
import socket
from typing import Optional

from zeroconf import ServiceInfo

import em_state

log = logging.getLogger("echohome.esphome_mdns")

SERVICE_TYPE = "_esphomelib._tcp.local."

# The node name is a DNS label (RFC 1035 §2.3.1, ESPHome's own rule for
# names): letters, digits and hyphens, at most 63, no leading or trailing
# hyphen. HA takes the host name as the device name it expects.
_LABEL = re.compile(r"^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$")
MAX_TXT = 32  # ESPHome announces under 15; a bound on what one Echo may send

# device_id → (the record and address it was announced with, the ServiceInfo)
_announced: dict[str, tuple[tuple, ServiceInfo]] = {}


def parse(msg: dict) -> Optional[tuple[str, int, dict[str, str]]]:
    """The record in an esphome_mdns message, or None if it is not one.

    TXT strings follow RFC 6763: each at most 255 bytes (§6.1), a key of
    printable US-ASCII other than "=" (§6.4), each key once (§6.4: only the
    first counts, so a second is refused rather than guessed at).
    """
    name, port, txt = msg.get("name"), msg.get("port"), msg.get("txt")
    if not isinstance(name, str) or not _LABEL.match(name):
        return None
    if isinstance(port, bool) or not isinstance(port, int) or not 1 <= port <= 65535:
        return None
    if not isinstance(txt, list) or len(txt) > MAX_TXT:
        return None
    props: dict[str, str] = {}
    for r in txt:
        if not isinstance(r, str) or len(r.encode("utf-8")) > 255:
            return None
        key, sep, value = r.partition("=")
        if not sep or not key or key in props:
            return None
        if any(not (0x20 <= ord(c) <= 0x7E) for c in key):
            return None
        props[key] = value
    if not props.get("mac"):
        return None  # HA aborts a discovery without it (mdns_missing_mac)
    return name, port, props


def _info(name: str, port: int, props: dict[str, str], address: str) -> ServiceInfo:
    return ServiceInfo(
        SERVICE_TYPE,
        f"{name}.{SERVICE_TYPE}",
        addresses=[socket.inet_aton(address)],
        port=port,
        properties=props,
        server=f"{name}.local.",
    )


async def on_esphome_mdns(device, msg: dict) -> None:
    """em_controller's handler for esphome_mdns: announce the Echo's record
    at its address, replacing the one before if anything changed."""
    device_id = device.device_id
    rec = parse(msg)
    if rec is None:
        log.warning(f"[{device_id}] esphome_mdns refused: not a record")
        return
    address = getattr(device, "ip", None)
    azc = em_state.zeroconf
    if azc is None or not address:
        return
    try:
        info = _info(*rec, address)
    except OSError:
        log.warning(f"[{device_id}] esphome_mdns: no IPv4 address to announce at")
        return
    key = (rec[0], rec[1], tuple(sorted(rec[2].items())), address)
    old = _announced.get(device_id)
    if old is not None and old[0] == key:
        return
    await _withdraw(device_id)
    try:
        await azc.async_register_service(info, allow_name_change=False)
    except Exception as e:
        log.warning(f"[{device_id}] ESPHome mDNS registration failed: {e}")
        return
    _announced[device_id] = (key, info)
    log.info(f"[{device_id}] mDNS registered: {info.name} → the Echo, port {info.port}")


async def _withdraw(device_id: str) -> None:
    old = _announced.pop(device_id, None)
    azc = em_state.zeroconf
    if old is not None and azc is not None:
        try:
            await azc.async_unregister_service(old[1])
        except Exception:
            pass


async def device_disconnected(device_id: str) -> None:
    await _withdraw(device_id)


async def device_deleted(device_id: str) -> None:
    await _withdraw(device_id)
