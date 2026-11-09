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
                 asr_device: str = "cpu", asr_model: str = "test-stub", asr_revision: str = "test"):
        self.temp = temp
        self.password = password
        self.profiles = temp / "profiles"
        self.port = free_port()
        self.base = f"http://127.0.0.1:{self.port}"
        self.asr_args = asr_args
        self.asr_device = asr_device
        self.asr_model = asr_model
        self.asr_revision = asr_revision
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
             "--asr-script", self.asr_args[2]],
            cwd=ROOT, env=self.env, stdout=self.web_log.open("a"), stderr=subprocess.STDOUT,
            start_new_session=True)
        wait_http(self.base + "/api/auth-info", self.web, self.web_log)

    def start_worker(self, profile: str) -> None:
        args = [str(ROOT / "bin/context-worker"), "--profile", profile, "--profiles-root", str(self.profiles),
                "--asr-provider", "local", "--asr-backend", self.asr_args[0],
                "--asr-python", self.asr_args[1], "--asr-script", self.asr_args[2],
                "--asr-device", self.asr_device,
                "--asr-window-ms", "30000", "--asr-overlap-ms", "2000",
                "--generator", "off", "--stages", "ingest,asr,extract,publish,embed",
                "--poll", "100ms", "--lease", "5s"]
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
    offset = 0
    for _ in range(200):
        page = client.fetch(f"/api/conversations/{conversation_id}/turns?recording_id={recording_id}&offset={offset}&limit=200")
        for item in page.get("items", []):
            if item.get("kind") != "turn":
                continue
            start, end = item.get("global_start_ms"), item.get("global_end_ms")
            if start is not None and end is not None:
                intervals.append((int(start), int(end)))
        nxt = page.get("next_offset")
        if nxt is None or nxt <= offset:
            break
        offset = nxt
    merged: list[tuple[int, int]] = []
    for start, end in sorted(intervals):
        if merged and start <= merged[-1][1]:
            merged[-1] = (merged[-1][0], max(merged[-1][1], end))
        else:
            merged.append((start, end))
    return sum(end - start for start, end in merged), len(intervals)


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
    args = parser.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    results: dict = {"started_at": now_iso(), "scenarios": [], "metrics": {}}
    temp = Path(tempfile.mkdtemp(prefix="long-audio-reliability-"))
    try:
        if args.mode in ("synthetic", "both"):
            results["scenarios"].append(synthetic_run(args, temp))
        if args.mode in ("real", "both"):
            results["scenarios"].extend(real_run(args, temp))
        if args.short_jobs:
            results["scenarios"].append(short_jobs_run(args, temp))
    finally:
        shutil.rmtree(temp, ignore_errors=True)
    results["scenarios"].extend(run_go_tests(args.out))
    results["metrics"] = {
        "passed": sum(1 for s in results["scenarios"] if s.get("status") == "PASS"),
        "failed": sum(1 for s in results["scenarios"] if s.get("status") == "FAIL"),
        "not_run": sum(1 for s in results["scenarios"] if s.get("status") == "NOT_RUN"),
        "local_only": True,
    }
    results["finished_at"] = now_iso()
    (args.out / "results.json").write_text(json.dumps(results, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps(results["metrics"], ensure_ascii=False))
    return 0 if results["metrics"]["failed"] == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
