#!/usr/bin/env python3
"""Unit tests for the offline ASR runner's window planning and dry-run output."""
from __future__ import annotations

import importlib.util
import json
import pathlib
import subprocess
import sys
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "local-asr.py"


def load_runner():
    spec = importlib.util.spec_from_file_location("local_asr_runner", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


class WindowPlanTests(unittest.TestCase):
    def test_windows_share_overlap_and_clamp_last(self):
        runner = load_runner()
        self.assertEqual(
            runner.plan_windows(31000, 30000, 2000),
            [(0, 30000), (28000, 31000)],
        )
        self.assertEqual(runner.plan_windows(30000, 30000, 2000), [(0, 30000)])
        self.assertEqual(runner.plan_windows(0, 30000, 2000), [])

    def test_invalid_overlap_is_rejected(self):
        runner = load_runner()
        with self.assertRaises(ValueError):
            runner.plan_windows(1000, 30000, 30000)
        with self.assertRaises(ValueError):
            runner.plan_windows(1000, 0, 0)

    def test_clean_text_strips_special_tokens(self):
        runner = load_runner()
        self.assertEqual(runner.clean_text(" <|zh|> 你好   世界 <|end|> "), "你好 世界")


class DryRunTests(unittest.TestCase):
    def test_dry_run_emits_window_plan_without_model(self):
        with tempfile.TemporaryDirectory() as tmp:
            pcm = pathlib.Path(tmp) / "audio.pcm"
            pcm.write_bytes(b"\0" * (31000 * 32))
            proc = subprocess.run(
                [sys.executable, str(SCRIPT), "--backend", "ark", "--model", "unused",
                 "--pcm", str(pcm), "--window-ms", "30000", "--overlap-ms", "2000", "--dry-run"],
                capture_output=True, text=True, check=False,
            )
            self.assertEqual(proc.returncode, 0, proc.stderr)
            payload = json.loads(proc.stdout)
            self.assertEqual(payload["backend"], "ark")
            self.assertEqual(
                [(w["start_ms"], w["end_ms"], w["text"]) for w in payload["windows"]],
                [(0, 30000, ""), (28000, 31000, "")],
            )

    def test_dry_run_rejects_empty_pcm(self):
        with tempfile.TemporaryDirectory() as tmp:
            pcm = pathlib.Path(tmp) / "empty.pcm"
            pcm.write_bytes(b"")
            proc = subprocess.run(
                [sys.executable, str(SCRIPT), "--backend", "ark", "--model", "unused",
                 "--pcm", str(pcm), "--dry-run"],
                capture_output=True, text=True, check=False,
            )
            self.assertNotEqual(proc.returncode, 0)
            self.assertIn("empty PCM", proc.stderr)


if __name__ == "__main__":
    unittest.main()
