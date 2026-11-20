#!/usr/bin/env python3
"""Long-audio reliability harness (synthetic fault injection + authorized real audio).

Modes:
  synthetic  2 h generated WAV through the test-only slow chunk runner; the
             worker is SIGKILLed twice mid-ASR and must resume from committed
             windows. Output is labelled synthetic/fault-injection.
  real       authorized fixtures only: T1 = 120 s segment, T2 = stitched
             36.95 min run. Runs local Qwen3-ASR (and ARK on T1). Aggregate
             metrics only; audio and transcripts never enter the repository.

T4-T8 always run as exact Go tests. --short-jobs N imports N short clips from
the authorized 120 s fixture into one running stack (SMART-01 acceptance).

Results land in artifacts/long-audio-reliability/results.json. Missing
fixtures/models are recorded as NOT_RUN with a reason, never substituted.
"""
from __future__ import annotations

import argparse
import http.client
import json
import os
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
T1_FIXTURE = ROOT / "data/full-transcription/segments/segment-01.pcm"
T2_RUN = ROOT / "data/asr/ph261004155454/run.json"
SLOW_RUNNER = ROOT / "scripts/testdata/slow-asr.py"
ASR_PYTHON = ROOT / ".venv-asr/bin/python"

GO_TESTS = [
    ("T4", "long-audio.window-resume", "./internal/ingest", "TestRunASRJobResumesCommittedWindows"),
    ("T5", "long-audio.coverage", "./internal/ingest", "TestRunASRJobCoverageAccounting"),
    ("T6", "long-audio.partial-failure", "./internal/ingest", "TestRunASRJobKeepsPartialStateOnWindowFailure"),
    ("T7", "long-audio.oom-actionable", "./internal/ingest", "TestRunASRJobMapsCUDAOutOfMemory"),
    ("T8", "long-audio.indexing-restart", "./internal/ingest", "TestEmbedJobResumesAfterTranscriptCommit"),
    ("T9", "long-audio.duplicate-import", "./internal/ingest", "TestImportDuplicateBytesReuseJob"),
    ("T10", "long-audio.progress-projection", "./internal/ingest", "TestProcessingStatusProjectsASRProgress"),
]


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def now_iso() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def wait_http(url: str, proc: subprocess.Popen, log: Path, timeout: float = 120) -> None:
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise RuntimeError(f"process exited {proc.returncode}; see {log}")
        try:
            with opener.open(url, timeout=1) as response:
                if response.status < 500:
                    return
        except Exception:  # noqa: BLE001
            time.sleep(0.25)
    raise TimeoutError(f"not ready: {url}; see {log}")


def stop(proc: subprocess.Popen | None) -> None:
    if proc is None or proc.poll() is not None:
        return
    try:
        os.killpg(proc.pid, signal.SIGTERM)
        proc.wait(timeout=20)
    except (ProcessLookupError, subprocess.TimeoutExpired):
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        proc.wait()


