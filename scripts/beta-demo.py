#!/usr/bin/env python3
"""Local-first Beta demo: ten reproducible steps on a disposable profile.

Everything runs against /tmp: a synthetic corpus, a temporary profile, a
generated PCM bridge and the real web/MCP binaries. No private recording is
read and no external API is called.
"""
from __future__ import annotations

import argparse
import http.client
import json
import os
import secrets
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
PASSWORD_ENV = "MEMORY_PROFILE_PASSWORD"


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
            time.sleep(0.2)
    raise TimeoutError(f"not ready: {url}; see {log}")


def stop(process: subprocess.Popen) -> None:
    if process.poll() is not None:
        return
    try:
        os.killpg(process.pid, signal.SIGTERM)
        process.wait(timeout=15)
    except (ProcessLookupError, subprocess.TimeoutExpired):
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait()


class Client:
    def __init__(self, base: str):
        self.base = base
        self.cookie = ""
        self.csrf = ""

    def request(self, method: str, path: str, body: bytes | None = None, extra: dict | None = None) -> tuple[int, bytes, dict]:
        host = self.base.split("://", 1)[1]
        conn = http.client.HTTPConnection(host, timeout=120)
        headers = {"Origin": self.base, "Accept": "application/json"}
        if body is not None:
            headers["Content-Type"] = "application/json"
            headers["Content-Length"] = str(len(body))
        if self.cookie:
            headers["Cookie"] = self.cookie
        if self.csrf and method != "GET":
            headers["X-CSRF-Token"] = self.csrf
        headers.update(extra or {})
        conn.request(method, path, body=body, headers=headers)
        response = conn.getresponse()
        payload = response.read()
        cookie = response.getheader("Set-Cookie") or ""
        if cookie:
            self.cookie = cookie.split(";", 1)[0]
        result_headers = dict(response.getheaders())
        conn.close()
        return response.status, payload, result_headers

    def json(self, method: str, path: str, body: dict | None = None) -> dict:
        status, payload, _ = self.request(method, path, json.dumps(body).encode() if body is not None else None)
        if status >= 400:
            raise RuntimeError(f"{method} {path}: {status} {payload[:300]}")
        return json.loads(payload) if payload else {}

    def login(self, password: str) -> None:
        data = self.json("POST", "/api/login", {"password": password})
        self.csrf = data["csrf_token"]


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


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, default=Path("artifacts/beta-demo"))
    parser.add_argument("--skip-build", action="store_true")
    args = parser.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    env = os.environ.copy()
    if not args.skip_build:
        subprocess.run(["make", "build"], cwd=ROOT, env=env, check=True)
    for binary in ("context-eval", "context-web", "context-memory", "context-profile"):
        if not (ROOT / "bin" / binary).exists():
            raise SystemExit(f"missing bin/{binary}")

    temp = Path(tempfile.mkdtemp(prefix="recording-beta-demo-"))
    profiles = temp / "profiles"
    password = secrets.token_urlsafe(24)
    env[PASSWORD_ENV] = password
    db = temp / "demo.db"
    audio = temp / "audio"
    steps: list[dict] = []

    def step(name: str, ok: bool, detail: str = "") -> None:
        steps.append({"step": name, "status": "PASS" if ok else "FAIL", "detail": detail})
        print(json.dumps(steps[-1], ensure_ascii=False), flush=True)

    web = None
    try:
        # 1. Build fixture database through the real eval harness.
        eval_log = (args.out / "context-eval.log").open("w")
        result = subprocess.run(
            [str(ROOT / "bin/context-eval"), "--db", str(db),
             "--corpus", str(ROOT / "fixtures/productization/beta-demo/corpus.json"),
             "--projection", str(ROOT / "fixtures/productization/beta-demo/projection.json"),
             "--questions", "", "--retrieval", "", "--links", "",
             "--embed-provider", "hash", "--out", str(temp / "eval.json")],
            cwd=ROOT, env=env, stdout=eval_log, stderr=subprocess.STDOUT, text=True)
        eval_log.close()
        step("1. 生成 fixture 资料库", result.returncode == 0, f"context-eval exit={result.returncode}")
        if result.returncode != 0:
            raise RuntimeError("context-eval failed")

        # The harness also runs the synthetic generator; the demo keeps only
        # its five hand-authored projection notes.
        import sqlite3
        with sqlite3.connect(db) as conn:
            conn.execute("DELETE FROM note_evidence WHERE note_id NOT LIKE 'beta-%'")
            conn.execute("DELETE FROM notes WHERE id NOT LIKE 'beta-%'")
            conn.commit()
            notes = conn.execute("SELECT COUNT(*) FROM notes").fetchone()[0]
        step("2. 只保留人工投影笔记", notes == 5, f"notes={notes}")

        # 3. Audio bridge + profile adoption.
        bridge = subprocess.run([sys.executable, str(ROOT / "scripts/fixture-audio-bridge.py"),
                                 "--db", str(db), "--audio-dir", str(audio)],
                                cwd=ROOT, env=env, capture_output=True, text=True)
        step("3. 生成合成音频桥接", bridge.returncode == 0, bridge.stdout.strip() or bridge.stderr.strip())
        adopt = subprocess.run([str(ROOT / "bin/context-profile"), "adopt", "--profiles-root", str(profiles),
                                "--name", "demo", "--db", str(db), "--audio-dir", str(audio)],
                               cwd=ROOT, env=env, capture_output=True, text=True)
        step("4. 创建隔离 profile", adopt.returncode == 0, adopt.stdout.strip() or adopt.stderr.strip())
        if adopt.returncode != 0:
            raise RuntimeError("profile adopt failed")

        # 5. Start web and read the conversations.
        port = free_port()
        base = f"http://127.0.0.1:{port}"
        web_log_path = args.out / "demo-web.log"
        web_log = web_log_path.open("w")
        web = subprocess.Popen([str(ROOT / "bin/context-web"), "--profile", "demo", "--profiles-root", str(profiles),
                                "--addr", f"127.0.0.1:{port}", "--web-dir", str(ROOT / "web/dist"),
                                "--semantic-provider", "hash", "--asr-provider", "off"],
                               cwd=ROOT, env=env, stdout=web_log, stderr=subprocess.STDOUT, start_new_session=True)
        wait_http(base + "/api/auth-info", web, web_log_path)
        client = Client(base)
        client.login(password)
        conversations = client.json("GET", "/api/conversations")
        step("5. 登录并读取 3 个会话", len(conversations["items"]) == 3,
             [item["title"] for item in conversations["items"]])

        # 6. Worldline + decision trail show three sessions and a candidate.
        worldline = client.json("GET", "/api/worldline?limit=10")
        topic_id = worldline["topics"][0]["id"] if worldline.get("topics") else ""
        trail = client.json("GET", "/api/decisions?" + urlencode({"topic_id": topic_id}))
        topic = trail["topics"][0] if trail.get("topics") else {}
        # Three decision cues produce three candidates; the projection must
        # report unresolved until a human confirms one.
        step("6. 决策轨迹包含候选", topic.get("counts", {}).get("candidates") == 3
             and topic.get("current_state") in ("unresolved", "decision_candidate"),
             f"state={topic.get('current_state')} candidates={topic.get('counts', {}).get('candidates')}")

        # 7. Confirm the final candidate through the web API.
        candidate = next((record for record in topic.get("history", [])
                          if record["state"] == "decision_candidate" and "最终确定采用方案 B" in record["text"]), None)
        if candidate is None:
            raise RuntimeError("final candidate not found")
        confirmed = client.json("POST", "/api/decisions/review", {
            "expected_version": trail["version"], "candidate_id": candidate["candidate_id"],
            "action": "confirm", "request_id": "beta-demo-confirm", "reviewer": "demo-user",
        })
        confirmed_topic = confirmed["topics"][0]
        step("7. 人工确认最终候选", confirmed_topic.get("current_state") == "confirmed"
             and "最终确定采用方案 B" in (confirmed_topic.get("current") or {}).get("text", ""),
             (confirmed_topic.get("current") or {}).get("text", ""))

        # 8. Range audio request for the first turn.
        detail = client.json("GET", f"/api/conversations/{conversations['items'][0]['conversation_id']}")
        recording = detail["recordings"][0]
        turns = client.json("GET", f"/api/conversations/{detail['conversation_id']}/turns?"
                            + urlencode({"recording_id": recording["recording_id"], "limit": 5}))
        turn = next(item for item in turns["items"] if item["kind"] == "turn")
        status, _, headers = client.request("GET", f"/api/turns/{turn['id']}/audio", extra={"Range": "bytes=0-1023"})
        lowered = {key.lower(): value for key, value in headers.items()}
        step("8. 音频请求返回片段元数据", status in (200, 206) and lowered.get("x-audio-start-ms") is not None,
             f"status={status} start={lowered.get('x-audio-start-ms')}")

        # 9. MCP reads the same decision from the profile event plane.
        mcp = MCPClient([str(ROOT / "bin/context-memory"), "--profile", "demo", "--profiles-root", str(profiles), "--read-only"], env)
        try:
            mcp.call(1, "initialize", {"protocolVersion": "2024-11-05", "capabilities": {},
                                       "clientInfo": {"name": "beta-demo", "version": "1"}})
            mcp.notify("notifications/initialized")
            result_payload = mcp.call(2, "tools/call", {"name": "get_decision_trail", "arguments": {}})
        finally:
            mcp.close()
        structured = result_payload.get("structuredContent") or {}
        mcp_topics = structured.get("topics") or []
        mcp_current = (mcp_topics[0].get("current") or {}) if mcp_topics else {}
        step("9. MCP 读取同一决策", "最终确定采用方案 B" in mcp_current.get("text", ""),
             f"schema={structured.get('schema_version')}")

        # 10. Restart web and confirm the decision persisted.
        stop(web)
        web_log.close()
        web_log = web_log_path.open("a")
        web = subprocess.Popen([str(ROOT / "bin/context-web"), "--profile", "demo", "--profiles-root", str(profiles),
                                "--addr", f"127.0.0.1:{port}", "--web-dir", str(ROOT / "web/dist"),
                                "--semantic-provider", "hash", "--asr-provider", "off"],
                               cwd=ROOT, env=env, stdout=web_log, stderr=subprocess.STDOUT, start_new_session=True)
        wait_http(base + "/api/auth-info", web, web_log_path)
        client = Client(base)
        client.login(password)
        after = client.json("GET", "/api/decisions?" + urlencode({"topic_id": topic_id}))
        step("10. 重启后决策仍在", (after["topics"][0].get("current") or {}).get("text", "").startswith("最终确定采用方案 B"),
             (after["topics"][0].get("current") or {}).get("text", ""))

        report = {
            "fixture_disclosure": "synthetic beta-demo corpus and fixture PCM; no real ASR or private audio",
            "profile": "demo", "profile_root": str(profiles), "password_printed": False,
            "steps": steps, "passed": sum(1 for item in steps if item["status"] == "PASS"), "total": len(steps),
        }
    finally:
        if web is not None:
            stop(web)
        shutil.rmtree(temp, ignore_errors=True)

    (args.out / "result.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps({"passed": report["passed"], "total": report["total"], "out": str(args.out)}, ensure_ascii=False))
    return 0 if report["passed"] == report["total"] == 10 else 1


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (RuntimeError, TimeoutError, subprocess.CalledProcessError, OSError) as exc:
        print(f"beta demo failed: {exc}", file=sys.stderr)
        raise SystemExit(1)
