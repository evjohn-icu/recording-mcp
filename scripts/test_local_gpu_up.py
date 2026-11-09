#!/usr/bin/env python3
"""Unit tests for scripts/local-gpu-up.sh.

Every external dependency is a stub: no GPU, model weights, network or real
stack are required. The tests assert the launcher's observable contract
(preflight, probe output, readiness ordering, cycles, lock forwarding), not the
shell implementation.
"""
from __future__ import annotations

import json
import os
import shutil
import subprocess
import tempfile
import time
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "local-gpu-up.sh"
PINS = ROOT / "scripts" / "model-revisions.json"

PROBE_JSON = {
    "backend": "ark",
    "model": "Edge0/ARK-ASR-0.6B",
    "revision": "45776b56d58cdfb2e2eb632f7e110f38684633e0",
    "device": "cuda:0",
    "dtype": "float16",
    "model_load_ms": 1234,
    "vram_total_mb": 8192,
    "vram_free_mb": 7000,
    "vram_peak_mb": 4096,
}

START_STUB = """#!/usr/bin/env bash
set -euo pipefail
echo $$ > "$STUB_PID_FILE"
{
  echo "start $*"
  echo "ASR_LOCK=${ASR_LOCK:-}"
  echo "ASR_PROVIDER=${ASR_PROVIDER:-}"
  echo "ASR_TIMEOUT=${ASR_TIMEOUT:-}"
  echo "WORKER_HEALTH_PORT=${WORKER_HEALTH_PORT:-}"
  echo "EMBED_PROVIDER=${EMBED_PROVIDER:-}"
  echo "EMBED_ENDPOINT=${EMBED_ENDPOINT:-}"
  echo "SEMANTIC_PROVIDER=${SEMANTIC_PROVIDER:-}"
  echo "SEMANTIC_ENDPOINT=${SEMANTIC_ENDPOINT:-}"
} >> "$STUB_LOG"
trap 'echo "stop" >> "$STUB_LOG"; exit 0' TERM INT
while true; do sleep 0.05; done
"""

NVIDIA_STUB = """#!/bin/sh
echo "${STUB_VRAM_FREE:-7000}"
"""

PROFILE_STUB = """#!/usr/bin/env bash
if [ "$1" = list ]; then echo fixture; exit 0; fi
exit 0
"""


class GPUUpTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="local-gpu-up-")
        self.root = Path(self.tmp.name)
        (self.root / "scripts").mkdir()
        (self.root / "bin").mkdir()
        (self.root / "web" / "dist").mkdir(parents=True)
        (self.root / "web" / "dist" / "index.html").write_text("fixture")
        shutil.copy(SCRIPT, self.root / "scripts" / "local-gpu-up.sh")
        shutil.copy(PINS, self.root / "scripts" / "model-revisions.json")
        pins = json.loads(PINS.read_text())
        hf_root = self.root / "hf"
        self.hf_root = hf_root
        for name in ("asr_ark", "embed_qwen"):
            model = pins["models"][name]
            snapshot = hf_root / f"models--{model['repo_id'].replace('/', '--')}" / "snapshots" / model["revision"]
            snapshot.mkdir(parents=True, exist_ok=True)
        self.stub("bin/context-web", "#!/bin/sh\nexit 0\n")
        self.stub("bin/context-worker", "#!/bin/sh\nexit 0\n")
        self.stub("bin/context-profile", PROFILE_STUB)
        self.stub("bin/python", "#!/bin/sh\nexit 0\n")
        self.stub("bin/nvidia-smi", NVIDIA_STUB)
        self.stub("bin/ffmpeg", "#!/bin/sh\nexit 0\n")
        self.stub("bin/ffprobe", "#!/bin/sh\nexit 0\n")
        self.stub("bin/launch.sh", START_STUB)
        self.log = self.root / "stub.log"
        self.profiles = self.root / "profiles"
        self.profiles.mkdir()
        self.artifacts = self.root / "artifacts"

    def tearDown(self):
        # A killed launcher must not leave its stub child holding the port.
        pid_file = self.root / "stub.pid"
        if pid_file.exists():
            try:
                pid = int(pid_file.read_text().strip())
            except ValueError:
                pid = 0
            if pid > 0:
                try:
                    cmdline = Path(f"/proc/{pid}/cmdline").read_bytes().decode("utf-8", "replace")
                except OSError:
                    cmdline = ""
                if str(self.root) in cmdline:
                    try:
                        os.kill(pid, 9)
                    except ProcessLookupError:
                        pass
        self.tmp.cleanup()

    def stub(self, rel: str, body: str) -> Path:
        path = self.root / rel
        path.write_text(body)
        path.chmod(0o755)
        return path

    def env(self, **overrides: str) -> dict[str, str]:
        env = {k: v for k, v in os.environ.items() if not k.startswith("LOCAL_GPU_UP_")}
        env.update({
            "LOCAL_GPU_UP_TEST_MODE": "1",
            "LOCAL_GPU_UP_ARTIFACTS_DIR": str(self.artifacts),
            "LOCAL_GPU_UP_PYTHON": str(self.root / "bin" / "python"),
            "LOCAL_GPU_UP_ASR_PROBE": "printf '%s\\n' '" + json.dumps(PROBE_JSON) + "'",
            "LOCAL_GPU_UP_WEB_BIN": str(self.root / "bin" / "context-web"),
            "LOCAL_GPU_UP_WORKER_BIN": str(self.root / "bin" / "context-worker"),
            "LOCAL_GPU_UP_PROFILE_BIN": str(self.root / "bin" / "context-profile"),
            "LOCAL_GPU_UP_START_SCRIPT": str(self.root / "bin" / "launch.sh"),
            "LOCAL_GPU_UP_HEALTH_CMD": "true",
            "LOCAL_GPU_UP_WORKER_HEALTH_CMD": "true",
            "LOCAL_GPU_UP_NVIDIA_SMI": str(self.root / "bin" / "nvidia-smi"),
            "LOCAL_GPU_UP_HF_ROOT": str(self.hf_root),
            "LOCAL_GPU_UP_FFMPEG": str(self.root / "bin" / "ffmpeg"),
            "LOCAL_GPU_UP_FFPROBE": str(self.root / "bin" / "ffprobe"),
            "STUB_LOG": str(self.log),
            "STUB_PID_FILE": str(self.root / "stub.pid"),
        })
        env.update(overrides)
        return env

    def run_launcher(self, *args: str, **env: str) -> subprocess.CompletedProcess:
        return subprocess.run(
            [str(self.root / "scripts" / "local-gpu-up.sh"), *args],
            env=self.env(**env), capture_output=True, text=True, timeout=120,
        )

    def test_preflight_rejects_missing_gpu(self):
        proc = self.run_launcher("--profile", "fixture", "--backend", "ark", "--no-build",
                                 LOCAL_GPU_UP_NVIDIA_SMI=str(self.root / "bin" / "missing-nvidia-smi"))
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("nvidia-smi not found", proc.stderr)
        self.assertFalse((self.artifacts / "probe.json").exists(), "preflight must stop before the probe")

    def test_env_file_permissions_fail_with_clean_error(self):
        # die() must be defined before the .env validation block; a broken
        # ordering turns this into "die: command not found" (exit 127).
        env_file = self.root / ".env"
        env_file.write_text("")
        env_file.chmod(0o644)
        proc = self.run_launcher("--env-file", str(env_file), "--no-build",
                                 LOCAL_GPU_UP_TEST_MODE="0")
        self.assertEqual(proc.returncode, 1, proc.stderr)
        self.assertIn("env file must not be group/world accessible", proc.stderr)
        self.assertNotIn("command not found", proc.stderr)

    def test_preflight_rejects_low_vram(self):
        proc = self.run_launcher("--profile", "fixture", "--backend", "qwen", "--no-build",
                                 STUB_VRAM_FREE="1024")
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("MiB required", proc.stderr)

    def test_probe_reports_vram(self):
        proc = self.run_launcher("--profile", "fixture", "--backend", "ark", "--check", "--no-build")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertIn("load=1234ms", proc.stdout)
        self.assertIn("vram_total=8192MiB", proc.stdout)
        self.assertIn("vram_peak=4096MiB", proc.stdout)
        payload = json.loads((self.artifacts / "probe.json").read_text())
        self.assertEqual(payload["model_load_ms"], 1234)
        self.assertIn("check passed", proc.stdout)

    def test_cold_start_waits_for_health(self):
        # A single cycle owns the foreground: readiness is printed only after
        # both health checks pass, and TERM writes the shutdown artifact.
        proc = subprocess.Popen(
            [str(self.root / "scripts" / "local-gpu-up.sh"), "--profile", "fixture",
             "--backend", "ark", "--port", "18788", "--no-build"],
            env=self.env(), stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        lines: list[str] = []
        try:
            deadline = time.monotonic() + 60
            while time.monotonic() < deadline:
                line = proc.stdout.readline()
                if not line:
                    break
                lines.append(line)
                if "Recording-MCP ready" in line:
                    break
            output = "".join(lines)
            self.assertIn("cycle 1/1 ready", output)
            self.assertIn("Recording-MCP ready: http://127.0.0.1:18788", output)
            proc.terminate()
            self.assertEqual(proc.wait(timeout=60), 0)
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait(timeout=10)
        startup = json.loads((self.artifacts / "startup.json").read_text())
        self.assertEqual(len(startup["cycles"]), 1)
        cycle = startup["cycles"][0]
        self.assertEqual(cycle["model_load_ms"], 1234)
        self.assertEqual(cycle["vram_peak_mb"], 4096)
        self.assertGreaterEqual(cycle["cold_start_ms"], 0)
        self.assertGreaterEqual(cycle["shutdown_ms"], 0)
        log = self.log.read_text()
        self.assertEqual(log.count("start "), 1)
        self.assertEqual(log.count("stop"), 1)

    def test_five_start_stop_cycles(self):
        proc = self.run_launcher("--profile", "fixture", "--backend", "ark", "--cycles", "5", "--no-build")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        log = self.log.read_text()
        self.assertEqual(log.count("start "), 5)
        self.assertEqual(log.count("stop"), 5)
        startup = json.loads((self.artifacts / "startup.json").read_text())
        self.assertEqual([c["cycle"] for c in startup["cycles"]], [1, 2, 3, 4, 5])

    def test_lock_and_timeouts_are_forwarded(self):
        proc = self.run_launcher("--profile", "fixture", "--profiles-root", str(self.profiles),
                                 "--backend", "ark", "--cycles", "2", "--no-build")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        log = self.log.read_text()
        self.assertIn(f"ASR_LOCK={self.profiles}/fixture/gpu.lock", log)
        self.assertIn("ASR_PROVIDER=ark", log)
        self.assertIn("ASR_TIMEOUT=2h", log)
        self.assertIn("WORKER_HEALTH_PORT=8789", log)

    def test_with_embedding_exports_openai_compatible_config(self):
        proc = self.run_launcher("--profile", "fixture", "--backend", "ark", "--with-embedding",
                                 "--cycles", "2", "--no-build",
                                 LOCAL_GPU_UP_EMBED_CMD="trap 'exit 0' TERM; while :; do sleep 0.05; done",
                                 LOCAL_GPU_UP_EMBED_HEALTH_CMD="true")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertIn("embedding server ready", proc.stderr)
        log = self.log.read_text()
        self.assertIn("EMBED_PROVIDER=local", log)
        self.assertIn("EMBED_ENDPOINT=http://127.0.0.1:8790/v1/embeddings", log)
        self.assertIn("SEMANTIC_PROVIDER=openai", log)
        self.assertIn("SEMANTIC_ENDPOINT=http://127.0.0.1:8790/v1/embeddings", log)

    def test_interrupted_cycles_exit_nonzero(self):
        proc = subprocess.Popen(
            [str(self.root / "scripts" / "local-gpu-up.sh"), "--profile", "fixture",
             "--backend", "ark", "--cycles", "3", "--no-build"],
            env=self.env(), stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        try:
            deadline = time.monotonic() + 60
            while time.monotonic() < deadline:
                line = proc.stdout.readline()
                if not line:
                    break
                if "cycle 1/3 ready" in line:
                    break
            proc.terminate()
            code = proc.wait(timeout=60)
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait(timeout=10)
        self.assertEqual(code, 130, "an interrupted acceptance run must not exit 0")
        startup = json.loads((self.artifacts / "startup.json").read_text())
        self.assertLess(len(startup["cycles"]), 3)

    def test_unhealthy_stack_fails(self):
        proc = self.run_launcher("--profile", "fixture", "--backend", "ark", "--no-build",
                                 LOCAL_GPU_UP_HEALTH_CMD="false", LOCAL_GPU_UP_HEALTH_TIMEOUT="1")
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("did not become healthy", proc.stderr)
        self.assertIn("stop", self.log.read_text(), "the child stack must be stopped on failure")


if __name__ == "__main__":
    unittest.main()
