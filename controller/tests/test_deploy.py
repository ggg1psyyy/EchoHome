"""
Deployment-shape guards — not logic tests. The controller Dockerfile
COPYs each module explicitly, so a new em_*.py that works fine on bare
metal crash-loops the container at import time if the COPY line is
forgotten (bitten by em_scenes.py 2026-07-10 and em_oww_models.py
2026-07-19).
"""

import _sources
from _emos_ref import emos_file
import ast
import re
from pathlib import Path

CONTROLLER = Path(__file__).resolve().parents[1]


def test_dockerfile_copies_every_controller_module():
    dockerfile = (CONTROLLER / "Dockerfile").read_text()
    copied = set(re.findall(r"^COPY\s+(\S+\.py)\s", dockerfile, re.M))
    modules = {p.name for p in CONTROLLER.glob("em_*.py")} | {"version.py"}
    missing = sorted(modules - copied)
    assert not missing, (
        f"Dockerfile is missing COPY lines for {missing} — the container "
        f"will crash-loop at import time"
    )


DOCKERFILE_MAX_INSTRUCTIONS = 120


def test_the_dockerfile_stays_clear_of_the_layer_limit():
    """
    One COPY per module is kept on purpose: what goes into the image stays
    readable line by line. Each is a layer, and the builder refuses an image
    past 127 ("max depth exceeded"), which is how EchoMuse's #1251 failed
    before #1252 went to one `COPY em_*.py version.py ./`. At 120 this fails
    first, with room left: fold the COPY lines into a glob then (#1252), and
    keep the module check above meaningful (match against the glob).
    """
    lines = (CONTROLLER / "Dockerfile").read_text().replace("\\\n", " ").splitlines()
    keywords = ("FROM", "RUN", "COPY", "ADD", "ENV", "WORKDIR", "ARG", "LABEL",
                "EXPOSE", "CMD", "ENTRYPOINT", "USER", "VOLUME", "HEALTHCHECK",
                "SHELL", "STOPSIGNAL", "ONBUILD")
    n = sum(1 for l in lines if l.split(maxsplit=1)[:1] and l.split(maxsplit=1)[0].upper() in keywords)
    assert n <= DOCKERFILE_MAX_INSTRUCTIONS, (
        f"controller/Dockerfile has {n} instructions, over "
        f"{DOCKERFILE_MAX_INSTRUCTIONS}: the builder stops at 127 layers. Fold "
        f"the per-module COPY lines into one glob (EchoMuse #1252).")


def test_ci_runs_every_dashboard_test():
    """
    The dashboard job lists its tests by hand, so a new one that nobody adds to
    the list passes locally and never runs in CI — green forever, testing
    nothing. `device_tools.test.mjs` sat that way: written, passing, and run by
    no job but a developer's own shell.

    Same shape as the Dockerfile guard above, for the same reason: an
    enumerated list is only honest while something checks it.
    """
    ci = (CONTROLLER.parent / ".github" / "workflows" / "ci.yml").read_text()
    listed = set(re.findall(r"node\s+controller/tests/(\S+\.test\.mjs)", ci))
    on_disk = {p.name for p in (CONTROLLER / "tests").glob("*.test.mjs")}
    assert on_disk <= listed, (
        f"dashboard tests on disk but never run by CI: {sorted(on_disk - listed)}"
    )


def test_dashboard_bundle_is_cache_busted():
    """
    /dashboard must not hand the browser a bare /static/dashboard.js URL.

    aiohttp's add_static sends Last-Modified and ETag but no Cache-Control, so
    browsers apply heuristic freshness and serve a stale bundle without
    revalidating. That failure is invisible server-side — deploy correct, file
    correct, compiled bundle correct, browser showing the previous UI — so it
    reads as "my change didn't work" and sends you hunting in the wrong place.
    Asserted at the source level because the alternative is starting an aiohttp
    app, which this suite deliberately does not do.
    """
    import re
    from pathlib import Path

    handler = _sources.def_source("_serve_dashboard")

    assert "dashboard.js?v=" in handler, \
        "the bundle URL must carry a cache-busting token"
    assert "no-cache" in handler, \
        "dashboard.html itself must be revalidated, or the new URL is never seen"
    # A version-string token would not change between two local "dev" builds;
    # mtime changes on every rebuild.
    # The token must be an mtime, not a version string: `version.py` reports
    # "dev" for every local build and so would never change between two of
    # them. The stamp moved into bundle_version() when /api/system/status
    # started publishing it for stale-tab detection, so the requirement is now
    # that the handler uses that ONE definition and that the definition is an
    # mtime — see test_the_bundle_stamp_has_exactly_one_definition.
    assert "bundle_version()" in handler, \
        "the handler must take its token from bundle_version()"
    helper = _sources.def_source("bundle_version")
    assert "st_mtime" in helper, \
        "cache-bust on the bundle's mtime, not on a version string"


def test_dashboard_paths_are_ingress_safe():
    """
    As a Home Assistant add-on, the dashboard is mounted below a generated
    path (Ingress), not at the site root — an absolute "/static/..." or
    "/api/..." reference bypasses that path straight to the root and 404s.
    Every asset/API reference must therefore be relative, resolved against
    the <base href> em_api.py injects for the add-on case.

    Source-level because starting an aiohttp app + browser is outside what
    this suite does elsewhere (see test_dashboard_bundle_is_cache_busted).
    """
    static = CONTROLLER / "static"
    index = (static / "index.html").read_text()
    dashboard = (static / "dashboard.html").read_text()
    jsx = (static / "dashboard.jsx").read_text()
    api = (CONTROLLER / "em_routes_static.py").read_text()
    config = (CONTROLLER / "config.yaml").read_text()

    for name, html in (("index.html", index), ("dashboard.html", dashboard)):
        assert 'href="/static/' not in html, f"{name} has an absolute /static href"
        assert 'src="/static/' not in html, f"{name} has an absolute /static src"
        assert "url('/static/" not in html, f"{name} has an absolute /static url()"

    assert "'/api/" not in index, \
        "index.html must fetch relative api/... paths, not /api/..."
    assert "location.replace('/dashboard')" not in index, \
        "index.html must redirect to a relative dashboard path"

    assert "function ingressPath(path)" in jsx, \
        "dashboard.jsx must relativize absolute paths for ingress"
    assert "function ingressWebSocketUrl(path)" in jsx, \
        "dashboard.jsx must build ingress-relative WebSocket URLs"
    assert "document.baseURI" in jsx, \
        "the WebSocket URL must resolve against the injected <base href>"
    assert "fetch(ingressPath(path)" in jsx, \
        "the shared API helpers must route through ingressPath"
    assert "location.replace('/')" not in jsx, \
        "an absolute root redirect bypasses the ingress path"

    assert '"static/dashboard.js"' in api, \
        "the cache-bust replace must target the relative asset URL"
    assert 'web.HTTPFound(".")' in api, \
        "/setup must redirect relatively, or it bounces out of the ingress path"
    assert "_with_ingress_base" in api, \
        "index.html and dashboard.html must get a <base href> injected"
    assert 'request.headers.get("X-Ingress-Path"' in api, \
        "the base path must come from Home Assistant's ingress header"
    assert "ECHOMUSE_HOME_ASSISTANT_INGRESS" in config, \
        "config.yaml must set the env var that gates ingress-only mode"


def test_addon_image_is_published_not_built_on_the_user_machine():
    """
    Without an `image:` key Supervisor builds the Dockerfile on whatever
    the user runs Home Assistant on — an onnxruntime/ffmpeg build on a Pi —
    and cannot pass EM_CONTROLLER_VERSION, so the controller reports "dev"
    and the update notice goes quiet. The arch list must also stay within
    what controller-release.yml actually publishes.
    """
    config = (CONTROLLER / "config.yaml").read_text()
    assert re.search(r"^image:\s*\S+", config, re.M), \
        "config.yaml must pull the published image, not build on the user's machine"

    workflow = (CONTROLLER.parent / ".github/workflows/controller-release.yml").read_text()
    platforms = re.search(r"platforms:\s*(\S+)", workflow)
    assert platforms, "controller-release.yml no longer declares platforms"
    published = platforms.group(1)
    arch_block = re.search(r"^arch:\n((?:\s+-\s*\w+\n)+)", config, re.M)
    assert arch_block, "config.yaml has no arch list"
    declared = set(re.findall(r"-\s*(\w+)", arch_block.group(1)))
    # Home Assistant's arch names vs Docker's platform names.
    equivalent = {"aarch64": "linux/arm64", "amd64": "linux/amd64"}
    for arch in declared:
        assert equivalent.get(arch) in published, (
            f"config.yaml offers {arch} but controller-release.yml only "
            f"publishes {published} — that install would find no image"
        )


def test_release_notes_survive_the_whole_relay():
    """
    Release notes have to make it through four places to be useful: captured
    from the GitHub response, persisted, re-read into the cache after a
    restart, and rendered. Miss any one and the dashboard shows a version
    number with no way to judge it — which is the state this replaced.

    The restart path is the one worth pinning: the in-memory cache is
    populated from the DB when cold, so notes omitted there would appear on
    first poll and silently vanish on every controller restart until the next
    one.
    """
    from pathlib import Path
    fetch = _sources.def_source("fetch_firmware")
    assert 'release.get("body")' in fetch or '.get("body")' in fetch, \
        "the GitHub release body must be captured"
    assert 'set_config("latest_notes"' in fetch, "notes must be persisted"

    cached = _sources.def_source("cached_firmware")
    assert '"latest_notes"' in cached and 'stored["latest_notes"]' in cached, \
        "the DB-cache path must restore notes, or they vanish on restart"

    jsx = (Path(__file__).resolve().parent.parent / "static" / "dashboard.jsx").read_text()
    assert "release.notes" in jsx, "the dashboard must render the notes"


def test_release_workflow_publishes_the_tag_annotation():
    """
    The notes shown in the dashboard come from the annotated tag, so the
    workflow must publish that rather than only GitHub's generated commit
    list. If this drifts, every future release silently shows a commit dump to
    whoever is deciding whether to update.
    """
    from pathlib import Path
    wf = (Path(__file__).resolve().parent.parent.parent
          / ".github" / "workflows" / "release.yml").read_text()
    assert "body_path:" in wf, "the release must publish notes from a file"
    assert "%(contents)" in wf, "notes must come from the tag annotation"


