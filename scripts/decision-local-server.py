#!/usr/bin/env python3
"""Local HTTP adapter for the Jev-Style-0.8B-Decision-v3 typed readout.

This serves the model package's calibrated verdict distributions through the
repository's /v1/systemone contract. It does not generate chat text.
"""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import logging
import math
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import socket
from typing import Any

DEFAULT_MODEL_DIR = Path.home() / ".cache/huggingface/hub/models--chaoliangUNSW--Jev-Style-0.8B-Decision-v3/snapshots/b023d1f9c7858fbf01504577a3bfc349ea5c7385"
MODEL_NAME = "Jev-Style-0.8B-Decision-v3"
MODEL_ALIAS = "decision-v3-0.8b"
WEIGHTS_VERSION = "b023d1f9c7858fbf01504577a3bfc349ea5c7385"
MODEL_IDENTITIES = {
    "Jev-Style-0.8B-Decision-v3": {
        "repo": "chaoliangUNSW/Jev-Style-0.8B-Decision-v3",
        "alias": "decision-v3-0.8b",
        "snapshot": "b023d1f9c7858fbf01504577a3bfc349ea5c7385",
        "digest": "e4f7b9ad83be3fd9d0c8cc21632fecae819da34c3dbac38c7faa881ecb285806",
    },
    "Jev-Style-2B-Decision-v3": {
        "repo": "chaoliangUNSW/Jev-Style-2B-Decision-v3",
        "alias": "decision-v3-2b",
        "snapshot": "5bad2d53ae04e832e2b30aa2a89bec76ec03deec",
        "digest": "25e668f193c409a53745d4e456cbbc7e1162ee5986d87f396196d6eabacf7429",
    },
}
MAX_BODY_BYTES = 4 << 20
MAX_QUESTIONS = 32


def convert_questions(questions: dict[str, dict[str, Any]]) -> tuple[list[dict[str, Any]], dict[str, dict[str, Any]]]:
    """Convert SystemOne typed questions into the model package's typed input."""
    ids = sorted(questions)
    model_questions: list[dict[str, Any]] = []
    originals: dict[str, dict[str, Any]] = {}
    for qid in ids:
        q = questions[qid]
        if not isinstance(q, dict):
            raise ValueError(f"question {qid!r} must be an object")
        if not qid or len(qid) > 128:
            raise ValueError("question ID is invalid")
        qtype = q.get("type")
        ins = q.get("instructions")
        if not isinstance(ins, str) or not ins.strip():
            raise ValueError(f"question {qid!r} needs non-empty instructions")
        if qtype == "choice":
            criteria = q.get("criteria")
            if not isinstance(criteria, dict) or not criteria:
                raise ValueError(f"choice question {qid!r} needs criteria")
            if len(criteria) > 64:
                raise ValueError("choice question exceeds the option limit")
            crit = {str(k): str(v) for k, v in criteria.items()}
        elif qtype == "noul":
            supplied = q.get("criteria")
            crit = ({str(k): str(v) for k, v in supplied.items()}
                    if isinstance(supplied, dict) and supplied
                    else {"false": "the statement does not hold", "true": "the statement holds"})
            if set(crit) != {"false", "true"}:
                raise ValueError(f"noul question {qid!r} criteria must use false and true")
        elif qtype == "score":
            rubric = q.get("rubric")
            if not isinstance(rubric, list) or not 2 <= len(rubric) <= 10:
                raise ValueError(f"score question {qid!r} needs 2..10 rubric levels")
            crit = [str(v) for v in rubric]
        else:
            raise ValueError(f"question {qid!r} has unsupported type {qtype!r}")
        model_questions.append({"t": qtype, "ins": ins, "crit": crit})
        originals[qid] = q
    return model_questions, originals


def adapt_results(results: list[dict[str, Any]], originals: dict[str, dict[str, Any]]) -> dict[str, dict[str, Any]]:
    """Map model verdict probabilities to the SystemOne union answer shape."""
    if len(results) != len(originals):
        raise ValueError("model returned the wrong number of answers")
    answers: dict[str, dict[str, Any]] = {}
    for qid, result in zip(sorted(originals), results):
        q = originals[qid]
        qtype = q["type"]
        probs = result["probabilities"]
        confidence = max(float(p) for p in probs.values())
        answer: dict[str, Any] = {
            "type": qtype,
            "confidence": confidence,
            "probabilities": probs,
        }
        if qtype == "choice":
            answer["choice"] = max(probs, key=probs.get)
        elif qtype == "noul":
            answer["noul"] = float(probs["true"])
        elif qtype == "score":
            rubric = q["rubric"]
            distribution: dict[str, float] = {}
            expected = 0.0
            for i, level in enumerate(rubric):
                p = float(probs[str(i)])
                distribution[str(i)] = p
                expected += i * p
            answer["probabilities"] = distribution
            answer["score"] = expected
        answers[qid] = answer
    return answers


