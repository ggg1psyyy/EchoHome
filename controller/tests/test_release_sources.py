"""
EchoHome fetches its two kinds of release from two repositories: firmware
(and the setup tool's own update notice) from EchoHome, emOS from upstream
EchoMuse, which builds and hardware-tests it.

The hazard this guards: before the split, one `github_repo` fed both, and
the database cached the newest firmware it saw there. EchoHome publishes no
firmware release yet, so fetch_firmware finds none and firmware_to_install
falls back to that cache -- which would install upstream's firmware on an
EchoHome Echo. A cache is therefore trusted only when stamped with the
repository it came from.
"""

import asyncio

import pytest

import em_releases
import em_routes_system


# ── owner/name, from GitHub's rules ─────────────────────────────────────────
#
# Owner: 1-39 characters, alphanumerics and hyphens, no leading, trailing or
# doubled hyphen. Name: 1-100 of alphanumerics, '-', '_', '.', but not '.'
# or '..'. Anything else would also change the API URL's path.

@pytest.mark.parametrize("value", [
    "ggg1psyyy/EchoHome", "wilbowes/EchoMuse", "a/b", "A-b-C/x.y_z-1",
    "a" * 39 + "/" + "n" * 100, "o/...", "o/.github",
])
def test_a_github_repo_is_accepted(value):
    assert em_releases.valid_repo(value)


@pytest.mark.parametrize("value", [
    "", "EchoHome", "a/b/c", "/b", "a/", "-a/b", "a-/b", "a--b/c",
    "a" * 40 + "/b", "a/" + "n" * 101, "a/.", "a/..", "a b/c", "a/b c",
    "a/b?x=1", "a/b#x", "a/../../x", "ä/b", None, 42,
])
def test_anything_else_is_refused(value):
    assert not em_releases.valid_repo(value)


@pytest.fixture
def stored(monkeypatch):
    config: dict = {}
    monkeypatch.setattr(em_releases.db, "get_config",
                        lambda k, d=None: config.get(k, d))
    monkeypatch.setattr(em_releases, "_release_cache", {})
    monkeypatch.setattr(em_releases, "_release_cache_ts", 0.0)
    monkeypatch.setattr(em_releases, "_release_cache_repo", "")
    return config


def test_the_defaults_split_firmware_from_emos(stored):
    assert em_releases.firmware_repo() == "ggg1psyyy/EchoHome"
    assert em_releases.emos_repo() == "wilbowes/EchoMuse"


def test_the_old_shared_key_no_longer_decides(stored):
    stored["github_repo"] = "wilbowes/EchoMuse"
    assert em_releases.firmware_repo() == em_releases.FIRMWARE_REPO


def test_a_stored_value_that_is_not_a_repo_falls_back(stored):
    stored["firmware_repo"] = "x/../../evil"
    assert em_releases.firmware_repo() == em_releases.FIRMWARE_REPO
    stored["emos_repo"] = "someone/fork"
    assert em_releases.emos_repo() == "someone/fork"


class _Resp:
    status = 404

    async def __aenter__(self):
        return self

    async def __aexit__(self, *a):
        return False


@pytest.fixture
def asked(monkeypatch):
    """Every GitHub URL a fetch asks for; each answers 404."""
    urls = []

    class Session:
        async def __aenter__(self):
            return self

        async def __aexit__(self, *a):
            return False

        def get(self, url, **kw):
            urls.append(url)
            return _Resp()
    monkeypatch.setattr(em_releases.aiohttp, "ClientSession", lambda *a, **k: Session())
    return urls


def test_each_fetch_asks_its_own_repository(stored, asked):
    stored["update_check_interval"] = "3600"
    asyncio.run(em_releases.fetch_firmware())
    asyncio.run(em_releases.fetch_emos())
    asyncio.run(em_releases.fetch_controller(force=True))
    assert asked[0].startswith("https://api.github.com/repos/ggg1psyyy/EchoHome/releases")
    assert asked[1].startswith("https://api.github.com/repos/wilbowes/EchoMuse/releases")
    assert "/repos/ggg1psyyy/EchoHome/" in asked[2]


def _upstream_cache(stored, **stamp):
    stored.update(latest_version="v2.28.0", latest_binary_url="https://up/server",
                  last_update_check="9999999999", update_check_interval="3600", **stamp)


def test_an_unstamped_cache_is_not_offered(stored, asked):
    """A database from before the split holds upstream's newest firmware."""
    _upstream_cache(stored)
    assert asyncio.run(em_releases.cached_firmware()) is None


def test_another_repositorys_cache_is_never_installed(stored, asked):
    _upstream_cache(stored, latest_repo="wilbowes/EchoMuse")
    assert asyncio.run(em_releases.firmware_to_install()) is None


def test_this_repositorys_cache_still_is(stored, asked):
    _upstream_cache(stored, latest_repo=em_releases.FIRMWARE_REPO)
    got = asyncio.run(em_releases.firmware_to_install())
    assert got["version"] == "v2.28.0"


def test_changing_the_repository_drops_the_memory_cache(stored, asked):
    _upstream_cache(stored, latest_repo=em_releases.FIRMWARE_REPO)
    assert asyncio.run(em_releases.cached_firmware())["version"] == "v2.28.0"
    stored["firmware_repo"] = "someone/fork"
    assert asyncio.run(em_releases.cached_firmware()) is None


def test_a_successful_fetch_stamps_its_repository():
    src = em_releases.__loader__.get_source("em_releases")
    assert 'db.set_config("latest_repo",       repo)' in src
    assert 'db.set_config("latest_controller_repo", repo)' in src


class _Req:
    def __init__(self, body):
        self._body = body

    async def json(self):
        return self._body


def test_a_bad_repository_is_refused_before_anything_is_written(monkeypatch):
    written = []
    monkeypatch.setattr(em_routes_system.db, "set_config",
                        lambda k, v: written.append((k, v)))
    handler = em_routes_system._patch_system_config.__wrapped__
    resp = asyncio.run(handler(_Req({"update_check_interval": "60",
                                     "emos_repo": "not a repo"})))
    assert resp.status == 400 and written == []
