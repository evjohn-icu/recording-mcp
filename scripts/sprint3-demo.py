#!/usr/bin/env python3
"""Sprint 3 twelve-step product demonstration on a disposable profile.

1 start via scripts/local-gpu-up.sh   2 capabilities/GPU readiness
3 import the authorized 120 s fixture 4 poll every processing stage
5 transcript + turn evidence          6 lexical search on transcript text
7 synthetic cross-session memory      8 sufficiency status + evidence refs
9 Decision Trail                      10 restart the stack
11 MCP build_context/get_decision_trail 12 persisted state + source integrity

Writes artifacts/sprint3-demo/result.json. Never copies audio or transcripts
into the repository; the profile password is redacted from the report.
"""
from __future__ import annotations

import argparse
import http.client
import json
import os
import secrets
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path
from urllib.parse import quote, urlencode

ROOT = Path(__file__).resolve().parents[1]
PASSWORD_ENV = "MEMORY_PROFILE_PASSWORD"
FIXTURE = ROOT / "data/full-transcription/segments/segment-01.pcm"
DEMO_CORPUS = ROOT / "fixtures/productization/beta-demo/corpus.json"
DEMO_PROJECTION = ROOT / "fixtures/productization/beta-demo/projection.json"


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def stop(proc: subprocess.Popen | None) -> None:
    if proc is None or proc.poll() is not None:
        return
    try:
        os.killpg(proc.pid, signal.SIGTERM)
        proc.wait(timeout=45)
    except (ProcessLookupError, subprocess.TimeoutExpired):
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        proc.wait()


class Client:
    def __init__(self, base: str):
        self.base = base
        self.cookie = ""
        self.csrf = ""

    def request(self, method: str, path: str, body: bytes | None = None,
                content_type: str = "application/json") -> tuple[int, bytes]:
        host = self.base.split("://", 1)[1]
        conn = http.client.HTTPConnection(host, timeout=1800)
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

    def json(self, method: str, path: str, body: dict | None = None) -> dict:
        status, payload = self.request(method, path, json.dumps(body).encode() if body is not None else None)
        if status >= 400:
            raise RuntimeError(f"{method} {path}: {status} {payload[:300]}")
        return json.loads(payload) if payload else {}

    def login(self, password: str) -> None:
        data = self.json("POST", "/api/login", {"password": password})
        self.csrf = data["csrf_token"]

    def upload(self, path: Path) -> dict:
        boundary = "----sprint3-demo"
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
            raise RuntimeError(f"upload: {response.status} {payload[:300]}")
        return json.loads(payload)


class MCPClient:
    def __init__(self, command: list[str], env: dict[str, str]):
        self.process = subprocess.Popen(command, cwd=ROOT, env=env, stdin=subprocess.PIPE,
                                        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)

    def call(self, request_id: int, method: str, params: dict | None = None) -> dict:
        message = {"jsonrpc": "2.0", "id": request_id, "method": method}
        if params is not None:
            message["params"] = params
        self.process.stdin.write(json.dumps(message) + "\n")
        self.process.stdin.flush()
        while True:
            line = self.process.stdout.readline()
            if not line:
                raise RuntimeError("MCP server closed the stream")
            try:
                payload = json.loads(line)
            except json.JSONDecodeError:
                continue
            if payload.get("id") == request_id:
                if "error" in payload:
                    raise RuntimeError(f"MCP error: {payload['error']}")
                return payload.get("result", {})

    def notify(self, method: str) -> None:
        self.process.stdin.write(json.dumps({"jsonrpc": "2.0", "method": method}) + "\n")
        self.process.stdin.flush()

    def close(self) -> None:
        try:
            self.process.stdin.close()
        except Exception:  # noqa: BLE001
            pass
        stop(self.process)


def start_launcher(env: dict[str, str], profiles: Path, profile: str, port: int, backend: str,
                   log: Path) -> tuple[subprocess.Popen, float]:
    started = time.monotonic()
    proc = subprocess.Popen(
        [str(ROOT / "scripts/local-gpu-up.sh"), "--profile", profile, "--port", str(port),
         "--profiles-root", str(profiles), "--backend", backend, "--no-build"],
        cwd=ROOT, env=env, stdout=subprocess.PIPE, stderr=log.open("a"), text=True, start_new_session=True)
    deadline = time.monotonic() + 900
    while time.monotonic() < deadline:
        line = proc.stdout.readline()
        if not line:
            raise RuntimeError(f"launcher exited before readiness; see {log}")
        if "Recording-MCP ready" in line:
            return proc, time.monotonic() - started
    raise TimeoutError("launcher did not become ready")


def stop_launcher(proc: subprocess.Popen) -> None:
    stop(proc)


def pcm_to_wav(pcm: Path, wav: Path) -> None:
    subprocess.run(["ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "s16le", "-ar", "16000", "-ac", "1",
                    "-i", str(pcm), "-c:a", "pcm_s16le", str(wav)], check=True)


