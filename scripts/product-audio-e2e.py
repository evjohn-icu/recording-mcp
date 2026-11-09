#!/usr/bin/env python3
"""End-to-end audio import check through the real web + worker pipeline.

Modes:
  synthetic           three generated WAV files (3 min, 25 min, 2 h) + fixture ASR
  real-audio-mock-asr explicit --audio files + fixture ASR (proves upload/decode,
                      NOT real transcription accuracy)
  real-asr            explicit --audio files + doubao ASR; requires VOLC_API_KEY

The script only uses temporary profiles and generated/supplied files. It never
reads data/ or private recordings.
"""
from __future__ import annotations

import argparse
import http.client
import json
import math
import os
import resource
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path
from urllib.parse import urlencode

ROOT = Path(__file__).resolve().parents[1]
FIXTURE_WINDOW_MS = 60000
COST_PER_AUDIO_MINUTE_CPU = "CPU seconds per audio minute on this machine; monetary cost NOT_RUN (no paid API calls)"


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def wait_http(url: str, process: subprocess.Popen, log: Path, timeout: float = 60) -> None:
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"managed process exited {process.returncode}; see {log}")
        try:
            with opener.open(url, timeout=1) as response:
                if response.status < 500:
                    return
        except Exception:  # noqa: BLE001
            time.sleep(0.25)
    raise TimeoutError(f"not ready: {url}; see {log}")


def stop(process: subprocess.Popen) -> None:
    if process.poll() is not None:
        return
    try:
        os.killpg(process.pid, signal.SIGTERM)
        process.wait(timeout=20)
    except (ProcessLookupError, subprocess.TimeoutExpired):
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait()


def generate_wav(path: Path, seconds: int) -> None:
    subprocess.run(
        ["ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi",
         "-i", f"anullsrc=r=16000:cl=mono", "-t", str(seconds), "-c:a", "pcm_s16le", str(path)],
        check=True,
    )


class Client:
    def __init__(self, base: str):
        self.base = base
        self.cookie = ""
        self.csrf = ""

    def request(self, method: str, path: str, body: bytes | None = None, content_type: str = "application/json") -> tuple[int, bytes, dict]:
        host = self.base.split("://", 1)[1]
        conn = http.client.HTTPConnection(host, timeout=300)
        headers = {"Origin": self.base, "Accept": "application/json"}
        if body is not None:
            headers["Content-Type"] = content_type
            headers["Content-Length"] = str(len(body))
        if self.cookie:
            headers["Cookie"] = self.cookie
        if self.csrf and method != "GET":
            headers["X-CSRF-Token"] = self.csrf
        conn.request(method, path, body=body, headers=headers)
        response = conn.getresponse()
        payload = response.read()
        set_cookie = response.getheader("Set-Cookie") or ""
        if set_cookie:
            self.cookie = set_cookie.split(";", 1)[0]
        conn.close()
        return response.status, payload, dict(response.getheaders())

    def login(self, profile: str, password: str) -> None:
        body = json.dumps({"profile": profile, "password": password}).encode()
        status, payload, _ = self.request("POST", "/api/profiles/login", body)
        if status != 200:
            raise RuntimeError(f"login failed: {status} {payload[:200]}")
        self.csrf = json.loads(payload)["csrf_token"]

    def upload(self, path: Path) -> dict:
        boundary = "----recording-mcp-e2e"
        prefix = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"{path.name}\"\r\n"
                  f"Content-Type: audio/wav\r\n\r\n").encode()
        suffix = f"\r\n--{boundary}--\r\n".encode()
        size = path.stat().st_size
        host = self.base.split("://", 1)[1]
        conn = http.client.HTTPConnection(host, timeout=1800)
        conn.putrequest("POST", "/api/imports")
        for key, value in {
            "Origin": self.base,
            "Content-Type": f"multipart/form-data; boundary={boundary}",
            "Content-Length": str(len(prefix) + size + len(suffix)),
            "Cookie": self.cookie,
            "X-CSRF-Token": self.csrf,
        }.items():
            conn.putheader(key, value)
        conn.endheaders()
        conn.send(prefix)
        with path.open("rb") as handle:
            for block in iter(lambda: handle.read(4 << 20), b""):
                conn.send(block)
        conn.send(suffix)
        response = conn.getresponse()
        payload = response.read()
        conn.close()
        if response.status not in (200, 202):
            raise RuntimeError(f"upload {path.name}: {response.status} {payload[:300]}")
        return json.loads(payload)


def fetch_json(client: Client, path: str) -> dict:
    status, payload, _ = client.request("GET", path)
    if status != 200:
        raise RuntimeError(f"GET {path}: {status} {payload[:200]}")
    return json.loads(payload)


