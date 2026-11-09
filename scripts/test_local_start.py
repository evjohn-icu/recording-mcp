import json
import os
import shutil
import subprocess
import tempfile
import textwrap
import time
import unittest
from pathlib import Path

WORKER = """#!/usr/bin/env python3
import json, os, signal, sys, time
role = 'worker'
open(os.environ['ARGS_LOG'], 'a').write(role + ' ' + json.dumps(sys.argv[1:]) + '\\n')
signal.signal(signal.SIGTERM, lambda *_: (open(os.environ['STOP_MARKER'], 'w').write('stopped'), sys.exit(0)))
open(os.environ['READY_MARKER'] + '.' + role, 'w').write('ready')
if os.environ['FAIL_CHILD'] == role:
 deadline = time.time() + 5
 while not os.path.exists(os.environ['READY_MARKER'] + '.web') and time.time() < deadline:
  time.sleep(.02)
 time.sleep(.1)
 sys.exit(7)
while True: time.sleep(1)
"""

WEB = """#!/usr/bin/env python3
import json, os, signal, sys, time
role = 'web'
open(os.environ['ARGS_LOG'], 'a').write(role + ' ' + json.dumps(sys.argv[1:]) + '\\n')
signal.signal(signal.SIGTERM, lambda *_: (open(os.environ['STOP_MARKER'], 'w').write('stopped'), sys.exit(0)))
open(os.environ['READY_MARKER'] + '.' + role, 'w').write('ready')
if os.environ['FAIL_CHILD'] == role:
 deadline = time.time() + 5
 while not os.path.exists(os.environ['READY_MARKER'] + '.worker') and time.time() < deadline:
  time.sleep(.02)
 time.sleep(.1)
 sys.exit(9)
while True: time.sleep(1)
"""


def flag_value(argv: list[str], flag: str) -> str:
    index = argv.index(flag)
    return argv[index + 1]


