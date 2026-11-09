#!/usr/bin/env python3
"""Protocol tests for scripts/local-embed.py using --fake-encoder.

No model weights, torch or network are required: the fake encoder derives
deterministic vectors from sha256(text), which is enough to verify request
shape, query instruction, ordering, bounds and health.
"""
from __future__ import annotations

import json
import socket
import subprocess
import sys
import tempfile
import unittest
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "local-embed.py"
QUERY_PREFIX = "Instruct: Given a web search query, retrieve relevant passages that answer the query\nQuery: "
DIMS = 1024


OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def get(url: str, timeout: float = 5):
    return OPENER.open(url, timeout=timeout)


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


class LocalEmbedProtocolTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.port = free_port()
        cls.base = f"http://127.0.0.1:{cls.port}"
        cls.proc = subprocess.Popen(
            [sys.executable, str(SCRIPT), "--serve", "--host", "127.0.0.1", "--port", str(cls.port),
             "--model", "test-fake", "--fake-encoder", "--preload"],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        import time
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            if cls.proc.poll() is not None:
                raise RuntimeError(cls.proc.stderr.read())
            try:
                with get(cls.base + "/healthz", timeout=1) as response:
                    if response.status == 200:
                        return
            except Exception:  # noqa: BLE001
                time.sleep(0.1)
        raise TimeoutError("local-embed did not become healthy")

    @classmethod
    def tearDownClass(cls):
        cls.proc.terminate()
        try:
            cls.proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            cls.proc.kill()
        for stream in (cls.proc.stdout, cls.proc.stderr):
            if stream:
                stream.close()

    def post(self, payload: dict, headers: dict | None = None) -> tuple[int, dict]:
        body = json.dumps(payload).encode()
        request = urllib.request.Request(self.base + "/v1/embeddings", data=body,
                                         headers={"Content-Type": "application/json", **(headers or {})})
        try:
            with OPENER.open(request, timeout=30) as response:
                return response.status, json.loads(response.read())
        except urllib.error.HTTPError as error:
            return error.code, json.loads(error.read())

    def embeddings(self, texts, input_type=None):
        payload = {"model": "test-fake", "input": texts}
        if input_type:
            payload["input_type"] = input_type
        status, body = self.post(payload)
        self.assertEqual(status, 200, body)
        return [item["embedding"] for item in body["data"]]

    def test_embeddings_protocol_and_query_instruction(self):
        health = json.loads(get(self.base + "/healthz").read())
        self.assertEqual(health["status"], "ok")
        self.assertEqual(health["model"], "test-fake")
        self.assertEqual(health["dims"], DIMS)
        self.assertTrue(health["loaded"])

        single = self.embeddings("季度预算")
        self.assertEqual(len(single), 1)
        self.assertEqual(len(single[0]), DIMS)

        batch = self.embeddings(["第一段", "第二段", "第三段"])
        self.assertEqual([len(vector) for vector in batch], [DIMS, DIMS, DIMS])
        # Deterministic fake encoder: the first vector must match a one-item call.
        self.assertEqual(self.embeddings("第一段")[0], batch[0])

        query = self.embeddings("预算", input_type="query")[0]
        document = self.embeddings("预算", input_type="document")[0]
        prefixed = self.embeddings(QUERY_PREFIX + "预算", input_type="document")[0]
        self.assertEqual(query, prefixed, "query inputs must receive the Qwen3 instruction prefix")
        self.assertNotEqual(query, document, "query and document embeddings must differ")

    def test_batch_and_model_bounds(self):
        too_many = ["x"] * 129
        status, body = self.post({"model": "test-fake", "input": too_many})
        self.assertEqual(status, 413, body)

        status, body = self.post({"model": "other-model", "input": "x"})
        self.assertEqual(status, 400, body)
        self.assertIn("unknown model", body["error"]["message"])

        status, body = self.post({"model": "test-fake", "input": "x"}, headers={"Origin": "http://evil.example"})
        self.assertEqual(status, 403, body)

    def test_non_loopback_bind_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            proc = subprocess.run(
                [sys.executable, str(SCRIPT), "--serve", "--host", "0.0.0.0", "--port", str(free_port()),
                 "--model", "test-fake", "--fake-encoder"],
                capture_output=True, text=True, timeout=30,
            )
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("loopback", proc.stderr)


if __name__ == "__main__":
    unittest.main()