def test_every_device_payload_has_an_update_path():
    """
    A payload installed only at provisioning drifts forever.

    This has now bitten twice: start_server.sh (Lounge was a revision behind
    Office, 2026-07-11) and the debloat pair (round 2 added a package and every
    fielded device needed a manual push, 2026-07-30). Both fixes were the same
    shape — an md5-compared sync riding the OTA — so this asserts that every
    file in device_payloads/ is named by a sync function, and fails when a
    fourth payload is added without one.
    """
    from pathlib import Path
    root = Path(__file__).resolve().parent.parent
    # The sync module, not em_api: the provisioning routes there name every
    # payload too, and "installed only by the wizard" is the case this catches.
    syncs = (root / "em_payloads.py").read_text()
    payloads = sorted(p.name for p in (root / "device_payloads").iterdir() if p.is_file())
    assert payloads, "device_payloads/ is empty — has it moved?"

    for name in payloads:
        assert name in syncs, (
            f"{name} has no update path: no sync in em_payloads.py references it. "
            f"A payload installed only by the provisioning wizard drifts on every "
            f"device already in the field."
        )


def test_stale_release_cache_is_not_returned_when_it_has_aged_out():
    """
    _get_cached_release used to fire a refresh into the background and return
    the STALE value. Two consequences, both seen on 2026-07-30: the dashboard
    reported "there's an update" only after someone pressed Check now, and an
    OTA pushed v2.9.9 while v2.9.10 was the current release.

    The refresh is now awaited when the DB cache has aged past the check
    interval, falling back to the stale value only if the fetch fails.
    """
    from pathlib import Path
    fn = _sources.def_source("cached_firmware")

    assert "await fetch_firmware()" in fn, \
        "an aged-out cache must be refreshed synchronously, not in the background"
    assert "create_task(fetch_firmware())" not in fn and "spawn(fetch_firmware())" not in fn, \
        "fire-and-forget refresh returns the stale value to this caller"


def test_release_change_is_pushed_to_open_dashboards():
    """A tab already showing the Updates panel should not sit on the old
    version until someone reloads."""
    from pathlib import Path
    root = Path(__file__).resolve().parent.parent
    api = _sources.all_source()
    assert '"type":         "release_update"' in api or '"release_update"' in api, \
        "a release change must be broadcast on the event stream"
    jsx = (root / "static" / "dashboard.jsx").read_text()
    # And the tab must keep asking, as a fallback for a missed event.
    upd = jsx[jsx.index("if (tab !== 'updates') return;"):]
    assert "setInterval" in upd[:600], \
        "the Updates tab must refresh while open, not fetch once on entry"


def test_controller_update_is_advisory_only():
    """
    The dashboard may TELL you a newer controller exists; it must never offer
    to apply it.

    The controller is a container the user owns and updates with their own
    docker tooling. An in-app update would have to restart the process serving
    the page, mid-request, with no way to report the outcome — and it is an
    explicit product decision (Wil, 2026-07-31) that this stays out of the
    interface. The notice is information for a decision the user takes
    elsewhere.
    """
    from pathlib import Path
    root = Path(__file__).resolve().parent.parent

    jsx = (root / "static" / "dashboard.jsx").read_text()
    start = jsx.index("{/* Controller update notice.")
    banner = jsx[start:jsx.index("{/* Summary */}", start)]
    for forbidden in ("API.post", "API.put", "API.delete", "onClick={doUpdate"):
        assert forbidden not in banner, (
            f"the controller update notice must not perform actions, found "
            f"{forbidden!r} — updating is the user's docker command to run"
        )

    api = _sources.all_source()
    assert 'add_get("/api/releases/controller"' in api, \
        "the controller release endpoint must be read-only (GET)"
    for verb in ("add_post", "add_put", "add_delete", "add_patch", "add_route"):
        assert f'{verb}("/api/releases/controller' not in api, \
            f"{verb} on /api/releases/controller would make the update actionable"


def test_controller_notes_come_from_the_tag_annotation():
    """
    controller-v* tags ship a GHCR image and no GitHub Release (CLAUDE.md,
    "Versioning / releases"), so the notes must be read from the annotated
    tag object. Reading them from the releases list would return the newest
    DEVICE firmware release instead — right shape, wrong product.
    """
    from pathlib import Path
    fn = _sources.def_source("fetch_controller")
    assert "GITHUB_TAGS_URL" in fn and "GITHUB_TAG_OBJECT_URL" in fn, \
        "controller notes must come from the tag annotation, not /releases"
    assert "GITHUB_API_URL" not in fn, \
        "that is the device firmware release feed, not the controller's"


def test_every_db_call_exists():
    """
    A typo'd db.<name> is invisible to pyflakes (it is a valid attribute
    expression) and raises AttributeError only on the request that uses it.
    That is the same shape as the NameError which stopped wake word
    fleet-wide on 2026-07-30: green CI, clean logs, broken at runtime.

    Written after db.get_global_config() — a function that has never existed —
    reached a live endpoint.
    """
    import ast
    from pathlib import Path
    root = Path(__file__).resolve().parent.parent

    # Every module that imports em_db as `db`, not only em_api: a function
    # moved out of em_api keeps its db calls.
    used = {
        n.attr for path in _sources.MODULES for n in ast.walk(_sources.tree(path))
        if isinstance(n, ast.Attribute)
        and isinstance(n.value, ast.Name) and n.value.id == "db"
    }
    db_tree = ast.parse((root / "em_db.py").read_text())
    defined = {
        n.name for n in db_tree.body
        if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef))
    } | {
        t.id for n in db_tree.body if isinstance(n, ast.Assign)
        for t in n.targets if isinstance(t, ast.Name)
    } | {
        # Annotated module constants, e.g. `MIGRATIONS: list[str] = [...]`.
        # Missing these produced a false positive on db.MIGRATIONS, which is
        # the failure mode that gets a guard disabled rather than fixed.
        n.target.id for n in db_tree.body
        if isinstance(n, ast.AnnAssign) and isinstance(n.target, ast.Name)
    }
    missing = sorted(used - defined)
    assert not missing, f"db.{{{', '.join(missing)}}} is called but em_db.py does not define it"


def test_no_unjustified_hardcoded_media_state():
    """
    Forbid the SHAPE, not just the instances.

    A hardcoded MediaPlayerState sent to HA asserts what the player is doing
    without asking em_player, so it is wrong whenever the guess is wrong — and
    it wins, because the feed announces PLAYING exactly once, at the start.

    This has now been the same bug twice: the turn-end IDLE (#53, "reports
    idle even though the music continues to play"), and then IDLE on every
    device volume report, which told Music Assistant the music had stopped
    while it was audibly playing. Fixing instances one at a time is how the
    second one survived the first fix, so this pins the rule.

    One remains legitimate: PLAYING, documented as optimistic — the feed
    pushes the authoritative state moments later. It covers the announce
    transition too, which used to send ANNOUNCING.

    ANNOUNCING is now FORBIDDEN rather than merely unused. It is a real
    protobuf value and the truthful one, and HA's esphome media_player has no
    mapping for it — `_STATES.from_esphome` raises `KeyError:
    <MediaPlayerState.ANNOUNCING: 4>` inside async_write_ha_state, on a path
    unrelated to the announcement's own result, so it surfaces only as "Task
    exception was never retrieved". Nothing in HA's UI says a word, which is
    why it survived.

    Anything else must go through _media_state_msg(), which reads em_player
    truth.
    """
    # Every module: a state message built outside em_esphome reaches HA too.
    src = _sources.all_source()

    code = "\n".join(
        l for l in src.splitlines() if not l.lstrip().startswith("#")
    )
    assert "MediaPlayerState.ANNOUNCING" not in code, (
        "HA cannot map ANNOUNCING — it raises KeyError in the entity state "
        "write on every announcement"
    )

    allowed = {"MediaPlayerState.PLAYING"}
    found = [
        line.strip()
        for line in src.splitlines()
        if "state=MediaPlayerState." in line
    ]
    offenders = [
        ln for ln in found
        if not any(a in ln for a in allowed)
    ]
    assert not offenders, (
        f"hardcoded media state(s) {offenders} — use _media_state_msg() so the "
        f"entity reflects what em_player is actually doing"
    )


def test_supervisor_log_path_matches_between_script_and_controller():
    """
    The supervisor writes its decisions to a persistent path and the
    controller reads them back from it. Two languages, one path — if they
    drift, the fetch silently returns nothing and a failed update stays
    unexplained, which is the exact failure this feature exists to remove.
    """
    from pathlib import Path
    root = Path(__file__).resolve().parent.parent

    script = (root / "device_payloads" / "start_server.sh").read_text()
    api = (root / "em_api.py").read_text()

    import re
    m = re.search(r"^SUP_LOG=(\S+)", script, re.M)
    assert m, "start_server.sh no longer defines SUP_LOG"
    script_path = m.group(1)

    m = re.search(r'^SUPERVISOR_LOG = "([^"]+)"', api, re.M)
    assert m, "em_api.py no longer defines SUPERVISOR_LOG"
    assert m.group(1) == script_path, (
        f"supervisor log path drifted: script writes {script_path}, "
        f"controller reads {m.group(1)}"
    )


def test_supervisor_log_is_persistent_and_bounded():
    """
    Two properties it cannot lose.

    PERSISTENT: under /data. /tmp is RAM-backed, so a log there is wiped by
    the reboot used to recover from the very failure it would explain.

    BOUNDED: these devices have ~350MB free and no operator. A crash-loop
    writing every few seconds must never be able to fill /data — so the trim
    happens BEFORE the append, not after.
    """
    from pathlib import Path
    import re
    script = (Path(__file__).resolve().parent.parent
              / "device_payloads" / "start_server.sh").read_text()

    m = re.search(r"^SUP_LOG=(\S+)", script, re.M)
    assert m.group(1).startswith("/data/"), \
        "the supervisor log must live on persistent storage, not /tmp"

    fn = script[script.index("sup_log() {"):]
    fn = fn[:fn.index("\n}")]
    trim = fn.index("SUP_MAX")
    append = fn.index('>> "$SUP_LOG"')
    assert trim < append, \
        "the size check must run before the append, or a crash-loop outruns it"


