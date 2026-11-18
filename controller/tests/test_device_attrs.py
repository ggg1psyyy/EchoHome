"""
Every attribute read off a Device is one a Device has.

pyflakes cannot see attributes, and the module split means no test imports
every caller. Twice in EchoHome's strip a Device attribute went while a
reader stayed: the stats handler's `device.shadow` failed every stats report
(39dd26a), and the disconnect cleanup's `device.tap_burst.cancel()` raised
before anything after it ran, so a disconnected Echo was never dropped from
em_state.devices or released (2026-11-18). This reads every module.
"""

import ast

import _sources

# How a Device is named where it is used.
NAMES = {"device", "live", "dev"}


def _device_members() -> set[str]:
    tree = _sources.tree(_sources.CONTROLLER / "em_controller.py")
    cls = next(n for n in tree.body if isinstance(n, ast.ClassDef) and n.name == "Device")
    have = {m.name for m in cls.body if isinstance(m, (ast.FunctionDef, ast.AsyncFunctionDef))}
    have |= {n.attr for n in ast.walk(cls) if isinstance(n, ast.Attribute)
             and isinstance(n.value, ast.Name) and n.value.id == "self"
             and isinstance(n.ctx, ast.Store)}
    # Set on a Device outside the class (`device.secure = ...`).
    for path in _sources.MODULES:
        for n in ast.walk(_sources.tree(path)):
            if (isinstance(n, ast.Attribute) and isinstance(n.ctx, ast.Store)
                    and isinstance(n.value, ast.Name) and n.value.id in NAMES):
                have.add(n.attr)
    return have


def test_every_device_attribute_read_exists():
    have = _device_members()
    missing = []
    for path in _sources.MODULES:
        for n in ast.walk(_sources.tree(path)):
            if (isinstance(n, ast.Attribute) and not isinstance(n.ctx, ast.Store)
                    and isinstance(n.value, ast.Name) and n.value.id == "device"
                    and n.attr not in have):
                missing.append(f"{path.name}:{n.lineno} device.{n.attr}")
    assert not missing, missing