def kill_hard(proc: subprocess.Popen) -> None:
    try:
        os.killpg(proc.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    proc.wait(timeout=10)


class Client:
    def __init__(self, base: str):
        self.base = base
        self.cookie = ""
        self.csrf = ""

    def request(self, method: str, path: str, body: bytes | None = None, content_type: str = "application/json"):
        host = self.base.split("://", 1)[1]
        conn = http.client.HTTPConnection(host, timeout=3600)
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
        cookie = response.getheader("Set-Cookie") or ""
        if cookie:
            self.cookie = cookie.split(";", 1)[0]
        conn.close()
        return response.status, payload

    def login(self, profile: str, password: str) -> None:
        status, payload = self.request("POST", "/api/profiles/login",
                                       json.dumps({"profile": profile, "password": password}).encode())
        if status != 200:
            raise RuntimeError(f"login failed: {status} {payload[:200]}")
        self.csrf = json.loads(payload)["csrf_token"]

    def fetch(self, path: str) -> dict:
        status, payload = self.request("GET", path)
        if status != 200:
            raise RuntimeError(f"GET {path}: {status} {payload[:200]}")
        return json.loads(payload)

    def fetch_bytes(self, path: str) -> tuple[int, bytes]:
        return self.request("GET", path)

    def upload(self, path: Path) -> dict:
        boundary = "----recording-mcp-long"
        prefix = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"{path.name}\"\r\n"
                  f"Content-Type: audio/wav\r\n\r\n").encode()
        suffix = f"\r\n--{boundary}--\r\n".encode()
        host = self.base.split("://", 1)[1]
        conn = http.client.HTTPConnection(host, timeout=1800)
        conn.putrequest("POST", "/api/imports")
        for key, value in {
            "Origin": self.base,
            "Content-Type": f"multipart/form-data; boundary={boundary}",
            "Content-Length": str(len(prefix) + path.stat().st_size + len(suffix)),
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


class Stack:
    def __init__(self, temp: Path, password: str, asr_args: list[str], extra_env: dict[str, str] | None = None,
                 asr_device: str = "cpu", asr_model: str = "test-stub", asr_revision: str = "test",
                 extra_web_args: list[str] | None = None, extra_worker_args: list[str] | None = None,
                 worker_stages: str = ""):
        self.temp = temp
        self.password = password
        self.profiles = temp / "profiles"
        self.port = free_port()
        self.base = f"http://127.0.0.1:{self.port}"
        self.asr_args = asr_args
        self.asr_device = asr_device
        self.asr_model = asr_model
        self.asr_revision = asr_revision
        self.extra_web_args = list(extra_web_args or [])
        self.extra_worker_args = list(extra_worker_args or [])
        self.worker_stages = worker_stages or ",".join(BASE_WORKER_STAGES)
        self.env = os.environ.copy()
        self.env["MEMORY_PROFILE_PASSWORD"] = password
        self.env.update(extra_env or {})
        self.web: subprocess.Popen | None = None
        self.worker: subprocess.Popen | None = None
        self.worker_log = temp / "worker.log"
        self.web_log = temp / "web.log"
        self.restarts = 0

    def create_profile(self, name: str) -> None:
        subprocess.run([str(ROOT / "bin/context-profile"), "create", "--profiles-root", str(self.profiles),
                        "--name", name], cwd=ROOT, env=self.env, check=True, capture_output=True)

    def start_web(self) -> None:
        self.web = subprocess.Popen(
            [str(ROOT / "bin/context-web"), "--profiles-root", str(self.profiles),
             "--addr", f"127.0.0.1:{self.port}", "--web-dir", str(ROOT / "web/dist"),
             "--semantic-provider", "hash", "--asr-provider", "local",
             "--asr-backend", self.asr_args[0], "--asr-python", self.asr_args[1],
             "--asr-script", self.asr_args[2]] + self.extra_web_args,
            cwd=ROOT, env=self.env, stdout=self.web_log.open("a"), stderr=subprocess.STDOUT,
            start_new_session=True)
        wait_http(self.base + "/api/auth-info", self.web, self.web_log)

    def start_worker(self, profile: str) -> None:
        args = [str(ROOT / "bin/context-worker"), "--profile", profile, "--profiles-root", str(self.profiles),
                "--asr-provider", "local", "--asr-backend", self.asr_args[0],
                "--asr-python", self.asr_args[1], "--asr-script", self.asr_args[2],
                "--asr-device", self.asr_device,
                "--asr-window-ms", "30000", "--asr-overlap-ms", "2000",
                "--generator", "off", "--stages", self.worker_stages,
                "--poll", "100ms", "--lease", "5s"] + self.extra_worker_args
        if self.asr_model:
            args += ["--asr-model", self.asr_model]
        if self.asr_revision:
            args += ["--asr-revision", self.asr_revision]
        self.worker = subprocess.Popen(args, cwd=ROOT, env=self.env, stdout=self.worker_log.open("a"),
                                       stderr=subprocess.STDOUT, start_new_session=True)

    def start(self, profile: str) -> None:
        self.create_profile(profile)
        self.start_web()
        self.start_worker(profile)

    def restart_worker(self, profile: str) -> None:
        kill_hard(self.worker) if self.worker else None
        self.restarts += 1
        self.start_worker(profile)

    def close(self) -> None:
        stop(self.worker)
        stop(self.web)


def wait_for_completed(client: Client, conversation_id: str, predicate, timeout: float) -> dict:
    deadline = time.monotonic() + timeout
    snapshot: dict = {}
    while time.monotonic() < deadline:
        snapshot = client.fetch(f"/api/conversations/{conversation_id}/processing")
        status = snapshot.get("status") or {}
        if predicate(status, snapshot):
            return snapshot
        time.sleep(0.2)
    raise TimeoutError(f"condition not met; last status={snapshot.get('status')} stage={snapshot.get('current_stage')}")


def completed_chunks(snapshot: dict) -> int:
    return int((snapshot.get("status") or {}).get("asr_completed_chunks") or 0)


def frozenset_of_jobs(snapshot: dict) -> list[str]:
    return [job.get("status", "") for job in snapshot.get("jobs") or []]


def generate_wav(path: Path, seconds: int) -> None:
    subprocess.run(["ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "anullsrc=r=16000:cl=mono",
                    "-t", str(seconds), "-c:a", "pcm_s16le", str(path)], check=True)


def pcm_to_wav(pcm: Path, wav: Path) -> None:
    subprocess.run(["ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "s16le", "-ar", "16000", "-ac", "1",
                    "-i", str(pcm), "-c:a", "pcm_s16le", str(wav)], check=True)


def stitch_t2(run_json: Path, out: Path) -> tuple[int, int]:
    doc = json.loads(run_json.read_text())
    overlap = int(doc.get("overlap_seconds", 2)) * 1000
    segments = doc["segments"]
    total = 0
    with out.open("wb") as sink:
        for segment in segments:
            pcm = run_json.parent / "segments" / segment["pcm"]
            data = pcm.read_bytes()
            start = int(segment["start_ms"])
            end = int(segment["end_ms"])
            skip = overlap if total > 0 else 0
            sink.write(data[skip * 32:(end - start) * 32])
            total += (end - start) - skip
    return total, len(segments)


def audio_profile(path: Path) -> dict:
    """Measured level/silence of the uploaded WAV, so low-speech results are
    verifiable from the artifact instead of a hand-written claim."""
    vol = subprocess.run(["ffmpeg", "-nostdin", "-i", str(path), "-af", "volumedetect", "-f", "null", "-"],
                         capture_output=True, text=True)
    mean = max_db = None
    for line in (vol.stderr or "").splitlines():
        if "mean_volume:" in line:
            mean = float(line.split("mean_volume:")[1].split("dB")[0].strip())
        elif "max_volume:" in line:
            max_db = float(line.split("max_volume:")[1].split("dB")[0].strip())
    sil = subprocess.run(["ffmpeg", "-nostdin", "-i", str(path), "-af", "silencedetect=noise=-40dB:d=5", "-f", "null", "-"],
                         capture_output=True, text=True)
    spans = sum(1 for line in (sil.stderr or "").splitlines() if "silence_start:" in line)
    seconds = 0.0
    for line in (sil.stderr or "").splitlines():
        if "silence_duration:" in line:
            seconds += float(line.split("silence_duration:")[1].split("s")[0].strip())
    return {"audio_mean_db": mean, "audio_max_db": max_db,
            "silence_spans": spans, "silence_seconds": round(seconds, 2)}


def vram_sampler(stop_event: threading.Event, samples: list[int]) -> None:
    while not stop_event.is_set():
        try:
            out = subprocess.run(["nvidia-smi", "--query-gpu=memory.used", "--format=csv,noheader,nounits", "-i", "0"],
                                 capture_output=True, text=True, timeout=5).stdout.strip().splitlines()
            if out:
                samples.append(int(out[0]))
        except Exception:  # noqa: BLE001
            pass
        stop_event.wait(0.5)


def coverage_ms(client: Client, conversation_id: str, recording_id: str) -> tuple[int, int]:
    intervals: list[tuple[int, int]] = []
    for item in collect_current_turns(client, conversation_id, recording_id):
        start, end = item.get("global_start_ms"), item.get("global_end_ms")
        if start is not None and end is not None:
            intervals.append((int(start), int(end)))
    merged: list[tuple[int, int]] = []
    for start, end in sorted(intervals):
        if merged and start <= merged[-1][1]:
            merged[-1] = (merged[-1][0], max(merged[-1][1], end))
        else:
            merged.append((start, end))
    return sum(end - start for start, end in merged), len(intervals)


TURN_PAGE_LIMIT = 200
TURN_PAGE_MAX = 1000

# Worker stages the harness asks for explicitly; an explicit --stages list
# suppresses the worker's automatic audio-stage append, so an enabled audio
# capability must be named here or its queued job is never claimed.
BASE_WORKER_STAGES = ("ingest", "asr", "extract", "publish", "embed")
AUDIO_CAPABILITIES = ("vad", "tagging", "diarization", "speaker")


def enabled_audio_capabilities(config_path: str) -> list[str]:
    """Capabilities the audio-analysis config enables, in stable order."""
    if not str(config_path or "").strip():
        return []
    path = Path(config_path)
    if not path.is_file():
        raise ValueError(f"audio analysis config missing: {path}")
    payload = json.loads(path.read_text(encoding="utf-8"))
    capabilities = payload.get("capabilities") or {}
    enabled = []
    for capability in AUDIO_CAPABILITIES:
        entry = capabilities.get(capability)
        if isinstance(entry, dict) and entry.get("enabled") is True:
            enabled.append(capability)
    return enabled


def worker_stages(audio_config: str = "") -> str:
    """The explicit --stages value, including every enabled audio capability."""
    stages = list(BASE_WORKER_STAGES)
    for capability in enabled_audio_capabilities(audio_config):
        stage = f"audio_{capability}"
        if stage not in stages:
            stages.append(stage)
    return ",".join(stages)


def collect_current_turns(client: Client, conversation_id: str, recording_id: str) -> list[dict]:
    """Every current turn of one recording, read page by page.

    A repeated turn id or a page that does not advance is an error: a silently
    truncated view would look like a complete transcript to every metric.
    """
    turns: list[dict] = []
    seen: set[str] = set()
    offset = 0
    for _page in range(TURN_PAGE_MAX):
        page = client.fetch(f"/api/conversations/{conversation_id}/turns"
                            f"?recording_id={recording_id}&offset={offset}&limit={TURN_PAGE_LIMIT}")
        for item in page.get("items") or []:
            if item.get("kind") != "turn":
                continue
            turn_id = item.get("id")
            if not turn_id:
                raise RuntimeError("a turn page item has no id")
            if turn_id in seen:
                raise RuntimeError(f"duplicate turn id {turn_id}; refusing a truncated transcript")
            seen.add(turn_id)
            turns.append(item)
        nxt = page.get("next_offset")
        if nxt is None:
            return turns
        if not isinstance(nxt, int) or nxt <= offset:
            raise RuntimeError(f"turn pagination did not advance (offset={offset}, next={nxt!r})")
        offset = nxt
    raise RuntimeError(f"turn pagination exceeded {TURN_PAGE_MAX} pages")


def run_go_tests(out: Path) -> list[dict]:
    entries = []
    for scenario_id, spec_id, package, anchor in GO_TESTS:
        started = time.monotonic()
        proc = subprocess.run(["go", "test", package, "-run", f"^{anchor}$", "-count=1"],
                              cwd=ROOT, capture_output=True, text=True)
        entries.append({
            "id": scenario_id, "spec": spec_id, "kind": "go-test",
            "command": f"go test {package} -run ^{anchor}$ -count=1",
            "status": "PASS" if proc.returncode == 0 else "FAIL",
            "exit_code": proc.returncode,
            "wall_seconds": round(time.monotonic() - started, 2),
            "output_tail": (proc.stdout + proc.stderr).strip().splitlines()[-5:],
        })
    return entries


def synthetic_run(args, temp: Path) -> dict:
    wav = temp / "synthetic-2h.wav"
    generate_wav(wav, args.t3_seconds)
    duration_ms = args.t3_seconds * 1000
    stack = Stack(temp, args.password, ["ark", str(ASR_PYTHON), str(SLOW_RUNNER)],
                  {"SLOW_ASR_DELAY": "0.02"})
    entry = {"id": "T3", "spec": "long-audio.window-resume", "kind": "synthetic-fault-injection",
             "label": "synthetic", "audio_seconds": args.t3_seconds}
    try:
        stack.start("synthetic")
        client = Client(stack.base)
        client.login("synthetic", args.password)
        started = time.monotonic()
        imported = client.upload(wav)
        conversation_id = imported["conversation_id"]
        recording_id = imported["recording_id"]
        wait_for_completed(client, conversation_id, lambda s, _snap: completed_chunks(_snap) >= 3, 300)
        stack.restart_worker("synthetic")
        wait_for_completed(client, conversation_id, lambda s, _snap: completed_chunks(_snap) >= 8, 300)
        stack.restart_worker("synthetic")
        final = wait_for_completed(
            client, conversation_id,
            lambda s, snap: s.get("asr_state") == "done" or all(j in ("succeeded", "failed", "cancelled") for j in frozenset_of_jobs(snap)),
            1800)
        status = final.get("status") or {}
        wall = round(time.monotonic() - started, 2)
        expected = int(status.get("asr_expected_chunks") or 0)
        completed = int(status.get("asr_completed_chunks") or 0)
        covered, turns = coverage_ms(client, conversation_id, recording_id)
        status_coverage = int(status.get("asr_coverage_ms") or 0)
        entry.update({
            "status": "PASS" if completed == expected and expected > 0 and status.get("asr_state") == "done"
            and status_coverage >= duration_ms and turns <= expected else "FAIL",
            "wall_seconds": wall,
            "rtf": round(wall / args.t3_seconds, 4),
            "expected_chunks": expected,
            "completed_chunks": completed,
            "failed_chunks": int(status.get("asr_failed_chunks") or 0),
            "retried_attempts": stack.restarts,
            "coverage_ms": covered,
            "status_coverage_ms": status_coverage,
            "duration_ms": duration_ms,
            "turns": turns,
            "asr_state": status.get("asr_state"),
            "duplicate_turns": max(0, turns - expected),
            "local_only": True,
        })
    except Exception as exc:  # noqa: BLE001
        entry.update({"status": "FAIL", "error": str(exc)})
    finally:
        stack.close()
    return entry


def real_run(args, temp: Path) -> list[dict]:
    entries: list[dict] = []
    t1_fixture = Path(args.t1)
    if not t1_fixture.exists():
        return [{"id": "T1", "status": "NOT_RUN", "reason": f"authorized fixture missing: {t1_fixture}"},
                {"id": "T2", "status": "NOT_RUN", "reason": "T2 depends on T1 authorization"}]
    t1_wav = temp / "t1-120s.wav"
    pcm_to_wav(t1_fixture, t1_wav)
    if not ASR_PYTHON.exists():
        return [{"id": "T1", "status": "NOT_RUN", "reason": f"ASR interpreter missing: {ASR_PYTHON}"},
                {"id": "T2", "status": "NOT_RUN", "reason": "ASR interpreter missing"}]
    for backend, scenario_id, label in (("qwen", "T1", "real-qwen"), ("ark", "T1-ark", "real-ark")):
        entries.append(run_real_audio(temp, args, t1_wav, backend, scenario_id, label))
    if not Path(args.t2_run).exists():
        entries.append({"id": "T2", "status": "NOT_RUN", "reason": f"stitch metadata missing: {args.t2_run}"})
        return entries
    stitched = temp / "t2-36m.pcm"
    duration_ms, segments = stitch_t2(Path(args.t2_run), stitched)
    t2_wav = temp / "t2-36m.wav"
    pcm_to_wav(stitched, t2_wav)
    entries.append(run_real_audio(temp, args, t2_wav, "qwen", "T2", "real-qwen", expected_ms=duration_ms,
                                  note=f"stitched from {segments} authorized segments, 2 s overlap skipped"))
    return entries


def run_real_audio(temp: Path, args, wav: Path, backend: str, scenario_id: str, label: str,
                   expected_ms: int | None = None, note: str = "") -> dict:
    stack = Stack(temp, args.password, [backend, str(ASR_PYTHON), str(ROOT / "scripts/local-asr.py")],
                  asr_device="cuda:0", asr_model="", asr_revision="")
    entry = {"id": scenario_id, "spec": "long-audio.window-resume", "kind": label, "label": "real-authorized"}
    samples: list[int] = []
    stop_event = threading.Event()
    sampler = None
    profile = "real" + scenario_id.lower().replace("-", "")
    try:
        stack.start(profile)
        client = Client(stack.base)
        client.login(profile, args.password)
        started = time.monotonic()
        sampler = threading.Thread(target=vram_sampler, args=(stop_event, samples), daemon=True)
        sampler.start()
        imported = client.upload(wav)
        conversation_id = imported["conversation_id"]
        recording_id = imported["recording_id"]
        final = wait_for_completed(
            client, conversation_id,
            lambda s, snap: all(j in ("succeeded", "failed", "cancelled") for j in frozenset_of_jobs(snap)) and bool(snap.get("jobs")),
            args.real_timeout)
        wall = round(time.monotonic() - started, 2)
        status = final.get("status") or {}
        # Window coverage is the authority: empty windows create no turns, so a
        # turn-interval union undercounts a long recording with silence.
        turn_coverage, turns = coverage_ms(client, conversation_id, recording_id)
        covered = int(status.get("asr_coverage_ms") or 0) or turn_coverage
        duration = expected_ms if expected_ms is not None else int(imported["audio"]["duration_ms"])
        log = stack.worker_log.read_text(errors="replace") if stack.worker_log.exists() else ""
        load_ms = None
        for line in log.splitlines():
            if "model loaded" in line or "model_load" in line:
                load_ms = line
        entry.update(audio_profile(wav))
        entry.update({
            "status": "PASS" if status.get("asr_state") == "done" and covered >= duration else "FAIL",
            "note": note,
            "backend": backend,
            "audio_duration_ms": duration,
            "wall_seconds": wall,
            "rtf": round(wall / (duration / 1000), 4),
            "expected_chunks": int(status.get("asr_expected_chunks") or 0),
            "completed_chunks": int(status.get("asr_completed_chunks") or 0),
            "failed_chunks": int(status.get("asr_failed_chunks") or 0),
            "coverage_ms": covered,
            "turn_interval_coverage_ms": turn_coverage,
            "turns": turns,
            "peak_vram_mb": max(samples) if samples else None,
            "model_load_line": load_ms,
            "local_only": True,
            "job_statuses": frozenset_of_jobs(final),
        })
    except Exception as exc:  # noqa: BLE001
        entry.update({"status": "FAIL", "error": str(exc)})
    finally:
        stop_event.set()
        if sampler:
            sampler.join(timeout=2)
        stack.close()
    return entry


def short_jobs_run(args, temp: Path) -> dict:
    fixture = Path(args.short_fixture)
    if not fixture.exists():
        return {"id": "smart01.short-jobs", "status": "NOT_RUN", "reason": f"authorized fixture missing: {fixture}"}
    stack = Stack(temp, args.password, ["ark", str(ASR_PYTHON), str(SLOW_RUNNER)])
    entry = {"id": "smart01.short-jobs", "spec": "local-gpu.cold-start", "kind": "consecutive-jobs"}
    try:
        stack.start("short")
        client = Client(stack.base)
        client.login("short", args.password)
        conversations: list[str] = []
        statuses: list[list[str]] = []
        clips: list[Path] = []
        raw = fixture.read_bytes()
        for index in range(args.short_jobs):
            offset_ms = index * 20000
            chunk = temp / f"short-{index + 1}.pcm"
            chunk.write_bytes(raw[offset_ms * 32:(offset_ms + 5000) * 32])
            wav = temp / f"short-{index + 1}.wav"
            pcm_to_wav(chunk, wav)
            clips.append(wav)
            imported = client.upload(wav)
            if imported.get("duplicate"):
                raise RuntimeError(f"short job {index + 1} was deduplicated unexpectedly")
            conversations.append(imported["conversation_id"])
            final = wait_for_completed(
                client, imported["conversation_id"],
                lambda s, snap: all(j in ("succeeded", "failed", "cancelled") for j in frozenset_of_jobs(snap)) and bool(snap.get("jobs")),
                600)
            statuses.append(frozenset_of_jobs(final))
        entry.update({
            "status": "PASS" if len(set(conversations)) == args.short_jobs and all(
                all(j == "succeeded" for j in jobs) for jobs in statuses) else "FAIL",
            "jobs": args.short_jobs,
            "distinct_conversations": len(set(conversations)),
            "job_statuses": statuses,
            "local_only": True,
            "runner": "fault-injection stub (protocol only, not real ASR)",
        })
    except Exception as exc:  # noqa: BLE001
        entry.update({"status": "FAIL", "error": str(exc)})
    finally:
        stack.close()
    return entry


def v04_synthetic_run(args, temp: Path) -> dict:
    """Run the real application and worker with the fixture provider."""
    import audio_acceptance as acceptance  # noqa: F401 - imported for parity

    entry = {"id": "v04.synthetic", "spec": "context-engine-v04.protocol", "kind": "synthetic-closed-loop",
             "label": "synthetic", "audio_analysis_config": bool(args.audio_analysis_config)}
    profiles = temp / "v04-profiles"
    port = free_port()
    env = os.environ.copy()
    env["MEMORY_PROFILE_PASSWORD"] = args.password
    web_log = temp / "v04-web.log"
    worker_log = temp / "v04-worker.log"
    web = worker = None
    try:
        subprocess.run([str(ROOT / "bin/context-profile"), "create", "--profiles-root", str(profiles),
                        "--name", "v04"], cwd=ROOT, env=env, check=True, capture_output=True)
        web_args = [str(ROOT / "bin/context-web"), "--profiles-root", str(profiles),
                    "--addr", f"127.0.0.1:{port}", "--web-dir", str(ROOT / "web/dist"),
                    "--semantic-provider", "hash", "--asr-provider", "fixture"]
        worker_args = [str(ROOT / "bin/context-worker"), "--profile", "v04", "--profiles-root", str(profiles),
                       "--asr-provider", "fixture", "--generator", "off",
                       "--stages", "ingest,asr,extract,publish,embed", "--poll", "50ms", "--lease", "5s"]
        if args.audio_analysis_config:
            web_args += ["--audio-analysis-config", str(args.audio_analysis_config)]
            worker_args += ["--audio-analysis-config", str(args.audio_analysis_config)]
        web = subprocess.Popen(web_args, cwd=ROOT, env=env, stdout=web_log.open("a"),
                               stderr=subprocess.STDOUT, start_new_session=True)
        wait_http(f"http://127.0.0.1:{port}/api/auth-info", web, web_log)
        worker = subprocess.Popen(worker_args, cwd=ROOT, env=env, stdout=worker_log.open("a"),
                                  stderr=subprocess.STDOUT, start_new_session=True)
        client = Client(f"http://127.0.0.1:{port}")
        client.login("v04", args.password)
        wav = temp / "v04-synthetic.wav"
        generate_wav(wav, 3)
        started = time.monotonic()
        imported = client.upload(wav)
        conversation_id = imported["conversation_id"]
        final = wait_for_completed(
            client, conversation_id,
            lambda s, snap: bool(snap.get("jobs")) and all(j in ("succeeded", "failed", "cancelled") for j in frozenset_of_jobs(snap)),
            600)
        wall = round(time.monotonic() - started, 2)
        status = final.get("status") or {}
        expected = int(status.get("asr_expected_chunks") or 0)
        completed = int(status.get("asr_completed_chunks") or 0)
        failed = int(status.get("asr_failed_chunks") or 0)
        coverage = int(status.get("asr_coverage_ms") or 0)
        duration = int(imported["audio"]["duration_ms"])
        recording_id = imported["recording_id"]
        page = client.fetch(f"/api/conversations/{conversation_id}/turns?recording_id={recording_id}")
        turns = page.get("items") or []
        playable = 0
        for turn in turns:
            if turn.get("global_start_ms") is None or turn.get("global_end_ms") is None:
                continue
            status_code, body = client.fetch_bytes(f"/api/turns/{turn['id']}/audio")
            want = 44 + 32 * (turn["global_end_ms"] - turn["global_start_ms"])
            if status_code == 200 and len(body) == want:
                playable += 1
        passed = expected > 0 and completed == expected and failed == 0 and coverage >= duration and playable == len(turns) and len(turns) > 0
        entry.update({
            "status": "PASS" if passed else "FAIL",
            "wall_seconds": wall,
            "rtf": round(wall / (duration / 1000), 4),
            "expected_chunks": expected,
            "completed_chunks": completed,
            "failed_chunks": failed,
            "coverage_ms": coverage,
            "duration_ms": duration,
            "turns": len(turns),
            "playable_turns": playable,
            "job_statuses": frozenset_of_jobs(final),
            "note": "fixture ASR and synthetic WAV; protocol only, not an accuracy result",
        })
    except Exception as exc:  # noqa: BLE001
        entry.update({"status": "FAIL", "error": str(exc)})
    finally:
        stop(worker)
        stop(web)
    return entry


# --- v0.4 authorized real acceptance ---------------------------------------
#
# Everything below runs the real application, worker and MCP server against
# authorized audio with an explicitly trusted local ASR configuration. It never
# probes for recordings or model files: a missing input is reported as NOT_RUN
# or BLOCKED with a reason.

TRUSTED_ASR_BACKENDS = ("ark", "qwen")
GENERATOR_FIELDS = {"generator", "generator-url", "generator-model", "generator-temperature",
                    "generator-key-env", "generator-timeout"}


def _sha256(path: Path) -> str:
    import hashlib
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def _config_sha256(payload: dict) -> str:
    import hashlib
    canonical = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(canonical).hexdigest()


def trusted_asr_config(args) -> dict:
    """Validate the explicit local ASR identity; never probe or auto-detect."""
    problems = []
    if args.asr_backend not in TRUSTED_ASR_BACKENDS:
        problems.append(f"--asr-backend must be one of {list(TRUSTED_ASR_BACKENDS)}")
    if not args.asr_python or not Path(args.asr_python).is_file():
        problems.append("--asr-python must name an existing interpreter")
    if not str(args.asr_revision or "").strip():
        problems.append("--asr-revision is required to pin the actual model identity")
    if (args.asr_model or "") in ("test-stub", "fixture", "synthetic"):
        problems.append("--asr-model must name a real local model, not a test provider")
    if problems:
        raise ValueError("; ".join(problems))
    return {"backend": args.asr_backend, "python": str(Path(args.asr_python).resolve()),
            "model": args.asr_model or "", "revision": args.asr_revision,
            "device": args.asr_device or ""}


def generator_config(path: str) -> tuple[list[str], dict]:
    """Map the optional generator document onto the worker's own flags."""
    if not path:
        return [], {"generator": "off"}
    payload = json.loads(Path(path).read_text(encoding="utf-8"))
    unknown = set(payload) - GENERATOR_FIELDS
    if unknown:
        raise ValueError(f"generator config has unknown fields: {sorted(unknown)}")
    name = payload.get("generator", "off")
    if name not in ("off", "openai"):
        raise ValueError("generator must be off or openai; synthetic is not an acceptance generator")
    args: list[str] = []
    if name == "openai":
        url = str(payload.get("generator-url") or "")
        model = str(payload.get("generator-model") or "")
        if not url or not model:
            raise ValueError("the openai generator needs generator-url and generator-model")
        parsed = urllib.parse.urlparse(url)
        if parsed.scheme != "http" or parsed.hostname not in ("127.0.0.1", "localhost", "::1"):
            raise ValueError("generator-url must be a loopback URL; remote endpoints are not accepted")
        args += ["--generator", "openai", "--generator-url", url, "--generator-model", model]
        if "generator-temperature" in payload:
            args += ["--generator-temperature", str(payload["generator-temperature"])]
        if "generator-key-env" in payload:
            key_env = str(payload["generator-key-env"])
            if not key_env.isidentifier():
                raise ValueError("generator-key-env must be an environment variable name, not a key")
            args += ["--generator-key-env", key_env]
        if "generator-timeout" in payload:
            args += ["--generator-timeout", str(payload["generator-timeout"])]
    return args, dict(payload)


def _proc_stat(pid: int) -> tuple[int, int] | None:
    try:
        fields = Path(f"/proc/{pid}/stat").read_text().split()
    except OSError:
        return None
    try:
        return int(fields[13]) + int(fields[14]), int(fields[3])
    except (IndexError, ValueError):
        return None


def _rss_kb(pid: int) -> int:
    try:
        for line in Path(f"/proc/{pid}/status").read_text().splitlines():
            if line.startswith("VmRSS:"):
                return int(line.split()[1])
    except OSError:
        return 0
    return 0


def _process_tree(roots) -> list[int]:
    pids = []
    try:
        entries = [name for name in os.listdir("/proc") if name.isdigit()]
    except OSError:
        return [pid for pid in roots if pid]
    children: dict[int, list[int]] = {}
    for name in entries:
        stat = _proc_stat(int(name))
        if stat:
            children.setdefault(stat[1], []).append(int(name))
    stack = [pid for pid in roots if pid]
    while stack:
        pid = stack.pop()
        pids.append(pid)
        stack.extend(children.get(pid, []))
    return pids


class ResourceSampler:
    """CPU percent and RSS of the stack's own process tree, plus GPU totals."""

    def __init__(self, pids, interval: float = 0.5):
        self.pids = list(pids)
        self.interval = interval
        self.cpu_samples: list[float] = []
        self.rss_samples: list[int] = []
        self.vram_samples: list[int] = []
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None

    def __enter__(self):
        self._thread = threading.Thread(target=self._run, daemon=True)
        self._thread.start()
        return self

    def __exit__(self, *_exc):
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=5)

    def _run(self) -> None:
        previous: dict[int, int] = {}
        ticks = os.sysconf("SC_CLK_TCK")
        while not self._stop.is_set():
            tree = _process_tree(self.pids)
            total = 0
            rss = 0
            current = {}
            for pid in tree:
                stat = _proc_stat(pid)
                if not stat:
                    continue
                current[pid] = stat[0]
                rss += _rss_kb(pid)
            for pid, ticks_now in current.items():
                if pid in previous:
                    total += max(0, ticks_now - previous[pid])
            previous = current
            if tree:
                self.cpu_samples.append(round(total / ticks / self.interval * 100.0, 2))
                self.rss_samples.append(rss)
            try:
                out = subprocess.run(["nvidia-smi", "--query-gpu=memory.used", "--format=csv,noheader,nounits"],
                                     capture_output=True, text=True, timeout=5).stdout.strip().splitlines()
                if out:
                    self.vram_samples.append(int(out[0]))
            except Exception:  # noqa: BLE001
                pass
            self._stop.wait(self.interval)

    def summary(self) -> dict:
        return {"cpu_percent_peak": max(self.cpu_samples) if self.cpu_samples else None,
                "cpu_samples": len(self.cpu_samples),
                "rss_kb_peak": max(self.rss_samples) if self.rss_samples else None,
                "device_vram_mb_peak": max(self.vram_samples) if self.vram_samples else None,
                "model_vram_mb_peak": None,
                "model_vram_note": "per-model device memory is not separated by this stack; NOT_RUN"}


