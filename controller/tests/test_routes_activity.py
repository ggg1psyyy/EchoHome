"""/api/devices/{id}/activity: the hourly metrics and the turns a controller
recorded before EchoHome, aggregated. Documented in docs/configuration.md."""

import asyncio
import json

import em_routes_activity as A


class _Req:
    def __init__(self, query):
        self.match_info = {"id": "dev1"}
        self.query = query


def _call(monkeypatch, query, turns=(), metrics=()):
    monkeypatch.setattr(A.db, "get_turns", lambda i, n, since: list(turns))
    monkeypatch.setattr(A.db, "get_wake_counters", lambda i, since: [])
    monkeypatch.setattr(A.db, "get_device_metrics", lambda i, since: list(metrics))
    return asyncio.run(A._get_device_activity.__wrapped__(_Req(query)))


def test_an_echo_with_no_turns_still_gets_its_metrics(monkeypatch):
    row = {"hour": 1, "cpu_pct": 12.0}
    resp = _call(monkeypatch, {"days": "3"}, metrics=[row])
    assert resp.status == 200
    body = json.loads(resp.text)
    assert body["metrics"] == [row]


def test_days_must_be_a_number(monkeypatch):
    assert _call(monkeypatch, {"days": "a week"}).status == 400
