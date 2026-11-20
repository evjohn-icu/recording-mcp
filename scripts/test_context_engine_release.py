#!/usr/bin/env python3
"""Tests for scripts/check_context_engine_release.py.

Each case builds a throwaway release document and asserts the checker's verdict,
so the rules that can silently turn into a green light stay pinned.
"""
from __future__ import annotations

import hashlib
import importlib.util
import json
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("release_check", ROOT / "scripts/check_context_engine_release.py")
checker = importlib.util.module_from_spec(SPEC)
assert SPEC and SPEC.loader
SPEC.loader.exec_module(checker)


def sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


class Document:
    """A minimal valid release document that each test perturbs."""

    def __init__(self, root: Path):
        self.root = root
        self.source = root / "internal" / "app" / "service.go"
        self.source.parent.mkdir(parents=True, exist_ok=True)
        self.source.write_text("package app\n", encoding="utf-8")
        self.config = root / "Makefile"
        self.config.write_text("all:\n", encoding="utf-8")
        self.artifact = root / "docs" / "engineering" / "evidence.json"
        self.artifact.parent.mkdir(parents=True, exist_ok=True)
        self.artifact.write_text('{"ok": true}\n', encoding="utf-8")
        self.results = [
            {"id": "go.test", "status": "PASS", "evidence_kind": "current",
             "command": "go test ./internal/app", "exit_code": 0,
             "artifact": "docs/engineering/evidence.json", "artifact_sha256": sha(self.artifact)},
            {"id": "v3.calibration", "status": "FAIL", "evidence_kind": "current",
             "command": "runner --phase calibrate", "exit_code": 143,
             "reason": "p95 latency gate fails"},
            {"id": "audio.gold", "status": "NOT_RUN", "evidence_kind": "current",
             "reason": "operator has not returned the reviewed gold"},
            {"id": "speaker.identity", "status": "EXPERIMENTAL", "evidence_kind": "current",
             "reason": "CAM++ spans are not bound to diarization clusters"},
        ]
        self.decision = {
            "schema": checker.DECISION_SCHEMA,
            "merge_readiness": "READY",
            "release_readiness": "BLOCKED",
            "production_ready_recommendation": False,
            "scope": {"retrieval": "v1 + hash", "v3": "closed"},
            "blockers": [{"id": "human-gold", "detail": "human gold is pending",
                          "blocks_merge": False, "blocks_release": True}],
            "required_for_merge": ["go.test"],
            "required_for_release": ["go.test", "audio.gold"],
            "evidence_snapshot": {
                "generated_at": "2026-11-13T00:00:00Z",
                "candidate_head": "a" * 40,
                "uncommitted_diff_sha256": "b" * 64,
                "source_sha256": {"internal/app/service.go": sha(self.source)},
                "config_sha256": {"Makefile": sha(self.config)},
            },
        }

    def document(self) -> dict:
        return {"schema": checker.SCHEMA, "results": self.results, "decision": self.decision}

    def row(self, row_id: str) -> dict:
        return next(row for row in self.results if row["id"] == row_id)