def _normalize_refs(pkg: dict, source: str) -> dict:
    refs = []
    for index, ref in enumerate(pkg.get("evidence") or []):
        if not isinstance(ref, dict):
            continue
        refs.append({
            "ref_id": f"{source}-{index}-{ref.get('turn_id')}",
            "turn_id": ref.get("turn_id"),
            "conversation_id": ref.get("conversation_id") or "",
            "recording_id": ref.get("recording_id") or "",
            "transcript_revision": ref.get("transcript_revision") or "",
            "start_ms": ref.get("global_start_ms"),
            "end_ms": ref.get("global_end_ms"),
            "valid": True,
            "current": True,
        })
    sufficiency = pkg.get("sufficiency") or {}
    return {"refs": refs, "sufficiency": sufficiency.get("status") or "unknown",
            "degraded": bool(pkg.get("degraded")),
            "gaps": [gap.get("kind") for gap in pkg.get("gaps") or [] if isinstance(gap, dict)]}


MCP_PROBE = r'''
import importlib.util, json, os, sys
spec = importlib.util.spec_from_file_location("sprint3_demo", sys.argv[1])
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
client = module.MCPClient([sys.argv[2], "--profile", sys.argv[3], "--profiles-root",
                           sys.argv[4], "--read-only"], os.environ.copy())
results = []
try:
    client.call(1, "initialize", {"protocolVersion": "2024-11-05", "capabilities": {},
                                 "clientInfo": {"name": "v04-acceptance", "version": "1"}})
    client.notify("notifications/initialized")
    for offset, query in enumerate(json.loads(sys.argv[5])):
        try:
            result = client.call(100 + offset, "tools/call",
                                 {"name": "build_context", "arguments": query["arguments"]})
            results.append({"query_id": query["query_id"], "result": result.get("structuredContent") or {}})
        except Exception as exc:
            results.append({"query_id": query["query_id"], "error": str(exc)[:300]})
finally:
    client.close()
print(json.dumps(results))
'''


