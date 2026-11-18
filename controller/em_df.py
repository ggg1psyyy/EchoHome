"""
Free space on an Echo, from its own `busybox df -m`: the OTA and the emOS
update both check it before sending a file (em_ota, em_emos_update). Was in
em_oww_assets with the wake word asset sync it also served, removed in
EchoHome.
"""

from __future__ import annotations


def parse_free_mb(df_line: str) -> int | None:
    """
    Free megabytes from a `busybox df -m` data line.

    Parsed here rather than with awk because the column INDEX is not stable:
    busybox wraps a long filesystem name onto its own line, so the data row is
    sometimes "1010 648 346 65% /data" and sometimes
    "/dev/block/x 1010 648 346 65% /data". An awk $4 gets one of those right
    and silently returns "65%" for the other — which parsed as no reading at
    all, and would have disabled the free-space check without saying so.

    The use-percentage column is the anchor: available is always the field
    immediately before it.
    """
    fields = (df_line or "").split()
    for i, f in enumerate(fields):
        if f.endswith("%") and i > 0 and fields[i - 1].isdigit():
            return int(fields[i - 1])
    return None
