#!/usr/bin/env python3
"""Unit tests for managed runner safety and process readiness."""
import importlib.util
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
import subprocess
import threading
import unittest

SCRIPT = Path(__file__).with_name('run-local-browser-tests.py')
spec = importlib.util.spec_from_file_location('browser_runner', SCRIPT)
runner = importlib.util.module_from_spec(spec)
assert spec.loader
spec.loader.exec_module(runner)


class RunnerTests(unittest.TestCase):
    def test_free_port_is_loopback_available(self):
        port = runner.free_port()
        self.assertGreater(port, 0)
        with __import__('socket').socket() as sock:
            sock.bind(('127.0.0.1', port))

    def test_wait_http_managed_server(self):
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.end_headers()
            def log_message(self, *_args):
                pass
        server = HTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        process = subprocess.Popen(['python3', '-c', 'import time; time.sleep(10)'])
        try:
            runner.wait_http(f'http://127.0.0.1:{server.server_port}/', process, Path('/dev/null'), 2)
        finally:
            process.terminate()
            process.wait()
            server.shutdown()
            thread.join()

    def test_wait_http_fails_if_child_exits(self):
        process = subprocess.Popen(['python3', '-c', 'raise SystemExit(7)'])
        process.wait()
        with self.assertRaisesRegex(RuntimeError, 'exited 7'):
            runner.wait_http('http://127.0.0.1:1/', process, Path('/dev/null'), .1)


if __name__ == '__main__':
    unittest.main()
