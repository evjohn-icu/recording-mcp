#!/usr/bin/env python3
"""Unit tests for the offline ASR runner's window planning and dry-run output."""
from __future__ import annotations

import importlib.util
import json
import os
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


class _FakeSlice:
    def __init__(self, raw: bytes):
        self.raw = raw

    def tobytes(self) -> bytes:
        return self.raw


class _FakeSamples:
    def __init__(self, path: str):
        with open(path, "rb") as handle:
            self.raw = handle.read()

    def __getitem__(self, item):
        return _FakeSlice(self.raw[item.start * 2:item.stop * 2])


class FakeNumpy:
    """Stands in for numpy.memmap so protocol tests never need the real wheel."""

    def memmap(self, path, dtype, mode):
        return _FakeSamples(path)


class ProtocolTests(unittest.TestCase):
    """Probe/stream/skip/timeout protocol; the model itself is faked."""

    def setUp(self):
        self.runner = load_runner()

    def run_main(self, argv):
        import contextlib
        import io
        from unittest import mock

        out, err = io.StringIO(), io.StringIO()
        with mock.patch.object(sys, "argv", ["local-asr.py", *argv]), \
             mock.patch.dict(sys.modules, {"numpy": FakeNumpy()}), \
             contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            try:
                code = self.runner.main()
            except SystemExit as exc:
                if isinstance(exc.code, int):
                    code = exc.code
                else:
                    code = 1
                    print(exc.code, file=err)
        return code, out.getvalue(), err.getvalue()

    def pcm(self, tmp, seconds):
        path = pathlib.Path(tmp) / "audio.pcm"
        path.write_bytes(b"\0" * (seconds * 1000 * 32))
        return str(path)

    def test_probe_prints_one_json_object(self):
        self.runner.load_model_with_timeout = lambda args: ("ark", None)
        code, out, _ = self.run_main(
            ["--backend", "ark", "--device", "cpu", "--probe", "--load-timeout", "5"])
        self.assertEqual(code, 0)
        payload = json.loads(out)
        self.assertEqual(payload["device"], "cpu")
        self.assertEqual(payload["dtype"], "float16")
        self.assertEqual(payload["vram_total_mb"], 0)
        self.assertIn("model_load_ms", payload)
        self.assertEqual(payload["revision"], self.runner.DEFAULT_REVISIONS["ark"])

    def test_probe_load_failure_exits_4(self):
        def boom(args):
            raise RuntimeError("no CUDA device")

        self.runner.load_model_with_timeout = boom
        code, out, err = self.run_main(["--backend", "qwen", "--probe"])
        self.assertEqual(code, 4)
        self.assertEqual(out, "")
        self.assertIn("model load failed: no CUDA device", err)

    def test_stream_skips_committed_windows(self):
        self.runner.load_model_with_timeout = lambda args: ("ark", None)
        seen = []

        def fake(args, handle, wav):
            seen.append(os.path.basename(wav))
            return "text", ""

        self.runner.transcribe_window = fake
        with tempfile.TemporaryDirectory() as tmp:
            code, out, _ = self.run_main([
                "--backend", "ark", "--model", "unused", "--pcm", self.pcm(tmp, 31),
                "--window-ms", "30000", "--overlap-ms", "2000", "--stream", "--skip", "2",
                "--device", "cpu"])
        self.assertEqual(code, 0)
        lines = [json.loads(line) for line in out.splitlines() if line.strip()]
        self.assertEqual([line["index"] for line in lines], [1])
        self.assertEqual(lines[0]["text"], "text")
        self.assertEqual(len(seen), 1)

    def test_skip_outside_plan_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            code, out, err = self.run_main([
                "--backend", "ark", "--model", "unused", "--pcm", self.pcm(tmp, 31),
                "--stream", "--skip", "7"])
        self.assertNotEqual(code, 0)
        self.assertIn("outside the", err)

    def test_window_timeout_exits_5(self):
        import time

        self.runner.load_model_with_timeout = lambda args: ("ark", None)

        def slow(args, handle, wav):
            time.sleep(2)
            return "", ""

        self.runner.transcribe_window = slow
        with tempfile.TemporaryDirectory() as tmp:
            code, _, err = self.run_main([
                "--backend", "ark", "--model", "unused", "--pcm", self.pcm(tmp, 4),
                "--stream", "--device", "cpu", "--window-timeout", "1"])
        self.assertEqual(code, 5)
        self.assertIn("window timeout", err)