def test_a_failed_update_asks_for_the_supervisor_log():
    """
    The fetch cannot happen at failure time — the device is gone, which IS the
    problem. So every failure path must record that an explanation is owed,
    and the connect path must collect it.
    """
    from pathlib import Path
    api = (Path(__file__).resolve().parent.parent / "em_api.py").read_text()

    # The failure paths live in _run_update_locked, which is what awaits
    # _monitor_reconnect and decides what its result means. (_run_update is
    # now the queue wrapper in front of it.)
    monitor = _sources.def_source("em_ota._run_update_locked")
    assert monitor.count("supervisor_log_wanted.add") >= 2, (
        "both update-failure paths (auto-rollback and timeout) must request "
        "the supervisor log"
    )

    connect = api[api.index("async def notify_device_connected"):]
    connect = connect[:connect.index("\nasync def ", 1)]
    assert "_collect_supervisor_log" in connect, \
        "nothing collects the supervisor log when the device comes back"


def test_support_bundle_attributes_metrics_to_a_device():
    """
    `db.get_device_metrics` builds its own result dicts and does NOT include
    the device, so the support handler must attach it. Without that, every
    device's hourly CPU, memory and RTT pool into one flat anonymous list —
    present in the bundle, useless for diagnosis, and wrong in the quiet way
    where the file still looks full of data. Shipped like that in #63.
    """
    body = _sources.def_source("_get_support_bundle")
    call = re.search(r"metrics \+= \[(.*?)\]\n", body, re.S)
    assert call, "could not find where the support bundle collects metrics"
    assert "device_id" in call.group(1), (
        "support bundle metrics rows must carry device_id — "
        "get_device_metrics does not return it"
    )


def test_support_bundle_redacts_account_names():
    """
    Account names reach the bundle through ordinary log prose ("Shell session
    opened by wil"), which no quote, URL or identifier rule matches. The
    handler must therefore pass the user table to em_support; the redaction
    itself is tested in test_support.py.
    """
    body = _sources.def_source("_get_support_bundle")
    assert "accounts=" in body and "get_all_users" in body, (
        "the support bundle must pass the real accounts to em_support — "
        "there is nothing in a log line to pattern-match them by"
    )
    assert '"role"' in body, (
        "accounts must carry the role: a name is replaced by <admin>, and a "
        "positional alias would be one-to-one with a real person"
    )


def _fn_body(src: str, name: str) -> str:
    """The source of the one top-level function `name` in `src`, exactly.

    It used to slice to the next `async def`, so it took in whatever came
    after, and grew when the next function moved to another module: a guard
    on _fetch_emos_payload passed on a check that lived in the function after
    it (#921)."""
    found = [n for n in ast.parse(src).body
             if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef))
             and n.name == name]
    assert len(found) == 1, f"{name}: {len(found)} top-level definitions"
    return ast.get_source_segment(src, found[0])


def _js_fn_body(src: str, name: str) -> str:
    """Slice one JavaScript function out of dashboard.jsx, by matching braces.

    Braces rather than the next declaration, because these are nested inside
    components at arbitrary indentation — there is no top-level boundary to cut
    at, the way _fn_body has for Python. Strings and comments are not parsed, so
    an unbalanced brace inside one would throw this off; none of the functions it
    is pointed at contain one, and a bad slice fails the caller's assertion
    rather than passing it.
    """
    for decl in (f"async function {name}(", f"function {name}("):
        if decl in src:
            start = src.index(decl)
            break
    else:
        raise AssertionError(f"no function {name} in the source")
    depth = 0
    for i in range(src.index("{", start), len(src)):
        if src[i] == "{":
            depth += 1
        elif src[i] == "}":
            depth -= 1
            if depth == 0:
                return src[start:i + 1]
    raise AssertionError(f"function {name} is never closed")


def test_firmware_transfer_is_verified_by_md5_not_by_an_exit_status():
    """
    TRANSFER_OK only ever proved that the decode pipeline and chmod exited 0 —
    not that the bytes on the device match the bytes we sent (#76).

    That matters because a corrupt binary and a genuinely broken one produce
    the SAME observable: three fast exits, a symlink flip, and a device back on
    its old version. Shipping an unverified binary therefore costs a reboot and
    a rollback to learn nothing at all.
    """
    fn  = _sources.def_source("stream_file")

    assert "hashlib.md5(data).hexdigest()" in fn, (
        "the transfer must hash what it actually sent — hashing anything else "
        "verifies the wrong thing"
    )
    assert ".part" in fn and "mv " in fn, (
        "bytes must land in .part and be renamed only once verified, so a bad "
        "transfer leaves the destination as it was"
    )
    # The rename must be conditional on the comparison, not merely nearby.
    assert re.search(r"case .*GOT.*in .*want.*mv ", fn, re.S), (
        "the rename must be guarded by the md5 comparison"
    )
    assert "rm -f" in fn, "a failed verification must remove the .part"

    # Anchored on the CALL, not the function body: the docstring explains why
    # require_verify is set, so a body-wide search passes on the prose alone
    # while the argument is gone (caught by reintroducing exactly that).
    slot = _sources.def_source("_stream_binary_to_slot")
    call = slot[slot.index("return await em_shell.stream_file"):]
    assert "require_verify=True" in call, (
        "firmware is the payload where an unverifiable transfer must fail "
        "rather than proceed — it is the one we are about to boot"
    )


def test_the_firmware_upload_leaves_the_event_loop():
    """One upload stalled the loop for the base64 encode and again for the send.

    The encode and the md5 are CPU on the whole binary, so they run in the
    executor. The heredoc send yields, because websockets' send does not
    while the buffer has room. Serialising uploads does not cover this: the
    stall was a single device.
    """
    fn = _sources.def_source("stream_file")
    assert "run_in_executor(None, _b64_lines, data)" in fn
    assert "_send_b64_lines" in fn
    md5 = fn.index("hashlib.md5(data).hexdigest()")
    window = fn[max(0, md5 - 180):md5]
    assert "run_in_executor" in window, (
        "the md5 of a firmware binary has to run off the event loop"
    )


def test_a_corrupt_binary_never_reaches_the_symlink_flip():
    """
    The real win of verifying is not the error message, it is the ordering:
    a mismatch must be caught while the device is still running fine, not
    after it has taken a reboot and a rollback to tell us the same thing.
    """
    src = (CONTROLLER / "em_ota.py").read_text()
    ota = src[src.index("_stream_binary_to_slot(live, binary"):]
    ota = ota[:ota.index("_monitor_reconnect")]

    guard = ota.index("if not ok:")
    flip  = ota.index("ln -sf")
    assert guard < flip, (
        "the transfer result must be checked BEFORE the symlink flip"
    )
    assert "return" in ota[guard:flip], (
        "a failed transfer must return, not fall through to the flip — "
        "otherwise verification changes the log message and nothing else"
    )


def test_ota_checks_free_space_before_writing_anything():
    """
    The OTA path had no space check at all, unlike the asset path.

    Two traps, both already paid for elsewhere: read the figure with
    parse_free_mb rather than an awk field index (busybox wraps a long
    filesystem name onto its own line, so $4 is the PERCENTAGE on these
    devices), and treat an unreadable df as "carry on" rather than as a full
    disk — refusing on an unparsed reading blocks updates on any device whose
    df we have not seen.
    """
    src = (CONTROLLER / "em_ota.py").read_text()
    ota = src[src.index("inactive_slot = "):src.index("_stream_binary_to_slot(live, binary")]

    assert "parse_free_mb" in ota, (
        "free space must be read with parse_free_mb, never an awk field index"
    )
    # Comments are stripped first: the trap is worth explaining in a comment,
    # and a test that reads its own warning as the bug is a test that can only
    # be silenced by deleting the explanation.
    code = "\n".join(l for l in ota.splitlines() if not l.lstrip().startswith("#"))
    assert "awk" not in code, "an awk field index reads the percentage on these devices"
    assert "free_mb is not None and free_mb <" in ota, (
        "an unknown reading must not be compared as if it were a number"
    )


def test_a_transfer_never_deletes_the_destination_before_sending():
    """
    For firmware the destination IS the rollback slot, so deleting it up front
    means a transfer that fails early leaves the device with a good active
    slot and an empty partner — and a later crash-loop flips the symlink onto
    nothing. Three Dots hit exactly that in #121, while being told the slot
    had been left untouched.

    The `.part` discipline only protects `dest` from a CORRUPT transfer. It
    cannot protect it from being removed before the transfer starts.
    """
    fn  = _sources.def_source("stream_file")
    code = "\n".join(l for l in fn.splitlines() if not l.lstrip().startswith("#"))

    assert "rm -f {dest}" not in code, (
        "deleting dest before sending destroys the rollback slot on any "
        "transfer that fails early"
    )
    # The .part cleanup on a bad md5 must survive — that one is load-bearing.
    assert "rm -f {landing}" in code or "rm -f {landing};" in code, (
        "a failed verification must still remove the .part"
    )


def test_a_failed_transfer_says_which_stage_it_failed_at():
    """
    One message covered five outcomes, and the two furthest apart are "the
    bytes arrived corrupt" and "no byte was ever sent". #121 was the second
    reported in the language of the first, and only the controller's own
    stdout could tell them apart — which is not something a user can produce
    mid-update.

    A device shell that answers nothing must NOT read as "no base64 decoder":
    one is a link problem worth retrying, the other is a property of the
    device that retrying cannot change.
    """
    src = (CONTROLLER / "em_shell.py").read_text()

    assert "class TransferResult" in src and "__bool__" in src, (
        "the result must stay truthy so `if not await ...` call sites keep "
        "their meaning"
    )

    fn = _sources.def_source("stream_file")
    for stage in ("shell", "decoder", "send", "verify", "corrupt"):
        assert f'_transfer_failed("{stage}"' in fn, (
            f"the {stage} failure must be distinguishable from the others"
        )

    assert "if DETECT_MARKER not in detect_buf" in fn, (
        "a silent shell must report as a link problem, not a missing decoder"
    )

    # The OTA message must carry the stage through rather than re-flattening it.
    api = (CONTROLLER / "em_ota.py").read_text()
    ota = api[api.index("_stream_binary_to_slot(live, binary"):]
    ota = ota[:ota.index("_monitor_reconnect")]
    assert "{ok}" in ota, (
        "the update failure message must name the stage the transfer reached"
    )
    # Comments stripped first, for the reason the free-space test gives: the
    # old wording is worth naming in a comment, and a test that reads its own
    # explanation as the bug can only be silenced by deleting the explanation.
    ota_code = "\n".join(l for l in ota.splitlines() if not l.lstrip().startswith("#"))
    assert "failed or did not verify" not in ota_code, (
        "the flattened message is what made #121 unreadable"
    )


