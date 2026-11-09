import os
import shutil
import subprocess
import tempfile
import textwrap
import time
import unittest
from pathlib import Path


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
            programs = {
                "context-worker": f"""#!/usr/bin/env python3
import os,signal,sys,time
open(os.environ['ARGS_LOG'],'a').write('worker '+ ' '.join(sys.argv[1:])+'\\n')
signal.signal(signal.SIGTERM, lambda *_: (open(os.environ['STOP_MARKER'],'w').write('stopped'), sys.exit(0)))
if os.environ['FAIL_CHILD']=='worker':
 time.sleep(.3); sys.exit(7)
while True: time.sleep(1)
""",
                "context-web": f"""#!/usr/bin/env python3
import os,signal,sys,time
open(os.environ['ARGS_LOG'],'a').write('web '+ ' '.join(sys.argv[1:])+'\\n')
signal.signal(signal.SIGTERM, lambda *_: (open(os.environ['STOP_MARKER'],'w').write('stopped'), sys.exit(0)))
if os.environ['FAIL_CHILD']=='web': sys.exit(9)
while True: time.sleep(1)
""",
            }
            for name, body in programs.items():
                path = root / "bin" / name
                path.write_text(textwrap.dedent(body))
                path.chmod(0o755)
            env = {k:v for k,v in os.environ.items() if not k.startswith(("GENERATOR_", "WORLDLINE_", "BRIEF_POLISH_", "ASR_"))}
            env.update(ARGS_LOG=str(log), STOP_MARKER=str(stopped), FAIL_CHILD=failing)
            if local_asr:
                env.update(ASR_PROVIDER="ark", ASR_BACKEND="ark", ASR_MODEL="/models/ark", ASR_PYTHON="/opt/asr/bin/python", ASR_DEVICE="cuda:1", ASR_REVISION="deadbeef")
            if configured:
                env.update(BRIEF_POLISH_PI_PROVIDER="fixture-provider", BRIEF_POLISH_MODEL="fixture-model", BRIEF_POLISH_PI_MODELS_FILE="/tmp/synthetic models.json", GENERATOR_PROVIDER="openai", GENERATOR_ENDPOINT="http://127.0.0.1:18080/v1/chat/completions", GENERATOR_MODEL="synthetic-config-only", WORLDLINE_DECISION_URL="http://127.0.0.1:18799/v1/systemone")
            proc = subprocess.run([str(root / "scripts" / "local-start.sh"), "fixture", "28771", str(root / "profiles with spaces")], env=env, capture_output=True, text=True, timeout=5)
            self.assertEqual(proc.returncode, 7 if failing == "worker" else 9, proc.stderr)
            self.assertTrue(stopped.exists(), "the surviving child was not stopped")
            args = log.read_text()
            self.assertIn("--profile fixture", args)
            self.assertIn("--profiles-root " + str(root / "profiles with spaces"), args)
            if configured:
                self.assertIn("--brief-polish-pi-provider fixture-provider", args)
                self.assertIn("--brief-polish-model fixture-model", args)
                self.assertIn("--brief-polish-pi-models-file /tmp/synthetic models.json", args)
                self.assertNotIn("--brief-polish", next(line for line in args.splitlines() if line.startswith("worker ")))
                self.assertIn("--generator openai", args)
                self.assertIn("--generator-url http://127.0.0.1:18080/v1/chat/completions", args)
                self.assertIn("--generator-model synthetic-config-only", args)
                self.assertEqual(args.count("--worldline-decision-url http://127.0.0.1:18799/v1/systemone"), 2)
            else:
                self.assertIn("--generator off --embed-provider hash", args)
                self.assertNotIn("--brief-polish", args)
            if local_asr:
                for flag in ("--asr-backend ark", "--asr-model /models/ark", "--asr-python /opt/asr/bin/python", "--asr-device cuda:1", "--asr-revision deadbeef"):
                    self.assertEqual(args.count(flag), 2, flag)

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
            (root / "scripts").mkdir(); (root / "bin").mkdir(); (root / "web" / "dist").mkdir(parents=True)
            shutil.copy(Path(__file__).parents[1] / "scripts" / "local-start.sh", root / "scripts" / "local-start.sh")
            for name in ("context-web", "context-worker"):
                p = root / "bin" / name; p.write_text("#!/bin/sh\nexit 99\n"); p.chmod(0o755)
            proc = subprocess.run([str(root / "scripts" / "local-start.sh"), "fixture"], capture_output=True, text=True, timeout=2)
            self.assertEqual(proc.returncode, 1)
            self.assertIn("run make web first", proc.stderr)


if __name__ == "__main__":
    unittest.main()
