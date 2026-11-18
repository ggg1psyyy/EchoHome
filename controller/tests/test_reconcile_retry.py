"""
A connect-time reconcile that does not finish must not hold its debounce.

Approving a device bounces its link about 3s after it connects, to hand it
credentials, which lands in the middle of the first reconcile. The redial was
then turned away by the failed run's stamp for 15 minutes (035T, 2026-10-24,
when the run was the wake word assets' push; since EchoHome the start script
is the first step).
"""
import asyncio
import time

import em_payloads
import em_reconcile
import em_state


class Live:
    android_userspace = False


def _run(coro):
    return asyncio.new_event_loop().run_until_complete(coro)


def _steps(monkeypatch, first):
    calls = []

    async def start_script(live, device_id):
        calls.append(("first", live))
        return await first(device_id, live)

    async def ok(live, device_id):
        calls.append(("other", live))
        return True

    monkeypatch.setattr(em_payloads, "sync_start_script", start_script)
    monkeypatch.setattr(em_payloads, "sync_endpoints", ok)
    return calls


def _fresh(monkeypatch, device_id, live):
    monkeypatch.setattr(em_reconcile, "stamps", {})
    monkeypatch.setattr(em_state, "devices", {device_id: live})


def test_a_finished_run_keeps_the_full_debounce(monkeypatch):
    live = Live()
    _fresh(monkeypatch, "D", live)

    async def fine(device_id, live):
        return True
    _steps(monkeypatch, fine)

    _run(em_reconcile.on_connect("D", live))
    now = time.monotonic()
    assert not em_reconcile._due("D", now + em_reconcile.RETRY_S + 5)


def test_a_failed_step_retries_after_a_minute_not_fifteen(monkeypatch):
    live = Live()
    _fresh(monkeypatch, "D", live)

    async def fails(device_id, live):
        return False
    calls = _steps(monkeypatch, fails)

    _run(em_reconcile.on_connect("D", live))
    assert [c[0] for c in calls] == ["first", "other"], "the others still run"
    now = time.monotonic()
    assert not em_reconcile._due("D", now + 1)
    assert em_reconcile._due("D", now + em_reconcile.RETRY_S + 1)


def test_a_raising_step_counts_as_not_finished(monkeypatch):
    live = Live()
    _fresh(monkeypatch, "D", live)

    async def boom(device_id, live):
        raise RuntimeError("shell gone")
    _steps(monkeypatch, boom)

    _run(em_reconcile.on_connect("D", live))
    assert em_reconcile._due("D", time.monotonic() + em_reconcile.RETRY_S + 1)


def test_a_device_that_redialled_mid_run_is_reconciled_again_at_once(monkeypatch):
    old, new = Live(), Live()
    _fresh(monkeypatch, "D", old)

    async def cut_off(device_id, live):
        if live is old:
            # The credential bounce: the device redials while this push runs,
            # and that connect is turned away by the stamp this run holds.
            em_state.devices["D"] = new
            assert not em_reconcile._due("D", time.monotonic())
            return False
        return True
    calls = _steps(monkeypatch, cut_off)

    async def go():
        await em_reconcile.on_connect("D", old)
        for _ in range(20):
            await asyncio.sleep(0)
    _run(go())
    assert ("first", new) in calls, "the new connection must be reconciled now"
    assert not em_reconcile._due("D", time.monotonic() + em_reconcile.RETRY_S + 5), (
        "a rerun that finished keeps the full debounce")


def test_none_from_a_step_is_success(monkeypatch):
    live = Live()
    _fresh(monkeypatch, "D", live)

    async def silent(device_id, live):
        return None
    _steps(monkeypatch, silent)
    assert _run(em_reconcile._reconcile_payloads("D", live)) is True
