#!/usr/bin/env python3
"""Check OpenSpec scenario mappings and exact automated-test anchors.

A mapped test name only proves that an anchor exists. It does not prove the
assertions are sufficient, that the test ran, or that it was authored first.
"""
from __future__ import annotations
import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
MAP = ROOT / "openspec/test-map.json"
SCENARIO_RE = re.compile(r"^#### Scenario: ([A-Za-z0-9_.-]+)\s*$", re.MULTILINE)

def check(data: dict, root: Path) -> tuple[list[str], list[str]]:
    errors: list[str] = []
    pending: list[str] = []
    spec_ids: set[str] = set()
    spec_files = set((root / "openspec/changes").glob("*/specs/**/spec.md"))
    spec_files.update((root / "openspec/specs").glob("**/spec.md"))
    for spec in spec_files:
        spec_ids.update(SCENARIO_RE.findall(spec.read_text(encoding="utf-8")))
    mapped: dict[str, dict] = {}
    for item in data.get("scenarios", []):
        sid = item.get("id", "")
        if not sid:
            errors.append("scenario mapping is missing an ID")
            continue
        if sid in mapped:
            errors.append(f"duplicate scenario mapping: {sid}")
        mapped[sid] = item
        if item.get("status") == "pending":
            if item.get("test_file") or item.get("test_anchor"):
                errors.append(f"{sid}: pending mapping must not claim a test anchor")
            pending.append(sid)
            continue
        if item.get("status") != "covered":
            errors.append(f"{sid}: status must be 'covered' or 'pending'")
            continue
        for key in ("spec", "test_file", "test_anchor"):
            if not item.get(key):
                errors.append(f"{sid}: covered mapping missing {key}")
        spec_path, test_path = root / item.get("spec", ""), root / item.get("test_file", "")
        if not spec_path.is_file():
            errors.append(f"{sid}: spec file not found: {item.get('spec')}")
        elif sid not in SCENARIO_RE.findall(spec_path.read_text(encoding="utf-8")):
            errors.append(f"{sid}: scenario absent from mapped spec")
        if not test_path.is_file():
            errors.append(f"{sid}: test file not found: {item.get('test_file')}")
        else:
            source = test_path.read_text(encoding="utf-8")
            anchor = re.escape(item.get("test_anchor", ""))
            if not (re.search(rf"\bfunc\s+{anchor}\s*\(", source) or
                    re.search(rf"\bdef\s+{anchor}\s*\(", source)):
                errors.append(f"{sid}: exact test function not found: {item.get('test_anchor')}")
    for sid in sorted(spec_ids - mapped.keys()):
        errors.append(f"spec scenario has no mapping: {sid}")
    for sid in sorted(mapped.keys() - spec_ids):
        errors.append(f"mapping has no spec scenario: {sid}")
    return errors, pending

def main() -> int:
    data = json.loads(MAP.read_text(encoding="utf-8"))
    errors, pending = check(data, ROOT)
    if errors:
        print("Scenario/test map errors:", file=sys.stderr)
        print("\n".join(f"- {error}" for error in errors), file=sys.stderr)
        return 1
    print(f"Scenario map structure OK ({len(data.get('scenarios', []))} scenarios).")
    if pending:
        print("Explicitly pending automated checks: " + ", ".join(sorted(pending)))
    print("Mapped anchors prove only existence, not coverage quality, execution, or TDD.")
    return 0

if __name__ == "__main__":
    raise SystemExit(main())