def test_push_log_callers_do_not_also_persist():
    """
    `em_events.push_log` persists AND pushes. A caller that also calls
    `db.log_device` writes the line twice.

    That is not hypothetical: the device `log` handler did both, so every
    device log line landed twice about 6ms apart, and roughly half of
    `device_logs` was duplicates. It stayed invisible because a doubled log
    line looks like a device that logged twice.

    Two things it cost beyond the wasted rows. `em_support.thin_noise` keeps
    the newest three `[mem]` lines per device, so duplication halved the
    distinct readings a leak hunt gets from a bundle. And the redundant call
    was a synchronous SQLite write on the event loop, which
    `event_loop_lag_monitor` exists to catch.

    Checked by source shape because the alternative is importing
    em_controller, which the suite deliberately does not do.
    """
    for path in _sources.MODULES:
        name = path.name
        lines = _sources.source(path).splitlines()
        for i, line in enumerate(lines):
            if "em_events.push_log(" not in line or "async def" in line:
                continue
            # The persist would sit just above the push, in the same block.
            window = lines[max(0, i - 6):i]
            offenders = [w.strip() for w in window if "db.log_device(" in w]
            assert not offenders, (
                f"{name}:{i+1} calls em_events.push_log, which already persists, "
                f"but is preceded by {offenders[0]!r}. That writes the log "
                f"line twice. Drop the db.log_device call."
            )


# ── Ambient light status reaches a support bundle (#90) ──────────────────────
#
# Two users reported no light sensor and the bundle could not say why: the
# firmware knows whether the chip is absent or the driver simply has not
# bound, but writes that only to its own log, which the bundle does not
# collect and a reboot clears. Diagnosing it needed a shell session on their
# hardware. These pin the three links in the chain that fixes it, because
# each fails silently — a missing field just looks like an old device.

def test_register_handler_stores_ambient_light_status():
    """The controller must keep what the device reported at registration."""
    root = Path(__file__).resolve().parent.parent
    ctl = (root / "em_controller.py").read_text()
    assert 'device.ambient_light_status = msg.get("ambient_light_status")' in ctl, (
        "the register handler must store ambient_light_status off the register "
        "message — without it the reason is received and dropped"
    )


def test_bundle_live_state_carries_ambient_light_status():
    """And the support bundle must actually carry it.

    em_support takes live_state wholesale rather than through an allowlist, so
    the field has to be put there by em_api. A reason that never reaches a
    bundle leaves us exactly where #90 started.
    """
    body = _sources.def_source("_get_support_bundle")
    assert '"ambient_light_status":' in body, (
        "the support bundle's live_state must include ambient_light_status so "
        "bundles can answer why a device reports no light sensor"
    )


def test_ingress_login_passes_the_real_deployment_flag_and_peer_address():
    """
    The ingress login endpoint must hand em_ingressauth.decide the LIVE
    INGRESS_ONLY value and the LIVE peer address — not a literal.

    Hardcoding either turns the decision function into theatre: `True` makes
    the standalone container honour an attacker-supplied X-Remote-User-Id,
    which is an unauthenticated admin session on a dashboard that proxies a
    root shell to every device. The decision itself is tested in
    test_ingressauth.py; this pins that it is actually consulted with real
    inputs.
    """
    src = _sources.def_source("_post_ingress_login")
    call = re.search(
        r"em_ingressauth\.decide\((.*?)\)\s*\n", src, re.S)
    assert call, "_post_ingress_login no longer calls em_ingressauth.decide"
    args = call.group(1)

    assert "ingress_only=em_ingressauth.INGRESS_ONLY" in args, (
        "ingress_only must be em_ingressauth.INGRESS_ONLY, never a literal")
    # ...and that flag is the deployment's, read from the environment once.
    flag = re.search(r"^INGRESS_ONLY = (.*)$",
                     _sources.source(CONTROLLER / "em_ingressauth.py"), re.M)
    assert flag and "ECHOMUSE_HOME_ASSISTANT_INGRESS" in flag.group(1), (
        "INGRESS_ONLY must come from the add-on's environment")
    assert "remote=request.remote" in args, (
        "remote must be the live peer address, never a literal")
    for literal in ("ingress_only=True", "remote=em_ingressauth.INGRESS_GATEWAY_IP"):
        assert literal not in args, f"{literal} defeats the check entirely"


def test_ingress_login_is_the_only_reader_of_the_remote_user_headers():
    """
    X-Remote-User-* may be read in exactly one place. A second reader is a
    second chance to forget that the headers are only meaningful behind
    Supervisor's gateway — Supervisor strips client copies, nothing else does.
    """
    # Match the READ, not the mention — em_ingressauth names these headers in
    # prose explaining where they come from and why they can be trusted,
    # which is documentation rather than a second trust boundary.
    readers = []
    for path in CONTROLLER.glob("*.py"):
        text = path.read_text()
        if re.search(r'headers(?:\.get\(|\[)\s*["\']X-Remote-User', text):
            readers.append(path.name)
    assert readers == ["em_routes_auth.py"], (
        f"X-Remote-User headers read in {readers} — expected em_routes_auth.py only")


def test_addon_panel_stays_admin_only():
    """
    panel_admin gates the sidebar entry for a dashboard that proxies a root
    shell. It is Supervisor's default, pinned here so a future edit to
    config.yaml has to be deliberate.
    """
    cfg = (CONTROLLER / "config.yaml").read_text()
    assert re.search(r"^panel_admin:\s*true\s*$", cfg, re.M), (
        "config.yaml must set panel_admin: true explicitly")


def test_role_changes_refuse_to_strand_the_install():
    """
    PATCH /api/users/{id} must refuse to demote the last admin. On the
    standalone container local accounts are the only auth, so an install with
    no admin has no way back in — and the endpoint that creates that state is
    the one an admin reaches for while tidying up.

    """
    body = _strip_prose(_sources.def_source("_patch_user"))

    assert "admin_count" in body, "must count admins before demoting one"
    assert "last_admin" in body, "must refuse to remove the final admin"


def test_addon_options_are_wired_end_to_end():
    """
    An add-on option needs four separate things to work: a default in
    `options`, a type in `schema`, a translation, and a line in em_start.py's
    OPTION_ENV_VARS. Miss one and it fails a different way each time, none of
    them loud:

      - no `schema` entry — Supervisor rejects the whole options block
      - no translation      — the raw key renders as the field label
      - no OPTION_ENV_VARS  — the setting is accepted, displayed, stored, and
                              then ignored; em_start warns to the add-on log
                              and starts anyway, which is the right call for
                              boot resilience and means nobody sees it

    The reverse direction matters too: an OPTION_ENV_VARS entry with no
    option is a mapping for a setting Supervisor will never send.

    Added with the `debug` option (2026-08-16). DEBUG had no add-on option at
    all, so controller debug logging — the first thing support asks for — was
    reachable on the container and not on the add-on.
    """
    import yaml

    config = yaml.safe_load((CONTROLLER / "config.yaml").read_text())
    translations = yaml.safe_load(
        (CONTROLLER / "translations/en.yaml").read_text())

    options = set(config["options"])
    schema = set(config["schema"])
    translated = set(translations["configuration"])

    start = (CONTROLLER / "em_start.py").read_text()
    block = re.search(r"OPTION_ENV_VARS\s*=\s*\{(.*?)\}", start, re.S)
    assert block, "em_start.py no longer defines OPTION_ENV_VARS"
    mapped = set(re.findall(r'"(\w+)"\s*:', block.group(1)))

    assert not options - schema, \
        f"options with no schema entry: {sorted(options - schema)}"
    assert not schema - options, \
        f"schema entries with no default: {sorted(schema - options)}"
    assert not options - translated, \
        f"options with no translation: {sorted(options - translated)}"
    assert not options - mapped, (
        f"options not in em_start.py's OPTION_ENV_VARS: "
        f"{sorted(options - mapped)} — Supervisor would accept and store "
        f"these and the controller would never see them")
    assert not mapped - options, (
        f"OPTION_ENV_VARS entries with no add-on option: "
        f"{sorted(mapped - options)}")


# The .env.example variables that deliberately have no add-on option, each
# carrying its reason. A bare list of names would rot the same way the option
# table did — a variable nobody can account for looks exactly like one nobody
# wanted, and the audit runs once.
FIXED_IN_THE_ADDON = {
    # config.yaml's `environment:` block pins this to /data/…, the add-on's own
    # persistent storage. A user option could point the database anywhere,
    # including somewhere that does not survive a restart.
    "DB_PATH": "pinned to /data in config.yaml's environment: block",
    # Must equal config.yaml's ingress_port (8768): Supervisor proxies the
    # dashboard to that port, so any other value leaves ingress with nothing
    # to forward to.
    "API_PORT": "must equal config.yaml's ingress_port or ingress breaks",
    # Home Assistant ingress is how the add-on signs people in: Supervisor has
    # already authenticated them. OpenID Connect is for the standalone
    # container, which has only local passwords otherwise, and it has not been
    # exercised under the add-on.
    "OIDC_ISSUER": "the add-on signs in through HA ingress; SSO is container-only",
}


def _env_example_vars() -> set[str]:
    """
    Every variable .env.example actually DEFINES. The file carries commented
    examples too (`EM_EXTRA_CA_CERT=/certs/internal-ca.crt` in a block
    describing docker-compose), and a commented line is not a variable the
    controller offers.
    """
    defined = set()
    for line in (CONTROLLER / ".env.example").read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        name, sep, _value = line.partition("=")
        if sep and re.fullmatch(r"[A-Z][A-Z0-9_]*", name):
            defined.add(name)
    return defined


