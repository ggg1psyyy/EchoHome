"""Metadata decisions and real refresh logic, without runtime dependencies."""
import ast
import asyncio
import logging
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock

import pytest
import em_oww_metadata as metadata
import em_oww_models


@pytest.mark.parametrize("values", [{}, {"wake_word": ""}, {"wake_word": None}, {"wake_word": []}, {"language": "invalid code"}])
def test_stock_fallback(values):
    assert metadata.from_metadata("hey_jarvis_v0.1", values) == metadata.WakeWord("hey jarvis")


def test_custom_identity_does_not_change_prediction_key():
    name = "/models/not_the_phrase.onnx"
    assert metadata.from_metadata(name, {"wake_word": " Hallo Welt ", "language": "de_DE"}) == metadata.WakeWord("Hallo Welt", ("de-DE",))
    assert em_oww_models.prediction_key(name) == "not_the_phrase"




def test_missing_and_unreadable_models_fall_back(tmp_path, monkeypatch):
    path = tmp_path / "hey_robot.onnx"
    assert metadata.resolve(str(path)).name == "hey robot"
    path.write_bytes(b"not an onnx model")
    def fail(*args):
        raise RuntimeError("unreadable metadata")
    monkeypatch.setattr(metadata, "_read", fail)
    assert metadata.resolve(str(path)).name == "hey robot"


def test_file_replacement_changes_reader_identity(tmp_path, monkeypatch):
    path = tmp_path / "robot.onnx"
    path.write_bytes(b"first")
    reader = Mock(return_value={"wake_word": "First"})
    monkeypatch.setattr(metadata, "_read", reader)
    assert metadata.resolve(str(path)).name == "First"
    old = reader.call_args
    path.write_bytes(b"a different model")
    reader.return_value = {"wake_word": "Second"}
    assert metadata.resolve(str(path)).name == "Second"
    assert reader.call_args != old




