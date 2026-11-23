"""
em_auth.ws_resolve_session: the dashboard's ?token= wins over a cookie.

Under ingress every app shares Home Assistant's origin, so a "session"
cookie another app set reaches the setup tool. Read first, it refused the
dashboard's valid token on every WebSocket (the shell, 2026-11-23).
"""

import asyncio

from aiohttp.test_utils import make_mocked_request

import em_auth

GOOD, FOREIGN = "a" * 64, "b" * 64
USER = {"id": 1, "username": "admin", "role": "admin"}


def _resolve(monkeypatch, path, cookie=None):
    monkeypatch.setattr(em_auth.db, "get_session",
                        lambda t: {"user_id": 1} if t == GOOD else None)
    monkeypatch.setattr(em_auth.db, "get_user_by_id",
                        lambda i: USER if i == 1 else None)
    headers = {"Cookie": f"{em_auth.AUTH_COOKIE}={cookie}"} if cookie else {}
    req = make_mocked_request("GET", path, headers=headers)
    return asyncio.run(em_auth.ws_resolve_session(req))


def test_a_foreign_cookie_does_not_hide_the_token(monkeypatch):
    got = _resolve(monkeypatch, f"/api/events?token={GOOD}", cookie=FOREIGN)
    assert got and got["token"] == GOOD


def test_the_cookie_still_counts_without_a_token(monkeypatch):
    assert _resolve(monkeypatch, "/api/events", cookie=GOOD)["role"] == "admin"


def test_a_good_cookie_rescues_a_stale_token(monkeypatch):
    assert _resolve(monkeypatch, f"/api/events?token={FOREIGN}", cookie=GOOD)


def test_nothing_valid_is_refused(monkeypatch):
    assert _resolve(monkeypatch, f"/api/events?token={FOREIGN}", cookie=FOREIGN) is None
    assert _resolve(monkeypatch, "/api/events") is None
