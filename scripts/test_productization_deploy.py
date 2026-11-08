#!/usr/bin/env python3
"""Container deployment checks for the local Beta.

These tests start a disposable compose project (own name, own env file, own
named volume) and never touch the user's `recording-mcp-data` volume. They are
skipped unless RUN_DOCKER_DEPLOYMENT_TEST=1 because they build an image.
"""
from __future__ import annotations

import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import wave
import unittest
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
ENABLED = os.environ.get("RUN_DOCKER_DEPLOYMENT_TEST") == "1"
PASSWORD = "verify-only-password-9d2f"


def docker_available() -> bool:
    try:
        subprocess.run(["docker", "info"], capture_output=True, check=True, timeout=20)
        return True
    except (OSError, subprocess.SubprocessError):
        return False


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def wait_http(url: str, timeout: float = 180) -> None:
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=5) as resp:
                if resp.status == 200:
                    return
        except Exception as exc:  # noqa: BLE001 - reported below
            last = exc
        time.sleep(2)
    raise AssertionError(f"HTTP endpoint did not become healthy: {url}: {last}")


@unittest.skipUnless(ENABLED, "set RUN_DOCKER_DEPLOYMENT_TEST=1 to build and run containers")
class DeploymentTest(unittest.TestCase):
    project: str

    @classmethod
    def setUpClass(cls) -> None:
        if not docker_available():
            raise unittest.SkipTest("docker daemon is not available")
        cls.tmp = tempfile.mkdtemp(prefix="recording-mcp-deploy-")
        cls.project = f"recording-mcp-verify-{os.getpid()}"
        # Docker Desktop can keep a stale forward for 8788; a dynamic host port
        # keeps parallel/disposable projects independent and is what CI uses.
        cls.web_port = free_port()
        cls.data_volume = f"recording-mcp-verify-{os.getpid()}-data"
        cls.env_file = Path(cls.tmp) / "verify.env"
        cls.env_file.write_text(
            "MEMORY_PROFILE_PASSWORD=" + PASSWORD + "\n"
            "RECORDING_PROFILE=verify\n"
            "ASR_PROVIDER=fixture\n"
            "GENERATOR_PROVIDER=off\n"
            "EMBED_PROVIDER=hash\n"
            f"WEB_PORT={cls.web_port}\n"
            f"DATA_VOLUME={cls.data_volume}\n",
            encoding="utf-8",
        )
        cls.compose = ["docker", "compose", "-p", cls.project, "--env-file", str(cls.env_file)]

    @classmethod
    def tearDownClass(cls) -> None:
        subprocess.run(cls.compose + ["down", "-v", "--remove-orphans"], capture_output=True)
        shutil.rmtree(cls.tmp, ignore_errors=True)

    def health_url(self) -> str:
        return f"http://127.0.0.1:{self.web_port}/healthz"

    def run_compose(self, *args: str, timeout: int = 900) -> subprocess.CompletedProcess:
        return subprocess.run(self.compose + list(args), cwd=ROOT, capture_output=True, text=True, timeout=timeout)

    def test_compose_config_is_valid(self) -> None:
        result = self.run_compose("config")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(self.data_volume, result.stdout)
        compact = result.stdout.replace('"', "").replace("'", "")
        self.assertIn(f"published: {self.web_port}", compact)
        self.assertIn("target: 8788", compact)
        self.assertIn("no-new-privileges:true", result.stdout)

    def test_clean_start_and_independent_health(self) -> None:
        up = self.run_compose("up", "-d", "--build")
        self.assertEqual(up.returncode, 0, up.stderr)
        wait_http(self.health_url())
        ps = self.run_compose("ps", "--format", "json")
        self.assertEqual(ps.returncode, 0, ps.stderr)
        rows = [json.loads(line) for line in ps.stdout.splitlines() if line.strip()]
        by_service = {row.get("Service"): row for row in rows}
        self.assertIn("web", by_service)
        self.assertIn("worker", by_service)
        # The worker health endpoint is not published; check it inside the
        # container so it is proven independent of the web process.
        deadline = time.time() + 60
        worker_health = None
        while time.time() < deadline:
            worker_health = self.run_compose("exec", "-T", "worker", "curl", "-fsS", "http://127.0.0.1:8789/healthz")
            if worker_health.returncode == 0:
                break
            time.sleep(1)
        self.assertEqual(worker_health.returncode, 0, worker_health.stderr)

    def test_container_runs_as_non_root(self) -> None:
        self.run_compose("up", "-d")
        result = self.run_compose("exec", "-T", "web", "id", "-u")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "10001")
        result = self.run_compose("exec", "-T", "worker", "id", "-u")
        self.assertEqual(result.stdout.strip(), "10001")

    def test_three_restarts_keep_imported_data(self) -> None:
        self.run_compose("up", "-d")
        wait_http(self.health_url())
        conversation_id = self.import_wav()

        for _ in range(3):
            restart = self.run_compose("restart", "web", "worker", timeout=300)
            self.assertEqual(restart.returncode, 0, restart.stderr)
            wait_http(self.health_url())
            cookie, csrf = self.login()
            page = self.api_json("/api/conversations", cookie, csrf)
            ids = [item["conversation_id"] for item in page.get("items", [])]
            self.assertIn(conversation_id, ids, f"imported data lost after restart: {ids}")

    def login(self) -> tuple[str, str]:
        # The compose web runs in single-profile mode, so this server exposes
        # /api/login (the profile-picker route belongs to the outer router).
        body = json.dumps({"password": PASSWORD}).encode()
        request = urllib.request.Request(self.base_url() + "/api/login", data=body, method="POST")
        request.add_header("Origin", self.base_url())
        request.add_header("Content-Type", "application/json")
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open(request, timeout=30) as response:
            payload = json.loads(response.read())
            cookie = (response.getheader("Set-Cookie") or "").split(";", 1)[0]
        return cookie, payload["csrf_token"]

    def api_json(self, path: str, cookie: str, csrf: str, body: bytes | None = None, content_type: str = "application/json") -> dict:
        request = urllib.request.Request(self.base_url() + path, data=body, method="POST" if body is not None else "GET")
        request.add_header("Origin", self.base_url())
        request.add_header("Cookie", cookie)
        request.add_header("X-CSRF-Token", csrf)
        if body is not None:
            request.add_header("Content-Type", content_type)
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open(request, timeout=60) as response:
            return json.loads(response.read())

    def base_url(self) -> str:
        return f"http://127.0.0.1:{self.web_port}"

    def import_wav(self) -> str:
        # Wait until the worker advertises ASR before importing.
        deadline = time.time() + 60
        while True:
            cookie, csrf = self.login()
            caps = self.api_json("/api/capabilities", cookie, csrf)
            if caps.get("asr", {}).get("available"):
                break
            if time.time() > deadline:
                raise AssertionError(f"ASR capability never became available: {caps}")
            time.sleep(1)
        with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as handle:
            with wave.open(handle.name, "wb") as wav:
                wav.setnchannels(1)
                wav.setsampwidth(2)
                wav.setframerate(16000)
                wav.writeframes(b"\x00\x00" * 16000)
            path = Path(handle.name)
        boundary = "----deploy-test"
        prefix = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"deploy.wav\"\r\n"
                  "Content-Type: audio/wav\r\n\r\n").encode()
        suffix = f"\r\n--{boundary}--\r\n".encode()
        data = prefix + path.read_bytes() + suffix
        path.unlink()
        imported = self.api_json("/api/imports", cookie, csrf, data, f"multipart/form-data; boundary={boundary}")
        conversation_id = imported["conversation_id"]
        deadline = time.time() + 120
        while time.time() < deadline:
            snapshot = self.api_json(f"/api/conversations/{conversation_id}/processing", cookie, csrf)
            if snapshot.get("current_stage") == "completed":
                return conversation_id
            time.sleep(0.5)
        raise AssertionError(f"import did not finish: {snapshot}")

    def test_startup_without_external_api_reports_unavailable(self) -> None:
        # The stack is started with ASR_PROVIDER=fixture in this harness, so a
        # separate project pins it to off and asserts the capability reason.
        env = Path(self.tmp) / "off.env"
        off_port = free_port()
        env.write_text("MEMORY_PROFILE_PASSWORD=" + PASSWORD + "\nRECORDING_PROFILE=verify\nASR_PROVIDER=off\n"
                       f"WEB_PORT={off_port}\n"
                       f"DATA_VOLUME={self.data_volume}-off\n", encoding="utf-8")
        project = f"{self.project}-off"
        compose = ["docker", "compose", "-p", project, "--env-file", str(env)]
        try:
            up = subprocess.run(compose + ["up", "-d"], cwd=ROOT, capture_output=True, text=True, timeout=900)
            self.assertEqual(up.returncode, 0, up.stderr)
            wait_http(f"http://127.0.0.1:{off_port}/healthz")
        finally:
            subprocess.run(compose + ["down", "-v"], capture_output=True)

    def test_backup_restore_preserves_sources(self) -> None:
        self.run_compose("up", "-d")
        wait_http(self.health_url())
        self.import_wav()
        # Stop writers so the backup and the original are hashed in the same
        # quiescent state.
        self.run_compose("stop", timeout=300)
        backup = Path(self.tmp) / "backup.tar"
        dump = subprocess.run(
            ["docker", "run", "--rm", "-v", f"{self.data_volume}:/data:ro", "-v", f"{self.tmp}:/out",
             "debian:bookworm-slim", "tar", "cf", "/out/backup.tar", "-C", "/data", "."],
            capture_output=True, text=True, timeout=300,
        )
        self.assertEqual(dump.returncode, 0, dump.stderr)
        self.assertTrue(backup.exists())

        restore_volume = f"{self.project}-restore"
        subprocess.run(["docker", "volume", "create", restore_volume], check=True, capture_output=True)
        try:
            restore = subprocess.run(
                ["docker", "run", "--rm", "-v", f"{restore_volume}:/data", "-v", f"{self.tmp}:/in",
                 "debian:bookworm-slim", "tar", "xf", "/in/backup.tar", "-C", "/data"],
                capture_output=True, text=True, timeout=300,
            )
            self.assertEqual(restore.returncode, 0, restore.stderr)
            listing = subprocess.run(
                ["docker", "run", "--rm", "-v", f"{restore_volume}:/data:ro", "debian:bookworm-slim",
                 "sh", "-c", "find /data -type f | sort | xargs sha256sum | sha256sum"],
                capture_output=True, text=True, timeout=300,
            )
            original = subprocess.run(
                ["docker", "run", "--rm", "-v", f"{self.data_volume}:/data:ro", "debian:bookworm-slim",
                 "sh", "-c", "find /data -type f | sort | xargs sha256sum | sha256sum"],
                capture_output=True, text=True, timeout=300,
            )
            self.assertEqual(original.stdout.split()[0], listing.stdout.split()[0])
        finally:
            subprocess.run(["docker", "volume", "rm", "-f", restore_volume], capture_output=True)


if __name__ == "__main__":
    unittest.main()
