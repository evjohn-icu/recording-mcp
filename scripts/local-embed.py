#!/usr/bin/env python3
"""Local OpenAI-compatible embedding server (Qwen3-Embedding by default).

The server is opt-in: the product default stays the offline hash embedder. It
implements POST /v1/embeddings ({model,input,input_type?}) and GET /healthz so
the existing OpenAICompatProvider can talk to it without a second Go provider.

Query inputs get the Qwen3 retrieval instruction; documents are embedded as-is.
No request text is logged. torch/transformers are imported lazily so the
protocol can be exercised with --fake-encoder on a machine without them.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
PINS = ROOT / "scripts" / "model-revisions.json"
QUERY_PREFIX = "Instruct: Given a web search query, retrieve relevant passages that answer the query\nQuery: "
DEFAULT_MODEL = "Qwen/Qwen3-Embedding-0.6B"
DEFAULT_DIMS = 1024
MAX_INPUTS = 128
MAX_BODY = 4 << 20
LOOPBACK = {"127.0.0.1", "localhost"}


def log(message: str) -> None:
    print(f"local-embed: {message}", file=sys.stderr, flush=True)


def load_pin() -> dict:
    try:
        return json.loads(PINS.read_text(encoding="utf-8"))["models"]["embed_qwen"]
    except Exception:  # noqa: BLE001 - a missing pin file is reported at verification
        return {}


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for block in iter(lambda: handle.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


class GPULock:
    """Exclusive advisory lock shared with the ASR runner on one GPU."""

    def __init__(self, path: str, timeout: float):
        self.path = path
        self.timeout = timeout
        self._file = None

    def acquire(self):
        import fcntl

        self._file = open(self.path, "a+")
        deadline = time.monotonic() + self.timeout
        while True:
            try:
                fcntl.flock(self._file.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                return
            except BlockingIOError:
                if time.monotonic() > deadline:
                    self._file.close()
                    self._file = None
                    raise TimeoutError("GPU busy")
                time.sleep(0.1)

    def release(self):
        if self._file is None:
            return
        import fcntl

        try:
            fcntl.flock(self._file.fileno(), fcntl.LOCK_UN)
        finally:
            self._file.close()
            self._file = None


class FakeEncoder:
    """Deterministic vectors derived from sha256(text); test-only."""

    dims = DEFAULT_DIMS

    def encode(self, texts: list[str]) -> list[list[float]]:
        out = []
        for text in texts:
            digest = hashlib.sha256(text.encode("utf-8")).digest()
            raw = (digest * (self.dims // len(digest) + 1))[: self.dims]
            vector = [(byte - 127.5) / 127.5 for byte in raw]
            norm = math.sqrt(sum(value * value for value in vector)) or 1.0
            out.append([value / norm for value in vector])
        return out


class QwenEncoder:
    def __init__(self, model: str, revision: str, device: str, dtype: str, max_length: int, lock: GPULock | None, source: str = ""):
        import torch
        from transformers import AutoModel, AutoTokenizer

        self.torch = torch
        self.device = device
        self.max_length = max_length
        self.lock = lock
        chosen = dtype
        if dtype == "auto":
            chosen = "float32" if device == "cpu" else "float16"
        torch_dtype = {"float32": torch.float32, "float16": torch.float16}.get(chosen)
        if torch_dtype is None:
            raise SystemExit(f"local-embed: unsupported dtype {dtype}")
        if device != "cpu" and chosen == "float16" and not torch.cuda.is_available():
            raise SystemExit("local-embed: CUDA device requested but torch.cuda.is_available() is false")
        if lock:
            lock.acquire()
        try:
            weights = source or model
            self.tokenizer = AutoTokenizer.from_pretrained(weights, revision=revision or None, padding_side="left")
            self.model = AutoModel.from_pretrained(weights, revision=revision or None, dtype=torch_dtype)
            self.model = self.model.to(device).eval()
        finally:
            if lock:
                lock.release()
        self.dims = int(getattr(self.model.config, "hidden_size", DEFAULT_DIMS))

    def encode(self, texts: list[str]) -> list[list[float]]:
        torch = self.torch
        if self.lock:
            self.lock.acquire()
        try:
            batch = self.tokenizer(list(texts), padding=True, truncation=True,
                                   max_length=self.max_length, return_tensors="pt").to(self.device)
            with torch.inference_mode():
                outputs = self.model(**batch)
            hidden = outputs.last_hidden_state
            # Left padding puts every real token at the end of the row, so the
            # last position is the last real token for every row. Indexing by
            # (mask.sum-1) would select the FIRST real token of short rows.
            pooled = hidden[:, -1]
            normalized = torch.nn.functional.normalize(pooled, p=2, dim=1)
            return normalized.to(torch.float32).cpu().tolist()
        finally:
            if self.lock:
                self.lock.release()


class Service:
    def __init__(self, args):
        self.args = args
        self.model = args.model
        self.revision = args.revision
        self.dims = DEFAULT_DIMS
        self.loaded = False
        self.sha_ok = False
        self.lock = None
        self._compat_lock = threading.Lock()
        self._infer_lock = threading.Lock()
        self.last_request = time.monotonic()
        self.encoder = None

    def touch(self) -> None:
        self.last_request = time.monotonic()

    def prepare(self) -> None:
        pin = load_pin()
        if self.args.model == "test-fake":
            self.encoder = FakeEncoder()
            self.dims = self.encoder.dims
            self.loaded = True
            self.sha_ok = True
            return
        verify = self.args.verify_model or self.args.model == DEFAULT_MODEL
        if verify:
            if not self.revision:
                self.revision = pin.get("revision", "")
            expected = pin.get("safetensors_sha256", "")
            self.sha_ok = self.verify(expected)
            if not self.sha_ok and not self.args.allow_unverified_model:
                raise SystemExit("local-embed: model.safetensors SHA-256 does not match scripts/model-revisions.json "
                                 "(pass --allow-unverified-model for a local directory)")
        if self.args.preload:
            self.ensure_encoder()

    def model_dir(self) -> Path:
        if self.args.model_dir:
            return Path(self.args.model_dir)
        hub = Path(os.environ.get("HF_HOME", str(Path.home() / ".cache" / "huggingface"))) / "hub"
        return hub / f"models--{self.args.model.replace('/', '--')}" / "snapshots" / (self.revision or "main")

    def verify(self, expected: str) -> bool:
        if not expected:
            return True
        weights = self.model_dir() / "model.safetensors"
        if not weights.exists():
            log(f"cannot verify {weights}; model file missing")
            return False
        actual = sha256_file(weights)
        if actual != expected:
            log(f"model.safetensors sha256={actual}, pin={expected}")
            return False
        return True

    def ensure_encoder(self):
        with self._compat_lock:
            if self.encoder is not None:
                return self.encoder
            if self.args.device.startswith("cuda") and self.args.gpu_lock:
                self.lock = GPULock(self.args.gpu_lock, self.args.lock_timeout)
            self.encoder = QwenEncoder(self.model, self.revision, self.args.device, self.args.dtype,
                                       self.args.max_length, self.lock, source=self.args.model_dir)
            self.dims = self.encoder.dims
            self.loaded = True
            log(f"model ready: {self.model}@{self.revision or 'main'} dims={self.dims} device={self.args.device}")
            return self.encoder

    def embed(self, texts: list[str], input_type: str) -> list[list[float]]:
        prepared = []
        for text in texts:
            if input_type == "query":
                prepared.append(QUERY_PREFIX + text)
            else:
                prepared.append(text)
        encoder = self.ensure_encoder()
        out: list[list[float]] = []
        # One model instance is shared by the threaded HTTP server; serialize
        # inference so concurrent requests cannot interleave CUDA calls.
        with self._infer_lock:
            for start in range(0, len(prepared), self.args.batch_size):
                out.extend(encoder.encode(prepared[start:start + self.args.batch_size]))
        return out

    def health(self) -> dict:
        return {
            "status": "ok",
            "model": self.model,
            "revision": self.revision,
            "device": self.args.device,
            "dtype": self.args.dtype,
            "dims": self.dims,
            "loaded": self.loaded,
            "model_sha256_ok": self.sha_ok,
            "verified": self.args.verify_model or self.model == DEFAULT_MODEL,
        }


def make_handler(service: Service):
    class Handler(BaseHTTPRequestHandler):
        server_version = "local-embed/1"

        def log_message(self, _fmt, *args):  # keep request text out of logs
            return

        def _send(self, status: int, payload: dict) -> None:
            body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def _reject_browser(self) -> bool:
            if self.headers.get("Origin"):
                self._send(403, {"error": {"message": "browser origins are not allowed"}})
                return True
            return False

        def do_GET(self):  # noqa: N802 - http.server API
            service.touch()
            if self._reject_browser():
                return
            if self.path.split("?", 1)[0] == "/healthz":
                self._send(200, service.health())
                return
            self._send(404, {"error": {"message": "not found"}})

        def do_POST(self):  # noqa: N802 - http.server API
            service.touch()
            if self._reject_browser():
                return
            if self.path.split("?", 1)[0] != "/v1/embeddings":
                self._send(404, {"error": {"message": "not found"}})
                return
            length = int(self.headers.get("Content-Length") or 0)
            if length <= 0 or length > MAX_BODY:
                self._send(413, {"error": {"message": "request body too large or empty"}})
                return
            try:
                payload = json.loads(self.rfile.read(length))
            except json.JSONDecodeError:
                self._send(400, {"error": {"message": "invalid JSON"}})
                return
            model = payload.get("model") or ""
            if model != service.model:
                self._send(400, {"error": {"message": "unknown model"}})
                return
            raw = payload.get("input")
            texts = [raw] if isinstance(raw, str) else raw
            if not isinstance(texts, list) or not texts or not all(isinstance(item, str) for item in texts):
                self._send(400, {"error": {"message": "input must be a string or a non-empty string list"}})
                return
            if len(texts) > MAX_INPUTS:
                self._send(413, {"error": {"message": f"at most {MAX_INPUTS} inputs per request"}})
                return
            try:
                vectors = service.embed(texts, str(payload.get("input_type") or ""))
            except TimeoutError:
                self._send(503, {"error": {"message": "GPU busy"}})
                return
            except Exception as exc:  # noqa: BLE001 - reported without request text
                log(f"embedding failed: {type(exc).__name__}: {exc}")
                self._send(500, {"error": {"message": f"embedding failed: {type(exc).__name__}"}})
                return
            self._send(200, {
                "model": service.model,
                "data": [{"embedding": vector, "index": index} for index, vector in enumerate(vectors)],
                "usage": {"prompt_tokens": sum(len(text) for text in texts), "total_tokens": sum(len(text) for text in texts)},
            })

    return Handler


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="local OpenAI-compatible embedding server")
    p.add_argument("--serve", action="store_true", default=True)
    p.add_argument("--host", default="127.0.0.1")
    p.add_argument("--port", type=int, default=8790)
    p.add_argument("--model", default=DEFAULT_MODEL)
    p.add_argument("--revision", default="")
    p.add_argument("--model-dir", default="")
    p.add_argument("--device", default="cpu")
    p.add_argument("--dtype", default="auto")
    p.add_argument("--max-length", type=int, default=2048)
    p.add_argument("--batch-size", type=int, default=16)
    p.add_argument("--preload", action="store_true")
    p.add_argument("--idle-timeout", type=float, default=0)
    p.add_argument("--gpu-lock", default="")
    p.add_argument("--lock-timeout", type=float, default=600)
    p.add_argument("--verify-model", action="store_true")
    p.add_argument("--allow-unverified-model", action="store_true")
    p.add_argument("--fake-encoder", action="store_true")
    args = p.parse_args()
    if args.fake_encoder and args.model != "test-fake":
        raise SystemExit("local-embed: --fake-encoder requires --model test-fake")
    return args


def main() -> int:
    args = parse_args()
    if args.host not in LOOPBACK:
        raise SystemExit("local-embed: --host must be a loopback address")
    if args.batch_size <= 0:
        raise SystemExit("local-embed: --batch-size must be positive")
    service = Service(args)
    try:
        service.prepare()
    except SystemExit:
        raise
    server = ThreadingHTTPServer((args.host, args.port), make_handler(service))
    if args.idle_timeout > 0:
        def watchdog() -> None:
            while True:
                time.sleep(min(5.0, args.idle_timeout))
                if time.monotonic() - service.last_request >= args.idle_timeout:
                    log(f"idle for {args.idle_timeout}s; shutting down")
                    server.shutdown()
                    return

        threading.Thread(target=watchdog, daemon=True).start()
    log(f"listening on http://{args.host}:{args.port} model={args.model} device={args.device}")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        raise SystemExit(130)