class LocalStartTests(unittest.TestCase):
    def run_fake_pair(self, failing, configured=False, local_asr=False):
        with tempfile.TemporaryDirectory(prefix="local-start-test-") as tmp:
            root = Path(tmp)
            (root / "scripts").mkdir()
            (root / "bin").mkdir()
            (root / "web" / "dist").mkdir(parents=True)
            (root / "web" / "dist" / "index.html").write_text("fixture")
            shutil.copy(Path(__file__).parents[1] / "scripts" / "local-start.sh", root / "scripts" / "local-start.sh")
            log = root / "args.log"
            stopped = root / "peer-stopped"
            for name, body in {"context-worker": WORKER, "context-web": WEB}.items():
                path = root / "bin" / name
                path.write_text(textwrap.dedent(body))
                path.chmod(0o755)
            env = {k: v for k, v in os.environ.items()
                   if not k.startswith(("GENERATOR_", "WORLDLINE_", "BRIEF_POLISH_", "ASR_"))}
            env.update(ARGS_LOG=str(log), STOP_MARKER=str(stopped), READY_MARKER=str(root / "ready"),
                       FAIL_CHILD=failing)
            if local_asr:
                env.update(ASR_PROVIDER="ark", ASR_BACKEND="ark", ASR_MODEL="/models/ark",
                           ASR_PYTHON="/opt/asr/bin/python", ASR_DEVICE="cuda:1", ASR_REVISION="deadbeef")
            if configured:
                env.update(BRIEF_POLISH_PI_PROVIDER="fixture-provider", BRIEF_POLISH_MODEL="fixture-model",
                           BRIEF_POLISH_PI_MODELS_FILE="/tmp/synthetic models.json",
                           GENERATOR_PROVIDER="openai",
                           GENERATOR_ENDPOINT="http://127.0.0.1:18080/v1/chat/completions",
                           GENERATOR_MODEL="synthetic-config-only",
                           WORLDLINE_DECISION_URL="http://127.0.0.1:18799/v1/systemone")
            proc = subprocess.run(
                [str(root / "scripts" / "local-start.sh"), "fixture", "28771", str(root / "profiles with spaces")],
                env=env, capture_output=True, text=True, timeout=10,
            )
            self.assertEqual(proc.returncode, 7 if failing == "worker" else 9, proc.stderr)
            self.assertTrue(stopped.exists(), "the surviving child was not stopped")
            argv = {}
            for line in log.read_text().splitlines():
                role, payload = line.split(" ", 1)
                argv[role] = json.loads(payload)
            self.assertEqual(set(argv), {"web", "worker"})
            for role in ("web", "worker"):
                self.assertEqual(flag_value(argv[role], "--profile"), "fixture")
                self.assertEqual(flag_value(argv[role], "--profiles-root"), str(root / "profiles with spaces"))
            if configured:
                web = argv["web"]
                self.assertEqual(flag_value(web, "--brief-polish-pi-provider"), "fixture-provider")
                self.assertEqual(flag_value(web, "--brief-polish-model"), "fixture-model")
                self.assertEqual(flag_value(web, "--brief-polish-pi-models-file"), "/tmp/synthetic models.json")
                self.assertNotIn("--brief-polish-endpoint", web)
                self.assertEqual(flag_value(argv["worker"], "--generator"), "openai")
                self.assertEqual(flag_value(argv["worker"], "--generator-url"),
                                 "http://127.0.0.1:18080/v1/chat/completions")
                self.assertEqual(flag_value(argv["worker"], "--generator-model"), "synthetic-config-only")
                self.assertEqual(flag_value(argv["worker"], "--worldline-decision-url"),
                                 "http://127.0.0.1:18799/v1/systemone")
                self.assertEqual(flag_value(web, "--worldline-decision-url"),
                                 "http://127.0.0.1:18799/v1/systemone")
            else:
                self.assertEqual(flag_value(argv["worker"], "--generator"), "off")
                self.assertEqual(flag_value(argv["worker"], "--embed-provider"), "hash")
                for role in ("web", "worker"):
                    self.assertNotIn("--brief-polish", argv[role])
            if local_asr:
                for role in ("web", "worker"):
                    args = argv[role]
                    self.assertEqual(flag_value(args, "--asr-provider"), "ark")
                    self.assertEqual(flag_value(args, "--asr-backend"), "ark")
                    self.assertEqual(flag_value(args, "--asr-model"), "/models/ark")
                    self.assertEqual(flag_value(args, "--asr-python"), "/opt/asr/bin/python")
                    self.assertEqual(flag_value(args, "--asr-device"), "cuda:1")
                    self.assertEqual(flag_value(args, "--asr-revision"), "deadbeef")

    def test_worker_exit_stops_web_and_propagates_status(self):
        self.run_fake_pair("worker")

    def test_web_exit_stops_worker_and_propagates_status(self):
        self.run_fake_pair("web")

    def test_explicit_model_configuration_reaches_both_services(self):
        self.run_fake_pair("worker", configured=True)

    def test_local_asr_configuration_reaches_both_services(self):
        self.run_fake_pair("worker", local_asr=True)

    def test_missing_built_site_fails_before_launch(self):
        with tempfile.TemporaryDirectory(prefix="local-start-test-") as tmp:
            root = Path(tmp)
            (root / "scripts").mkdir()
            (root / "bin").mkdir()
            (root / "web" / "dist").mkdir(parents=True)
            shutil.copy(Path(__file__).parents[1] / "scripts" / "local-start.sh", root / "scripts" / "local-start.sh")
            for name in ("context-web", "context-worker"):
                path = root / "bin" / name
                path.write_text("#!/bin/sh\nexit 99\n")
                path.chmod(0o755)
            proc = subprocess.run([str(root / "scripts" / "local-start.sh"), "fixture"],
                                  capture_output=True, text=True, timeout=2)
            self.assertEqual(proc.returncode, 1)
            self.assertIn("run make web first", proc.stderr)


if __name__ == "__main__":
    unittest.main()