DECISION_PROBE = r'''
import importlib.util, json, os, sys
spec = importlib.util.spec_from_file_location("sprint3_demo", sys.argv[1])
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
client = module.MCPClient([sys.argv[2], "--profile", sys.argv[3], "--profiles-root",
                           sys.argv[4], "--read-only"], os.environ.copy())
results = []
try:
    client.call(1, "initialize", {"protocolVersion": "2024-11-05", "capabilities": {},
                                 "clientInfo": {"name": "v04-acceptance-decisions", "version": "1"}})
    client.notify("notifications/initialized")
    for offset, item in enumerate(json.loads(sys.argv[5])):
        try:
            result = client.call(100 + offset, "tools/call",
                                 {"name": "get_decision_trail", "arguments": item})
            results.append({"topic_id": item.get("topic_id", ""),
                            "result": result.get("structuredContent") or {}})
        except Exception as exc:
            results.append({"topic_id": item.get("topic_id", ""), "error": str(exc)[:300]})
finally:
    client.close()
print(json.dumps(results))
'''

DECISION_LIMIT = 200
DECISION_MAX_BYTES = 262144
DECISION_STATES = ("decision_candidate", "confirmed")


def _mcp_query_outputs(args, profile: str, queries: list[dict], timeout: float,
                       profiles_root, env: dict[str, str]) -> list[dict]:
    """Run the MCP probe in its own process so a stuck readline cannot hang us.

    ``profiles_root`` and ``env`` are the stack's own profile root and process
    environment: the probe must open the same profile (and password) the Web
    path used, never a path derived from the output directory.
    """
    if not queries:
        return []
    payload = [{"query_id": query["id"], "arguments": {
        "query": query["query"], "retrieval_version": "v1", "max_items": 10, "max_bytes": 65536,
        "include_raw_evidence": True}}
        for query in queries]
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    try:
        proc = subprocess.run([sys.executable, "-c", MCP_PROBE, str(ROOT / "scripts/sprint3-demo.py"),
                               str(ROOT / "bin/context-memory"), profile, str(profiles_root),
                               json.dumps(payload)],
                              cwd=ROOT, env=env, capture_output=True, text=True, timeout=timeout,
                              start_new_session=True)
    except subprocess.TimeoutExpired:
        (out / f"mcp-{profile}.log").write_text(
            "MCP probe timed out; the probe process was terminated\n", encoding="utf-8")
        return [{"query_id": query["id"], "error": "MCP probe timed out"} for query in queries]
    if proc.returncode != 0:
        detail = (proc.stderr or "").strip()[-300:]
        return [{"query_id": query["id"], "error": f"MCP probe failed: {detail}"} for query in queries]
    try:
        return json.loads(proc.stdout.strip().splitlines()[-1])
    except (IndexError, json.JSONDecodeError):
        return [{"query_id": query["id"], "error": "MCP probe produced no JSON"} for query in queries]