def test_every_env_var_reaches_the_addon_or_states_why_not():
    """
    The other half of test_addon_options_are_wired_end_to_end, which starts
    from config.yaml and so cannot see a variable that never became an option.
    This starts from .env.example — the list of settings a container user can
    change — and demands each one be reachable under the add-on or carry a
    stated reason it is fixed there.

    The audit this replaces was a reading of CLAUDE.md, and it is exactly the
    kind of check that rots: a missing option is invisible, because a setting
    nobody wanted and a setting nobody added look the same from the outside.
    SERVER_TLS_PORT sat that way for the life of the add-on: 8770 is the wss
    port, it is advertised to devices, and no add-on user could move it or see
    it was taken (#163).
    """
    start = (CONTROLLER / "em_start.py").read_text()
    block = re.search(r"OPTION_ENV_VARS\s*=\s*\{(.*?)\}", start, re.S)
    assert block, "em_start.py no longer defines OPTION_ENV_VARS"
    pairs = dict(re.findall(r'"(\w+)"\s*:\s*"(\w+)"', block.group(1)))

    # The inversion, and the reason this is not a one-liner: .env.example
    # names the ENV VAR while OPTION_ENV_VARS maps OPTION -> ENV VAR, so it is
    # the VALUES that answer "can an add-on user set this". Reading the keys
    # instead would see `server_tls_port`, report SERVER_TLS_PORT as
    # unreachable, and be silenced by listing it as fixed — the vacuous pass.
    reachable = set(pairs.values())
    defined = _env_example_vars()

    unreachable = defined - reachable - set(FIXED_IN_THE_ADDON)
    assert not unreachable, (
        f".env.example variables with no add-on option: "
        f"{sorted(unreachable)} — settable on the standalone container and "
        f"unreachable under the add-on, which is the divergence #163 was "
        f"about. Add an option (options + schema + translations/en.yaml + "
        f"em_start.py's OPTION_ENV_VARS) or a reason in FIXED_IN_THE_ADDON.")

    # The other direction, and the one OPTION_ENV_VARS' own keys cannot catch:
    # an option mapped to an env var em_controller never reads is accepted,
    # stored and displayed, and does nothing. That is the same quiet failure
    # as a missing option, from the far end of the table.
    unreadable = reachable - defined
    assert not unreadable, (
        f"OPTION_ENV_VARS values that .env.example does not define: "
        f"{sorted(unreadable)} — em_controller.py reads no such env var, so "
        f"the option would be stored and ignored")

    for name, reason in FIXED_IN_THE_ADDON.items():
        assert len(reason.split()) >= 4, (
            f"FIXED_IN_THE_ADDON[{name!r}] needs a reason a reader can act "
            f"on, not a placeholder")
        assert name in defined, (
            f"FIXED_IN_THE_ADDON lists {name}, which .env.example no longer "
            f"defines — either the exclusion is stale or the file changed and "
            f"this one needs re-deciding")


def test_debug_env_var_is_not_truthy_for_zero():
    """
    em_start.py renders a false bool option as the STRING "0", and every
    non-empty string is truthy in Python. So a bare
    `if os.environ.get("DEBUG")` puts every add-on install at DEBUG level
    with the toggle showing off — the failure being verbose logs nobody
    asked for, on the deployment least able to rotate them.

    Pinned rather than assumed because the original line read exactly that
    way, and the same shape is the obvious thing to write for the next
    bool env var.
    """
    src = (CONTROLLER / "em_controller.py").read_text()
    assert not re.search(r'if\s+os\.environ\.get\(\s*"DEBUG"\s*\)\s*else', src), (
        "DEBUG is being read as a bare truthiness test — the string \"0\" "
        "that em_start.py writes for a false option is truthy")

    match = re.search(r'^DEBUG\s*=\s*(.+)$', src, re.M)
    assert match, "em_controller.py no longer defines a DEBUG flag"
    assert '"0"' in match.group(1) and '"1"' in match.group(1), (
        "DEBUG must default to \"0\" and test for \"1\", the convention "
        "REQUIRE_DEVICE_TLS already uses")


# ── The wake listener must not be able to die quietly ─────────────────────────
#
# Source-shape tests, because the suite cannot import em_controller — which is
# precisely why this had no coverage. On 2026-08-20 a device went deaf with no
# error line and stayed that way until the add-on was restarted: the listener
# is started with create_task and catches only CancelledError, so any other
# exception ends it, and the connection handler holding a reference means
# asyncio never even logs "Task exception was never retrieved". The device
# meanwhile scores wake words and reports them into a dead loop, so it looks
# healthy from every side.


def test_deleting_a_device_bounces_its_link():
    """
    Link auth is decided ONCE, at register time, so deleting a device's row
    does nothing to the socket it is already on: it disappears from the
    dashboard and carries on serving turns, and only comes back as pending
    after something else drops the link — a reboot, a controller restart, a
    WiFi blip. From the front that reads as a delete that did not happen.

    The bounce must come AFTER the row is gone. The device redials in 5s
    (`control.go` Run), and a close issued first races that redial against
    the delete — it re-registers into the row being deleted and survives.
    """
    fn  = _sources.def_source("_delete_device")
    # Comments stripped: this one explains the ordering it is pinning, and a
    # test that reads its own explanation as the code can be satisfied by
    # prose alone.
    code = "\n".join(l for l in fn.splitlines() if not l.lstrip().startswith("#"))

    assert "_disconnect_device(device_id)" in code, (
        "a deleted device keeps running on its existing connection unless the "
        "control plane is closed"
    )
    assert code.index("db.delete_device") < code.index("_disconnect_device"), (
        "the row must be gone before the device is told to redial, or it "
        "re-registers into the row being deleted"
    )


# ── The ring must say something when there is nothing to talk to ──────────────
#
# Source-shape, because the suite cannot import em_controller. A turn with no
# HA behind it ends in milliseconds, so without a cue the ring lights and
# clears too fast to register and the device reads as broken at exactly the
# moment it is working — the same failure ack_anim was added for, and the one
# the ESPHome port-collision incident presented as ("the wake word stopped
# working", while every part of it worked).


def _strip_prose(src: str) -> str:
    """
    Drop comments and string literals from a source slice.

    A guard that greps for the thing it forbids finds the comment explaining
    it and passes anyway — three separate tests in this tree have done exactly
    that. The prose below this function's subject is unusually chatty about
    locks, so strip it before asserting on code.
    """
    src = re.sub(r'""".*?"""', "", src, flags=re.S)
    src = re.sub(r"#.*", "", src)
    return src


def test_firmware_updates_are_serialised_across_the_whole_controller():
    """
    Three concurrent OTAs stalled the event loop for 11.1 seconds (measured
    2026-09-02, `[loop] event loop stalled` in the GA log) — and that loop is
    what sends speaker periods and LED frames, so a device answering someone
    pays for a device being updated.

    `_updates_in_progress` cannot prevent it: it stops ONE device being
    updated twice and says nothing about two devices at once. The serialiser
    has to be a global lock taken inside `_run_update`, so both entry points —
    the fleet deploy and a hand-clicked single update — go through it.
    """
    body = _strip_prose(_sources.def_source("em_ota.run_update"))

    assert "queue.acquire()" in body, (
        "run_update must take the global OTA lock — a per-device guard does "
        "not stop two devices updating at once"
    )
    assert "queue.release()" in body, "the lock must be released"

    # Nothing may reach the device before the lock is held, or the serialising
    # is decorative: the transfer is the expensive part, and it all lives
    # inside _run_update_locked.
    acquire = body.index("queue.acquire()")
    work    = body.index("_run_update_locked")
    assert acquire < work, (
        "the lock must be acquired BEFORE the update work starts"
    )
    inner = _strip_prose(_sources.def_source("em_ota._run_update_locked"))
    assert "_stream_binary_to_slot" in inner, (
        "the transfer must live inside the locked half"
    )
    assert not re.search(r"\bqueue\b", inner), (
        "the locked half must not touch the lock — one owner, or a release "
        "on a path that never acquired frees somebody else's turn"
    )

    # Bounded, or one wedged device holds the whole fleet's queue: the base64
    # send loop has no timeout of its own.
    assert "MAX_HOLD_S" in body and "wait_for" in body, (
        "the locked half must run under a timeout — serialising turns a "
        "device-local stall into a fleet-wide one"
    )

    # Released in the finally, not on the success path — an update that raises
    # would otherwise hold the lock for the life of the process and no device
    # could ever be updated again without a restart.
    tail = body[body.rindex("finally:"):]
    assert "queue.release()" in tail, (
        "release the lock in the finally — a failed update must not strand "
        "every later one behind it"
    )


def test_a_queued_update_is_reported_as_queued_not_as_in_progress():
    """
    Serialising means a device can be started and not yet touched. Reporting
    that as `update_in_progress` claims a transfer that has not begun, which
    is the same failure as any control that appears to work and does not.
    """
    src = _sources.def_source("merge_device")
    assert '"update_queued":' in src, (
        "/api/devices must expose update_queued, or the dashboard cannot tell "
        "waiting from working"
    )

    body = _strip_prose(_sources.def_source("em_ota.run_update"))
    add     = body.index("queued.add")
    acquire = body.index("queue.acquire()")
    assert add < acquire, "a device must be marked queued BEFORE it waits"
    assert "queued.discard" in body, (
        "queued state must be cleared once the lock is held, or a device "
        "reads as queued for the whole of its own update"
    )

    jsx = (CONTROLLER / "static" / "dashboard.jsx").read_text()
    assert "update_queued" in jsx, (
        "the fleet deploy modal must render the queued state it is sent"
    )


def test_ble_adverts_leave_the_control_plane_only_when_negotiated():
    """
    Adverts moved to the data plane because the control plane is where
    liveness is measured — `SendBleAdverts` wrote under the same mutex and TCP
    stream as the RTT echo and the keepalive pong (#404, proven by crossover
    on 2026-09-01).

    The negotiation is the half that fails SILENTLY if it is dropped: an older
    controller ignores unknown frame types, so a device sending 0x06
    unannounced loses every advertisement with no error at either end.
    """
    src = (CONTROLLER / "em_controller.py").read_text()

    assert "BLE_ADVERTS_TYPE" in src, "the data-plane frame code must be named"
    # Double-quoted: the prose around it uses backticks, so this matches the
    # announcement itself rather than the comment explaining it.
    assert '"ble_adverts_data"' in src, (
        "the controller must announce the feature, or no device will ever "
        "use the data plane"
    )
    # Anchored on the CALL, not on the first mention: the module docstring
    # documents the ack too, and a body-wide search passes on the prose while
    # the field itself is gone.
    ack = re.search(r"send_control\(\{[^}]*\"type\": \"ack\"[^}]*\}", src, re.S)
    assert ack, "could not find the ack send_control call — did it move?"
    assert "CONTROLLER_FEATURES" in ack.group(0), (
        "the ack must carry the controller's feature list"
    )


# ─── Connect-time reconcile ───────────────────────────────────────────────────
#
# Three payloads reach a device: the wake word assets, start_server.sh and the
# debloat pair. Until 2026-09-02 the first ran on connect but returned early
# unless the device scored locally, and the other two ran ONLY inside an OTA or
# from the Maintenance button — so a device already on the latest firmware
# never received a payload change. Office sat without three of the four stock
# classifiers from 17 August with every panel reporting it healthy.

