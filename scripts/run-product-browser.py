#!/usr/bin/env python3
"""Run the product workflow browser suite against a disposable real profile.

Starts context-web and context-worker with the fixture ASR provider, signs in
to a temporary profile, uploads a synthetic WAV through the browser and checks
search/evidence/playback/refresh/layout/profile isolation. Never touches
data/ or the user's real profiles.
"""
from __future__ import annotations

import argparse
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
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def wait_http(url: str, process: subprocess.Popen, log: Path, timeout: float = 60) -> None:
    deadline = time.monotonic() + timeout
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"managed server exited {process.returncode}; see {log}")
        try:
            with opener.open(url, timeout=1) as response:
                if response.status < 500:
                    return
        except (urllib.error.URLError, TimeoutError, OSError):
            time.sleep(0.25)
    raise TimeoutError(f"server did not become ready at {url}; see {log}")


def stop(process: subprocess.Popen) -> None:
    if process.poll() is not None:
        return
    try:
        os.killpg(process.pid, signal.SIGTERM)
        process.wait(timeout=10)
    except (ProcessLookupError, subprocess.TimeoutExpired):
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait()


def create_profile(root: Path, name: str, password: str) -> None:
    env = os.environ | {"MEMORY_PROFILE_PASSWORD": password}
    subprocess.run([str(ROOT / "bin/context-profile"), "create", "--profiles-root", str(root), "--name", name],
                   cwd=ROOT, env=env, check=True, capture_output=True, text=True)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--artifacts", type=Path, default=Path("artifacts/product-browser"))
    parser.add_argument("--skip-build", action="store_true")
    args = parser.parse_args()
    artifacts = args.artifacts.resolve()
    artifacts.mkdir(parents=True, exist_ok=True)
    env = os.environ.copy()
    env["NO_PROXY"] = ",".join(filter(None, [env.get("NO_PROXY", ""), "127.0.0.1", "localhost"]))
    if not args.skip_build:
        subprocess.run(["make", "build", "web"], cwd=ROOT, env=env, check=True)
    for expected in (ROOT / "bin/context-web", ROOT / "bin/context-worker", ROOT / "bin/context-profile",
                     ROOT / "web/dist/index.html"):
        if not expected.exists():
            raise RuntimeError(f"missing prerequisite: {expected}")

    temp = Path(tempfile.mkdtemp(prefix="recording-product-browser-"))
    profiles = temp / "profiles"
    alpha_password = secrets.token_urlsafe(24)
    beta_password = secrets.token_urlsafe(24)
    create_profile(profiles, "alpha", alpha_password)
    create_profile(profiles, "beta", beta_password)

    port = free_port()
    base = f"http://127.0.0.1:{port}"
    web_log = (artifacts / "product-web.log").open("w")
    worker_log = (artifacts / "product-worker.log").open("w")
    web_env = env | {"MEMORY_PROFILE_PASSWORD": alpha_password}
    web = subprocess.Popen(
        [str(ROOT / "bin/context-web"), "--profiles-root", str(profiles), "--addr", f"127.0.0.1:{port}",
         "--web-dir", str(ROOT / "web/dist"), "--semantic-provider", "hash", "--asr-provider", "fixture",
         "--fixture-window-ms", "60000"],
        cwd=ROOT, env=web_env, stdout=web_log, stderr=subprocess.STDOUT, start_new_session=True)
    worker = subprocess.Popen(
        [str(ROOT / "bin/context-worker"), "--profile", "alpha", "--profiles-root", str(profiles),
         "--asr-provider", "fixture", "--fixture-window-ms", "60000",
         "--generator", "off", "--stages", "ingest,asr,extract,publish,embed", "--poll", "200ms"],
        cwd=ROOT, env=web_env, stdout=worker_log, stderr=subprocess.STDOUT, start_new_session=True)
    try:
        wait_http(base + "/api/auth-info", web, artifacts / "product-web.log")
        run_env = env | {
            "BASE_URL": base, "ALPHA_PASSWORD": alpha_password, "BETA_PASSWORD": beta_password,
            "BROWSER_ARTIFACT_DIR": str(artifacts),
        }
        python = env.get("PYTHON") or shutil.which("python3.14") or sys.executable
        result = subprocess.run([python, str(ROOT / "scripts/product-workflow-browser.py")],
                                cwd=ROOT, env=run_env, text=True)
        report_path = artifacts / "product-workflow-results.json"
        summary = json.loads(report_path.read_text()) if report_path.exists() else {}
        print(json.dumps({"exit": result.returncode, "artifacts": str(artifacts),
                          "passed": summary.get("passed"), "total": summary.get("total")}, ensure_ascii=False))
        return result.returncode
    finally:
        stop(worker)
        stop(web)
        web_log.close()
        worker_log.close()
        shutil.rmtree(temp, ignore_errors=True)


if __name__ == "__main__":
    raise SystemExit(main())