def _mcp_decision_outputs(args, profile: str, topics: list[str], timeout: float,
                          profiles_root, env: dict[str, str]) -> list[dict]:
    """Read the decision trail of every topic through the MCP tool."""
    if not topics:
        return []
    payload = [{"topic_id": topic, "include_history": True,
                "limit": DECISION_LIMIT, "max_bytes": DECISION_MAX_BYTES} for topic in topics]
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    try:
        proc = subprocess.run([sys.executable, "-c", DECISION_PROBE, str(ROOT / "scripts/sprint3-demo.py"),
                               str(ROOT / "bin/context-memory"), profile, str(profiles_root),
                               json.dumps(payload)],
                              cwd=ROOT, env=env, capture_output=True, text=True, timeout=timeout,
                              start_new_session=True)
    except subprocess.TimeoutExpired:
        (out / f"mcp-decisions-{profile}.log").write_text(
            "MCP decision probe timed out; the probe process was terminated\n", encoding="utf-8")
        return [{"topic_id": topic, "error": "MCP decision probe timed out"} for topic in topics]
    if proc.returncode != 0:
        detail = (proc.stderr or "").strip()[-300:]
        return [{"topic_id": topic, "error": f"MCP decision probe failed: {detail}"}
                for topic in topics]
    try:
        return json.loads(proc.stdout.strip().splitlines()[-1])
    except (IndexError, json.JSONDecodeError):
        return [{"topic_id": topic, "error": "MCP decision probe produced no JSON"}
                for topic in topics]