def test_the_connect_handler_reconciles_all_three_payloads():
    """
    The handler must call the umbrella, not one payload's reconcile. Calling
    reconcile_oww_assets directly is what left the other two with no trigger
    that reaches a device on current firmware.
    """
    src = _strip_prose((CONTROLLER / "em_controller.py").read_text())
    assert "em_reconcile.on_connect(" in src, (
        "the connect handler must call em_reconcile.on_connect"
    )
    elsewhere = "\n".join(_sources.code_only(_sources.source(m))
                          for m in _sources.MODULES if m.name != "em_reconcile.py")
    assert "em_oww_sync.reconcile(" not in elsewhere, (
        "call the umbrella, not one payload — the other two then have no "
        "trigger that reaches a device already on the latest firmware"
    )


def test_the_reconcile_runs_the_payloads_sequentially():
    """
    All of them talk to one device over one shell plane. Gathering them
    contends for a single session for no gain — nothing is on a critical path
    here. (The wake word assets were a step until EchoHome.)
    """
    fn = _strip_prose(_sources.def_source("em_reconcile._reconcile_payloads"))

    for step in ("em_payloads.sync_start_script", "em_payloads.sync_endpoints"):
        assert step in fn, f"{step} must run on connect"
    assert "asyncio.gather" not in fn, (
        "the payloads share one shell plane — run them in order"
    )


def test_one_failing_payload_does_not_skip_the_others():
    """
    Unrelated payloads. A device with a stale debloat list must still get its
    start script, so each step is caught individually rather than the loop
    being wrapped in one try.
    """
    fn = _strip_prose(_sources.def_source("em_reconcile._reconcile_payloads"))
    body = fn[fn.index("for name"):]
    assert "try:" in body and "except Exception" in body, (
        "each step must be caught inside the loop, not around it"
    )


def test_the_reconcile_is_debounced_and_claimed_before_the_work():
    """
    Reconnects are routine on this fleet and the payloads are not. The stamp
    goes in BEFORE the work: a run takes shell round trips and possibly a
    multi-megabyte push, and a device that reconnects mid-run must not start a
    second one against the same shell plane.
    """
    fn  = _strip_prose(_sources.def_source("em_reconcile.on_connect"))
    assert "_due(" in fn, "the reconcile must be debounced per device"

    decide = _strip_prose(_sources.def_source("em_reconcile._due"))
    stamp  = decide.index("stamps[device_id] = now")
    ret    = decide.index("return True")
    assert stamp < ret, "claim the debounce before returning, not after the work"


def test_emos_crash_logs_are_checked_on_connect_before_the_debounce():
    """
    A device that crashed and reconnected inside the debounce window is the
    one most worth looking at, so the crash check sits in front of it — and
    only for a device that positively reported emOS, which is what saves the
    ram console.
    """
    fn = _strip_prose(_sources.def_source("em_reconcile.on_connect"))
    assert "_collect_crash_log(" in fn
    assert fn.index("_collect_crash_log(") < fn.index("_due(")
    gate = fn[:fn.index("_collect_crash_log(")]
    assert "not live.android_userspace" in gate


def test_the_crash_log_marker_is_written_only_after_a_complete_read():
    """
    The marker says "this boot was examined". Writing it before the read
    completed would lose a crash to a dropped shell session.
    """
    fn = _strip_prose(_sources.def_source("em_reconcile._collect_crash_log"))
    read = fn.index("cat {kmsg}")
    assert fn.index("em_shell.OK_MARK not in out") > read
    assert fn.index("summarise(") > fn.index("em_shell.OK_MARK not in out")
    assert fn.index("> {seen}") > fn.index("summarise(")


def test_deleting_a_device_forgets_its_debounce():
    """
    A re-added device is the one whose payloads are least likely to be right;
    inheriting the deleted row's stamp would skip its first reconcile.
    """
    fn = _strip_prose(_sources.def_source("_delete_device"))
    assert "em_reconcile.forget(device_id)" in fn


def test_the_payload_syncs_tell_a_missing_file_from_a_silent_device():
    """
    em_shell.run swallows every exception and returns "" — so an absent md5 and
    a device that never answered are the same string, and they want opposite
    actions. Without the marker, a shell plane that is not up yet (likely,
    seconds after connect) produces a push attempt and a user-visible "out of
    date" event that is not true.
    """
    for name in ("sync_start_script",):
        fn = _strip_prose(_sources.def_source(name))
        assert "em_shell.OK_MARK" in fn, (
            f"{name} must be able to tell a missing file from a silent device"
        )
        # The guard has to precede the push decision, or it guards nothing.
        assert fn.index("em_shell.OK_MARK not in out") < fn.index("em_shell.stream_file"), (
            f"{name} must check the device answered BEFORE deciding to push"
        )


# ─── Installing what a device already runs ────────────────────────────────────
#
# Hit by hand on 2026-09-03: the same engineering build uploaded to a device
# twice, costing a transfer, a reboot and a slot. The fleet endpoint had
# skipped this for releases since it was written; the single-device endpoint
# checked nothing, and neither could check an UPLOAD, because an uploaded
# binary was labelled `local-<timestamp>` rather than being read for its
# version. So the case most likely to happen by accident was the one case
# nothing guarded.

def test_the_update_endpoint_refuses_a_version_the_device_already_runs():
    fn = _strip_prose(_sources.def_source("_post_device_update"))

    assert 'row["firmware_ver"] == release["version"]' in fn, (
        "the update endpoint must compare the target against what the device "
        "is already running"
    )
    assert "already_running" in fn, "the refusal needs a named error code"
    assert 'body.get("force")' in fn, (
        "re-flashing the same version repairs a corrupt slot — force must "
        "exist, or this is a wall between an operator and their own device"
    )


def test_a_refused_update_does_not_consume_the_uploaded_binary():
    """
    The token is peeked and only popped once the update is actually started.
    Popping first meant a refusal cost an 11MB re-upload to act on the very
    advice the refusal had just given.
    """
    fn = _strip_prose(_sources.def_source("_post_device_update"))

    assert "_pending_uploads.get(upload_token)" in fn, (
        "peek the token — every refusal below it must leave it usable"
    )
    pop = fn.index("_pending_uploads.pop")
    run = fn.index("em_ota.run_update(")
    refuse = fn.index("already_running")
    assert refuse < pop, "the duplicate check must come before the token is consumed"
    assert pop < run, "consume the token only once the update is committed"


def test_the_fleet_endpoint_reads_the_version_out_of_an_uploaded_binary():
    """
    While an upload was labelled `local-<timestamp>`, it looked like a version
    no device had ever run — so `already_current` could never fire for one and
    a fleet push re-flashed every device with what it was already running.
    """
    fn  = _strip_prose(_sources.def_source("_post_deploy_all"))

    assert "em_ota.extract_binary_version(binary_override)" in fn, (
        "the fleet path must read the uploaded binary's own version"
    )
    assert "not upload_token and" not in fn, (
        "the already_current skip must no longer exclude uploads"
    )


def test_the_upload_endpoint_reports_what_was_uploaded():
    """
    The version rides back so the dashboard can warn at the point of deciding
    rather than after the operator has committed.
    """
    fn = _strip_prose(_sources.def_source("_post_upload_binary"))
    assert "em_ota.extract_binary_version(binary)" in fn
    assert '"version": version' in fn


# ─── The emOS provisioning flow ───────────────────────────────────────────────
#
# This flow writes a boot partition, so its guards are the kind that must not
# quietly disappear in a refactor. Source-shape tests, like the rest of this
# file: the dashboard compiles to one classic script with no module boundary,
# so there is nothing to import.

def _jsx():
    return (CONTROLLER / "static" / "dashboard.jsx").read_text()


def _step_ids(table):
    steps = _jsx()
    steps = steps[steps.index(f"const {table} = ["):]
    steps = steps[:steps.index("\n];")]
    return re.findall(r"\{ id: '([a-z_]+)'", steps)


def test_the_emos_flow_drops_every_android_only_step():
    """
    The whole point of the emOS flow. Magisk, the boot-image patch, the root
    pre-seed and the root check all exist to obtain root INSIDE Android, which
    this flow never boots; Disable Alexa never worked and silences a userspace
    about to be replaced; Debloat's package list targets that same userspace
    and its service.d script lives in the image being overwritten.

    Patch Boot Image is the one that matters most: it is the most dangerous
    step in the wizard, because in the wrong mode it writes over the amonet
    unlock payload.
    """
    ids = set(_step_ids("_EMOS_STEPS"))
    for gone in ("patch_boot", "install_magisk", "preseed_db", "verify_root",
                 "disable_alexa", "debloat"):
        assert gone not in ids, (
            f"'{gone}' is an Android-only step and must not be in the emOS flow")
    assert len(ids) == 8, f"the emOS flow is eight steps, found {len(ids)}: {ids}"


def test_the_emos_flow_escrows_before_it_flashes():
    """
    The escrowed image is both the build input and the ten-second undo, so
    every destructive step has to come after it. Ordering is the guard here —
    a flash before an escrow is a device with no way back.
    """
    ids = _step_ids("_EMOS_STEPS")
    assert ids.index("escrow_boot") < ids.index("build_emos") < ids.index("flash_emos"), \
        f"escrow must precede build must precede flash, got {ids}"
    # And the install lands on /data before the partition write, which is what
    # lets it survive the flash.
    assert ids.index("install_em") < ids.index("flash_emos")


def test_base_os_survives_the_device_going_offline():
    """
    base_os rides the register message and is stored (schema v21). The API
    used to read it off the live session only, so the dashboard's emOS /
    FireOS 5 slug would vanish whenever a device went offline — exactly when
    someone is trying to work out what it was. A live report still wins.
    """
    src = _sources.def_source("merge_device")
    at = src.index('"baseOs":')
    line = src[at:src.index("\n", src.index("\n", at) + 1)]
    assert 'getattr(live, "base_os", None)' in line, "a live report must come first"
    assert 'row["base_os"]' in line, "offline, baseOs must fall back to the stored value"


def test_only_a_registered_device_blocks_the_wizard():
    """
    The wizard refuses a device already on the controller — but a row with no
    firmware_ver is not one. ensure_device_token creates it when the TLS token
    is minted, before the device has ever connected, so matching on the serial
    alone refused every re-run of a provision that had got that far.

    Located by FUNCTION NAME rather than by scanning back from the error
    string: the decision moved into duplicateVerdict() so it could be unit
    tested, and textual adjacency then pointed at nothing. Behaviour is covered
    by controller/tests/duplicate_device.test.mjs; this pins that the rule is
    still expressed in the source at all.
    """
    src = _jsx()
    at = src.index("function duplicateVerdict")
    body = src[at:src.index("\n  }", at)]
    assert "knownDevices" in body and "d.firmware_ver" in body, (
        "the already-registered check must ignore rows that never registered")


