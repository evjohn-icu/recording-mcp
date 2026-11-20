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
from argparse import Namespace
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("release_check", ROOT / "scripts/check_context_engine_release.py")
checker = importlib.util.module_from_spec(SPEC)
assert SPEC and SPEC.loader
SPEC.loader.exec_module(checker)
RUNNER_SPEC = importlib.util.spec_from_file_location(
    "retrieval_quality_v2", ROOT / "scripts/run-retrieval-quality-v2.py")
runner = importlib.util.module_from_spec(RUNNER_SPEC)
assert RUNNER_SPEC and RUNNER_SPEC.loader
RUNNER_SPEC.loader.exec_module(runner)


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


V3_LOCK_SCHEMA = checker.V3_LOCK_SCHEMA
V3_ELIGIBLE_IDS = ("q-v3-01", "q-v3-02", "q-v3-03", "q-v3-04")


def v3_run_metrics(*, recall: float, mrr: float, p95_ms: float,
                   eligible_ids=V3_ELIGIBLE_IDS, **overrides) -> dict:
    """A metrics object shaped like the runner's ``extract_run_metrics`` output.

    The defaults pass the shared ``quality_gate_v3`` against the matching
    baseline produced by this builder; ``overrides`` drive the negative cases.
    ``recall_eligible_ids`` is the lifecycle eligibility list that plan 7.1
    requires on every v3-protocol run, and both sides of a comparison must pin
    the same sorted, non-empty list.
    """
    metrics = {
        "split": "test", "mode": "hybrid",
        "recall_at_5": recall, "mrr_at_10": mrr,
        "false_positive_rate": 0.0, "false_positive_queries": 0,
        "no_evidence_denominator": 20, "answerable_denominator": 60,
        "recall_denominator": 60, "mrr_denominator": 60,
        "p95_denominator": 80, "p95_ms": p95_ms,
        "stale_leaks": 0, "ambient_leaks": 0, "scope_leaks": 0,
        "returned_items_denominator": 480, "invalid_refs": 0,
        "fixture_cross_split_leaks": 0, "provider": "openai",
        "rerank_configured": True, "assessor_configured": True,
        "false_sufficient": 0, "false_sufficient_denominator": 8,
        "false_sufficient_rate": 0.0,
        "sufficiency": {"status_counts": {"sufficient": 68, "insufficient": 8, "unknown": 0},
                        "scored": 76, "matches": 74},
        "embedding_invocations": 140, "model_calls": 260, "rerank_calls": 140, "assessor_calls": 260,
        "failure_counts": {"embedding": 0, "rerank": 0, "assessment": 0},
        "bucket_metrics": {}, "forgotten_leakage": 0, "admission_failures": 0,
        "none_false_positive_rate": 0.0, "all_unanswerable_false_positive_rate": 0.0,
        "stage_latencies": {},
        "recall_eligible_ids": sorted(eligible_ids),
    }
    metrics.update(overrides)
    return metrics