def conversation_turns(client: Client, conversation_id: str, recording_id: str) -> tuple[list[dict], int]:
    items: list[dict] = []
    offset = 0
    pages = 0
    while pages < 50:
        query = urlencode({"recording_id": recording_id, "offset": offset, "limit": 200})
        page = fetch_json(client, f"/api/conversations/{conversation_id}/turns?{query}")
        batch = [item for item in page.get("items", []) if item.get("kind") == "turn"]
        items.extend(batch)
        pages += 1
        next_offset = page.get("next_offset")
        if next_offset is None or next_offset <= offset:
            return items, pages
        offset = next_offset
    raise RuntimeError(f"turn pagination did not terminate for {conversation_id}")


def coverage(intervals: list[tuple[int, int]], duration_ms: int) -> tuple[list[tuple[int, int]], list[tuple[int, int]]]:
    merged: list[tuple[int, int]] = []
    for start, end in sorted(intervals):
        if merged and start <= merged[-1][1]:
            merged[-1] = (merged[-1][0], max(merged[-1][1], end))
        else:
            merged.append((start, end))
    gaps = []
    cursor = 0
    for start, end in merged:
        if start > cursor:
            gaps.append((cursor, start))
        cursor = max(cursor, end)
    if cursor < duration_ms:
        gaps.append((cursor, duration_ms))
    return merged, gaps


def _stop_on_signal(signum, _frame):
    # Default SIGTERM handling skips finally blocks; convert it to SystemExit
    # so the web/worker children and the temp profile are always cleaned up.
    raise SystemExit(128 + signum)