class ReleaseCheckTests(unittest.TestCase):
    def check(self, document: Document) -> tuple[list[str], list[str]]:
        return checker.check(document.document(), document.root, document.root / "docs" / "engineering")

    def assert_error(self, document: Document, fragment: str) -> None:
        errors, _warnings = self.check(document)
        self.assertTrue(any(fragment in error for error in errors), f"{fragment!r} not in {errors}")

    def test_baseline_document_passes(self):
        with tempfile.TemporaryDirectory() as tmp:
            errors, _warnings = self.check(Document(Path(tmp)))
            self.assertEqual(errors, [])

    def test_pass_without_a_command_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = Document(Path(tmp))
            document.row("go.test").pop("command")
            self.assert_error(document, "needs the command")

    def test_pass_with_nonzero_exit_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = Document(Path(tmp))
            document.row("go.test")["exit_code"] = 1
            self.assert_error(document, "needs exit_code 0")

    def test_missing_artifact_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = Document(Path(tmp))
            document.artifact.unlink()
            self.assert_error(document, "artifact not found")

    def test_artifact_hash_mismatch_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = Document(Path(tmp))
            document.artifact.write_text('{"ok": false}\n', encoding="utf-8")
            self.assert_error(document, "artifact hash does not match")

    def test_incomplete_statuses_need_a_reason(self):
        for status in ("NOT_RUN", "BLOCKED", "EXPERIMENTAL"):
            with self.subTest(status), tempfile.TemporaryDirectory() as tmp:
                document = Document(Path(tmp))
                row = document.row("audio.gold")
                row["status"] = status
                row.pop("reason")
                self.assert_error(document, f"{status} needs a reason")

    def test_fail_evidence_cannot_support_merge_ready(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = Document(Path(tmp))
            document.decision["required_for_merge"] = ["v3.calibration"]
            self.assert_error(document, "merge READY requires PASS evidence")

    def test_interactive_pass_needs_an_artifact_instead_of_an_exit_code(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = Document(Path(tmp))
            row = document.row("go.test")
            row.update({"verification": "interactive", "exit_code": None, "artifact": None,
                        "artifact_sha256": None})
            self.assert_error(document, "interactive PASS evidence needs an artifact")
            row.update({"artifact": "docs/engineering/evidence.json",
                        "artifact_sha256": sha(document.artifact)})
            errors, _warnings = self.check(document)
            self.assertEqual(errors, [])

    def test_experimental_evidence_cannot_support_production(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = Document(Path(tmp))
            document.decision.update({
                "release_readiness": "READY",
                "production_ready_recommendation": True,
                "production_threshold": {"approved": True, "source": "signed-off"},
                "required_for_release": ["go.test", "speaker.identity"],
                "blockers": [{"id": "human-gold", "detail": "resolved", "blocks_merge": False,
                              "blocks_release": False}],
            })
            self.assert_error(document, "EXPERIMENTAL evidence cannot support production readiness")

    def test_production_needs_a_pre_approved_threshold(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = Document(Path(tmp))
            document.decision.update({
                "release_readiness": "READY",
                "production_ready_recommendation": True,
                "required_for_release": ["go.test"],
                "blockers": [{"id": "human-gold", "detail": "resolved", "blocks_merge": False,
                              "blocks_release": False}],
            })
            self.assert_error(document, "pre-approved production_threshold")

    def test_historical_evidence_cannot_justify_merge(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = Document(Path(tmp))
            document.row("go.test")["evidence_kind"] = "historical"
            self.assert_error(document, "requires current evidence")

    def test_snapshot_hash_mismatch_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = Document(Path(tmp))
            document.source.write_text("package app\n// drift\n", encoding="utf-8")
            self.assert_error(document, "hash does not match internal/app/service.go")

    def test_snapshot_must_pin_a_head_and_diff(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = Document(Path(tmp))
            document.decision["evidence_snapshot"]["candidate_head"] = "not-a-sha"
            self.assert_error(document, "candidate_head")
            document.decision["evidence_snapshot"]["candidate_head"] = "a" * 40
            document.decision["evidence_snapshot"]["uncommitted_diff_sha256"] = "zz"
            self.assert_error(document, "uncommitted_diff_sha256")

    def test_v3_failure_does_not_block_v1_merge_but_blocks_release(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = Document(Path(tmp))
            # v3 is recorded FAIL but excluded from the v1 scope evidence.
            errors, warnings = self.check(document)
            self.assertEqual(errors, [])
            self.assertEqual(document.decision["merge_readiness"], "READY")
            self.assertEqual(document.decision["release_readiness"], "BLOCKED")
            self.assertFalse(document.decision["production_ready_recommendation"])

    def test_blocked_release_needs_a_blocking_blocker(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = Document(Path(tmp))
            document.decision["blockers"][0]["blocks_release"] = False
            self.assert_error(document, "BLOCKED needs at least one blocker")

    def test_merge_ready_contradicting_a_merge_blocker_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = Document(Path(tmp))
            document.decision["blockers"].append(
                {"id": "isolation", "detail": "known isolation failure", "blocks_merge": True,
                 "blocks_release": True})
            self.assert_error(document, "merge READY contradicts blocker isolation")

    def test_unknown_required_evidence_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = Document(Path(tmp))
            document.decision["required_for_merge"].append("ghost")
            self.assert_error(document, "required evidence id does not exist")

    def test_duplicate_result_ids_are_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = Document(Path(tmp))
            document.results.append(dict(document.row("go.test")))
            self.assert_error(document, "duplicate result id")

    def test_wrong_document_schema_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = Document(Path(tmp))
            payload = document.document()
            payload["schema"] = "something-else"
            errors, _warnings = checker.check(payload, document.root)
            self.assertTrue(any("schema must be" in error for error in errors))


def pad_thresholds() -> dict:
    return {"schema": checker.THRESHOLDS_SCHEMA, "approved": True,
            "approved_at": "2026-11-19T13:55:00+08:00", "approved_by": "operator",
            "categories": list(checker.CATEGORIES),
            "metrics": {"cer_max": 0.15, "wer_max": 0.30, "der_max": 0.20,
                        "event_precision_min": 0.80, "event_recall_min": 0.80, "eer_max": 0.05,
                        "evidence_accuracy_min": 0.95, "semantic_accuracy_min": 0.95,
                        "decision_precision_min": 0.95}}


def boundary_categories() -> dict:
    return {category: {
        "cer": {"errors": 15, "reference": 100},
        "wer": {"errors": 30, "reference": 100},
        "der": {"false_alarm_ms": 100.0, "missed_ms": 60.0, "confusion_ms": 40.0,
                "reference_speech_ms": 1000.0},
        "events": {"tp": 8, "fp": 2, "fn": 2},
        "web": {"supported_refs": 19, "returned_refs": 20,
                "semantic_accepted": 19, "semantic_adjudicated": 20},
        "mcp": {"supported_refs": 19, "returned_refs": 20,
                "semantic_accepted": 19, "semantic_adjudicated": 20},
        "decision": {"accepted": 19, "total": 20, "adjudicated": 20},
        "eer": {"value": 0.05, "same_trials": 30, "different_trials": 30, "experimental": False},
    } for category in checker.CATEGORIES}


class GateDocument(Document):
    """A release document whose audio gate is internally consistent."""

    def __init__(self, root: Path):
        super().__init__(root)
        self.gate_id = "audio.production-gate"
        self.thresholds = pad_thresholds()
        self.thresholds_path = root / "docs" / "engineering" / "thresholds.json"
        self.thresholds_path.write_text(json.dumps(self.thresholds), encoding="utf-8")
        self.score_path = root / "docs" / "engineering" / "audio-gate.json"
        self.gate = {
            "schema": checker.GATE_SCHEMA,
            "status": "PASS",
            "thresholds_sha256": checker.canonical_digest(self.thresholds),
            "manifest_sha256": "1" * 64, "predictions_sha256": "2" * 64,
            "adjudications_sha256": "3" * 64,
            "thresholds": dict(self.thresholds["metrics"]),
            "categories": boundary_categories(),
            "failures": [], "blocked": [],
        }
        self.write_score()
        self.results = [
            {"id": "go.test", "status": "PASS", "evidence_kind": "current",
             "command": "go test ./internal/app", "exit_code": 0,
             "artifact": "docs/engineering/evidence.json", "artifact_sha256": sha(self.artifact)},
            {"id": self.gate_id, "status": "PASS", "evidence_kind": "current",
             "command": "review-audio-acceptance.py score --production-thresholds ...", "exit_code": 0,
             "artifact": "docs/engineering/audio-gate.json", "artifact_sha256": sha(self.score_path)},
            {"id": "v3.calibration", "status": "PASS", "evidence_kind": "current",
             "command": "run-retrieval-quality-v2.py --phase calibrate", "exit_code": 0},
            {"id": "v3.holdout", "status": "PASS", "evidence_kind": "current",
             "command": "run-retrieval-quality-v2.py --phase holdout", "exit_code": 0},
        ]
        self.decision.update({
            "release_readiness": "READY",
            "production_ready_recommendation": True,
            "required_for_release": ["go.test", self.gate_id, "v3.calibration", "v3.holdout"],
            "blockers": [],
            "audio_quality_evidence_id": self.gate_id,
            "production_threshold": {"approved": True, "source": "operator",
                                     "artifact": "docs/engineering/thresholds.json",
                                     "artifact_sha256": sha(self.thresholds_path)},
        })

    def write_score(self) -> None:
        self.score_path.write_text(json.dumps({"schema": "context-engine-acceptance-score-v1",
                                               "production_gate": self.gate}), encoding="utf-8")

    def refresh(self) -> None:
        self.write_score()
        next(row for row in self.results if row["id"] == self.gate_id)["artifact_sha256"] = sha(self.score_path)


class ProductionGateCheckTests(unittest.TestCase):
    def check(self, document: GateDocument) -> tuple[list[str], list[str]]:
        return checker.check(document.document(), document.root, document.root / "docs" / "engineering")

    def assert_error(self, document: GateDocument, fragment: str) -> None:
        errors, _warnings = self.check(document)
        self.assertTrue(any(fragment in error for error in errors), f"{fragment!r} not in {errors}")

    def test_a_consistent_release_document_passes(self):
        with tempfile.TemporaryDirectory() as tmp:
            errors, _warnings = self.check(GateDocument(Path(tmp)))
            self.assertEqual(errors, [])

    def test_release_ready_needs_a_required_list(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = GateDocument(Path(tmp))
            document.decision["required_for_release"] = []
            self.assert_error(document, "needs at least one required evidence row")

    def test_release_ready_needs_current_pass_rows(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = GateDocument(Path(tmp))
            document.row("v3.holdout")["status"] = "BLOCKED"
            document.row("v3.holdout")["reason"] = "not run"
            self.assert_error(document, "release READY requires PASS evidence")
        with tempfile.TemporaryDirectory() as tmp:
            document = GateDocument(Path(tmp))
            document.row("v3.calibration")["evidence_kind"] = "historical"
            self.assert_error(document, "release READY requires current evidence")

    def test_release_ready_contradicts_a_release_blocker(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = GateDocument(Path(tmp))
            document.decision["blockers"] = [{"id": "human-gold", "detail": "pending",
                                              "blocks_merge": False, "blocks_release": True}]
            self.assert_error(document, "release READY contradicts blocker")

    def test_release_ready_requires_the_production_anchors(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = GateDocument(Path(tmp))
            document.decision["required_for_release"] = ["go.test", document.gate_id, "v3.calibration"]
            self.assert_error(document, "requires the current production evidence")

    def test_a_hand_written_gate_pass_is_recomputed(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = GateDocument(Path(tmp))
            document.gate["categories"]["quiet_two_person"]["cer"] = {"errors": 50, "reference": 100}
            document.gate["failures"] = []
            document.refresh()
            self.assert_error(document, "cer 0.5000 exceeds 0.15")

    def test_a_threshold_digest_mismatch_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = GateDocument(Path(tmp))
            document.gate["thresholds_sha256"] = "f" * 64
            document.refresh()
            self.assert_error(document, "thresholds_sha256 does not match")

    def test_negative_or_non_integer_counts_are_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = GateDocument(Path(tmp))
            document.gate["categories"]["quiet_two_person"]["events"]["tp"] = -1
            document.refresh()
            self.assert_error(document, "must be a finite non-negative")
        with tempfile.TemporaryDirectory() as tmp:
            document = GateDocument(Path(tmp))
            document.gate["categories"]["quiet_two_person"]["events"]["fp"] = 1.5
            document.refresh()
            self.assert_error(document, "must be an integer count")

    def test_a_mismatched_adjudication_denominator_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = GateDocument(Path(tmp))
            document.gate["categories"]["quiet_two_person"]["decision"]["adjudicated"] = 19
            document.refresh()
            self.assert_error(document, "every decision must be adjudicated")
        with tempfile.TemporaryDirectory() as tmp:
            document = GateDocument(Path(tmp))
            document.gate["categories"]["quiet_two_person"]["web"]["semantic_adjudicated"] = 18
            document.refresh()
            self.assert_error(document, "every returned ref must be adjudicated")

    def test_an_experimental_eer_cannot_pass(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = GateDocument(Path(tmp))
            document.gate["categories"]["quiet_two_person"]["eer"]["experimental"] = True
            document.refresh()
            self.assert_error(document, "experimental")

    def test_a_missing_gate_artifact_or_threshold_hash_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = GateDocument(Path(tmp))
            document.score_path.write_text('{"schema": "context-engine-acceptance-score-v1"}',
                                           encoding="utf-8")
            next(row for row in document.results
                 if row["id"] == document.gate_id)["artifact_sha256"] = sha(document.score_path)
            self.assert_error(document, "has no production_gate")
        with tempfile.TemporaryDirectory() as tmp:
            document = GateDocument(Path(tmp))
            document.decision["production_threshold"]["artifact_sha256"] = "0" * 64
            self.assert_error(document, "artifact hash does not match")

    def test_a_missing_gate_pointer_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = GateDocument(Path(tmp))
            document.decision.pop("audio_quality_evidence_id")
            self.assert_error(document, "audio_quality_evidence_id")
        with tempfile.TemporaryDirectory() as tmp:
            document = GateDocument(Path(tmp))
            document.decision["required_for_release"].remove("go.test")
            document.decision["audio_quality_evidence_id"] = "go.test"
            self.assert_error(document, "must be a required release row")

    def test_a_gate_artifact_without_a_score_shape_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            document = GateDocument(Path(tmp))
            document.gate["status"] = "BLOCKED"
            document.refresh()
            self.assert_error(document, "audio quality gate is BLOCKED")


if __name__ == "__main__":
    unittest.main()