class DecisionHandler(BaseHTTPRequestHandler):
    server_version = "decision-local/1"

    def log_message(self, fmt: str, *args: Any) -> None:
        # Do not log request bodies, which can contain private local state.
        logging.info("%s - %s", self.address_string(), fmt % args)

    def _json(self, status: int, body: dict[str, Any]) -> None:
        raw = json.dumps(body, ensure_ascii=False, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self) -> None:  # noqa: N802
        if self.path == "/healthz":
            self._json(200, {"status": "ok", "model": MODEL_NAME, "weights_version": WEIGHTS_VERSION})
            return
        self._json(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        if self.path != "/v1/systemone":
            self._json(404, {"error": "not found"})
            return
        if self.headers.get("Origin"):
            self._json(403, {"error": "browser-origin requests are not accepted"})
            return
        try:
            length = int(self.headers.get("Content-Length", "-1"))
            if length < 0 or length > MAX_BODY_BYTES:
                raise ValueError("request body size is invalid or too large")
            request = json.loads(self.rfile.read(length))
            state = request.get("state")
            questions = request.get("questions")
            if not isinstance(state, str) or not state.strip():
                raise ValueError("state is required")
            if len(state) > 300_000:
                raise ValueError("state exceeds the adapter character limit")
            if not isinstance(questions, dict) or not questions:
                raise ValueError("questions must be a non-empty object")
            if len(questions) > MAX_QUESTIONS:
                raise ValueError("request exceeds the question limit")
            requested_model = request.get("model", "")
            if requested_model and requested_model not in (MODEL_NAME, MODEL_ALIAS):
                raise ValueError("requested model is not available")
            model_questions, originals = convert_questions(questions)
            started = time.monotonic()
            # The model uses shared inference buffers; serialize access per process.
            with self.server.inference_lock:  # type: ignore[attr-defined]
                results = self.server.engine.decide_many(state, model_questions)  # type: ignore[attr-defined]
            answers = adapt_results(results, originals)
            elapsed = int((time.monotonic() - started) * 1000)
            tokens = sum(int(r.get("input_tokens", 0)) for r in results)
            self._json(200, {
                "model": MODEL_NAME,
                "weights_version": WEIGHTS_VERSION,
                "answers": answers,
                "usage": {"latency_ms": elapsed, "input_tokens": tokens, "output_tokens": 0},
            })
        except json.JSONDecodeError:
            self._json(400, {"error": "invalid_json"})
        except (ValueError, TypeError, KeyError):
            self._json(400, {"error": "invalid_request_or_model_budget"})
        except Exception as exc:  # Keep exception text private; it may contain input fragments.
            logging.error("local Decision inference failed (%s)", type(exc).__name__)
            self._json(500, {"error": "inference_failed"})


class BoundedThreadingHTTPServer(ThreadingHTTPServer):
    """Keep a small bounded number of active/queued local clients."""
    daemon_threads = True
    request_queue_size = 8

    def __init__(self, server_address, handler_class):
        self.request_slots = threading.BoundedSemaphore(4)
        super().__init__(server_address, handler_class)

    def process_request(self, request, client_address):
        if not self.request_slots.acquire(blocking=False):
            try:
                request.sendall(
                    b"HTTP/1.1 503 Service Unavailable\r\n"
                    b"Content-Type: application/json\r\n"
                    b"Content-Length: 16\r\n"
                    b"Connection: close\r\n\r\n"
                    b'{"error":"busy"}'
                )
            except OSError:
                pass
            finally:
                self.shutdown_request(request)
            return
        try:
            super().process_request(request, client_address)
        except Exception:
            self.request_slots.release()
            raise

    def process_request_thread(self, request, client_address):
        try:
            super().process_request_thread(request, client_address)
        finally:
            self.request_slots.release()


def load_engine(model_dir: Path, device: str, dtype: str, verify: bool):
    global MODEL_NAME, MODEL_ALIAS, WEIGHTS_VERSION
    script = model_dir / "jev_style_decision.py"
    if not script.is_file():
        raise FileNotFoundError(f"Decision runner not found under model directory: {script}")
    spec = importlib.util.spec_from_file_location("jev_style_decision_local", script)
    if spec is None or spec.loader is None:
        raise RuntimeError("could not load bundled Jev-Style Decision runner")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    model_name = getattr(module, "MODEL_NAME", "")
    identity = MODEL_IDENTITIES.get(model_name)
    if identity is None:
        raise RuntimeError("model directory does not contain a supported Decision-v3 runner")
    # Always verify the manifest before advertising the packaged weight identity.
    report = module.verify_manifest(model_dir)
    if not report.get("ok"):
        raise RuntimeError("model files do not match their packaged manifest")
    manifest = json.loads((model_dir / "manifest.json").read_text())
    if manifest.get("repo") != identity["repo"]:
        raise RuntimeError("runner and weight repository identities do not match")
    files = manifest.get("files", {})
    critical = {k: files[k]["sha256"] for k in sorted(files)
                if k == "model.safetensors" or k.startswith("model-") or k == "readout_config.json"}
    if not critical:
        raise RuntimeError("model manifest does not identify weight and readout files")
    digest = hashlib.sha256(json.dumps(critical, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    snapshot = model_dir.name
    WEIGHTS_VERSION = (identity["snapshot"] if snapshot == identity["snapshot"] and digest == identity["digest"]
                       else f"sha256:{digest}")
    MODEL_NAME = model_name
    MODEL_ALIAS = identity["alias"]
    kwargs: dict[str, Any] = {"model_dir": str(model_dir), "verify": verify}
    if device != "auto":
        kwargs["device"] = device
    if dtype != "auto":
        kwargs["dtype"] = dtype
    else:
        import torch
        use_cuda = device == "cuda" or (device == "auto" and torch.cuda.is_available())
        kwargs["dtype"] = ("bfloat16" if model_name == "Jev-Style-2B-Decision-v3" else "float16") if use_cuda else "float32"
    return module.JevStyleDecision(**kwargs)


def smoke_test(engine: Any) -> None:
    """Exercise real model distributions using only synthetic prompt text."""
    state = "Synthetic adapter smoke fixture: status complete; category alpha."
    questions = [
        {"t": "noul", "ins": "Does the synthetic state say the task is complete?", "crit": None},
        {"t": "choice", "ins": "Which synthetic category is stated?", "crit": {"alpha": "alpha is stated", "beta": "beta is stated"}},
        {"t": "score", "ins": "How much does the state support completion?", "crit": ["not supported", "partly supported", "fully supported"]},
    ]
    results = engine.decide_many(state, questions)
    if len(results) != 3:
        raise RuntimeError("smoke test returned unexpected answer count")
    converted = adapt_results(results, {
        "a": {"type": "noul"},
        "b": {"type": "choice"},
        "c": {"type": "score", "rubric": ["not supported", "partly supported", "fully supported"]},
    })
    for qid in ("a", "b", "c"):
        probs = converted[qid]["probabilities"]
        if not probs or any(not math.isfinite(float(v)) or float(v) < 0 for v in probs.values()):
            raise RuntimeError(f"smoke test produced invalid distribution for {qid}")
        if abs(sum(map(float, probs.values())) - 1) > 1e-6:
            raise RuntimeError(f"smoke test distribution for {qid} does not sum to one")
    print(json.dumps({"status": "ok", "model": MODEL_NAME, "weights_version": WEIGHTS_VERSION,
                      "questions": len(results), "synthetic_only": True}))


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--model-dir", type=Path, default=DEFAULT_MODEL_DIR)
    parser.add_argument("--addr", default="127.0.0.1:8799", help="local bind address (default: 127.0.0.1:8799)")
    parser.add_argument("--device", choices=("auto", "cuda", "cpu", "mps"), default="auto")
    parser.add_argument("--dtype", choices=("auto", "float32", "float16", "bfloat16"), default="auto")
    parser.add_argument("--verify", action="store_true", help="compatibility flag; packaged files are always verified")
    parser.add_argument("--smoke-test", action="store_true", help="run synthetic-only model inference and exit")
    args = parser.parse_args()

    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    model_load_started = time.monotonic()
    engine = load_engine(args.model_dir, args.device, args.dtype, verify=True)
    model_load_ms = int((time.monotonic() - model_load_started) * 1000)
    logging.info("Decision model loaded in %dms", model_load_ms)
    if args.smoke_test:
        smoke_test(engine)
        return
    host, sep, port = args.addr.rpartition(":")
    if not sep or not host or not port.isdigit():
        parser.error("--addr must be HOST:PORT")
    if host not in ("127.0.0.1", "localhost", "::1"):
        parser.error("refusing non-loopback bind address")
    server = BoundedThreadingHTTPServer((host, int(port)), DecisionHandler)
    server.engine = engine  # type: ignore[attr-defined]
    server.inference_lock = threading.Lock()  # type: ignore[attr-defined]
    logging.info("Decision model ready: %s (%s), listening on http://%s/v1/systemone", MODEL_NAME, WEIGHTS_VERSION, args.addr)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