def collect_audio_analysis(client: Client, conversation_id: str, recording_id: str) -> dict:
    """Read the audio-analysis overlay of the target recording.

    Only a current, active, succeeded run contributes events; an event's
    window clock is shifted by ``run_start_ms`` (falling back to the run's own
    ``start_ms``) so every span is in the recording clock. A disabled or
    failed capability keeps its status, which is not the same as a successful
    run that produced no events.
    """
    detail = client.fetch(f"/api/conversations/{conversation_id}")
    recording = None
    for candidate in detail.get("recordings") or []:
        if candidate.get("recording_id") == recording_id:
            recording = candidate
            break
    collected = {"capabilities": {}, "runs": [], "acoustic_events": [], "diarization": []}
    analysis = (recording or {}).get("audio_analysis")
    if not isinstance(analysis, dict):
        return collected
    for capability in analysis.get("capabilities") or []:
        if isinstance(capability, dict) and capability.get("capability"):
            collected["capabilities"][capability["capability"]] = capability.get("status") or "unknown"
    for run in analysis.get("runs") or []:
        if not isinstance(run, dict):
            continue
        if not (run.get("active") and run.get("current") and run.get("status") == "succeeded"):
            continue
        identity = {"run_id": run.get("run_id"), "capability": run.get("capability"),
                    "model_id": run.get("model_id"), "model_revision": run.get("model_revision"),
                    "generation": run.get("generation")}
        collected["runs"].append(identity)
        run_start = run.get("start_ms")
        for event in run.get("events") or []:
            if not isinstance(event, dict):
                continue
            start, end = event.get("start_ms"), event.get("end_ms")
            if not isinstance(start, int) or not isinstance(end, int) or end <= start:
                continue
            shift = event.get("run_start_ms")
            shift = shift if isinstance(shift, int) else (run_start if isinstance(run_start, int) else 0)
            payload = event.get("payload") or {}
            if event.get("event_type") == "acoustic_event":
                label = payload.get("label")
                if not isinstance(label, str) or not label:
                    continue
                collected["acoustic_events"].append(
                    {"start_ms": start + shift, "end_ms": end + shift, "label": label, **identity})
            elif event.get("event_type") == "speaker_segment":
                speaker = payload.get("speaker")
                if not isinstance(speaker, str) or not speaker:
                    continue
                collected["diarization"].append(
                    {"start_ms": start + shift, "end_ms": end + shift, "speaker": speaker, **identity})
    return collected


def decision_claims(sample_id: str, conversation_id: str, recording_id: str,
                    trails: list[dict]) -> tuple[list[dict], dict]:
    """Turn read-only decision-trail records into decision predictions.

    Only a candidate or a human-confirmed record becomes a prediction; a
    record whose evidence cannot be attributed to this sample is skipped
    instead of being scored against the wrong audio. A truncated or failed
    read is reported as incomplete so quality can never pass on a partial
    view.
    """
    claims: list[dict] = []
    seen: set[str] = set()
    diagnostics: dict = {"errors": [], "truncated": False, "topics": 0, "skipped": {}}

    def skip(reason: str, claim_id: str) -> None:
        diagnostics["skipped"].setdefault(reason, []).append(claim_id)

    for entry in trails:
        if entry.get("error"):
            diagnostics["errors"].append({"topic_id": entry.get("topic_id"),
                                          "error": str(entry["error"])[:300]})
            continue
        trail = entry.get("result") or {}
        if trail.get("truncated"):
            diagnostics["truncated"] = True
        for topic in trail.get("topics") or []:
            if not isinstance(topic, dict):
                continue
            diagnostics["topics"] += 1
            records = []
            if isinstance(topic.get("current"), dict):
                records.append(topic["current"])
            records.extend(record for record in topic.get("history") or [] if isinstance(record, dict))
            for record in records:
                if record.get("state") not in DECISION_STATES:
                    continue
                candidate = record.get("candidate_id") or record.get("id")
                if not isinstance(candidate, str) or not candidate:
                    skip("missing_id", str(record.get("id")))
                    continue
                claim_id = f"{sample_id}:{candidate}"
                if claim_id in seen:
                    continue
                evidence = [ref for ref in record.get("evidence") or [] if isinstance(ref, dict)]
                foreign = [ref for ref in evidence
                           if (ref.get("conversation_id") and ref["conversation_id"] != conversation_id)
                           or (ref.get("recording_id") and ref["recording_id"] != recording_id)]
                attributed = [ref for ref in evidence if ref.get("conversation_id") == conversation_id]
                if foreign:
                    skip("foreign_evidence", claim_id)
                    continue
                if not attributed:
                    skip("unattributed_evidence", claim_id)
                    continue
                text = str(record.get("text") or record.get("summary") or "").strip()
                if not text:
                    skip("empty_text", claim_id)
                    continue
                seen.add(claim_id)
                claims.append({
                    "claim_id": claim_id,
                    "kind": "decision",
                    "sample_id": sample_id,
                    "text": text,
                    "status": record.get("state"),
                    "evidence": [{"turn_id": ref.get("turn_id") or "",
                                  "conversation_id": ref.get("conversation_id") or "",
                                  "recording_id": ref.get("recording_id") or "",
                                  "revision": ref.get("revision") or "",
                                  "text": ref.get("text") or "",
                                  "source_status": ref.get("source_status") or ""}
                                 for ref in attributed],
                })
    diagnostics["complete"] = not diagnostics["truncated"] and not diagnostics["errors"]
    return claims, diagnostics


def _web_query_outputs(client: Client, queries: list[dict]) -> list[dict]:
    outputs = []
    for query in queries:
        path = ("/api/search/hybrid?q=" + urllib.parse.quote(query["query"]) +
                "&retrieval_version=v1&max_items=10&max_bytes=65536&include_raw_evidence=true")
        try:
            pkg = client.fetch(path)
            outputs.append({"query_id": query["id"], **_normalize_refs(pkg, "web")})
        except Exception as exc:  # noqa: BLE001
            outputs.append({"query_id": query["id"], "error": str(exc)[:300]})
    return outputs


def _turn_audio_checks(client: Client, turns: list[dict]) -> dict:
    """Every current turn must expose a WAV whose length matches its span."""
    statuses = {}
    playable = 0
    duplicates = 0
    seen: set[str] = set()
    for turn in turns:
        turn_id = turn.get("id")
        if turn_id in seen:
            duplicates += 1
            continue
        seen.add(turn_id)
        start, end = turn.get("global_start_ms"), turn.get("global_end_ms")
        if start is None or end is None:
            statuses[turn_id] = "missing_span"
            continue
        code, body = client.fetch_bytes(f"/api/turns/{turn_id}/audio")
        if code != 200:
            statuses[turn_id] = f"http_{code}"
            continue
        expected = 44 + 32 * (int(end) - int(start))
        if len(body) != expected:
            statuses[turn_id] = f"length_{len(body)}_expected_{expected}"
            continue
        playable += 1
    return {"turns": len(seen), "playable": playable, "duplicates": duplicates,
            "problems": {key: value for key, value in statuses.items()}}


