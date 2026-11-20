#!/usr/bin/env python3
"""Controlled localhost assessor smoke for the v3 warmup head.

The mocked runner tests cannot observe a real adapter request. This smoke
serves the /v1/systemone contract with synthetic verdicts on localhost, runs
the real ``bin/context-eval`` over the fixed first train question of the
frozen v3 fixture, and asserts that the adapter saw the requests and that the
report executed only that single train question.

Run:  python3 scripts/test_warmup_assessor_smoke.py --binary bin/context-eval
It skips cleanly when the binary has not been built.
"""
from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location(
    "retrieval_quality_v2", ROOT / "scripts/run-retrieval-quality-v2.py"
)
runner = importlib.util.module_from_spec(SPEC)
assert SPEC and SPEC.loader
SPEC.loader.exec_module(runner)

FIXTURE = ROOT / "fixtures/eval/quality-v3-r1"


class SyntheticAssessorHandler(BaseHTTPRequestHandler):
    """Serves valid typed verdicts and records every request it receives."""

    protocol_version = "HTTP/1.1"
    observations: list[dict] = []

    def do_POST(self):  # noqa: N802 - BaseHTTPRequestHandler API
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length) or b"{}")
        answers = {}
        for qid, question in (body.get("questions") or {}).items():
            qtype = question.get("type")
            if qtype == "choice":
                criteria = list((question.get("criteria") or {}).keys())
                chosen = criteria[0] if criteria else "supports"
                answers[qid] = {
                    "type": "choice", "choice": chosen,
                    "probabilities": {key: (1.0 if key == chosen else 0.0) for key in criteria},
                    "confidence": 1.0,
                }
            elif qtype == "noul":
                answers[qid] = {"type": "noul", "noul": 0.5, "confidence": 1.0}
            elif qtype == "score":
                rubric = question.get("rubric") or []
                answers[qid] = {
                    "type": "score",
                    "score": (len(rubric) - 1) / 2 if rubric else 0.0,
                    "probabilities": [1.0 / len(rubric)] * len(rubric) if rubric else [1.0],
                    "confidence": 1.0,
                }
            else:
                answers[qid] = {"type": qtype or "unknown"}
        payload = json.dumps({
            "model": body.get("model", "synthetic-assessor"),
            "weights_version": "smoke-synthetic-v1",
            "answers": answers,
            "usage": {"latency_ms": 1, "input_tokens": 1, "output_tokens": 1},
        }).encode()
        self.observations.append({"path": self.path, "questions": len(body.get("questions") or {})})
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *_args):  # keep the smoke output clean
        return


class WarmupAssessorSmokeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.binary = ROOT / "bin/context-eval"
        if not cls.binary.is_file():
            raise unittest.SkipTest("bin/context-eval is not built; run `make build` first")

    def test_warmup_head_reaches_the_real_adapter(self):
        with tempfile.TemporaryDirectory() as temp:
            work = Path(temp)
            out = work / "warmup-report.json"
            handler = SyntheticAssessorHandler
            handler.observations = []
            server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            self.addCleanup(server.server_close)
            self.addCleanup(server.shutdown)
            endpoint = f"http://127.0.0.1:{server.server_address[1]}/v1/systemone"

            warmup_split = runner.warmup_split(FIXTURE / "retrieval.json", work)
            warmup_questions = json.loads(warmup_split.read_text())["questions"]
            self.assertEqual(len(warmup_questions), 1)
            self.assertEqual(warmup_questions[0]["split"], "train")

            hashes = runner.verify_frozen_hashes_v3(FIXTURE)
            policy = {"schema_version": "evidence-admission-policy-v1", "retrieval_version": "v3",
                      "provider_id": "hash-ngram-v1", "model_id": "hash-ngram-v1",
                      "model_revision": "smoke-hash-rev-1", "assessor_model": "synthetic-assessor",
                      "fixture_sha256": hashes["retrieval.json"], "threshold": 0.0,
                      "phase": "selected"}
            policy_path = work / "policy-selected.json"
            policy_path.write_text(json.dumps(policy), encoding="utf-8")

            command = [
                str(self.binary),
                "--corpus", str(FIXTURE / "corpus.json"),
                "--questions", "", "--links", "", "--projection", "",
                "--retrieval", str(warmup_split),
                "--retrieval-version", "v3",
                "--embed-provider", "hash",
                "--max-items", "10", "--max-bytes", "65536",
                "--admission-policy", str(policy_path),
                "--lifecycle", str(FIXTURE / "lifecycle.json"),
                "--assessor-decision-url", endpoint,
                "--assessor-decision-model", "synthetic-assessor",
                "--timeout", "120s",
                "--out", str(out),
            ]
            proc = __import__("subprocess").run(command, capture_output=True, text=True, timeout=180)
            self.assertEqual(proc.returncode, 0,
                             f"context-eval failed:\n{proc.stdout[-2000:]}\n{proc.stderr[-2000:]}")
            report = json.loads(out.read_text())
            retrieval_rows = [row for row in report["per_question"] if row.get("split")]
            self.assertTrue(retrieval_rows, "the report has no retrieval rows")
            self.assertEqual({row["id"] for row in retrieval_rows}, {warmup_questions[0]["id"]})
            self.assertEqual({row["mode"] for row in retrieval_rows}, {"lexical", "hybrid"})
            self.assertGreaterEqual(len(handler.observations), 1,
                                    "the adapter never observed a request")
            for observation in handler.observations:
                self.assertGreaterEqual(observation["questions"], 1)
                self.assertLessEqual(observation["questions"], 32)
            print(f"\nwarmup adapter smoke: {len(handler.observations)} requests, "
                  f"question counts {[entry['questions'] for entry in handler.observations]}")


def main() -> int:
    unittest.main(module=__name__, argv=[sys.argv[0], "-v"] + sys.argv[1:], exit=False)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
