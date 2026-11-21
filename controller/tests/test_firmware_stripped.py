"""Every firmware build is stripped (-s -w).

An unstripped build (18.5 MB) could not be uploaded through the dashboard:
Home Assistant's ingress takes at most 16 MiB per request (Supervisor's
MAX_CLIENT_SIZE), 2026-11-21. Stripping drops the DWARF and symbol tables
(4.9 MB) and keeps .gopclntab, so panics still name file and line. The local
build, the release and CI must agree, or a test upload and a release differ.
"""
import pathlib
import re

ROOT = pathlib.Path(__file__).resolve().parents[2]
BUILDS = {
    "device/compile.sh": ROOT / "device" / "compile.sh",
    "release.yml": ROOT / ".github" / "workflows" / "release.yml",
    "ci.yml": ROOT / ".github" / "workflows" / "ci.yml",
}


def go_builds(text):
    """Each `go build ...` of build/server, joined across line continuations."""
    flat = re.sub(r"\\\s*\n\s*", " ", text)
    return [m.group(0) for m in re.finditer(r"go build[^\n]*?-o build/server", flat)]


def test_every_firmware_build_is_stripped():
    for name, path in BUILDS.items():
        builds = go_builds(path.read_text())
        assert builds, f"{name}: no go build of build/server found"
        for b in builds:
            assert re.search(r"-ldflags\s+\\?[\"']-s -w\b", b), f"{name} builds unstripped: {b[:160]}"


def test_release_and_ci_check_stripped_and_budget():
    for name in ("release.yml", "ci.yml"):
        text = BUILDS[name].read_text()
        assert ', stripped"' in text, f"{name} does not check the binary is stripped"
        assert "15728640" in text, f"{name} does not hold the 15 MiB budget"