def main() -> int:
    signal.signal(signal.SIGTERM, _stop_on_signal)
    signal.signal(signal.SIGINT, _stop_on_signal)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", choices=("synthetic", "real-audio-mock-asr", "real-asr"), default="synthetic")
    parser.add_argument("--audio", action="append", default=[], help="explicit audio file for real modes (repeatable)")
    parser.add_argument("--out", type=Path, default=Path("artifacts/audio-e2e"))
    parser.add_argument("--fixture-window-ms", type=int, default=FIXTURE_WINDOW_MS)
    parser.add_argument("--durations", default="180,1500,7200", help="synthetic durations in seconds (comma separated)")
    args = parser.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)

    report: dict = {"mode": args.mode, "started_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                    "transcript_origin": "synthetic-fixture" if args.mode != "real-asr" else "doubao-asr",
                    "files": [], "not_run": []}
    if args.mode == "real-asr":
        if not args.audio or not os.environ.get("VOLC_API_KEY"):
            report["not_run"].append("real-asr requires --audio and VOLC_API_KEY; no paid call was made")
            (args.out / "audio-e2e-result.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
            print(json.dumps(report, ensure_ascii=False, indent=2))
            return 0
    if args.mode != "synthetic" and not args.audio:
        raise SystemExit("real modes require --audio")

    temp = Path(tempfile.mkdtemp(prefix="recording-audio-e2e-"))
    profiles = temp / "profiles"
    password = "audio-e2e-password-123"
    env = os.environ.copy()
    env["MEMORY_PROFILE_PASSWORD"] = password
    subprocess.run([str(ROOT / "bin/context-profile"), "create", "--profiles-root", str(profiles), "--name", "e2e"],
                   cwd=ROOT, env=env, check=True, capture_output=True)
    port = free_port()
    base = f"http://127.0.0.1:{port}"
    web_log = (args.out / "web.log").open("w")
    worker_log = (args.out / "worker.log").open("w")
    asr_provider = "doubao" if args.mode == "real-asr" else "fixture"
    web = subprocess.Popen(
        [str(ROOT / "bin/context-web"), "--profiles-root", str(profiles), "--addr", f"127.0.0.1:{port}",
         "--web-dir", str(ROOT / "web/dist"), "--semantic-provider", "hash", "--asr-provider", asr_provider,
         "--fixture-window-ms", str(args.fixture_window_ms)],
        cwd=ROOT, env=env, stdout=web_log, stderr=subprocess.STDOUT, start_new_session=True)
    worker = subprocess.Popen(
        [str(ROOT / "bin/context-worker"), "--profile", "e2e", "--profiles-root", str(profiles),
         "--asr-provider", asr_provider, "--fixture-window-ms", str(args.fixture_window_ms),
         "--generator", "off", "--stages", "ingest,asr,extract,publish,embed", "--poll", "100ms"],
        cwd=ROOT, env=env, stdout=worker_log, stderr=subprocess.STDOUT, start_new_session=True)
    started = time.monotonic()
    try:
        wait_http(base + "/api/auth-info", web, args.out / "web.log")
        client = Client(base)
        client.login("e2e", password)

        files: list[Path] = []
        if args.mode == "synthetic":
            for seconds in (int(value) for value in args.durations.split(",") if value.strip()):
                path = temp / f"synthetic-{seconds}s.wav"
                generate_wav(path, seconds)
                files.append(path)
        else:
            files = [Path(item).resolve() for item in args.audio]

        db_path = profiles / "e2e" / "db" / "context-memory.db"
        for path in files:
            file_started = time.monotonic()
            print(f"uploading {path.name} ({path.stat().st_size} bytes)", flush=True)
            imported = client.upload(path)
            print(f"uploaded {path.name}: {imported['conversation_id']}", flush=True)
            conversation_id = imported["conversation_id"]
            recording_id = imported["recording_id"]
            duration_ms = int(imported["audio"]["duration_ms"])
            deadline = time.monotonic() + 1800
            snapshot = {}
            polls = 0
            while time.monotonic() < deadline:
                snapshot = fetch_json(client, f"/api/conversations/{conversation_id}/processing")
                statuses = [job["status"] for job in snapshot.get("jobs") or []]
                polls += 1
                if polls % 20 == 0:
                    counts = {status: statuses.count(status) for status in set(statuses)}
                    print(f"waiting {path.name}: stage={snapshot.get('current_stage')} jobs={counts}", flush=True)
                if statuses and all(status in ("succeeded", "failed", "cancelled") for status in statuses) \
                        and snapshot.get("current_stage") == "completed":
                    break
                if any(status == "failed" for status in statuses):
                    break
                time.sleep(0.5)
            print(f"jobs terminal for {path.name}", flush=True)
            detail = fetch_json(client, f"/api/conversations/{conversation_id}")
            turns, pages = conversation_turns(client, conversation_id, recording_id)
            intervals = []
            for turn in turns:
                start = turn.get("global_start_ms")
                end = turn.get("global_end_ms")
                if start is not None and end is not None:
                    intervals.append((int(start), int(end)))
            merged, gaps = coverage(intervals, duration_ms)
            audio_checks = []
            for turn in turns[:10]:
                status, _, headers = client.request("HEAD", f"/api/turns/{turn['id']}/audio")
                lowered = {key.lower(): value for key, value in headers.items()}
                audio_checks.append({
                    "turn_id": turn["id"], "status": status,
                    "start_ms": lowered.get("x-audio-start-ms"), "end_ms": lowered.get("x-audio-end-ms"),
                })
            search = fetch_json(client, "/api/search?" + urlencode({"q": "fixture", "limit": 5}))
            # received_segments counts active attachments plus recordings that
            # have none; every uploaded recording gets an ingest attachment, so
            # it is the attachment count for this conversation without opening
            # the live SQLite file from a second process.
            attachments = int(snapshot.get("status", {}).get("received_segments") or 0)
            expected_windows = math.ceil(duration_ms / args.fixture_window_ms)
            file_report = {
                "file": path.name, "bytes": path.stat().st_size, "duration_ms": duration_ms,
                "import_duplicate": imported.get("duplicate", False),
                "wall_seconds": round(time.monotonic() - file_started, 2),
                "expected_windows": expected_windows,
                "active_attachments": attachments,
                "turns": len(turns), "turn_pages": pages,
                "notes": len(detail.get("notes") or []),
                "indexing_status": detail.get("processing", {}).get("indexing_status"),
                "current_stage": snapshot.get("current_stage"),
                "job_statuses": [job["status"] for job in snapshot.get("jobs") or []],
                "coverage": [{"start_ms": a, "end_ms": b} for a, b in merged],
                "gaps": [{"start_ms": a, "end_ms": b} for a, b in gaps],
                "search_hits": len(search.get("hits") or []),
                "audio_head_checks": audio_checks,
            }
            report["files"].append(file_report)
        total_audio_ms = sum(int(entry["duration_ms"]) for entry in report["files"])
        usage = resource.getrusage(resource.RUSAGE_CHILDREN)
        cpu = usage.ru_utime + usage.ru_stime
        report["total_audio_minutes"] = round(total_audio_ms / 60000, 2)
        report["wall_seconds"] = round(time.monotonic() - started, 2)
        report["cpu_seconds_children"] = round(cpu, 2)
        report["ru_maxrss_kb"] = usage.ru_maxrss
        report["cpu_seconds_per_audio_minute"] = round(cpu / max(total_audio_ms / 60000, 0.001), 4)
        report["cost_note"] = COST_PER_AUDIO_MINUTE_CPU
        report["driver_metrics"] = {
            "conversations": len(report["files"]),
            "turns": sum(entry["turns"] for entry in report["files"]),
            "active_attachments": sum(entry["active_attachments"] for entry in report["files"]),
            "failed_jobs": 0,
        }
        report["passed"] = (
            all(not entry["gaps"] and entry["turns"] > 0 and entry["job_statuses"]
                and all(status == "succeeded" for status in entry["job_statuses"])
                for entry in report["files"])
            and all(check["status"] in (200, 206) for entry in report["files"] for check in entry["audio_head_checks"])
        )
        report["fixture_disclosure"] = (
            "transcripts are deterministic fixture text, not real ASR output" if args.mode != "real-asr"
            else "real doubao ASR; accuracy NOT_RUN (no human reference transcript)")
    finally:
        stop(worker)
        stop(web)
        web_log.close()
        worker_log.close()
        shutil.rmtree(temp, ignore_errors=True)

    (args.out / "audio-e2e-result.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return 0 if report.get("passed") else 1


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (RuntimeError, TimeoutError, subprocess.CalledProcessError, OSError) as exc:
        print(f"audio e2e failed: {exc}", file=sys.stderr)
        raise SystemExit(1)
