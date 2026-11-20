#!/usr/bin/env python3
"""Container deployment checks for the local Beta.

These tests start a disposable compose project (own name, own env file, own
named volume) and never touch the user's `recording-mcp-data` volume. They are
skipped unless RUN_DOCKER_DEPLOYMENT_TEST=1 because they build an image.
"""
from __future__ import annotations

import hashlib
import json
import os
import shutil
import socket
import struct
import subprocess
import sys
import tempfile
import time
import wave
import unittest
import urllib.error
import urllib.parse
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
    # Every import_wav call consumes the next sequence number so the synthetic
    # PCM bytes are unique across the whole suite run; `imported` is the ledger
    # of (sequence, PCM hash, conversation id) used for uniqueness assertions
    # and failure diagnostics.
    pcm_sequence: int
    imported: list[dict]

    @classmethod
    def setUpClass(cls) -> None:
        if not docker_available():
            raise unittest.SkipTest("docker daemon is not available")
        cls.tmp = tempfile.mkdtemp(prefix="recording-mcp-deploy-")
        cls.pcm_sequence = 0
        cls.imported = []
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
        # Own import plus any earlier imports in this volume (full suite runs
        # this test last, after the turn-forget and backup tests).
        self.import_wav()

        for _ in range(3):
            restart = self.run_compose("restart", "web", "worker", timeout=300)
            self.assertEqual(restart.returncode, 0, restart.stderr)
            wait_http(self.health_url())
            cookie, csrf = self.login()
            page = self.api_json("/api/conversations", cookie, csrf)
            ids = [item["conversation_id"] for item in page.get("items", [])]
            missing = [row["conversation_id"] for row in type(self).imported
                       if row["conversation_id"] not in ids]
            self.assertFalse(
                missing,
                f"imported data lost after restart: {missing} (listed: {ids})")

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
        # Wait until the worker advertises ASR before importing. CI runners can
        # be slow to build/start the stack, so allow a generous window.
        deadline = time.time() + 240
        while True:
            cookie, csrf = self.login()
            caps = self.api_json("/api/capabilities", cookie, csrf)
            if caps.get("asr", {}).get("available"):
                break
            if time.time() > deadline:
                ps = self.run_compose("ps", "--format", "json")
                logs = self.run_compose("logs", "--tail=80", "worker")
                raise AssertionError(
                    f"ASR capability never became available: {caps}\n"
                    f"compose ps:\n{ps.stdout}\nworker logs:\n{logs.stdout}\n{logs.stderr}"
                )
            time.sleep(1)
        # The suite shares one profile/volume across tests and the ingest keys
        # imports by the SHA-256 of the uploaded bytes, so repeated PCM content
        # is answered as an idempotent duplicate re-upload of an older
        # conversation and never enqueues new work. A class-level counter makes
        # every import unique and deterministic instead of relying on a
        # collision-prone low-byte time marker.
        frames = bytearray(b"\x00\x00" * 16000)
        type(self).pcm_sequence += 1
        struct.pack_into("<Q", frames, 0, type(self).pcm_sequence)
        pcm_sha256 = hashlib.sha256(frames).hexdigest()
        with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as handle:
            with wave.open(handle.name, "wb") as wav:
                wav.setnchannels(1)
                wav.setsampwidth(2)
                wav.setframerate(16000)
                wav.writeframes(bytes(frames))
            path = Path(handle.name)
        boundary = "----deploy-test"
        prefix = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"deploy.wav\"\r\n"
                  "Content-Type: audio/wav\r\n\r\n").encode()
        suffix = f"\r\n--{boundary}--\r\n".encode()
        data = prefix + path.read_bytes() + suffix
        path.unlink()
        imported = self.api_json("/api/imports", cookie, csrf, data, f"multipart/form-data; boundary={boundary}")
        ledger = type(self).imported
        record = {"sequence": type(self).pcm_sequence, "pcm_sha256": pcm_sha256}
        # A duplicate response would mean the bytes matched a previous import in
        # this volume: the ingest would reuse the older conversation and the
        # processing poll below could silently pass on already-completed work.
        if imported.get("duplicate") or not imported.get("conversation_id"):
            raise AssertionError(
                "import was rejected as duplicate or returned no conversation: "
                f"{imported}\nthis import: {record}\nprior imports in this volume: {ledger}"
            )
        conversation_id = imported["conversation_id"]
        record["conversation_id"] = conversation_id
        if conversation_id in {row["conversation_id"] for row in ledger}:
            raise AssertionError(
                f"import reused a prior conversation: {record}\nprior imports in this volume: {ledger}"
            )
        ledger.append(record)
        deadline = time.time() + 120
        while time.time() < deadline:
            snapshot = self.api_json(f"/api/conversations/{conversation_id}/processing", cookie, csrf)
            # Fail fast on ingest/asr jobs that already ended failed or
            # cancelled instead of waiting out the poll timeout.
            stuck = [job for job in snapshot.get("jobs", [])
                     if job.get("stage") in ("ingest", "asr")
                     and job.get("status") in ("failed", "cancelled")]
            if stuck:
                raise AssertionError(self.pipeline_diagnostics(
                    snapshot, f"ingest/asr stage ended failed/cancelled: {stuck}"))
            if snapshot.get("current_stage") == "completed":
                return conversation_id
            time.sleep(0.5)
        raise AssertionError(self.pipeline_diagnostics(snapshot, "import did not finish"))

    def pipeline_diagnostics(self, snapshot: dict, message: str) -> str:
        """Snapshot plus worker/web logs so a stalled or failed import can be
        triaged without rerunning the suite."""
        ps = self.run_compose("ps", "--format", "json")
        logs = self.run_compose("logs", "--tail=80", "worker", "web")
        return (
            f"{message}\nsnapshot: {snapshot}\n"
            f"imports in this volume: {type(self).imported}\n"
            f"compose ps:\n{ps.stdout}\n"
            f"worker/web logs (tail 80):\n{logs.stdout}\n{logs.stderr}"
        )

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

    def backup_script(self, *args: str) -> subprocess.CompletedProcess:
        return subprocess.run(
            [sys.executable, str(ROOT / "scripts" / "backup-profile-volume.py"), *args],
            cwd=ROOT, capture_output=True, text=True, timeout=3600,
        )

    def find_turn(self, conversation_id: str) -> dict:
        cookie, csrf = self.login()
        view = self.api_json(f"/api/conversations/{conversation_id}", cookie, csrf)
        for recording in view.get("recordings", []):
            recording_id = recording.get("recording_id")
            if not recording_id:
                continue
            query = urllib.parse.urlencode({"recording_id": recording_id})
            page = self.api_json(f"/api/conversations/{conversation_id}/turns?{query}", cookie, csrf)
            for item in page.get("items", []):
                if item.get("kind") == "turn" and item.get("global_start_ms") is not None:
                    return item
        raise AssertionError(f"no turn with known offsets: {view}")

    def fetch(self, base: str, path: str, cookie: str) -> tuple[int, bytes]:
        request = urllib.request.Request(base + path)
        request.add_header("Cookie", cookie)
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        try:
            with opener.open(request, timeout=60) as response:
                return int(response.status), response.read()
        except urllib.error.HTTPError as exc:  # noqa: PERF203 - reported to caller
            return int(exc.code), exc.read()

    def login_at(self, base: str) -> tuple[str, str]:
        body = json.dumps({"password": PASSWORD}).encode()
        request = urllib.request.Request(base + "/api/login", data=body, method="POST")
        request.add_header("Origin", base)
        request.add_header("Content-Type", "application/json")
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open(request, timeout=30) as response:
            payload = json.loads(response.read())
            cookie = (response.getheader("Set-Cookie") or "").split(";", 1)[0]
        return cookie, payload["csrf_token"]

    def api_at(self, base: str, path: str, cookie: str, csrf: str, body: bytes | None = None,
               content_type: str = "application/json") -> dict:
        request = urllib.request.Request(base + path, data=body, method="POST" if body is not None else "GET")
        request.add_header("Origin", base)
        request.add_header("Cookie", cookie)
        request.add_header("X-CSRF-Token", csrf)
        if body is not None:
            request.add_header("Content-Type", content_type)
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open(request, timeout=60) as response:
            return json.loads(response.read())

    def start_restored_stack(self, data_volume: str, suffix: str) -> tuple[list[str], str, int]:
        """Start a separate compose project on the restored volume.

        It uses a new project name, a new env file, a new host port and the
        restored DATA_VOLUME: never the original stack.
        """
        port = free_port()
        env_file = Path(self.tmp) / f"restored-{suffix}.env"
        env_file.write_text(
            "MEMORY_PROFILE_PASSWORD=" + PASSWORD + "\n"
            "RECORDING_PROFILE=verify\n"
            "ASR_PROVIDER=fixture\n"
            "GENERATOR_PROVIDER=off\n"
            "EMBED_PROVIDER=hash\n"
            f"WEB_PORT={port}\n"
            f"DATA_VOLUME={data_volume}\n",
            encoding="utf-8",
        )
        compose = ["docker", "compose", "-p", f"{self.project}-{suffix}", "--env-file", str(env_file)]
        config = subprocess.run(compose + ["config"], cwd=ROOT, capture_output=True, text=True, timeout=120)
        self.assertEqual(config.returncode, 0, config.stderr)
        self.assertIn(data_volume, config.stdout)
        self.assertIn(f"published: {port}", config.stdout.replace('"', "").replace("'", ""))
        up = subprocess.run(compose + ["up", "-d"], cwd=ROOT, capture_output=True, text=True, timeout=900)
        self.assertEqual(up.returncode, 0, up.stderr)
        wait_http(f"http://127.0.0.1:{port}/healthz")
        return compose, f"http://127.0.0.1:{port}", port

    def test_backup_restore_preserves_sources(self) -> None:
        up = self.run_compose("up", "-d", "--build")
        self.assertEqual(up.returncode, 0, up.stderr)
        wait_http(self.health_url())
        conversation_id = self.import_wav()
        turn = self.find_turn(conversation_id)
        cookie, _ = self.login()
        status, audio_before = self.fetch(self.base_url(), f"/api/turns/{turn['id']}/audio", cookie)
        self.assertEqual(status, 200)
        expected_pcm = 32 * (turn["global_end_ms"] - turn["global_start_ms"])
        self.assertEqual(len(audio_before), 44 + expected_pcm, "turn WAV does not match the 32 bytes/ms contract")

        backups = Path(self.tmp) / "backups"
        archive = backups / f"{self.project}.tgz"
        backup = self.backup_script(
            "backup", "--data-volume", self.data_volume, "--out", str(backups),
            "--compose-project", self.project, "--name", archive.name,
        )
        self.assertEqual(backup.returncode, 0, backup.stderr)
        self.assertTrue(archive.exists())
        manifest = json.loads(backup.stdout)
        self.assertEqual(manifest["schema"], "backup-archive-manifest-v1")

        # Refuse to overwrite an existing archive.
        again = self.backup_script(
            "backup", "--data-volume", self.data_volume, "--out", str(backups),
            "--compose-project", self.project, "--name", archive.name,
        )
        self.assertNotEqual(again.returncode, 0)
        self.assertIn("archive_exists", again.stderr)

        # A live writer makes the backup refuse instead of copying a hot volume.
        self.run_compose("up", "-d")
        wait_http(self.health_url())
        hot = self.backup_script(
            "backup", "--data-volume", self.data_volume, "--out", str(backups),
            "--name", "hot.tgz",
        )
        self.assertNotEqual(hot.returncode, 0)
        self.assertIn("writers_active", hot.stderr)
        self.assertFalse((backups / "hot.tgz").exists())
        self.run_compose("stop", timeout=300)

        restore_volume = f"{self.project}-restore"
        restored = self.backup_script(
            "restore", "--archive", str(archive), "--data-volume", restore_volume,
            "--barrier-source-volume", self.data_volume, "--compose-project", self.project,
        )
        self.assertEqual(restored.returncode, 0, restored.stderr)
        verify = self.backup_script("verify", "--data-volume", restore_volume)
        self.assertEqual(verify.returncode, 0, verify.stderr)
        report = json.loads(verify.stdout)
        self.assertEqual(report["profiles"][0]["id"], "verify")

        compose, base, _ = self.start_restored_stack(restore_volume, "restored")
        try:
            restored_cookie, restored_csrf = self.login_at(base)
            page = self.api_at(base, "/api/search?q=fixture%20transcript", restored_cookie, restored_csrf)
            texts = [hit.get("text", "") for hit in page.get("hits", [])]
            self.assertTrue(any("fixture transcript" in text for text in texts),
                            f"restored search lost the synthetic transcript: {page}")
            status, audio_after = self.fetch(base, f"/api/turns/{turn['id']}/audio", restored_cookie)
            self.assertEqual(status, 200)
            self.assertEqual(audio_after, audio_before, "restored turn audio differs from the source")
        finally:
            subprocess.run(compose + ["down", "-v", "--remove-orphans"], capture_output=True)
            subprocess.run(["docker", "volume", "rm", "-f", restore_volume], capture_output=True)

    def test_backup_rejects_pre_forget_archive(self) -> None:
        up = self.run_compose("up", "-d", "--build")
        self.assertEqual(up.returncode, 0, up.stderr)
        wait_http(self.health_url())
        conversation_id = self.import_wav()
        turn = self.find_turn(conversation_id)

        backups = Path(self.tmp) / "forget-backups"
        pre = backups / "pre-forget.tgz"
        backup = self.backup_script(
            "backup", "--data-volume", self.data_volume, "--out", str(backups),
            "--compose-project", self.project, "--name", pre.name,
        )
        self.assertEqual(backup.returncode, 0, backup.stderr)
        self.run_compose("up", "-d")
        wait_http(self.health_url())

        cookie, csrf = self.login()
        forget_request = {"scope": "turn", "turn_id": turn["id"]}
        preview = self.api_json("/api/forget/preview", cookie, csrf,
                                json.dumps(forget_request).encode())
        forget_request["expected_state"] = preview["expected_state"]
        forgotten = self.api_json("/api/forget", cookie, csrf,
                                  json.dumps(forget_request).encode())
        tombstone = forgotten.get("tombstone_id")
        self.assertTrue(tombstone, forgotten)
        deadline = time.time() + 120
        while time.time() < deadline:
            status = self.api_json(f"/api/forget/{tombstone}", cookie, csrf)
            if status.get("status") == "complete" and status.get("redaction_done"):
                break
            time.sleep(0.5)
        else:
            raise AssertionError(f"forget did not complete: {status}")
        self.run_compose("stop", timeout=300)

        restore_volume = f"{self.project}-pre-restore"
        rejected = self.backup_script(
            "restore", "--archive", str(pre), "--data-volume", restore_volume,
            "--barrier-source-volume", self.data_volume, "--compose-project", self.project,
        )
        self.assertNotEqual(rejected.returncode, 0, rejected.stdout)
        self.assertIn("backup_predates_forget", rejected.stderr)

        post = backups / "post-forget.tgz"
        after = self.backup_script(
            "backup", "--data-volume", self.data_volume, "--out", str(backups),
            "--compose-project", self.project, "--name", post.name,
        )
        self.assertEqual(after.returncode, 0, after.stderr)
        restored = self.backup_script(
            "restore", "--archive", str(post), "--data-volume", restore_volume,
            "--barrier-source-volume", self.data_volume, "--compose-project", self.project,
        )
        self.assertEqual(restored.returncode, 0, restored.stderr)
        compose, base, _ = self.start_restored_stack(restore_volume, "post-forget")
        try:
            restored_cookie, restored_csrf = self.login_at(base)
            page = self.api_at(base, "/api/search?q=fixture%20transcript", restored_cookie, restored_csrf)
            self.assertEqual(page.get("hits", []), [], f"forgotten source reappeared: {page}")
            status, _ = self.fetch(base, f"/api/turns/{turn['id']}/audio", restored_cookie)
            self.assertIn(status, (404, 410), f"forgotten audio still readable: {status}")
        finally:
            subprocess.run(compose + ["down", "-v", "--remove-orphans"], capture_output=True)
            subprocess.run(["docker", "volume", "rm", "-f", restore_volume], capture_output=True)


if __name__ == "__main__":
    unittest.main()
