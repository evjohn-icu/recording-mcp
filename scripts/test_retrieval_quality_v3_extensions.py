"""Isolated tests for v3 runner APIs that do not exist yet (warmup plumbing).

These stay in their own file so a missing keyword (TypeError) cannot mask the
independent repros in test_retrieval_quality_v2.py.
"""
from __future__ import annotations

import importlib.util
import json
import tempfile
import unittest
from argparse import Namespace
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location(
    "retrieval_quality_v2", ROOT / "scripts/run-retrieval-quality-v2.py"
)
runner = importlib.util.module_from_spec(SPEC)
assert SPEC and SPEC.loader
SPEC.loader.exec_module(runner)


def warmup_args(binary: str = "fake-binary") -> Namespace:
    return Namespace(binary=binary, timeout=2, corpus="corpus.json", flush_embed_cache_url="",
                     embed_endpoint="", embed_model="", embed_key_env="", embed_dims=0,
                     embed_input_type=False, rerank_url="", rerank_model="",
                     assessor_url="", assessor_model="", admission_batch_questions=0,
                     fixture_cross_split_leaks=0)


class RunWarmupFlagTests(unittest.TestCase):
    """`_run(warmup=True)` judges the tool exit code and report presence only."""

    def test_warmup_run_records_exit_code_and_report_presence(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            split = root / "split.json"
            split.write_text(json.dumps({"questions": [
                {"id": "warm-1", "split": "train", "unanswerable": False}]}))
            out = root / "warmup.json"

            def fake_subprocess(cmd, **kwargs):
                out.write_text(json.dumps({"warmup": True}))
                return SimpleNamespace(returncode=0, stdout="", stderr="")

            with mock.patch.object(runner.subprocess, "run", side_effect=fake_subprocess):
                result = runner._run("fake-binary", warmup_args(), split, "v3", "hash", False,
                                     "train", out, warmup=True)
            self.assertEqual(result["status"], "PASS")
            self.assertEqual(result["exit_code"], 0)
            self.assertNotIn("metrics", result,
                             "a warmup result must never carry extracted metrics")
            self.assertNotIn("protocol_error", result)

    def test_warmup_run_fails_when_the_report_is_missing(self):
        with tempfile.TemporaryDirectory() as temp:
            split = Path(temp) / "split.json"
            split.write_text(json.dumps({"questions": []}))
            with mock.patch.object(runner.subprocess, "run",
                                   side_effect=lambda cmd, **kwargs: SimpleNamespace(
                                       returncode=0, stdout="", stderr="")):
                result = runner._run("fake-binary", warmup_args(), split, "v3", "hash", False,
                                     "train", Path(temp) / "warmup.json", warmup=True)
            self.assertEqual(result["status"], "FAIL")

    def test_warmup_run_reports_a_nonzero_exit(self):
        with tempfile.TemporaryDirectory() as temp:
            split = Path(temp) / "split.json"
            split.write_text(json.dumps({"questions": []}))
            out = Path(temp) / "warmup.json"
            out.write_text("{}")
            with mock.patch.object(runner.subprocess, "run",
                                   side_effect=lambda cmd, **kwargs: SimpleNamespace(
                                       returncode=3, stdout="", stderr="crash")):
                result = runner._run("fake-binary", warmup_args(), split, "v3", "hash", False,
                                     "train", out, warmup=True)
            self.assertEqual(result["status"], "FAIL")
            self.assertEqual(result["exit_code"], 3)


class WarmupExtraTests(unittest.TestCase):
    """`_warmup_run(extra=...)` must forward the real policy and lifecycle."""

    def test_warmup_run_forwards_the_policy_and_lifecycle_extra(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            retrieval = root / "retrieval.json"
            retrieval.write_text(json.dumps({"questions": [
                {"id": "warm-1", "split": "train", "query": "热身"}]}))
            corpus = root / "corpus.json"
            corpus.write_text("{}")
            args = warmup_args()
            captured = []

            def record_run(*call_args, **call_kwargs):
                captured.append((call_args, call_kwargs))
                return {"status": "PASS"}

            with mock.patch.object(runner, "_run", side_effect=record_run):
                result = runner._warmup_run(args, retrieval, root, "v3", "hash", False, corpus,
                                            extra=["--admission-policy", "policy.json",
                                                   "--lifecycle", "lifecycle.json"])
            self.assertEqual(result.get("questions"), 1)
            self.assertTrue(captured)
            call_args, call_kwargs = captured[0]
            self.assertEqual(call_args[3], "v3")
            self.assertEqual(call_kwargs.get("extra"),
                             ["--admission-policy", "policy.json", "--lifecycle", "lifecycle.json"])
            self.assertEqual(Path(call_args[2]).name, "retrieval-warmup.json")


if __name__ == "__main__":
    unittest.main()
