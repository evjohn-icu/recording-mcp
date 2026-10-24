#!/usr/bin/env python3
"""Run synthetic browser suites against managed disposable local processes only."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = Path(__file__).resolve().parents[1]


class FakePolishHandler(BaseHTTPRequestHandler):
    requests = []

    def log_message(self, *_args):
        pass

    def do_OPTIONS(self):
        self.send_response(204)
        self.send_header('Access-Control-Allow-Origin', '*')
        self.send_header('Access-Control-Allow-Methods', 'GET, POST, OPTIONS')
        self.send_header('Access-Control-Allow-Headers', 'Content-Type, Authorization')
        self.end_headers()

    def do_GET(self):
        if self.path != '/stats':
            self.send_error(404)
            return
        payload = json.dumps({'count': len(type(self).requests)}).encode()
        self.send_response(200)
        self.send_header('Access-Control-Allow-Origin', '*')
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
        type(self).requests.append(body)
        if len(type(self).requests) > 1:
            self.send_response(503)
            self.end_headers()
            return
        try:
            request = json.loads(body)
            content = request['messages'][1]['content']
            entries = json.loads(content.split('Input entries:\n', 1)[1])
            candidates = [] if not entries else [{
                'entry_id': entries[0]['id'],
                'text': 'synthetic polish candidate',
                'evidence_ids': entries[0]['evidence_ids'],
            }]
            response = json.dumps({'choices': [{'message': {'content': json.dumps({'candidates': candidates})}}]}).encode()
        except Exception:
            self.send_response(400)
            self.end_headers()
            return
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(response)))
        self.end_headers()
        self.wfile.write(response)


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def wait_http(url: str, process: subprocess.Popen, log: Path, timeout: float = 45) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"managed server exited {process.returncode}; see {log}")
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        try:
            with opener.open(url, timeout=1) as response:
                if response.status < 500:
                    return
        except (urllib.error.URLError, TimeoutError, OSError):
            time.sleep(.25)
    raise TimeoutError(f"server did not become ready at {url}; see {log}")


def run() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--artifacts', type=Path, help='output directory (defaults to a fresh temporary directory)')
    parser.add_argument('--skip-build', action='store_true')
    parser.add_argument('--browser', help='Playwright Chromium executable override')
    args = parser.parse_args()
    artifact_owner = tempfile.mkdtemp(prefix='recording-browser-') if args.artifacts is None else None
    artifacts = args.artifacts or Path(artifact_owner)
    artifacts.mkdir(parents=True, exist_ok=True)
    print(f'Synthetic-only browser artifacts: {artifacts}', flush=True)
    env = os.environ.copy()
    env['BROWSER_ARTIFACT_DIR'] = str(artifacts)
    if args.browser:
        env['BROWSER_PATH'] = args.browser
    if not args.skip_build:
        subprocess.run(['make', 'build', 'web'], cwd=ROOT, env=env, check=True)
    for expected in (ROOT/'bin/context-web', ROOT/'bin/context-eval', ROOT/'web/dist/index.html'):
        if not expected.exists():
            raise RuntimeError(f'missing managed-runner prerequisite: {expected}')

    port = free_port()
    base = f'http://127.0.0.1:{port}'
    env['BASE_URL'] = base
    log_path = artifacts/'web-demo.log'
    log = log_path.open('w')
    child = subprocess.Popen(['sh', 'scripts/web-demo.sh', str(port)], cwd=ROOT, env=env,
                             stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
    try:
        wait_http(base+'/api/auth-info', child, log_path)
        demo_log = log_path.read_text()
        demo_dir_line = next((line.split(': ', 1)[1] for line in demo_log.splitlines() if line.startswith('demo directory: ')), None)
        if not demo_dir_line:
            raise RuntimeError('synthetic web-demo did not report its temporary fixture directory')
        fixture_dir = Path(demo_dir_line)
        profiles_root = artifacts/'profiles'
        fixture = subprocess.run([sys.executable, str(ROOT/'scripts/synthetic-profile-fixture.py'),
                                  '--profiles-root', str(profiles_root), '--db', str(fixture_dir/'synthetic-eval.db'),
                                  '--audio-dir', str(fixture_dir/'synthetic-audio')],
                                 cwd=ROOT, env=env, text=True, capture_output=True, check=True)
        credentials = __import__('json').loads(fixture.stdout.splitlines()[-1])
        profile_port = free_port()
        profile_base = f'http://127.0.0.1:{profile_port}'
        profile_log_path = artifacts/'profile-web.log'
        profile_log = profile_log_path.open('w')
        profile_server = subprocess.Popen([str(ROOT/'bin/context-web'), '--profiles-root', str(profiles_root),
                                           '--addr', f'127.0.0.1:{profile_port}', '--web-dir', str(ROOT/'web/dist'),
                                           '--semantic-provider', 'hash'], cwd=ROOT, env=env,
                                          stdout=profile_log, stderr=subprocess.STDOUT, start_new_session=True)
        try:
            wait_http(profile_base+'/api/auth-info', profile_server, profile_log_path)
            env.update({'BASE_URL': profile_base, 'ALPHA_PASSWORD': credentials['alpha_password'],
                        'BETA_PASSWORD': credentials['beta_password'], 'SECOND_PROFILE': 'beta',
                        'SECOND_PASSWORD': credentials['beta_password']})
            python = env.get('PYTHON') or shutil.which('python3.14') or sys.executable
            subprocess.run([python, str(ROOT/'scripts/retrieval-version-browser.py')], cwd=ROOT, env=env, check=True)
            # Memory Highlights and Brief include irreversible forget/revision
            # operations. Give that flow its own freshly adopted profile so the
            # existing local-v2 suite retains the pristine synthetic fixture.
            feature_profiles = artifacts/'highlights-brief-profiles'
            feature_fixture = subprocess.run([sys.executable, str(ROOT/'scripts/synthetic-profile-fixture.py'),
                                              '--profiles-root', str(feature_profiles), '--db', str(fixture_dir/'synthetic-eval.db'),
                                              '--audio-dir', str(fixture_dir/'synthetic-audio')],
                                             cwd=ROOT, env=env, text=True, capture_output=True, check=True)
            feature_credentials = __import__('json').loads(feature_fixture.stdout.splitlines()[-1])
            feature_port = free_port()
            feature_base = f'http://127.0.0.1:{feature_port}'
            FakePolishHandler.requests = []
            fake_polish = ThreadingHTTPServer(('127.0.0.1', 0), FakePolishHandler)
            fake_polish_thread = threading.Thread(target=fake_polish.serve_forever, daemon=True)
            fake_polish_thread.start()
            polish_endpoint = f'http://127.0.0.1:{fake_polish.server_address[1]}/chat/completions'
            feature_log_path = artifacts/'highlights-brief-web.log'
            feature_log = feature_log_path.open('w')
            feature_server = subprocess.Popen([str(ROOT/'bin/context-web'), '--profiles-root', str(feature_profiles),
                                                '--addr', f'127.0.0.1:{feature_port}', '--web-dir', str(ROOT/'web/dist'),
                                                '--semantic-provider', 'hash', '--brief-polish-endpoint', polish_endpoint,
                                                '--brief-polish-model', 'test'], cwd=ROOT, env=env,
                                               stdout=feature_log, stderr=subprocess.STDOUT, start_new_session=True)
            try:
                wait_http(feature_base+'/api/auth-info', feature_server, feature_log_path)
                feature_env = env | {'BASE_URL': feature_base, 'ALPHA_PASSWORD': feature_credentials['alpha_password'],
                                    'FAKE_POLISH_BASE': f'http://127.0.0.1:{fake_polish.server_address[1]}'}
                subprocess.run([python, str(ROOT/'scripts/highlights-brief-browser.py')], cwd=ROOT, env=feature_env, check=True)
            finally:
                _stop(feature_server)
                feature_log.close()
                fake_polish.shutdown()
                fake_polish.server_close()
                fake_polish_thread.join(timeout=5)
            subprocess.run([python, str(ROOT/'scripts/local-v2-flow-browser.py')], cwd=ROOT, env=env, check=True)
        finally:
            _stop(profile_server)
            profile_log.close()
        # Serve the built application from Vite preview for the mocked browser
        # suites. Preview serves only built assets, so /api route mocks cannot
        # accidentally intercept source modules such as /src/api/client.ts.
        vite_port = free_port()
        vite_base = f'http://127.0.0.1:{vite_port}'
        vite_log_path = artifacts/'vite.log'
        vite_log = vite_log_path.open('w')
        vite = subprocess.Popen(['npm', 'exec', 'vite', '--', 'preview', '--host', '127.0.0.1', '--port', str(vite_port), '--strictPort'],
                                cwd=ROOT/'web', env=env, stdout=vite_log, stderr=subprocess.STDOUT, start_new_session=True)
        try:
            wait_http(vite_base, vite, vite_log_path)
            python = env.get('PYTHON') or shutil.which('python3.14') or sys.executable
            env['BASE_URL'] = vite_base
            subprocess.run([python, str(ROOT/'scripts/worldline-synthetic-browser.py')], cwd=ROOT, env=env, check=True)
            subprocess.run([python, str(ROOT/'scripts/processing-poll-browser.py')], cwd=ROOT, env=env, check=True)
        finally:
            _stop(vite)
            vite_log.close()
        print(f'Live synthetic profile browser suite completed at {profile_base}; profile seed is disposable.', flush=True)
        return 0
    finally:
        _stop(child)
        log.close()
        if 'fixture_dir' in locals() and fixture_dir.name.startswith('recording-mcp-web-demo.') and fixture_dir.parent == Path('/tmp'):
            shutil.rmtree(fixture_dir, ignore_errors=True)
        if artifact_owner:
            print(f'Artifacts retained at {artifact_owner}', flush=True)


def _stop(process: subprocess.Popen) -> None:
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


if __name__ == '__main__':
    try:
        raise SystemExit(run())
    except (RuntimeError, TimeoutError, subprocess.CalledProcessError, OSError) as exc:
        print(f'browser runner failed: {exc}', file=sys.stderr)
        raise SystemExit(1)
