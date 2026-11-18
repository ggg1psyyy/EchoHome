"""
The controller's live, process-wide state: the device registry, shell
session coordination, the OWW model caches, the wake arbiter, the loop
lag peak, and this controller's link ports and TLS directory.

It is a module of its own because em_controller cannot hold it. em_start.py
execvp's em_controller.py, so the running controller is `__main__`, and any
other module that does `import em_controller` loads a SECOND copy whose
state is all defaults (#306: /api/system/status read a loop lag peak of 0.0
while the log beside it showed an 881ms stall). This module is only ever
imported by name, so every reader and writer gets the same object.

Holds state and nothing that acts on it beyond keeping it consistent. No
aiohttp, websockets or openwakeword import, so the suite can import it
without stubs.
"""

from __future__ import annotations

import asyncio
from typing import TYPE_CHECKING, Any


if TYPE_CHECKING:
    from em_controller import Device


# The live device registry — keyed by device_id (ro.serialno).
devices: dict[str, "Device"] = {}

# Device id -> the board its firmware reported (em_boards), connected or not.
# Seeded from the database at startup so satellites started before their Echo
# connects show the right model.
boards: dict[str, str] = {}


# Shell session coordination — keyed by device_id.
#
# shell_pending:   Future resolved with the device ws when handle_shell receives it.
# shell_dashboard: dashboard WebSocket set by em_api for interactive sessions.
shell_pending:   dict[str, asyncio.Future] = {}
shell_dashboard: dict[str, Any]            = {}


# Peak event-loop lag observed since start, in ms (see
# em_controller.event_loop_lag_monitor). Read by the API for
# /api/system/status and the support bundle.
loop_lag_peak_ms: float = 0.0


# Device-link TLS material directory — set by em_controller.main() once
# em_pki.ensure_pki() succeeds. None = TLS listener not running (no
# cryptography package / setup failure); credential endpoints then 503.
tls_dir: str | None = None


def set_tls_dir(path: str) -> None:
    global tls_dir
    tls_dir = path


# This controller's own address and device ports: the defaults for an entry
# in controllerEndpoints, and what the dashboard offers to fill in.
# tls_port is 0 when the wss listener is not running.
link = {"ip": "", "port": 8767, "tls_port": 0}


def set_link_ports(ip: str, port: int, tls_port: int) -> None:
    link.update(ip=ip or "", port=port, tls_port=tls_port)


# The controller's mDNS responder (AsyncZeroconf, bound to SERVER_IP's
# interface by em_controller.main), shared with what else announces from it:
# the Voice Incubator satellites of Echoes it presents (em_incubator).
zeroconf = None
