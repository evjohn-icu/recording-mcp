#!/usr/bin/env python3
"""Beta demo + release-material checks."""
from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
RUN_DEMO = os.environ.get("RUN_BETA_DEMO") == "1"


class BetaDemoTest(unittest.TestCase):
    @unittest.skipUnless(RUN_DEMO, "set RUN_BETA_DEMO=1 to run the full demo")
    def test_ten_step_demo(self) -> None:
        with tempfile.TemporaryDirectory(prefix="beta-demo-result-") as out:
            result = subprocess.run(
                [sys.executable, str(ROOT / "scripts/beta-demo.py"), "--skip-build", "--out", out],
                cwd=ROOT, capture_output=True, text=True, timeout=1800,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            report = json.loads((Path(out) / "result.json").read_text(encoding="utf-8"))
        self.assertEqual(report["total"], 10)
        self.assertEqual(report["passed"], 10, report["steps"])
        names = [item["step"] for item in report["steps"]]
        self.assertIn("1. 生成 fixture 资料库", names)
        self.assertIn("7. 人工确认最终候选", names)
        self.assertIn("9. MCP 读取同一决策", names)
        self.assertIn("10. 重启后决策仍在", names)
        self.assertIn("synthetic", report["fixture_disclosure"])
        self.assertFalse(report["password_printed"])

    def test_beta_checklist_artifacts(self) -> None:
        checklist = (ROOT / "docs/productization/beta-release-checklist.md").read_text(encoding="utf-8")
        report = (ROOT / "docs/productization/beta-release-report.md").read_text(encoding="utf-8")
        deployment = (ROOT / "docs/productization/deployment.md").read_text(encoding="utf-8")
        for step in range(1, 11):
            self.assertIn(f"| {step} |", checklist)
        for needle in ("SMART", "隐私", "DoD", "截图"):
            self.assertIn(needle, checklist)
        for section in ("A.", "B.", "C.", "D.", "E.", "F."):
            self.assertIn(section, report)
        self.assertIn("未推送", report)
        self.assertIn("docker compose", deployment)
        self.assertIn("备份", deployment)


if __name__ == "__main__":
    unittest.main()