class V3Evidence:
    """A calibration selection lock plus the holdout manifest that consumed it.

    Every pin (fixture, configuration, binary, identity, selected candidate,
    lock digest) and every stored gate agree with what the shared
    ``quality_gate_v3`` and the lock's own digests recompute, so a document
    carrying them is structurally consistent. Tests mutate single fields to
    prove the checker recomputes instead of trusting the stored verdicts.
    """

    LOCK_ARTIFACT = "docs/engineering/v3-selection-lock.json"
    MANIFEST_ARTIFACT = "docs/engineering/v3-holdout-manifest.json"

    def __init__(self, root: Path):
        self.root = root
        self.lock_path = root / V3Evidence.LOCK_ARTIFACT
        self.manifest_path = root / V3Evidence.MANIFEST_ARTIFACT
        self.refresh_rows = lambda: None
        self.binary_sha256 = hashlib.sha256(b"contextd v0.4 release binary").hexdigest()
        self.fixture_sha256 = {name: hashlib.sha256(f"frozen {name}".encode()).hexdigest()
                               for name in ("corpus.json", "retrieval.json", "lifecycle.json")}
        self.eligible_ids = list(V3_ELIGIBLE_IDS)
        self.configuration = {
            "protocol": "v3",
            "embedding_endpoint": "https://embeddings.example/v1",
            "embedding_model": "text-embedding-3-small",
            "embedding_dims": 1536, "embedding_input_type": False,
            "embedding_key_env": "V3_EMBED_KEY", "embedding_revision": "rev-2026-01-30",
            "rerank_endpoint": "https://rerank.example/v1", "rerank_model": "bge-reranker-v2-m3",
            "assessor_endpoint": "https://assessor.example/v1", "assessor_model": "assessor-2026-02-11",
            "admission_batch_questions": 8, "max_items": 10, "max_bytes": 65536,
        }
        self.selected = {"version": "v3", "backend": "openai", "threshold_hundredths": 100,
                         "threshold": 1.0, "rerank": True}
        self.identity = {
            "embedding_provider": "openai-compat",
            "embedding_model": self.configuration["embedding_model"],
            "embedding_revision": self.configuration["embedding_revision"],
            "embedding_dims": self.configuration["embedding_dims"],
            "embedding_input_type": False,
            "assessor_model": self.configuration["assessor_model"],
            "assessor_actual_model": "assessor-2026-02-11",
            "binary_sha256": self.binary_sha256,
            "prompt_identity": runner.PROMPT_IDENTITY,
            "prompt_note": ("the admission prompt lives in the pinned binary; the policy template "
                            "hash pins the rubric and threshold document"),
        }
        self.calibration_metrics = v3_run_metrics(recall=0.93, mrr=0.72, p95_ms=7600.0)
        self.calibration_baseline_metrics = v3_run_metrics(recall=0.62, mrr=0.55, p95_ms=10000.0)
        self.holdout_v3_metrics = v3_run_metrics(recall=0.88, mrr=0.68, p95_ms=8200.0)
        self.holdout_v1_metrics = v3_run_metrics(recall=0.58, mrr=0.52, p95_ms=9800.0)
        self.lock = {
            "schema": V3_LOCK_SCHEMA,
            "created_utc": "2026-11-20T08:00:00Z",
            "fixture_sha256": self.fixture_sha256,
            "configuration": self.configuration,
            "configuration_sha256": "",
            "policy_template_sha256": "",
            "identity": self.identity,
            "budget": dict(runner.LOCK_BUDGET),
            "warmup": dict(runner.WARMUP),
            "binary_sha256": self.binary_sha256,
            "selected": self.selected,
            "selected_sha256": "",
            "selected_policy_sha256": "",
            "calibration_metrics": self.calibration_metrics,
            "calibration_baseline_metrics": self.calibration_baseline_metrics,
            "calibration_gate": runner.quality_gate_v3(self.calibration_metrics,
                                                       self.calibration_baseline_metrics),
            "selection_basis": ("calibration only; recall@5 desc, mrr@10 desc, p95 asc, stricter "
                                "threshold first; final gate from one uncached rerun"),
            "cache_used_for_screening": True,
            "final_rerun_uncached": True,
            "holdout_started": True,
            "holdout_started_utc": "2026-11-20T09:00:00Z",
        }
        holdout_gate = runner.quality_gate_v3(self.holdout_v3_metrics, self.holdout_v1_metrics)
        self.manifest = {
            "protocol": "v3", "phase": "holdout",
            "fixture_sha256": dict(self.fixture_sha256),
            "fixture": {"questions": 780, "groups": 60, "lifecycle_sources": 312},
            "protocol_config": {"max_items": runner.LOCK_BUDGET["max_items"],
                                "max_bytes": runner.LOCK_BUDGET["max_bytes"],
                                "test_touched": True,
                                "threshold_grid": [0, 100],
                                "latency_comparison": runner.V3_LATENCY_COMPARISON,
                                "lifecycle_sources": 312,
                                "warmup": dict(runner.WARMUP),
                                "cache_flush": "https://embeddings.example/v1/cache/flush",
                                "configuration": self.configuration, "configuration_sha256": ""},
            "provider_status": {"openai_embedding": "CONFIGURED", "reranker": "CONFIGURED",
                                "assessor": "CONFIGURED"},
            "identity": dict(self.identity),
            "binary_sha256": self.binary_sha256,
            "runs": {
                "v1_test": {"status": "PASS", "exit_code": 0, "metrics": self.holdout_v1_metrics},
                "v3_test": {"status": "PASS", "exit_code": 0, "metrics": self.holdout_v3_metrics},
            },
            "warmup_runs": [{"version": "v1", "backend": "openai", "status": "PASS",
                             "cache_flushed": True, "questions": 1},
                            {"version": "v3", "backend": "openai", "status": "PASS",
                             "cache_flushed": True, "questions": 1}],
            "selected_candidate": dict(self.selected),
            "holdout_gate": holdout_gate,
            "quality_gate_passed": True,
            "selection_lock_sha256": "",
        }
        self.sync()

    def policy_document(self) -> dict:
        """The phase='selected' admission policy the lock's digest must pin."""
        args = Namespace(embed_model=self.configuration["embedding_model"],
                         embed_revision=self.configuration["embedding_revision"],
                         assessor_model=self.configuration["assessor_model"])
        return runner.admission_policy_document(args, self.selected["backend"],
                                                self.selected["threshold_hundredths"],
                                                self.fixture_sha256["retrieval.json"], "selected")

    def resign_selected(self) -> None:
        """Re-sign only the selected payload, for isolated type mutations."""
        self.lock["selected_sha256"] = runner.config_signature(self.lock["selected"])

    def sync(self, *, resign: bool = True, relink: bool = True) -> None:
        """Re-derive digests, rewrite both artifacts and refresh the row pins.

        ``resign=False`` keeps tampered digests; ``relink=False`` keeps a
        tampered ``selection_lock_sha256`` pointer.
        """
        if resign:
            self.lock["configuration_sha256"] = runner.config_signature(self.lock["configuration"])
            self.lock["selected_sha256"] = runner.config_signature(self.lock["selected"])
            template = dict(self.policy_document())
            template.pop("threshold", None)
            template.pop("phase", None)
            self.lock["policy_template_sha256"] = runner.config_signature(template)
            self.lock["selected_policy_sha256"] = runner.config_signature(self.policy_document())
            self.manifest["protocol_config"]["configuration_sha256"] = self.lock["configuration_sha256"]
        self.lock_path.write_text(json.dumps(self.lock, indent=2) + "\n", encoding="utf-8")
        if relink:
            self.manifest["selection_lock_sha256"] = sha(self.lock_path)
        self.manifest_path.write_text(json.dumps(self.manifest, indent=2) + "\n", encoding="utf-8")
        self.refresh_rows()


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
        self.v3 = V3Evidence(root)
        self.v3.refresh_rows = self.refresh_v3_rows
        self.results = [
            {"id": "go.test", "status": "PASS", "evidence_kind": "current",
             "command": "go test ./internal/app", "exit_code": 0,
             "artifact": "docs/engineering/evidence.json", "artifact_sha256": sha(self.artifact)},
            {"id": self.gate_id, "status": "PASS", "evidence_kind": "current",
             "command": "review-audio-acceptance.py score --production-thresholds ...", "exit_code": 0,
             "artifact": "docs/engineering/audio-gate.json", "artifact_sha256": sha(self.score_path)},
            {"id": "v3.calibration", "status": "PASS", "evidence_kind": "current",
             "command": "run-retrieval-quality-v2.py --phase calibrate", "exit_code": 0,
             "artifact": V3Evidence.LOCK_ARTIFACT, "artifact_sha256": sha(self.v3.lock_path)},
            {"id": "v3.holdout", "status": "PASS", "evidence_kind": "current",
             "command": "run-retrieval-quality-v2.py --phase holdout", "exit_code": 0,
             "artifact": V3Evidence.MANIFEST_ARTIFACT, "artifact_sha256": sha(self.v3.manifest_path)},
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

    def refresh_v3_rows(self) -> None:
        """Re-pin the v3 rows after their artifacts were rewritten."""
        for row in self.results:
            if row["id"] == "v3.calibration":
                row["artifact_sha256"] = sha(self.v3.lock_path)
            elif row["id"] == "v3.holdout":
                row["artifact_sha256"] = sha(self.v3.manifest_path)

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


class V3ReleaseEvidenceTests(unittest.TestCase):
    """Plan 7.5: the checker verifies the v3 selection lock and the holdout
    manifest with the shared quality gate instead of trusting the rows.

    ``GateDocument`` now carries a matched lock/holdout pair whose pins, digests
    and metrics all agree; every negative case below breaks exactly one thing
    and expects a descriptive error naming it, with no unhandled exception.
    """

    def verdict(self, mutate=None):
        with tempfile.TemporaryDirectory() as tmp:
            document = GateDocument(Path(tmp))
            if mutate is not None:
                mutate(document)
            return checker.check(document.document(), document.root, document.root / "docs" / "engineering")

    def assert_rejected(self, verdict, fragment: str) -> None:
        errors, _warnings = verdict
        self.assertTrue(any(fragment in error for error in errors), f"{fragment!r} not in {errors}")

    def strip_row_artifact(self, row_id: str):
        def mutate(document: GateDocument) -> None:
            row = document.row(row_id)
            row.pop("artifact", None)
            row.pop("artifact_sha256", None)
        return mutate

    def test_fixture_lock_and_holdout_really_pass_the_shared_v3_gate(self):
        with tempfile.TemporaryDirectory() as tmp:
            v3 = GateDocument(Path(tmp)).v3
            self.assertTrue(runner.quality_gate_v3(v3.lock["calibration_metrics"],
                                                  v3.lock["calibration_baseline_metrics"])["passed"])
            self.assertTrue(runner.quality_gate_v3(v3.manifest["runs"]["v3_test"]["metrics"],
                                                  v3.manifest["runs"]["v1_test"]["metrics"])["passed"])
            self.assertTrue(v3.lock["calibration_gate"]["passed"])
            self.assertTrue(v3.manifest["holdout_gate"]["passed"])
            self.assertEqual(v3.manifest["selection_lock_sha256"], sha(v3.lock_path))

    def test_a_matched_lock_and_holdout_release_document_passes(self):
        errors, _warnings = self.verdict()
        self.assertEqual(errors, [])

    def test_a_missing_selection_lock_artifact_is_rejected(self):
        errors, _warnings = self.verdict(self.strip_row_artifact("v3.calibration"))
        self.assertTrue(any("v3.calibration" in error and "artifact" in error for error in errors),
                        errors)

    def test_a_missing_holdout_manifest_artifact_is_rejected(self):
        errors, _warnings = self.verdict(self.strip_row_artifact("v3.holdout"))
        self.assertTrue(any("v3.holdout" in error and "artifact" in error for error in errors),
                        errors)

    def test_deleted_lock_or_manifest_files_are_rejected(self):
        for path_name in ("lock_path", "manifest_path"):
            with self.subTest(artifact=path_name):
                with tempfile.TemporaryDirectory() as tmp:
                    document = GateDocument(Path(tmp))
                    getattr(document.v3, path_name).unlink()
                    errors, _warnings = checker.check(document.document(), document.root,
                                                      document.root / "docs" / "engineering")
                    self.assertTrue(any("not found" in error for error in errors), errors)

    def test_calibration_must_point_at_the_lock_and_holdout_at_the_manifest(self):
        def mutate(document: GateDocument) -> None:
            document.row("v3.calibration")["artifact"] = V3Evidence.MANIFEST_ARTIFACT
            document.row("v3.calibration")["artifact_sha256"] = sha(document.v3.manifest_path)
        errors, _warnings = self.verdict(mutate)
        self.assertTrue(any("v3.calibration" in error for error in errors), errors)

    def test_a_structurally_consistent_holdout_with_failing_metrics_is_rejected(self):
        def mutate(document: GateDocument) -> None:
            # The stored flag keeps claiming PASS; the checker must recompute.
            document.v3.manifest["runs"]["v3_test"]["metrics"]["recall_at_5"] = 0.84
            document.v3.manifest["quality_gate_passed"] = True
            document.v3.sync(resign=False)
        self.assert_rejected(self.verdict(mutate), "recall_at_5")

    def test_failing_calibration_metrics_are_rejected(self):
        def mutate(document: GateDocument) -> None:
            document.v3.lock["calibration_metrics"]["admission_failures"] = 1
            document.v3.lock["calibration_gate"]["passed"] = True
            document.v3.sync(resign=False)
        self.assert_rejected(self.verdict(mutate), "admission_failures")

    def test_a_lifecycle_id_list_that_only_one_side_applied_is_rejected(self):
        def mutate(document: GateDocument) -> None:
            document.v3.manifest["runs"]["v1_test"]["metrics"]["recall_eligible_ids"] = \
                sorted(V3_ELIGIBLE_IDS) + ["q-v3-05"]
            document.v3.sync(resign=False)
        self.assert_rejected(self.verdict(mutate), "recall_eligible_ids")

    def test_an_empty_lifecycle_id_list_is_rejected(self):
        def mutate(document: GateDocument) -> None:
            document.v3.lock["calibration_metrics"]["recall_eligible_ids"] = []
            document.v3.lock["calibration_baseline_metrics"]["recall_eligible_ids"] = []
            document.v3.sync(resign=False)
        self.assert_rejected(self.verdict(mutate), "recall_eligible_ids")

    def test_a_holdout_pinned_to_a_different_lock_is_rejected(self):
        def mutate(document: GateDocument) -> None:
            other = json.loads(json.dumps(document.v3.lock))
            other["selected"] = dict(other["selected"], threshold_hundredths=0, threshold=0.0)
            other_path = document.root / "docs" / "engineering" / "other-lock.json"
            other_path.write_text(json.dumps(other, indent=2) + "\n", encoding="utf-8")
            document.v3.manifest["selection_lock_sha256"] = sha(other_path)
            document.v3.sync(resign=False, relink=False)
        self.assert_rejected(self.verdict(mutate), "selection_lock_sha256")

    def test_holdout_pins_must_match_the_lock(self):
        cases = {
            "binary_sha256": lambda doc: doc.v3.manifest.__setitem__("binary_sha256", "d" * 64),
            "fixture_sha256":
                lambda doc: doc.v3.manifest["fixture_sha256"].__setitem__("lifecycle.json", "e" * 64),
            "configuration":
                lambda doc: doc.v3.manifest["protocol_config"]["configuration"]
                .__setitem__("admission_batch_questions", 4),
            "identity": lambda doc: doc.v3.manifest["identity"].__setitem__("assessor_model", "other"),
            "selected_candidate":
                lambda doc: doc.v3.manifest["selected_candidate"].__setitem__("threshold_hundredths", 0),
        }
        for fragment, tamper in cases.items():
            with self.subTest(pin=fragment):
                def mutate(document: GateDocument, tamper=tamper) -> None:
                    tamper(document)
                    document.v3.sync(resign=False)
                self.assert_rejected(self.verdict(mutate), fragment)

    def test_wrong_field_types_are_rejected_without_raising(self):
        def threshold_string(document: GateDocument) -> None:
            document.v3.lock["selected"]["threshold_hundredths"] = "100"
            document.v3.resign_selected()
            document.v3.sync(resign=False)

        def threshold_fraction(document: GateDocument) -> None:
            document.v3.lock["selected"]["threshold"] = 0.5
            document.v3.sync()

        def touched(document: GateDocument) -> None:
            document.v3.manifest["protocol_config"]["test_touched"] = "true"
            document.v3.sync(resign=False)

        def gate_flag(document: GateDocument) -> None:
            document.v3.manifest["quality_gate_passed"] = "yes"
            document.v3.sync(resign=False)

        def run_status(document: GateDocument) -> None:
            document.v3.manifest["runs"]["v3_test"]["status"] = 0
            document.v3.sync(resign=False)

        def metrics_type(document: GateDocument) -> None:
            document.v3.lock["calibration_metrics"] = []
            document.v3.sync(resign=False)

        for fragment, mutate in (("threshold_hundredths", threshold_string),
                                 ("threshold", threshold_fraction), ("test_touched", touched),
                                 ("quality_gate_passed", gate_flag), ("status", run_status),
                                 ("calibration_metrics", metrics_type)):
            with self.subTest(field=fragment):
                errors, _warnings = self.verdict(mutate)
                self.assertTrue(any(fragment in error for error in errors), (fragment, errors))

    def test_a_lock_that_the_holdout_never_consumed_is_rejected(self):
        def mutate(document: GateDocument) -> None:
            document.v3.lock["holdout_started"] = False
            document.v3.lock.pop("holdout_started_utc", None)
            document.v3.sync(resign=False)
        self.assert_rejected(self.verdict(mutate), "holdout")

    def test_an_old_selection_lock_protocol_is_rejected(self):
        def mutate(document: GateDocument) -> None:
            document.v3.lock["schema"] = "retrieval-v3-selection-lock-v2"
            document.v3.sync(resign=False)
        self.assert_rejected(self.verdict(mutate), "schema")

    def test_a_calibration_gate_that_disagrees_with_recomputation_is_rejected(self):
        def mutate(document: GateDocument) -> None:
            document.v3.lock["calibration_gate"]["passed"] = False
            document.v3.sync(resign=False)
        self.assert_rejected(self.verdict(mutate), "calibration_gate")

    def test_calibration_must_be_the_uncached_final_rerun(self):
        def mutate(document: GateDocument) -> None:
            document.v3.lock["final_rerun_uncached"] = False
            document.v3.sync(resign=False)
        self.assert_rejected(self.verdict(mutate), "final_rerun_uncached")

    def test_calibration_must_use_a_real_embedding_provider(self):
        def mutate(document: GateDocument) -> None:
            document.v3.lock["identity"]["embedding_provider"] = "hash-ngram-v1"
            document.v3.sync(resign=False)
        self.assert_rejected(self.verdict(mutate), "embedding_provider")

    def test_a_tampered_selected_payload_is_rejected_by_its_digest(self):
        def mutate(document: GateDocument) -> None:
            document.v3.lock["selected"]["rerank"] = False
            document.v3.sync(resign=False)
        self.assert_rejected(self.verdict(mutate), "selected_sha256")

    def test_a_tampered_selected_policy_digest_is_rejected(self):
        def mutate(document: GateDocument) -> None:
            document.v3.lock["selected_policy_sha256"] = "f" * 64
            document.v3.sync(resign=False)
        self.assert_rejected(self.verdict(mutate), "selected_policy_sha256")

    def test_the_shared_gate_requires_the_same_ordered_string_lifecycle_lists(self):
        base = {"recall_at_5": 0.9, "mrr_at_10": 0.7, "false_positive_rate": 0.0,
                "false_positive_queries": 0, "no_evidence_denominator": 20,
                "answerable_denominator": 60, "recall_denominator": 60, "mrr_denominator": 60,
                "p95_denominator": 80, "p95_ms": 8000.0, "stale_leaks": 0, "ambient_leaks": 0,
                "scope_leaks": 0, "invalid_refs": 0, "fixture_cross_split_leaks": 0,
                "assessor_configured": True, "false_sufficient_rate": 0.0,
                "false_sufficient_denominator": 8, "none_false_positive_rate": 0.0,
                "forgotten_leakage": 0, "admission_failures": 0,
                "failure_counts": {"embedding": 0, "rerank": 0, "assessment": 0}}
        baseline = dict(base, recall_at_5=0.6, mrr_at_10=0.5, p95_ms=10000.0,
                        recall_eligible_ids=["q-a", "q-b"])
        for ids, expected in ((["q-a", "q-b"], True), (["q-b", "q-a"], False),
                              (["q-a", 2], False), ([], False)):
            with self.subTest(ids=ids):
                gate = runner.quality_gate_v3(dict(base, recall_eligible_ids=ids), dict(baseline))
                self.assertEqual(gate["checks"]["recall_eligible_ids_match"], expected)

    def test_reordered_lifecycle_id_lists_are_rejected(self):
        def mutate(document: GateDocument) -> None:
            document.v3.manifest["runs"]["v1_test"]["metrics"]["recall_eligible_ids"] = \
                sorted(V3_ELIGIBLE_IDS, reverse=True)
            document.v3.sync(resign=False)
        self.assert_rejected(self.verdict(mutate), "recall_eligible_ids")

    def test_a_near_miss_threshold_is_rejected(self):
        def mutate(document: GateDocument) -> None:
            document.v3.lock["selected"]["threshold"] = 1.0 + 1e-12
            document.v3.sync()
        self.assert_rejected(self.verdict(mutate), "threshold")

    def test_an_oversized_threshold_does_not_crash_the_checker(self):
        def mutate(document: GateDocument) -> None:
            document.v3.lock["selected"]["threshold"] = 10 ** 400
            document.v3.sync()
        self.assert_rejected(self.verdict(mutate), "threshold")

    def test_the_shared_gate_tolerates_malformed_failure_counts(self):
        gate = runner.quality_gate_v3({"failure_counts": "oops"}, {"failure_counts": None})
        self.assertFalse(gate["passed"])
        self.assertIn("embedding_and_rerank_failures_zero", gate["checks"])

    def test_malformed_failure_counts_are_rejected_without_raising(self):
        def lock_side(document: GateDocument) -> None:
            document.v3.lock["calibration_metrics"]["failure_counts"] = ["embedding"]
            document.v3.sync(resign=False)
        self.assert_rejected(self.verdict(lock_side), "failure_counts")

        def run_side(document: GateDocument) -> None:
            document.v3.manifest["runs"]["v3_test"]["metrics"]["failure_counts"] = None
            document.v3.sync(resign=False)
        self.assert_rejected(self.verdict(run_side), "failure_counts")

    def test_a_hash_backend_cannot_carry_the_real_provider_lock(self):
        def mutate(document: GateDocument) -> None:
            document.v3.lock["selected"]["backend"] = "hash"
            document.v3.sync()
        self.assert_rejected(self.verdict(mutate), "backend")

    def test_selected_rerank_must_match_the_locked_rerank_configuration(self):
        def mutate(document: GateDocument) -> None:
            document.v3.lock["selected"]["rerank"] = False
            document.v3.resign_selected()
            document.v3.sync(resign=False)
        self.assert_rejected(self.verdict(mutate), "rerank")

    def test_identity_must_be_present_consistent_and_pinned(self):
        def missing_assessor(document: GateDocument) -> None:
            document.v3.identity.pop("assessor_model")
            document.v3.sync(resign=False)
        self.assert_rejected(self.verdict(missing_assessor), "assessor_model")

        def mismatched_embedding(document: GateDocument) -> None:
            document.v3.identity["embedding_model"] = "other-embed"
            document.v3.sync(resign=False)
        self.assert_rejected(self.verdict(mismatched_embedding), "embedding_model")

        def floating_alias(document: GateDocument) -> None:
            document.v3.configuration["assessor_model"] = "gpt-latest"
            document.v3.identity["assessor_model"] = "gpt-latest"
            document.v3.identity["assessor_actual_model"] = ""
            document.v3.sync()
        self.assert_rejected(self.verdict(floating_alias), "assessor_actual_model")

    def test_fixture_and_binary_pins_must_be_hash_shaped(self):
        def bad_fixture_shape(document: GateDocument) -> None:
            document.v3.lock["fixture_sha256"]["retrieval.json"] = "not-a-hash"
            document.v3.manifest["fixture_sha256"]["retrieval.json"] = "not-a-hash"
            document.v3.sync(resign=False)
        self.assert_rejected(self.verdict(bad_fixture_shape), "must be 64 hex")

        def bad_binary_shape(document: GateDocument) -> None:
            document.v3.lock["binary_sha256"] = "g" * 64
            document.v3.manifest["binary_sha256"] = "g" * 64
            document.v3.sync(resign=False)
        self.assert_rejected(self.verdict(bad_binary_shape), "must be 64 hex")

    def test_the_audio_gate_stays_independent_of_the_v3_evidence(self):
        def mutate(document: GateDocument) -> None:
            document.gate["categories"]["quiet_two_person"]["cer"] = {"errors": 90, "reference": 100}
            document.refresh()
        errors, _warnings = self.verdict(mutate)
        self.assertTrue(any("cer" in error for error in errors), errors)
        self.assertFalse(any("v3" in error for error in errors), errors)


if __name__ == "__main__":
    unittest.main()