def test_a_restore_ends_the_wizard_run():
    """
    A restore undoes the partition write that every later step builds on, and
    the wizard cannot step backwards, so carrying on provisions on top of a
    stock boot image — found on VVV 2026-09-18, sitting on Install Magisk as if
    Patch Boot had held. A successful restore therefore ends the run: the step
    controls go, nothing auto-runs, and the operator is told to start again.
    """
    src = _jsx()
    fn = src[src.index("async function restoreEscrowedBoot"):]
    fn = fn[:fn.index("\n  async function ", 1)]
    ok = fn.index("Escrowed image restored and verified")
    assert "setRestored(true)" in fn[ok:], (
        "setRestored(true) must follow the verified restore, never precede it")
    assert fn.index("setRestored(true)") > fn.index("_writeBootPartition"), (
        "the run may only end once the restore has been written and verified")
    assert "{!restored && (<>" in src, "the step controls must be hidden after a restore"
    assert "|| running || restored || stepState[step] !== 'pending') return;" in src, (
        "no step may auto-run after a restore")


def test_the_flash_step_verifies_against_the_partition():
    """
    A write that reports implausible throughput went to cache, and a read-back
    from that same cache passes. So the caches are dropped before reading, and
    a mismatch refuses rather than warns — and says not to reboot, because a
    device that has not rebooted is still recoverable from where it stands.
    """
    src = _jsx()
    # The write itself lives in _writeBootPartition, shared by the flash and the
    # restore. Shared rather than copied on purpose: this is the only code in
    # the wizard that can leave a device unbootable, and a second copy is one
    # that drifts from the checks this one carries — so pin that both callers
    # go through it and that neither grew a dd of its own.
    write = src[src.index("async function _writeBootPartition"):]
    write = write[:write.index("\n  async function runFlashEmos")]
    assert "conv=fsync" in write, "the write must be fsync'd"

    # The read-back must cover exactly what was written. It used to read whole
    # megabytes and compare against the image zero-padded to match, so 425,984
    # bytes of the PREVIOUS boot image were checked against zeros nobody wrote
    # — every emOS flash failed on a write that was provably complete. It hid
    # because the only path that had ever run was the restore, whose image is
    # the whole 16MB partition and therefore an exact number of blocks.
    assert "bs=2048" in write, (
        "the read-back must use page-sized blocks so it can cover exactly the "
        "bytes written, not round up to the next megabyte")
    assert "bytes.length / 2048" in write, (
        "the read-back block count must come from the image length")
    assert "% 2048 === 0" in write, (
        "an image that is not page-aligned must be detected rather than "
        "silently read short")
    assert "drop_caches" in write, (
        "the page cache must be dropped before the read-back, or the read-back "
        "confirms the cache rather than the partition")
    assert write.index("drop_caches") < write.index("Reading it back"), \
        "the caches must be dropped BEFORE the read-back, not after"

    fn = src[src.index("async function runFlashEmos"):]
    fn = fn[:fn.index("\n  // ── Steps 7 and 8")]
    assert "DO NOT REBOOT" in fn, (
        "a verification failure must tell the operator not to reboot")
    # Both callers write through the shared path, and neither writes a
    # partition any other way.
    for caller in ("runFlashEmos", "restoreEscrowedBoot"):
        body = fn[fn.index(f"async function {caller}"):]
        body = body[:body.index("\n  async function ", 1)] if "\n  async function " in body[1:] else body
        assert "_writeBootPartition" in body, (
            f"{caller} must write through _writeBootPartition, not its own dd")
    # Matched against shell CALLS, not against the text "of=" — the restore's
    # failure message quotes a dd command for the operator to run by hand, and
    # a guard that greps for the thing it forbids finds the prose explaining it
    # and fails on a file that is correct. Three times now.
    for line in fn.splitlines():
        assert not ("c.shell(" in line and "of=" in line), (
            "no caller may issue its own partition write — the one dd that "
            f"does lives in _writeBootPartition: {line.strip()}")


def test_the_serial_console_disables_echo_before_anything_else():
    """
    A port opened with default termios echoes everything the device sends back
    into its own input; the shell then executes its own prompt and every
    command returns 127. It looks alive, echoes what you type, and runs
    nothing. Web Serial has no stty to remind anyone, which is why this belongs
    in the client and why it is pinned.

    It cost an evening on 2026-09-04, a confident wrong diagnosis and a
    reverted commit.
    """
    src = _jsx()
    cls = src[src.index("class _EmosConsole"):]
    cls = cls[:cls.index("\n}")]
    assert "stty -echo" in cls, "the console client must disable echo"

    watch = src[src.index("async function _watchFirstBoot"):]
    watch = watch[:watch.index("\n  async function ", 1)]
    assert "disableEcho" in watch, "the reboot step must disable echo"
    assert watch.index("disableEcho") < watch.index("con.run("), \
        "echo must be disabled BEFORE the first command, or its reply is garbled"

    # Disabling echo is not enough on its own, and 2026-09-06 is how we know:
    # stty is not guaranteed present and the shell is not guaranteed up, so
    # echo survived and every run() returned the text of its OWN request. The
    # marker must therefore be assembled ON THE DEVICE, so it cannot appear in
    # the shell's echo of the command that asks for it.
    assert "; echo ${mark}" not in cls and "; echo ${mark}`" not in cls, (
        "the completion marker must not be sent whole — with echo on it "
        "arrives before the command runs and run() answers with its own request")
    assert "__a=__EM" in cls, (
        "the marker must be assembled on the device from parts that do not "
        "spell it in the echoed command")

    # Two markers, so the echo is discarded by POSITION. Recognising it
    # textually does not work — the terminal wraps at 80 columns, so the
    # request arrives split across lines and no line holds the whole command.
    assert "S__" in cls and "lastIndexOf(start)" in cls, (
        "run() must bracket the answer between a start and an end marker, or "
        "the console's echo of the request is returned as part of the answer")

    # A failed attempt must release the serial port. The browser refuses to
    # reopen one that is already open, and no amount of retrying clears it.
    step = src[src.index("async function runRebootAndWatch"):]
    step = step[:step.index("\n  async function ", 1)]
    assert "con.close()" in step, (
        "a failed first-boot watch must close the port, or every retry fails "
        "with 'The port is already open'")


def test_the_wizard_reads_the_device_list_the_way_the_api_returns_it():
    """
    /api/devices returns a bare ARRAY (`em_http.ok([...])`), not {devices: [...]}.

    The emOS WiFi step read `.devices` off it, which is undefined, and the
    `|| []` turned that into an empty list on every pass — so its wait loop
    never examined a single device and always timed out on a device that had
    registered perfectly. Three successive rewrites of the success condition
    were all debugging a predicate that was never evaluated against anything
    (2026-09-06).

    A shape mismatch between an endpoint and its caller is invisible at every
    layer: the fetch succeeds, the parse succeeds, and the empty result is
    indistinguishable from "nothing matched yet".
    """
    src = _jsx()
    assert "(await API.get('/api/devices')).devices" not in src, (
        "/api/devices returns an array; reading .devices off it is silently "
        "undefined")

    handler = _sources.def_source("_get_devices")
    assert "em_http.ok([" in handler, (
        "this test assumes /api/devices returns a bare list — if that changed, "
        "every caller in dashboard.jsx has to change with it")


def test_emos_wpa_cli_calls_carry_the_control_socket_path():
    """
    wpa_cli defaults to /var/run/wpa_supplicant. emOS starts its supplicant
    with -p/data/misc/wifi/sockets (init.c), so a bare `wpa_cli -i wlan0`
    fails every time with "Failed to connect to non-global ctrl_ifname".

    Found by hand on the console on 2026-09-06, before the WiFi step had ever
    run on hardware — it would have failed on its first call. The FireOS flow
    has always passed -p; the emOS step was written without it.

    Comments are stripped first: the explanation of this rule necessarily
    quotes the broken form, and a guard that greps for the thing it forbids
    otherwise fails a file that is correct.
    """
    src = _jsx()
    body = re.sub(r"^\s*//.*$", "", src, flags=re.M)
    bare = [ln.strip() for ln in body.splitlines()
            if "wpa_cli -i " in ln]
    assert not bare, (
        "every wpa_cli invocation must pass -p /data/misc/wifi/sockets: "
        + "; ".join(bare))


def test_the_emos_build_endpoint_stores_nothing():
    """
    The reference is the user's own boot partition and the one file we take
    care never to redistribute. Keeping a copy would mean holding a device
    image for no reason.
    """
    fn = _strip_prose(_sources.def_source("_post_provision_emos_image"))
    for writer in ("open(", "write_bytes", "Path(", "tempfile"):
        assert writer not in fn, (
            f"the emOS build endpoint must not persist anything (found {writer!r})")


def test_the_emos_and_firmware_release_namespaces_cannot_select_each_other():
    """
    Two release kinds live in one repo's release list, and each selector has
    to be blind to the other's releases. `emos-v0.1` does not start with "v",
    and a firmware release carries `server` rather than `init` — either
    property alone is enough, and both are present, which is what makes this
    safe rather than lucky.

    Same mechanism that keeps controller-v* out of the firmware poll. The
    failure if it broke would be the OTA offering an aarch64 init to a fleet
    of armv7a devices as a firmware update.
    """
    fw = _strip_prose(_sources.def_source("fetch_firmware"))
    assert "_choose_firmware_release(" in fw, \
        "the firmware poll must select through version.choose_firmware_release"
    vsrc = (CONTROLLER / "version.py").read_text()
    node = next(n for n in ast.parse(vsrc).body
                if isinstance(n, ast.FunctionDef) and n.name == "choose_firmware_release")
    chooser = _strip_prose(ast.get_source_segment(vsrc, node))
    assert 'startswith("v")' in chooser and '"server"' in chooser, \
        "the firmware poll must select on a v* tag AND a server asset"

    emos = _strip_prose(_sources.def_source("fetch_emos"))
    assert "choose_release(" in emos, \
        "the emOS poll must select through em_emos_update.choose_release"
    chooser = _strip_prose(_sources.def_source("choose_release"))
    assert 'startswith("emos-v")' in chooser and '"init"' in chooser, \
        "the emOS poll must select on an emos-v* tag AND an init asset"

    # EchoMuse publishes emOS; its release workflow, at the pinned commit.
    workflow = emos_file(".github/workflows/emos-release.yml").read_text()
    assert "'emos-v*'" in workflow, "the emOS release workflow must fire on emos-v* tags"
    fw_workflow = (CONTROLLER.parent / ".github" / "workflows" / "release.yml").read_text()
    assert "'v*'" in fw_workflow