def wait_terminal(client: Client, conversation_id: str, timeout: float = 1800) -> dict:
    deadline = time.monotonic() + timeout
    snapshot: dict = {}
    while time.monotonic() < deadline:
        snapshot = client.json("GET", f"/api/conversations/{conversation_id}/processing")
        jobs = snapshot.get("jobs") or []
        if jobs and all(job.get("status") in ("succeeded", "failed", "cancelled") for job in jobs):
            return snapshot
        time.sleep(0.5)
    raise TimeoutError(f"processing did not finish; last={snapshot.get('current_stage')}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, default=ROOT / "artifacts/sprint3-demo")
    parser.add_argument("--backend", choices=("ark", "qwen"), default="ark")
    parser.add_argument("--skip-build", action="store_true")
    parser.add_argument("--mcp-query", default="最终确定采用方案 B")
    args = parser.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    if not FIXTURE.exists():
        raise SystemExit(f"authorized fixture missing: {FIXTURE}")
    if not args.skip_build:
        subprocess.run(["make", "build"], cwd=ROOT, check=True)

    temp = Path(tempfile.mkdtemp(prefix="sprint3-demo-"))
    profiles = temp / "profiles"
    password = secrets.token_urlsafe(24)
    env = os.environ.copy()
    env[PASSWORD_ENV] = password
    profile = "sprint3"
    db_path = profiles / profile / "db" / "context-memory.db"
    port = free_port()
    base = f"http://127.0.0.1:{port}"
    log = args.out / "launcher.log"
    steps: list[dict] = []
    commands: list[str] = []
    launcher: subprocess.Popen | None = None

    def step(name: str, ok: bool, detail: str = "") -> None:
        steps.append({"step": name, "status": "PASS" if ok else "FAIL", "detail": detail})
        print(json.dumps(steps[-1], ensure_ascii=False), flush=True)

    try:
        # Preparation (not a numbered step): create the disposable profile and
        # seed the synthetic multi-session corpus before the stack opens it.
        subprocess.run([str(ROOT / "bin/context-profile"), "create", "--profiles-root", str(profiles),
                        "--name", profile], cwd=ROOT, env=env, check=True, capture_output=True)
        seed_log = (args.out / "context-eval-seed.log").open("w")
        seeded = subprocess.run(
            [str(ROOT / "bin/context-eval"), "--db", str(db_path),
             "--corpus", str(DEMO_CORPUS), "--projection", str(DEMO_PROJECTION),
             "--questions", "", "--retrieval", "", "--links", "",
             "--embed-provider", "hash", "--out", str(temp / "eval.json")],
            cwd=ROOT, env=env, stdout=seed_log, stderr=subprocess.STDOUT, text=True)
        seed_log.close()
        commands.append(f"bin/context-eval --db {db_path} --corpus {DEMO_CORPUS} --projection {DEMO_PROJECTION}")
        if seeded.returncode != 0:
            raise RuntimeError(f"context-eval seed failed: exit {seeded.returncode}")

        # 1. one-command startup
        commands.append(f"scripts/local-gpu-up.sh --profile {profile} --port {port} --profiles-root {profiles} --backend {args.backend} --no-build")
        launcher, startup_seconds = start_launcher(env, profiles, profile, port, args.backend, log)
        step("1. 单命令启动", True, f"ready in {startup_seconds:.1f}s")

        client = Client(base)
        client.login(password)

        # 2. capabilities
        caps = client.json("GET", "/api/capabilities")
        asr = caps.get("asr") or {}
        step("2. 能力与 GPU 就绪", bool(asr.get("available")),
             f"provider={asr.get('provider')} gpu={asr.get('gpu_name')} vram={asr.get('vram_free_mb')}/{asr.get('vram_total_mb')} reason={asr.get('reason')}")

        # 3. import the authorized fixture
        wav = temp / "segment-01.wav"
        pcm_to_wav(FIXTURE, wav)
        imported = client.upload(wav)
        conversation_id = imported["conversation_id"]
        recording_id = imported["recording_id"]
        step("3. 导入授权音频", bool(conversation_id), f"conversation={conversation_id} duration_ms={imported['audio']['duration_ms']}")

        # 4. poll every stage
        snapshot = wait_terminal(client, conversation_id)
        status = snapshot.get("status") or {}
        stages = [phase.get("name") for phase in snapshot.get("phases") or []]
        step("4. 阶段轮询", status.get("asr_state") == "done",
             f"stages={stages} asr={status.get('asr_completed_chunks')}/{status.get('asr_expected_chunks')}")

        # 5. transcript + evidence
        detail = client.json("GET", f"/api/conversations/{conversation_id}")
        turns: list[dict] = []
        offset = 0
        for _ in range(20):
            page = client.json("GET", f"/api/conversations/{conversation_id}/turns?"
                                      + urlencode({"recording_id": recording_id, "offset": offset, "limit": 200}))
            turns.extend(item for item in page.get("items") or [] if item.get("kind") == "turn")
            nxt = page.get("next_offset")
            if nxt is None or nxt <= offset:
                break
            offset = nxt
        if not turns:
            raise RuntimeError("no transcript turns after ASR")
        first_turn = turns[0]
        evidence = client.json("GET", f"/api/turns/{quote(first_turn['id'], safe='')}/evidence")
        step("5. 转写与证据", bool(evidence), f"turns={len(turns)} evidence_keys={sorted(evidence)}")

        # 6. lexical search on real transcript text
        phrase = (first_turn.get("content") or "")[:12].strip()
        search = client.json("GET", "/api/search?" + urlencode({"q": phrase, "limit": 5}))
        step("6. 字面检索", len(search.get("hits") or []) > 0, f"q={phrase!r} hits={len(search.get('hits') or [])}")

        # 7. cross-session memory over the seeded synthetic corpus
        hybrid = client.json("GET", "/api/search/hybrid?" + urlencode({
            "q": args.mcp_query, "mode": "hybrid", "retrieval_version": "v2", "max_items": 10}))
        step("7. 跨会话记忆检索", len(hybrid.get("items") or []) > 0,
             f"items={len(hybrid.get('items') or [])}")

        # 8. sufficiency status and valid refs
        assessment = hybrid.get("sufficiency") or {}
        evidence_ids = {item.get("turn_id") for item in hybrid.get("evidence") or []}
        refs = assessment.get("evidence_refs") or []
        valid_refs = all(ref in evidence_ids for ref in refs)
        step("8. 证据充分性", assessment.get("status") in ("sufficient", "insufficient", "unknown") and valid_refs,
             f"status={assessment.get('status')} refs={len(refs)} valid={valid_refs}")

        # 9. decision trail
        trail = client.json("GET", "/api/decisions")
        step("9. 决策轨迹", isinstance(trail.get("topics"), list) and "version" in trail,
             f"version={trail.get('version')} topics={len(trail.get('topics') or [])}")

        # 10. restart the stack
        stop_launcher(launcher)
        launcher = None
        launcher, restart_seconds = start_launcher(env, profiles, profile, port, args.backend, log)
        client = Client(base)
        client.login(password)
        after = client.json("GET", f"/api/conversations/{conversation_id}")
        step("10. 重启后持久化", after.get("conversation_id") == conversation_id,
             f"ready in {restart_seconds:.1f}s")

        # 11. MCP read-back
        mcp = MCPClient([str(ROOT / "bin/context-memory"), "--profile", profile,
                         "--profiles-root", str(profiles), "--read-only"], env)
        mcp.call(1, "initialize", {"protocolVersion": "2024-11-05", "capabilities": {},
                                   "clientInfo": {"name": "sprint3-demo", "version": "1"}})
        mcp.notify("notifications/initialized")
        context_result = mcp.call(2, "tools/call", {"name": "build_context",
                                                    "arguments": {"query": args.mcp_query, "retrieval_version": "v2"}})
        trail_result = mcp.call(3, "tools/call", {"name": "get_decision_trail", "arguments": {}})
        mcp.close()
        structured = context_result.get("structuredContent") or {}
        trail_structured = trail_result.get("structuredContent") or {}
        step("11. MCP 读取", bool(structured.get("items")) and "topics" in trail_structured,
             f"items={len(structured.get('items') or [])} topics={len(trail_structured.get('topics') or [])}")

        # 12. persisted state and source integrity
        duplicate = client.upload(wav)
        final = client.json("GET", f"/api/conversations/{conversation_id}/processing")
        final_status = final.get("status") or {}
        step("12. 状态与来源完整性",
             duplicate.get("duplicate") is True and duplicate.get("conversation_id") == conversation_id
             and int(final_status.get("received_segments") or 0) > 0,
             f"duplicate={duplicate.get('duplicate')} segments={final_status.get('received_segments')}")
    except Exception as exc:  # noqa: BLE001
        if not steps or steps[-1]["status"] == "PASS":
            step("demo", False, repr(exc))
    finally:
        stop_launcher(launcher)
        result = {
            "started_profile": profile,
            "backend": args.backend,
            "ui": base,
            "fixture": str(FIXTURE.relative_to(ROOT)),
            "commands": commands,
            "steps": steps,
            "passed": sum(1 for s in steps if s["status"] == "PASS"),
            "total": len(steps),
            "password": "<redacted>",
        }
        (args.out / "result.json").write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n")
        print(json.dumps({k: result[k] for k in ("passed", "total")}, ensure_ascii=False))
    return 0 if result["passed"] == 12 and result["total"] == 12 else 1


if __name__ == "__main__":
    raise SystemExit(main())
