"""
The per-day activity aggregates (/api/devices/{id}/activity): the hourly
hardware metrics, and the voice turns a controller recorded before EchoHome
removed its voice path. The Activity tab, the turn and recording routes and
the wake samples went with it.
"""

from __future__ import annotations

import asyncio
import logging
import time

from aiohttp import web

import em_auth as auth
import em_db as db
import em_http

log = logging.getLogger("echohome.api")


def register(app: web.Application) -> None:
    app.router.add_get("/api/devices/{id}/activity",      _get_device_activity)


@auth.require_auth
async def _get_device_activity(request: web.Request) -> web.Response:
    """GET /api/devices/{id}/activity?days=7 — aggregated activity stats
    for trend review: per-day turn buckets (counts, outcomes, latency
    percentiles, wake scores, underruns), per-wake-model rollups, hourly
    near-miss counters, and hourly hardware metrics (CPU/RAM/storage/RSSI)."""
    device_id = request.match_info["id"]
    try:
        days = min(int(request.query.get("days", 7)), 180)
    except ValueError:
        return em_http.error("bad_request", "days must be an integer", 400)
    since = time.time() - days * 86400

    loop     = asyncio.get_event_loop()
    turns    = await loop.run_in_executor(
        None, lambda: db.get_turns(device_id, 50_000, since)
    )
    counters = await loop.run_in_executor(
        None, lambda: db.get_wake_counters(device_id, since)
    )
    metrics  = await loop.run_in_executor(
        None, lambda: db.get_device_metrics(device_id, since)
    )

    def pct(sorted_vals, p):
        if not sorted_vals:
            return None
        return sorted_vals[min(len(sorted_vals) - 1, int(len(sorted_vals) * p))]

    # Per-day buckets (local time), oldest first.
    day_buckets: dict[str, list[dict]] = {}
    for t in turns:
        day = time.strftime("%Y-%m-%d", time.localtime(t["ts"]))
        day_buckets.setdefault(day, []).append(t)

    days_out = []
    for day in sorted(day_buckets):
        ts_list   = day_buckets[day]
        ok        = [t for t in ts_list if t["outcome"] == "ok"]
        totals    = sorted(t["total_ms"] for t in ok if (t["total_ms"] or 0) > 0)
        scores    = [t["wake_score"] for t in ts_list if t["wake_score"] is not None]
        underruns = sum(t["underruns"] or 0 for t in ts_list)
        outcomes: dict[str, int] = {}
        for t in ts_list:
            outcomes[t["outcome"] or "?"] = outcomes.get(t["outcome"] or "?", 0) + 1
        days_out.append({
            "date":           day,
            "turns":          len(ts_list),
            "ok":             len(ok),
            "outcomes":       outcomes,
            "total_ms_p50":   pct(totals, 0.50),
            "total_ms_p95":   pct(totals, 0.95),
            "wake_score_avg": round(sum(scores) / len(scores), 3) if scores else None,
            "wake_score_min": round(min(scores), 3) if scores else None,
            "underruns":      underruns,
        })

    # Per-wake-model rollup — supports A/B-ing custom OWW models.
    models: dict[str, dict] = {}
    for t in turns:
        if not t["wake_model"]:
            continue
        m = models.setdefault(
            t["wake_model"], {"turns": 0, "score_sum": 0.0, "score_min": None}
        )
        m["turns"] += 1
        if t["wake_score"] is not None:
            m["score_sum"] += t["wake_score"]
            m["score_min"] = (
                t["wake_score"] if m["score_min"] is None
                else min(m["score_min"], t["wake_score"])
            )
    models_out = {
        name: {
            "turns":     m["turns"],
            "score_avg": round(m["score_sum"] / m["turns"], 3) if m["turns"] else None,
            "score_min": m["score_min"],
        }
        for name, m in models.items()
    }

    # On-device shadow comparison (schema v13) — the verdict, computed here so
    # a reader is not left to derive it from raw columns.
    #
    # Denominator is turns where the device was KNOWN to be scoring
    # (dev_shadow=1); a NULL score on those is a genuine miss, whereas a NULL
    # anywhere else is absence of data and must not be counted either way.
    scoring    = [t for t in turns if (t["dev_shadow"] or 0) == 1]
    # A turn is only COMPARABLE if the device was scoring against a bar this
    # controller's wake would have cleared. During playback the controller drops
    # to bargeInThreshold, so a turn that fired at 0.055 was never something a
    # device scoring against 0.5 could have caught — counting those as misses
    # made the agreement figure pessimistic, which is how this was found.
    # A device that reports no threshold (older firmware) is also not comparable:
    # unknown, rather than guessed at.
    def _comparable(t) -> bool:
        dev_thr = t["dev_threshold"]
        if dev_thr is None:
            return False
        wake_thr = t["wake_threshold"]
        return wake_thr is not None and wake_thr >= dev_thr

    compared   = [t for t in scoring if _comparable(t)]
    incomparable = len(scoring) - len(compared)
    agreed     = [t for t in compared if t["dev_wake_score"] is not None]
    deltas     = sorted(t["dev_wake_delta_ms"] for t in agreed
                        if t["dev_wake_delta_ms"] is not None)
    dev_scores = [t["dev_wake_score"] for t in agreed]
    crossings  = sum(r["dev_crossings"] or 0 for r in counters)
    shadow_out = {
        "turns_scoring":  len(scoring),
        "turns_compared": len(compared),
        # Turns where the device was scoring but the comparison is not valid —
        # the controller used a lower (barge-in) bar, or the device's threshold
        # is unknown. Reported rather than hidden: a large number here means the
        # agreement figure is describing a small slice of reality.
        "not_comparable": incomparable,
        "agreed":         len(agreed),
        "missed":         len(compared) - len(agreed),
        "agreement_pct":  round(100.0 * len(agreed) / len(compared), 1) if compared else None,
        # Signed: negative means the device crossed FIRST, which is the
        # expected direction — it scores the frame it just captured while the
        # controller scores the same frame after a network hop.
        "delta_ms_p50":   pct(deltas, 0.50),
        "delta_ms_p95":   pct(deltas, 0.95),
        "dev_score_avg":  round(sum(dev_scores) / len(dev_scores), 3) if dev_scores else None,
        "dev_score_min":  round(min(dev_scores), 3) if dev_scores else None,
        "crossings":      crossings,
        # Crossings that never matched a turn. This is the false-accept side of
        # the comparison, which per-turn rows structurally cannot show — but it
        # is an ESTIMATE, not a count: the hourly counters and the turn rows are
        # pruned on different schedules (WAKE_COUNTER_RETENTION_DAYS vs
        # TURN_RETENTION rows), so over a long window this drifts. Treat a
        # small number as noise and a large one as worth investigating.
        "unmatched_crossings": max(0, crossings - len(agreed)),
        "frames":         sum(r["dev_frames"] or 0 for r in counters),
        # Nonzero drops mean the device could not keep up, so every figure
        # above is describing a subset of the audio.
        "drops":          sum(r["dev_drops"] or 0 for r in counters),
    }

    return em_http.ok({
        "days":          days_out,
        "wake_models":   models_out,
        "wake_counters": [dict(r) for r in counters],
        "metrics":       metrics,
        "shadow":        shadow_out,
    })