def v04_real_run(args, temp: Path) -> list[dict]:
    """Execute the authorized acceptance samples through the real application."""
    import audio_acceptance as acceptance

    if not args.acceptance_manifest:
        return acceptance.no_manifest_entries()
    manifest_path = Path(args.acceptance_manifest)
    try:
        manifest = acceptance.load_manifest(manifest_path)
    except (OSError, json.JSONDecodeError, acceptance.AcceptanceError) as exc:
        return [{"id": "v04.real.manifest", "status": "FAIL", "reason": str(exc)}]
    try:
        asr = trusted_asr_config(args)
        generator_args, generator_document = generator_config(args.generator_config)
    except (ValueError, OSError, json.JSONDecodeError) as exc:
        return [{"id": "v04.real.config", "status": "BLOCKED", "reason": str(exc)}]
    known_ids = {sample["id"] for sample in manifest["samples"]}
    audio_config = str(args.audio_analysis_config or "")
    if audio_config and not Path(audio_config).is_file():
        return [{"id": "v04.real.config", "status": "BLOCKED",
                 "reason": f"audio analysis config missing: {audio_config}"}]
    extra_web: list[str] = []
    extra_worker: list[str] = list(generator_args)
    for extra in (extra_web, extra_worker):
        if asr["device"]:
            extra += ["--asr-device", asr["device"]]
        if asr["model"]:
            extra += ["--asr-model", asr["model"]]
        if asr["revision"]:
            extra += ["--asr-revision", asr["revision"]]
        if audio_config:
            extra += ["--audio-analysis-config", audio_config]
    config = {"asr": asr, "generator": generator_document, "audio_analysis_config": audio_config,
              "audio_analysis_config_sha256": _sha256(Path(audio_config)) if audio_config else ""}
    entries = []
    bundles = []
    for index, sample in enumerate(manifest["samples"]):
        entries.append(_run_v04_sample(args, temp, sample, known_ids, asr, extra_web, extra_worker,
                                       config, index, bundles))
    covered = {sample["category"] for sample in manifest["samples"]}
    for category in acceptance.CATEGORIES:
        if category not in covered:
            entries.append({"id": f"v04.real.{category}", "category": category, "status": "NOT_RUN",
                            "reason": "no authorized sample for this category"})
    if bundles:
        combined = _combine_bundles(bundles, manifest)
        out = Path(args.out)
        out.mkdir(parents=True, exist_ok=True)
        os.chmod(out, 0o700)
        target = out / "predictions.json"
        handle = os.open(target, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        with os.fdopen(handle, "w", encoding="utf-8") as sink:
            json.dump(combined, sink, ensure_ascii=False, indent=2)
            sink.write("\n")
    return entries


def _bundle_hash_for(bundle: dict, key: str, sample_id: str):
    value = bundle.get(key)
    if isinstance(value, dict):
        return value.get(sample_id)
    return value


def _combine_bundles(bundles: list[dict], manifest: dict) -> dict:
    """Merge per-sample bundles, refusing an inconsistent or duplicated set."""
    if not bundles:
        raise RuntimeError("no prediction bundle to combine")
    authorized = {sample["id"]: sample for sample in manifest["samples"]}
    identity: dict[str, str] = {}
    sample_ids: list[str] = []
    query_ids: set[str] = set()
    claim_ids: set[str] = set()
    for bundle in bundles:
        current = {
            "config_sha256": str(bundle.get("config_sha256")),
            "binary_sha256": str(bundle.get("binary_sha256")),
            "model": json.dumps(bundle.get("model") or {}, sort_keys=True, separators=(",", ":")),
        }
        if not identity:
            identity = current
        else:
            for label in ("config_sha256", "binary_sha256", "model"):
                if current[label] != identity[label]:
                    raise RuntimeError(f"prediction bundles disagree on {label}; refusing to merge")
        for entry in bundle.get("samples") or []:
            sample_id = entry.get("sample_id")
            if sample_id not in authorized:
                raise RuntimeError(f"prediction sample {sample_id!r} is not authorized by the manifest")
            if sample_id in sample_ids:
                raise RuntimeError(f"duplicate sample_id {sample_id} in the prediction bundles")
            sample_ids.append(sample_id)
            if _bundle_hash_for(bundle, "audio_sha256", sample_id) != authorized[sample_id]["audio_sha256"]:
                raise RuntimeError(f"audio_sha256 for {sample_id} does not match the frozen manifest")
            if _bundle_hash_for(bundle, "gold_sha256", sample_id) != authorized[sample_id]["gold_sha256"]:
                raise RuntimeError(f"gold_sha256 for {sample_id} does not match the frozen manifest")
        for output in bundle.get("query_outputs") or []:
            query_id = output.get("query_id")
            if query_id in query_ids:
                raise RuntimeError(f"duplicate query_id {query_id!r} in the prediction bundles")
            query_ids.add(query_id)
        for claim in bundle.get("claims") or []:
            claim_id = claim.get("claim_id")
            if claim_id in claim_ids:
                raise RuntimeError(f"duplicate claim_id {claim_id!r} in the prediction bundles")
            claim_ids.add(claim_id)
    if len(bundles) == 1:
        return bundles[0]
    return {
        "schema": "context-engine-predictions-v1",
        "run_id": bundles[0]["run_id"],
        "audio_sha256": {sample["id"]: sample["audio_sha256"] for sample in manifest["samples"]},
        "gold_sha256": {sample["id"]: sample["gold_sha256"] for sample in manifest["samples"]},
        "config_sha256": bundles[0]["config_sha256"],
        "binary_sha256": bundles[0]["binary_sha256"],
        "model": bundles[0]["model"],
        "samples": [entry for item in bundles for entry in item["samples"]],
        "query_outputs": [output for item in bundles for output in item["query_outputs"]],
        "claims": [claim for item in bundles for claim in item["claims"]],
        "stages": {"complete": all(item["stages"]["complete"] for item in bundles)},
        "resources": {"per_sample": [item["resources"] for item in bundles]},
    }


def _run_v04_sample(args, temp, sample, known_ids, asr, extra_web, extra_worker, config,
                    index, bundles: list) -> dict:
    import audio_acceptance as acceptance

    entry = {"id": sample["id"], "spec": "context-engine-v04.real", "category": sample["category"],
             "kind": "authorized-real", "label": "real-authorized"}
    problems = acceptance.verify_sample_files(sample)
    if problems:
        entry.update({"status": "FAIL" if any("hash" in item for item in problems) else "NOT_RUN",
                      "reason": "; ".join(problems)})
        return entry
    try:
        gold = acceptance.load_gold(Path(sample["_gold_path"]), sample["id"], known_ids)
    except acceptance.AcceptanceError as exc:
        entry.update({"status": "FAIL", "reason": f"{exc.code}: {exc.detail}"})
        return entry
    wav = Path(sample["_audio_path"])
    sample_root = temp / f"accept-{index}"
    profile = f"acc{index}"
    stack = None
    try:
        stack = Stack(sample_root, args.password,
                      [asr["backend"], asr["python"], str(ROOT / "scripts/local-asr.py")],
                      asr_device=asr["device"], asr_model=asr["model"], asr_revision=asr["revision"],
                      extra_web_args=extra_web, extra_worker_args=extra_worker,
                      worker_stages=worker_stages(config.get("audio_analysis_config") or ""))
        stack.start(profile)
        client = Client(stack.base)
        client.login(profile, args.password)
        pids = [proc.pid for proc in (stack.web, stack.worker) if proc]
        with ResourceSampler(pids) as sampler:
            started = time.monotonic()
            imported = client.upload(wav)
            conversation_id = imported["conversation_id"]
            recording_id = imported["recording_id"]
            final = wait_for_completed(
                client, conversation_id,
                lambda _status, snap: bool(snap.get("jobs")) and all(
                    job.get("status") in ("succeeded", "failed", "cancelled") for job in snap["jobs"]),
                args.sample_timeout)
            wall = round(time.monotonic() - started, 3)
            resources = sampler.summary()
        status = final.get("status") or {}
        jobs = {(job.get("kind") or job.get("id")): job.get("status") for job in final.get("jobs") or []}
        turns = collect_current_turns(client, conversation_id, recording_id)
        audio_checks = _turn_audio_checks(client, turns)
        audio = collect_audio_analysis(client, conversation_id, recording_id)
        coverage = int(status.get("asr_coverage_ms") or 0)
        duration = int(imported["audio"]["duration_ms"])
        expected = int(status.get("asr_expected_chunks") or 0)
        completed = int(status.get("asr_completed_chunks") or 0)
        failed = int(status.get("asr_failed_chunks") or 0)
        queries = gold.get("queries") or []
        web_outputs = _web_query_outputs(client, queries)
        mcp_by_id = {output.get("query_id"): output for output in _mcp_query_outputs(
            args, profile, queries, args.sample_timeout, stack.profiles, stack.env)}
        query_outputs = []
        for output in web_outputs:
            mcp = mcp_by_id.get(output["query_id"], {"error": "no MCP output"})
            query_outputs.append({
                "query_id": output["query_id"],
                "ambient_ids": [],
                "forgotten_ids": [],
                "web": output,
                "mcp": ({"refs": [], "error": mcp["error"]} if mcp.get("error")
                        else _normalize_refs(mcp.get("result") or {}, "mcp")),
            })
        topic_list = client.fetch("/api/topics?conversation_id=" + urllib.parse.quote(conversation_id))
        topic_ids = [topic.get("id") for topic in topic_list.get("topics") or [] if topic.get("id")]
        trails = _mcp_decision_outputs(args, profile, topic_ids, args.sample_timeout,
                                       stack.profiles, stack.env)
        claims, decision_diagnostics = decision_claims(sample["id"], conversation_id, recording_id, trails)
        revision = str(status.get("transcript_revision") or "")
        predicted_sample = {
            "sample_id": sample["id"],
            "conversation_id": conversation_id,
            "recording_id": recording_id,
            "transcript_revision": revision,
            "turns": [{"turn_id": turn.get("id"),
                       "speaker": turn.get("speaker_label") or turn.get("speaker") or "unknown",
                       "start_ms": turn.get("global_start_ms"), "end_ms": turn.get("global_end_ms"),
                       "text": turn.get("text") or ""} for turn in turns if turn.get("id")],
            "acoustic_events": audio["acoustic_events"],
            "diarization": audio["diarization"],
            "audio_capabilities": audio["capabilities"],
            "audio_runs": audio["runs"],
        }
        stages_complete = (expected > 0 and completed == expected and failed == 0 and coverage >= duration
                           and bool(jobs) and all(value == "succeeded" for value in jobs.values())
                           and decision_diagnostics["complete"])
        entry.update({
            "status": "PASS" if stages_complete and not audio_checks["problems"] else "FAIL",
            "wall_seconds": wall,
            "rtf": round(wall / (duration / 1000), 4) if duration else None,
            "audio_duration_ms": duration,
            "expected_chunks": expected,
            "completed_chunks": completed,
            "failed_chunks": failed,
            "coverage_ms": coverage,
            "jobs": jobs,
            "capabilities": {key: value for key, value in jobs.items() if key not in ("ingest", "asr")},
            "audio_capabilities": audio["capabilities"],
            "turns": audio_checks["turns"],
            "turn_audio": {"playable": audio_checks["playable"],
                           "duplicate_turns": audio_checks["duplicates"],
                           "problems": audio_checks["problems"]},
            "acoustic_events": len(audio["acoustic_events"]),
            "diarization_events": len(audio["diarization"]),
            "queries": len(queries),
            "decision_claims": len(claims),
            "decision_collection": decision_diagnostics,
            "resources": resources,
            "asr": asr,
            "generator": config["generator"].get("generator", "off"),
            "gold_claims": len(gold.get("claims") or []),
            "quality": "NOT_RUN until scripts/review-audio-acceptance.py score runs on the frozen bundle",
            "note": "execution success only; a zero exit status is not a quality result",
        })
        if not decision_diagnostics["complete"]:
            entry["decision_incomplete"] = True
        if gold.get("transcript") is not None and not turns:
            entry["zero_transcription"] = True
            entry["status"] = "PASS" if stages_complete else "FAIL"
        bundles.append({
            "schema": "context-engine-predictions-v1",
            "run_id": f"{now_iso()}-{sample['id']}",
            "audio_sha256": sample["audio_sha256"],
            "gold_sha256": sample["gold_sha256"],
            "config_sha256": _config_sha256(config),
            "binary_sha256": _sha256(ROOT / "bin/context-worker"),
            "model": {"provider": f"local-{asr['backend']}", "revision": asr["revision"],
                      "model": asr["model"], "device": asr["device"],
                      "audio_analysis_config_sha256": config["audio_analysis_config_sha256"]},
            "samples": [predicted_sample],
            "query_outputs": query_outputs,
            "claims": claims,
            "stages": {"complete": stages_complete, "jobs": jobs},
            "resources": resources,
        })
    except Exception as exc:  # noqa: BLE001
        entry.update({"status": "FAIL", "error": str(exc)})
    finally:
        if stack is not None:
            stack.close()
    return entry


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", choices=("synthetic", "real", "both"), default="synthetic")
    parser.add_argument("--out", type=Path, default=ROOT / "artifacts/long-audio-reliability")
    parser.add_argument("--password", default="long-audio-password-123")
    parser.add_argument("--t1", type=Path, default=T1_FIXTURE)
    parser.add_argument("--t2-run", type=Path, default=T2_RUN)
    parser.add_argument("--short-jobs", type=int, default=0)
    parser.add_argument("--short-fixture", type=Path, default=T1_FIXTURE)
    parser.add_argument("--t3-seconds", type=int, default=7200)
    parser.add_argument("--real-timeout", type=float, default=7200)
    parser.add_argument("--protocol", choices=("legacy", "context-engine-v04"), default="legacy")
    parser.add_argument("--acceptance-manifest", default="")
    parser.add_argument("--audio-analysis-config", default="")
    parser.add_argument("--asr-backend", choices=("ark", "qwen"), default="")
    parser.add_argument("--asr-python", default="")
    parser.add_argument("--asr-model", default="")
    parser.add_argument("--asr-revision", default="")
    parser.add_argument("--asr-device", default="")
    parser.add_argument("--generator-config", default="")
    parser.add_argument("--sample-timeout", type=float, default=1800.0)
    args = parser.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    results: dict = {"started_at": now_iso(), "scenarios": [], "metrics": {}}
    temp = Path(tempfile.mkdtemp(prefix="long-audio-reliability-"))
    try:
        if args.protocol == "context-engine-v04":
            results["protocol"] = "context-engine-v04"
            if args.mode in ("synthetic", "both"):
                results["scenarios"].append(v04_synthetic_run(args, temp))
            if args.mode in ("real", "both"):
                results["scenarios"].extend(v04_real_run(args, temp))
        else:
            if args.mode in ("synthetic", "both"):
                results["scenarios"].append(synthetic_run(args, temp))
            if args.mode in ("real", "both"):
                results["scenarios"].extend(real_run(args, temp))
            if args.short_jobs:
                results["scenarios"].append(short_jobs_run(args, temp))
    finally:
        shutil.rmtree(temp, ignore_errors=True)
    if not (args.protocol == "context-engine-v04" and args.mode in ("real", "both")):
        # The Go integration tests are exercised separately during verification;
        # a real acceptance run must not imply they ran here.
        results["scenarios"].extend(run_go_tests(args.out))
    results["metrics"] = {
        "passed": sum(1 for s in results["scenarios"] if s.get("status") == "PASS"),
        "failed": sum(1 for s in results["scenarios"] if s.get("status") == "FAIL"),
        "not_run": sum(1 for s in results["scenarios"] if s.get("status") == "NOT_RUN"),
        "blocked": sum(1 for s in results["scenarios"] if s.get("status") == "BLOCKED"),
        "local_only": True,
    }
    results["finished_at"] = now_iso()
    (args.out / "results.json").write_text(json.dumps(results, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps(results["metrics"], ensure_ascii=False))
    return 0 if results["metrics"]["failed"] == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