def test_the_emos_init_is_verified_before_it_is_served():
    """
    A release built wrong is wrong for everyone, so it is refused at download
    rather than at boot. The checks live in the shared resolver: there are two
    ways to get an init, and a check at only one is a way in that does not
    verify.
    """
    resolver = _strip_prose(_sources.def_source("em_emos_ota._fetch_one_init"))
    assert "init_binary_problems" in resolver, (
        "the init must be checked for architecture/static before it is served")
    assert "bad_release_asset" in resolver
    # The architecture asked for is what it is checked against, not a constant.
    assert "init_binary_problems(binary, arch)" in resolver, (
        "the init must be verified against the architecture that was requested")

    # Every route to an init goes through that one function, so none of them can
    # serve an unchecked one.
    assert "_fetch_one_init" in _strip_prose(_sources.def_source("em_emos_ota.fetch_init")), \
        "the download endpoint's resolver must go through _fetch_one_init"
    assert "_fetch_one_init" in _strip_prose(_sources.def_source("em_emos_ota.fetch_payload")), \
        "the image endpoint's resolver must go through _fetch_one_init"
    for name, want in (("_get_provision_emos_init", "em_emos_ota.fetch_init"),
                       ("_post_provision_emos_image", "em_emos_ota.fetch_payload")):
        fn = _strip_prose(_sources.def_source(name))
        assert want in fn, f"{name} must resolve through {want}"


def test_the_init_architecture_is_decided_where_the_reference_is():
    """
    An init of the wrong architecture flashes fine and then produces no output at
    all, so the decision belongs where the reference is. The wizard must not pick
    — that needs a second copy of reference_kernel_arch in JavaScript. Shape
    rather than behaviour, the build step having no seam to test through.
    """
    jsx = (CONTROLLER / "static" / "dashboard.jsx").read_text()
    build = _js_fn_body(jsx, "runBuildEmos")

    # The wizard asks the controller to resolve it, and does not fetch one to
    # send. Fetching would mean choosing an architecture with no reference in
    # hand, which is the guess this whole path removes.
    assert "use_latest_init" in build, (
        "the wizard must ask the controller to resolve the init, so the "
        "architecture is read off the reference rather than guessed")
    assert "/api/provision/emos_init" not in build, (
        "the build step must not fetch an init — that endpoint cannot know "
        "which architecture this image needs")

    # Neither the sniffer nor its magic numbers may be reimplemented here.
    for magic in ("016f2818", "ARM\\x64", "0x24", "reference_kernel_arch"):
        assert magic not in build, (
            f"{magic!r} in the wizard means a second copy of the architecture "
            f"sniffer — it lives in em_emos_build.reference_kernel_arch")

    # The endpoint reads it off the reference, and refuses rather than
    # defaulting when it cannot: an init chosen by guess is the one failure with
    # no symptom on the device.
    handler = _sources.def_source("_post_provision_emos_image")
    assert "reference_kernel_arch" in handler, (
        "the image endpoint must read the architecture off the reference")
    assert "unknown_reference_arch" in handler, (
        "an unreadable reference must refuse, not fall back to an architecture")


def test_the_wifi_tools_go_only_into_a_32_bit_image():
    """
    init prefers /sbin/wpa_supplicant the moment one exists, so including these
    in a FireOS 5 image would move the whole fleet off Amazon's working
    supplicant as a side effect of a provisioning change. Same for wpa_cli, which
    init's nudge now prefers.

    Missing them on FireOS 6 is fatal, not degraded: Amazon's aborts under emOS,
    so the image would have no WiFi and no way to report it but a cable.
    """
    ota = (CONTROLLER / "em_emos_ota.py").read_text()
    # The gate is in select_payload, which fetch_payload hands the release to.
    assert "select_payload(" in _strip_prose(
        _sources.def_source("em_emos_ota.fetch_payload"))
    fn = _strip_prose(_sources.def_source("em_emos_ota.select_payload"))
    assert "ARCH_ARM" in fn, (
        "the WiFi tools must be gated on the reference's kernel architecture")
    assert "no_wifi_tools_for_arch" in fn, (
        "a 32-bit image with no WiFi tools available must refuse, not build "
        "something with no network")

    # The gate must test for the 32-bit kernel specifically. `ARCH_ARM64` is a
    # prefix-free constant but `ARCH_ARM` is not a substring test anyone should
    # rely on, so the comparison is pinned literally.
    assert "arch == em_emos_build.ARCH_ARM" in fn, (
        "the gate must be an equality against ARCH_ARM, so arm64 cannot satisfy it")

    # And all three travel together: em-wifi is useless without the two binaries
    # it drives, and wpa_cli alone would change which binary init's nudge uses.
    assert "SBIN_ASSETS" in fn, \
        "the tools must come from the one list, so they cannot diverge"
    tools = ota[ota.index("\nSBIN_ASSETS = ("):]
    tools = tools[:tools.index(")") + 1]
    for name in ("wpa_supplicant", "wpa_cli", "em-wifi", "busybox"):
        assert f'"{name}"' in tools, f"{name} missing from SBIN_ASSETS"


def test_the_bundle_stamp_has_exactly_one_definition():
    """
    A stale-tab check is worthless if the two ends derive the stamp
    separately.

    `_serve_dashboard` stamps the bundle URL and `/api/system/status`
    publishes the same value for a running page to compare against. If those
    two ever compute it independently they can drift, and the failure is
    silent in both directions: permanently stale (a reload prompt nobody can
    satisfy) or permanently fresh (the check does nothing, which is the state
    this replaced).

    The bug it exists for: a tab open across a controller update keeps its old
    JavaScript, the wizard silently ran a step that had shipped hours earlier,
    and the version in the header named the NEW controller because it comes
    from the API — so the one number somebody checks to rule this out was the
    number lying to them (2026-09-10).
    """
    _sources.find_def("bundle_version")   # exactly one, in any module
    status = _sources.def_source("_get_system_status")
    assert '"bundle_version": em_routes_static.bundle_version()' in status, (
        "/api/system/status must publish the stamp via bundle_version(), so "
        "the page can compare what it loaded against what is being served")

    # The URL stamp must come from the same call, not a second stat().
    serve = _sources.def_source("_serve_dashboard")
    assert "bundle_version()" in serve, (
        "_serve_dashboard must stamp the URL from bundle_version() rather "
        "than re-deriving the mtime")
    assert "st_mtime" not in serve, (
        "_serve_dashboard re-derives the bundle mtime — that is the drift "
        "this test exists to prevent; use bundle_version()")


def test_the_stale_bundle_check_does_not_refresh_the_displayed_version():
    """
    The header must keep naming the controller the page was LOADED against.

    Calling setStatus from the staleness poll would show the new version
    beside a reload prompt, which reads as a bug in the prompt rather than a
    stale page — and it is exactly the misleading behaviour being fixed, just
    arrived at from the other side.
    """
    jsx = (Path(__file__).resolve().parent.parent
           / "static" / "dashboard.jsx").read_text()

    start = jsx.index("const stale = setInterval(")
    block = jsx[start:jsx.index("return () => {", start)]
    assert "bundle_version" in block, "the staleness poll must compare bundle_version"
    assert "setStatus" not in block, (
        "the staleness poll must not call setStatus — the header's version "
        "has to keep naming the controller this page was loaded against")


def test_the_image_keeps_its_data_on_the_mounted_directory():
    """
    #629: with no .env, DB_PATH fell back to the relative "echomuse.db", which
    in the image is /app, outside the ./data:/app/data volume. The database,
    the device-link CA and the recordings were lost on every recreate.

    Read from the Dockerfile's instructions, not its comments.
    """
    root = Path(__file__).resolve().parent.parent
    instructions = [ln.strip() for ln in (root / "Dockerfile").read_text().splitlines()
                    if ln.strip() and not ln.lstrip().startswith("#")]
    assert "ENV DB_PATH=/app/data/echomuse.db" in instructions
    # sqlite will not create the directory, and without a volume nothing else does.
    assert any(ln.startswith("RUN mkdir -p") and "/app/data" in ln.split()
               for ln in instructions)
    # The directory the default points at is the one both compose files mount.
    for compose in ("docker-compose.yml", "docker-compose.deploy.yml"):
        assert ":/app/data" in (root / compose).read_text(), compose
    # The add-on keeps its own path, which has to win over the image's.
    assert 'DB_PATH: "/data/echomuse.db"' in (root / "config.yaml").read_text()


def test_every_emos_panel_button_runs_its_own_step_by_id():
    """
    #1183 dropped the wake word step and moved the emOS runner and panel
    conditions to ids, but left the panel BUTTONS on their old numbers. On
    41C8 (2026-11-15) "Flash emOS" ran Reboot and Watch, so nothing was
    flashed, and Configure WiFi's Connect named a step that no longer existed
    and hung. Each emOS panel's buttons must run that panel's step, by id.
    """
    from pathlib import Path
    jsx = (Path(__file__).resolve().parent.parent / "static" / "dashboard.jsx").read_text()
    marks = list(re.finditer(
        r"\{(?:step === _E\.(\w+)|\(step === _E\.)", jsx))
    assert marks, "no wizard panels found; update this test"
    emos = [(m, marks[i + 1].start() if i + 1 < len(marks) else len(jsx))
            for i, m in enumerate(marks) if m.group(1)]
    assert len(emos) >= 5, f"expected the emOS panels, found {[m.group(1) for m, _ in emos]}"
    for m, _ in emos:
        # The panel's own block: from its `{` to the matching `}`.
        depth, k = 0, m.start()
        while True:
            depth += {"{": 1, "}": -1}.get(jsx[k], 0)
            if depth == 0:
                break
            k += 1
        region = jsx[m.start():k + 1]
        args = re.findall(r"runStep\(([^,)]*)", region)
        for a in args:
            assert a.strip() == f"_E.{m.group(1)}", (
                f"a button in the {m.group(1)} panel runs runStep({a}); it must "
                f"run its own step, by id")
