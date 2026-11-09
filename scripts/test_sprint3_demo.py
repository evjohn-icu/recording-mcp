#!/usr/bin/env python3
"""Run the twelve-step Sprint 3 demo when explicitly requested.

The demo needs a local GPU, the pinned ASR model and a free port, so the
default unit run records it as skipped rather than passed.
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


class Sprint3DemoTests(unittest.TestCase):
    def test_demo_twelve_steps(self):
        if os.environ.get("RUN_SPRINT3_DEMO") != "1":
            self.skipTest("RUN_SPRINT3_DEMO=1 not set; the demo needs a GPU and the pinned ASR model")
        out = ROOT / "artifacts/sprint3-demo"
        proc = subprocess.run(
            [sys.executable, str(ROOT / "scripts/sprint3-demo.py"), "--out", str(out),
             "--backend", os.environ.get("SPRINT3_DEMO_BACKEND", "ark"), "--skip-build"],
            cwd=ROOT, capture_output=True, text=True, timeout=3600,
        )
        result = json.loads((out / "result.json").read_text())
        self.assertEqual(proc.returncode, 0, proc.stdout[-2000:] + proc.stderr[-2000:])
        self.assertEqual(result["total"], 12, result)
        self.assertEqual(result["passed"], 12, result)
        self.assertEqual([s["status"] for s in result["steps"]], ["PASS"] * 12, result)
        self.assertNotIn("password", json.dumps(result["steps"]))


if __name__ == "__main__":
    unittest.main()
