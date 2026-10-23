#!/usr/bin/env python3
"""Synthetic contract tests for decision-local-server.py.

Set JEVD3_RUN_MODEL_SMOKE=1 to load the cached real weights and verify the
actual typed readout. All prompt text in that test is synthetic.
"""

import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import unittest

SCRIPT = Path(__file__).with_name("decision-local-server.py")
SPEC = importlib.util.spec_from_file_location("decision_local_server", SCRIPT)
server_module = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = server_module
SPEC.loader.exec_module(server_module)


class AdapterMappingTests(unittest.TestCase):
    def test_converts_all_question_types(self):
        qs = {
            "z-score": {"type": "score", "instructions": "Rate synthetic support", "rubric": ["low", "medium", "high"]},
            "b-noul": {"type": "noul", "instructions": "Is the synthetic claim supported?", "criteria": {"false": "unsupported", "true": "supported"}},
            "a-choice": {"type": "choice", "instructions": "Pick the synthetic label", "criteria": {"x": "label X", "y": "label Y"}},
        }
        model_qs, originals = server_module.convert_questions(qs)
        self.assertEqual([q["t"] for q in model_qs], ["choice", "noul", "score"])
        self.assertEqual(model_qs[1]["crit"], {"false": "unsupported", "true": "supported"})
        model_results = [
            {"probabilities": {"x": 0.8, "y": 0.2}},
            {"probabilities": {"false": 0.1, "true": 0.9}},
            {"probabilities": {"0": 0.1, "1": 0.2, "2": 0.7}},
        ]
        answers = server_module.adapt_results(model_results, originals)
        self.assertEqual(answers["a-choice"]["choice"], "x")
        self.assertAlmostEqual(answers["b-noul"]["noul"], 0.9)
        self.assertEqual(answers["z-score"]["probabilities"], {"0": 0.1, "1": 0.2, "2": 0.7})
        self.assertAlmostEqual(answers["z-score"]["score"], 1.6)

    def test_rejects_invalid_question(self):
        with self.assertRaises(ValueError):
            server_module.convert_questions({"bad": {"type": "score", "instructions": "x", "rubric": ["only one"]}})


@unittest.skipUnless(os.environ.get("JEVD3_RUN_MODEL_SMOKE") == "1", "set JEVD3_RUN_MODEL_SMOKE=1 to run the actual cached model")
class RealModelSyntheticSmoke(unittest.TestCase):
    def test_bundled_decision_v3_inference_smoke(self):
        args = [sys.executable, str(SCRIPT), "--verify", "--smoke-test"]
        model_dir = os.environ.get("JEVD3_MODEL_DIR")
        if model_dir:
            args.extend(["--model-dir", model_dir])
        proc = subprocess.run(
            args,
            check=False,
            capture_output=True,
            text=True,
            timeout=180,
        )
        self.assertEqual(proc.returncode, 0, proc.stderr[-2000:])
        lines = [line for line in proc.stdout.splitlines() if line.startswith('{')]
        self.assertTrue(lines, proc.stdout[-2000:])
        report = json.loads(lines[-1])
        self.assertEqual(report["status"], "ok")
        self.assertIn(report["model"], server_module.MODEL_IDENTITIES)
        self.assertTrue(report["synthetic_only"])


if __name__ == "__main__":
    unittest.main()
