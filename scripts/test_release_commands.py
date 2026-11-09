#!/usr/bin/env python3
"""Validate the machine-readable release command manifest.

The release report's claims are only checkable if every required command has a
status and every skip carries a reason. This test does not run the commands; it
checks the accounting the report depends on.
"""
from __future__ import annotations

import json
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
MANIFEST = ROOT / "docs/productization/release-commands.json"
VALID = {"PASS", "FAIL", "NOT_RUN"}


class ReleaseCommandTests(unittest.TestCase):
    def load(self) -> dict:
        return json.loads(MANIFEST.read_text(encoding="utf-8"))

    def test_release_command_manifest(self) -> None:
        data = self.load()
        required = data.get("required") or []
        self.assertTrue(required, "manifest must list required commands")
        commands = data.get("commands") or []
        seen: dict[str, dict] = {}
        for entry in commands:
            command = entry.get("command", "")
            self.assertTrue(command, f"entry without a command: {entry}")
            self.assertNotIn(command, seen, f"duplicate command entry: {command}")
            seen[command] = entry
            self.assertIn(entry.get("status"), VALID, f"{command}: invalid status {entry.get('status')!r}")
        for command in required:
            self.assertIn(command, seen, f"required command missing from the manifest: {command}")
        for command in data.get("narrow_required") or []:
            self.assertIn(command, seen, f"required narrow check missing: {command}")

    def test_not_run_requires_reason(self) -> None:
        data = self.load()
        for entry in data.get("commands") or []:
            status = entry.get("status")
            if status == "NOT_RUN":
                self.assertTrue((entry.get("reason_if_not_run") or "").strip(),
                                f"{entry.get('command')}: NOT_RUN needs a reason")
            if status in ("PASS", "FAIL"):
                self.assertTrue((entry.get("evidence") or "").strip(),
                                f"{entry.get('command')}: {status} needs evidence")


if __name__ == "__main__":
    unittest.main()
