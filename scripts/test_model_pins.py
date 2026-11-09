#!/usr/bin/env python3
"""Assert every code path agrees on the pinned model snapshots.

A pin that lives only in a document is not a pin: the runner, the Go selector
and the retrieval evaluation must all resolve the same repository revision.
This test reads the constants out of the source instead of importing them, so
it fails loudly when one side drifts.
"""
from __future__ import annotations

import json
import re
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
PINS = ROOT / "scripts" / "model-revisions.json"
SELECT_GO = ROOT / "internal" / "cloudsync" / "select.go"
ASR_PY = ROOT / "scripts" / "local-asr.py"


def load_pins() -> dict:
    return json.loads(PINS.read_text(encoding="utf-8"))["models"]


def go_const(name: str) -> str:
    source = SELECT_GO.read_text(encoding="utf-8")
    match = re.search(rf'^\s*{re.escape(name)}\s*=\s*"([^"]+)"', source, re.MULTILINE)
    if not match:
        raise AssertionError(f"internal/cloudsync/select.go has no constant {name}")
    return match.group(1)


def python_dict(name: str) -> dict[str, str]:
    source = ASR_PY.read_text(encoding="utf-8")
    match = re.search(rf"^{re.escape(name)}\s*=\s*\{{(.*?)^\}}", source, re.MULTILINE | re.DOTALL)
    if not match:
        raise AssertionError(f"scripts/local-asr.py has no {name}")
    return dict(re.findall(r'"([^"]+)"\s*:\s*"([^"]+)"', match.group(1)))


class ModelPinTests(unittest.TestCase):
    def test_go_selector_matches_pins(self) -> None:
        pins = load_pins()
        self.assertEqual(go_const("defaultARKModel"), pins["asr_ark"]["repo_id"])
        self.assertEqual(go_const("defaultARKRevision"), pins["asr_ark"]["revision"])
        self.assertEqual(go_const("defaultQwenModel"), pins["asr_qwen"]["repo_id"])
        self.assertEqual(go_const("defaultQwenRevision"), pins["asr_qwen"]["revision"])

    def test_runner_defaults_match_pins(self) -> None:
        pins = load_pins()
        self.assertEqual(python_dict("DEFAULT_MODELS"), {
            "ark": pins["asr_ark"]["repo_id"],
            "qwen": pins["asr_qwen"]["repo_id"],
        })
        self.assertEqual(python_dict("DEFAULT_REVISIONS"), {
            "ark": pins["asr_ark"]["revision"],
            "qwen": pins["asr_qwen"]["revision"],
        })

    def test_pins_are_commit_shas(self) -> None:
        for name, model in load_pins().items():
            with self.subTest(model=name):
                self.assertRegex(model["revision"], r"^[0-9a-f]{40}$")
                self.assertTrue(model["repo_id"])
                digest = model.get("safetensors_sha256")
                if digest is not None:
                    self.assertRegex(digest, r"^[0-9a-f]{64}$")


if __name__ == "__main__":
    unittest.main()
