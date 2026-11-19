import importlib.util
import json
import tempfile
import unittest
from argparse import Namespace
from pathlib import Path
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location(
    "retrieval_quality_v2", ROOT / "scripts/run-retrieval-quality-v2.py"
)
runner = importlib.util.module_from_spec(SPEC)
assert SPEC and SPEC.loader
SPEC.loader.exec_module(runner)


def synthetic_hybrid_report():
    questions = [
        {"id": "q-positive", "bucket": "synonym", "unanswerable": False},
        {"id": "q-negative", "bucket": "no_evidence", "unanswerable": True},
    ]
    rows = []
    for mode in ("lexical", "hybrid"):
        for question in questions:
            attempted = mode == "lexical" and question["id"] == "q-positive" or mode == "hybrid" and question["id"] == "q-positive"
            gold_count = 0 if question["unanswerable"] else 1
            rows.append({
                "id": question["id"], "split": "calibration", "bucket": question["bucket"],
                "mode": mode, "unanswerable": question["unanswerable"], "returned_items": 0,
                "stale_items": 0, "ambient_items": 0, "scope_escapes": 0, "latency_ms": 2,
                "reciprocal_rank_at_10": 0, "recall_at_5": 0, "gold_evidence_count": gold_count,
                "gold_evidence_found": 0, "final_gold_evidence_found": gold_count,
                "rerank_calls": 0, "assessor_calls": int(attempted),
                "model_calls": int(attempted), "assessment_failed": attempted, "rerank_failed": False,
                "expected_sufficiency": "insufficient" if question["unanswerable"] else "sufficient",
                "evidence_completeness": "none" if question["unanswerable"] else "complete",
                "sufficiency_status": "unknown" if attempted else "insufficient",
            })
    bucket_rows = {}
    for mode in ("lexical", "hybrid"):
        for bucket in ("synonym", "no_evidence"):
            bucket_rows[f"calibration/{mode}/{bucket}"] = {
                "questions": 1, "answerable_questions": int(bucket == "synonym"),
                "no_evidence_questions": int(bucket == "no_evidence"), "false_positive_queries": 0,
                "false_positive_rate": 0, "recall_at_5": 0, "mrr_at_10": 0,
                "latency_ms_p50": 2, "latency_ms_p95": 2, "stale_items": 0,
                "ambient_items": 0, "scope_escapes": 0,
            }
    return {
        "metrics": {
            "lexical_recall_at_5": 0, "hybrid_recall_at_5": 0,
            "retrieval_lexical_mrr_at_10": 0, "retrieval_hybrid_mrr_at_10": 0,
            "retrieval_lexical_latency_ms_p95": 2, "retrieval_hybrid_latency_ms_p95": 2,
            "retrieval_requests": 4, "retrieval_invalid_evidence_refs": 0,
            "retrieval_lexical_invalid_evidence_refs": 0, "retrieval_hybrid_invalid_evidence_refs": 0,
            "semantic_provider_embed_errors": 0, "semantic_provider_embed_requests": 4,
            "retrieval_rerank_failures": 0, "evidence_assessment_failures": 2,
            "model_calls": 2, "scope_escape_count": 0, "evidence_assessor_configured": True,
            "retrieval_buckets": bucket_rows,
        },
        "per_question": rows,
    }


class FrozenQualityV2Tests(unittest.TestCase):
    def test_independent_fixture_shape_and_group_split(self):
        summary = runner.validate_fixture(
            ROOT / "fixtures/eval/quality-v2-r3/retrieval.json",
            ROOT / "fixtures/eval/quality-v2-r3/corpus.json",
        )
        self.assertEqual(summary["questions"], 240)
        self.assertEqual(summary["positive"], 160)
        self.assertEqual(summary["no_evidence"], 80)
        self.assertEqual(summary["split_group_counts"], {"train": 80, "calibration": 40, "test": 40})
        self.assertEqual(summary["split_question_counts"], {"train": 120, "calibration": 60, "test": 60})
        self.assertEqual(summary["positive_bucket_counts"], {"synonym": 40, "negation": 40, "time": 40, "cross_session": 40})

    def test_source_original_index_must_be_positive(self):
        import tempfile
        corpus = json.loads((ROOT / "fixtures/eval/quality-v2-r3/corpus.json").read_text())
        corpus["conversations"][0]["segments"][0]["turns"][0]["original_index"] = 0
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "corpus.json"
            path.write_text(json.dumps(corpus, ensure_ascii=False))
            with self.assertRaisesRegex(ValueError, "original_index"):
                runner.validate_fixture(ROOT / "fixtures/eval/quality-v2-r3/retrieval.json", path)

    def test_frozen_hashes_are_pinned(self):
        manifest = json.loads((ROOT / "fixtures/eval/quality-v2-r3/SHA256SUMS.json").read_text(encoding="utf-8"))
        self.assertEqual(manifest, {
            "corpus.json": "b95ade6df3221b8c0fd4653d88d2761da33c15efed5aa4be3cd9402b3fb63dc2",
            "retrieval.json": "03e8ed678f35cc6632092daacb0f031871048f596853e01cf664d2997b3cc4d3",
        })
        self.assertEqual(runner.verify_frozen_hashes(ROOT / "fixtures/eval/quality-v2-r3"), manifest)

    def test_gate_is_strict_on_recall_and_checks_mrr_fp_leaks_and_p95(self):
        baseline = {"recall_at_5": .6, "mrr_at_10": .4, "false_positive_rate": .05, "p95_ms": 10,
                    "p95_denominator": 15, "answerable_denominator": 10,
                    "failure_counts": {"embedding": 0, "rerank": 0}}
        good = {"recall_at_5": .61, "mrr_at_10": .4, "false_positive_rate": .05,
                "stale_leaks": 0, "ambient_leaks": 0, "scope_leaks": 0,
                "invalid_refs": 0, "fixture_cross_split_leaks": 0, "p95_ms": 15,
                "answerable_denominator": 10, "recall_denominator": 10, "mrr_denominator": 10,
                "no_evidence_denominator": 20, "false_positive_queries": 1, "p95_denominator": 15,
                "assessor_configured": True, "false_sufficient_rate": .05, "false_sufficient_denominator": 1,
                "failure_counts": {"embedding": 0, "rerank": 0, "assessment": 0}}
        self.assertTrue(runner.quality_gate(good, baseline)["passed"])
        good["recall_at_5"] = .6
        self.assertFalse(runner.quality_gate(good, baseline)["passed"])
        good["recall_at_5"] = .61
        good["mrr_at_10"] = .39
        self.assertFalse(runner.quality_gate(good, baseline)["passed"])
        good["mrr_at_10"] = .4
        good["p95_ms"] = 15.01
        self.assertFalse(runner.quality_gate(good, baseline)["passed"])
        good["p95_ms"] = 15
        good["false_sufficient_rate"] = .051
        self.assertFalse(runner.quality_gate(good, baseline)["passed"])

    def test_source_group_cannot_cross_splits_even_when_concept_ids_differ(self):
        import tempfile
        source = json.loads((ROOT / "fixtures/eval/quality-v2-r3/retrieval.json").read_text(encoding="utf-8"))
        source["questions"][0]["source_group_id"] = "shared-source"
        source["questions"][-1]["source_group_id"] = "shared-source"
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "retrieval.json"
            path.write_text(json.dumps(source, ensure_ascii=False), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "source group .* crosses"):
                runner.validate_fixture(path, ROOT / "fixtures/eval/quality-v2-r3/corpus.json")

    def test_duplicate_normalized_query_is_rejected(self):
        import tempfile
        source = json.loads((ROOT / "fixtures/eval/quality-v2-r3/retrieval.json").read_text(encoding="utf-8"))
        source["questions"][1]["query"] = source["questions"][0]["query"]
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "retrieval.json"
            path.write_text(json.dumps(source, ensure_ascii=False), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "duplicated"):
                runner.validate_fixture(path, ROOT / "fixtures/eval/quality-v2-r3/corpus.json")

    def test_missing_real_provider_configuration_is_not_run(self):
        self.assertEqual(runner.provider_status("openai", {"key_configured": True}), "NOT_RUN")
        self.assertEqual(runner.provider_status("openai", {"endpoint": "http://local", "model": "m", "dims": 256}), "CONFIGURED")

    def test_model_failure_blocks_quality_gate(self):
        baseline = {"recall_at_5": .2, "mrr_at_10": .2, "p95_ms": 10,
                    "p95_denominator": 15, "answerable_denominator": 10,
                    "failure_counts": {"embedding": 0, "rerank": 0}}
        candidate = {"recall_at_5": .3, "mrr_at_10": .2, "false_positive_rate": 0,
                     "stale_leaks": 0, "ambient_leaks": 0, "scope_leaks": 0,
                     "invalid_refs": 0, "fixture_cross_split_leaks": 0, "p95_ms": 10,
                     "answerable_denominator": 10, "recall_denominator": 10, "mrr_denominator": 10,
                     "no_evidence_denominator": 5, "false_positive_queries": 0, "p95_denominator": 15,
                     "assessor_configured": True, "false_sufficient_rate": 0, "false_sufficient_denominator": 1,
                     "failure_counts": {"embedding": 0, "rerank": 1, "assessment": 0}}
        self.assertFalse(runner.quality_gate(candidate, baseline)["passed"])

    def test_frozen_hash_manifest_rejects_tampering(self):
        import tempfile
        import shutil
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            shutil.copytree(ROOT / "fixtures/eval/quality-v2-r3", root, dirs_exist_ok=True)
            with (root / "retrieval.json").open("a", encoding="utf-8") as stream:
                stream.write(" ")
            with self.assertRaisesRegex(ValueError, "hash mismatch"):
                runner.verify_frozen_hashes(root)

    def test_missing_metrics_and_nonfinite_values_fail_gate(self):
        baseline = {"recall_at_5": .1, "mrr_at_10": .1, "p95_ms": 10,
                    "p95_denominator": 15, "answerable_denominator": 10,
                    "failure_counts": {"embedding": 0, "rerank": 0}}
        self.assertFalse(runner.quality_gate({}, baseline)["passed"])
        bad = {"recall_at_5": float("nan"), "mrr_at_10": .2, "false_positive_rate": 0,
               "stale_leaks": 0, "ambient_leaks": 0, "scope_leaks": 0,
               "invalid_refs": 0, "fixture_cross_split_leaks": 0, "p95_ms": 1,
               "answerable_denominator": 10, "recall_denominator": 10, "mrr_denominator": 10,
               "no_evidence_denominator": 5, "false_positive_queries": 0, "p95_denominator": 15,
               "assessor_configured": True, "false_sufficient_rate": 0, "false_sufficient_denominator": 1}
        self.assertFalse(runner.quality_gate(bad, baseline)["passed"])

    def test_retrieval_only_gate_is_diagnostic_without_assessor(self):
        baseline = {"recall_at_5": .2, "mrr_at_10": .2, "p95_ms": 10,
                    "p95_denominator": 15, "answerable_denominator": 10,
                    "failure_counts": {"embedding": 0, "rerank": 0}}
        candidate = {"recall_at_5": .3, "mrr_at_10": .2, "false_positive_rate": 0,
                     "stale_leaks": 0, "ambient_leaks": 0, "scope_leaks": 0,
                     "invalid_refs": 0, "fixture_cross_split_leaks": 0, "p95_ms": 10,
                     "answerable_denominator": 10, "recall_denominator": 10, "mrr_denominator": 10,
                     "no_evidence_denominator": 5, "false_positive_queries": 0, "p95_denominator": 15,
                     "failure_counts": {"embedding": 0, "rerank": 0, "assessment": 0}}
        self.assertTrue(runner.retrieval_gate(candidate, baseline)["passed"])
        self.assertFalse(runner.quality_gate(candidate, baseline)["passed"])

    def test_run_fails_closed_when_report_omits_metrics(self):
        import tempfile
        from types import SimpleNamespace
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            split = root / "split.json"
            split.write_text(json.dumps({"questions": [{"unanswerable": False}, {"unanswerable": True}]}))
            out = root / "report.json"
            args = Namespace(timeout=2, corpus="corpus.json", classic="classic.json",
                             embed_endpoint="", embed_model="", embed_key_env="", embed_dims=0,
                             rerank_url="", rerank_model="", assessor_url="", assessor_model="",
                             fixture_cross_split_leaks=0)

            def fake_run(cmd, **kwargs):
                out.write_text(json.dumps({"metrics": {}, "per_question": []}))
                return SimpleNamespace(returncode=0, stdout="", stderr="")

            with mock.patch.object(runner.subprocess, "run", side_effect=fake_run):
                result = runner._run("fake-binary", args, split, "v2", "lexical", False, "calibration", out)
            self.assertEqual(result["status"], "FAIL")
            self.assertIn("protocol_error", result)

    def test_latency_protocol_discloses_stage_difference(self):
        self.assertIn("migration-budget cap", runner.LATENCY_COMPARISON)
        self.assertIn("v1 measures retrieval build without evidence assessor", runner.LATENCY_COMPARISON)
        self.assertIn("not a matched-stage latency comparison", runner.LATENCY_COMPARISON)

    def test_run_records_timeout(self):
        import tempfile
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            split = root / "split.json"
            split.write_text('{"questions": []}')
            args = Namespace(timeout=1, corpus="corpus.json", classic="classic.json",
                             embed_endpoint="", embed_model="", embed_key_env="", embed_dims=0,
                             rerank_url="", rerank_model="", assessor_url="", assessor_model="")
            with mock.patch.object(runner.subprocess, "run", side_effect=runner.subprocess.TimeoutExpired("fake", 1)) as mocked_run:
                result = runner._run("fake-binary", args, split, "v2", "lexical", False, "calibration", root / "out.json")
            self.assertEqual(result["status"], "TIMEOUT")
            invoked = mocked_run.call_args.args[0]
            question_flag = invoked.index("--questions")
            self.assertEqual(invoked[question_flag + 1], "")

    def test_sufficiency_counts_are_mode_specific_and_count_failed_calls(self):
        import tempfile
        with tempfile.TemporaryDirectory() as temp:
            split_path = Path(temp) / "split.json"
            split_path.write_text(json.dumps({"questions": [
                {"id": "q-positive", "bucket": "synonym", "unanswerable": False},
                {"id": "q-negative", "bucket": "no_evidence", "unanswerable": True},
            ]}))
            args = Namespace(rerank_url="", rerank_model="", fixture_cross_split_leaks=0)
            metrics = runner.extract_run_metrics(synthetic_hybrid_report(), split_path,
                                                 "calibration", "v2", "hash", args)
        self.assertEqual(metrics["assessor_calls"], 1)
        self.assertEqual(metrics["model_calls"], 1)
        self.assertEqual(metrics["failure_counts"]["assessment"], 1)
        self.assertEqual(metrics["sufficiency"]["scored"], 1)
        self.assertEqual(metrics["false_sufficient_denominator"], 0)
        self.assertIsNone(metrics["false_sufficient_rate"])

    def test_expected_sufficiency_must_match_final_visible_required_evidence(self):
        import tempfile
        report = synthetic_hybrid_report()
        hybrid_positive = next(q for q in report["per_question"] if q["mode"] == "hybrid" and q["id"] == "q-positive")
        hybrid_positive["final_gold_evidence_found"] = 0
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "split.json"
            path.write_text(json.dumps({"questions": [
                {"id": "q-positive", "bucket": "synonym", "unanswerable": False},
                {"id": "q-negative", "bucket": "no_evidence", "unanswerable": True},
            ]}))
            args = Namespace(rerank_url="", rerank_model="", fixture_cross_split_leaks=0)
            with self.assertRaisesRegex(runner.ReportProtocolError, "final evidence coverage"):
                runner.extract_run_metrics(report, path, "calibration", "v2", "hash", args)

    def test_selection_lock_rejects_fixture_config_and_binary_mismatches(self):
        hashes = {"corpus.json": "c" * 64, "retrieval.json": "r" * 64}
        config_hash = "d" * 64
        binary_hash = "b" * 64
        lock = {"fixture_sha256": hashes, "configuration_sha256": config_hash,
                "binary_sha256": binary_hash, "selected": {"version": "v2", "backend": "hash", "rerank": False},
                "calibration_gate": {"passed": True}}
        runner.validate_selection_lock(lock, hashes, config_hash, binary_hash)
        for field, wrong in (("fixture_sha256", {"corpus.json": "x" * 64, "retrieval.json": "r" * 64}),
                             ("configuration_sha256", "x" * 64), ("binary_sha256", "x" * 64)):
            changed = dict(lock)
            changed[field] = wrong
            with self.subTest(field=field), self.assertRaisesRegex(ValueError, "selection lock"):
                runner.validate_selection_lock(changed, hashes, config_hash, binary_hash)

    def test_holdout_lock_consumes_once_without_real_holdout_io(self):
        import tempfile
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            corpus = root / "corpus.json"
            retrieval = root / "retrieval.json"
            binary = root / "binary"
            corpus.write_text("synthetic corpus pin")
            retrieval.write_text("synthetic retrieval pin")
            binary.write_text("synthetic executable pin")
            hashes = {"corpus.json": runner.digest(corpus), "retrieval.json": runner.digest(retrieval)}
            args = Namespace(binary=str(binary), corpus=str(corpus), retrieval=str(retrieval),
                             classic="unused-classic", out=str(root / "out"), phase="holdout",
                             selection_lock=str(root / "selection-lock.json"), max_items=10, max_bytes=65536,
                             timeout=2, embed_endpoint="", embed_model="", embed_key_env="", embed_dims=0,
                             rerank_url="", rerank_model="", assessor_url="", assessor_model="",
                             fixture_cross_split_leaks=0)
            config = {"embedding_endpoint": "", "embedding_model": "", "embedding_dims": 0,
                      "embedding_key_env": "", "rerank_endpoint": "", "rerank_model": "",
                      "assessor_endpoint": "", "assessor_model": ""}
            lock = {"fixture_sha256": hashes, "configuration_sha256": runner.config_signature(config),
                    "binary_sha256": runner.digest(binary),
                    "selected": {"version": "v2", "backend": "hash", "rerank": False},
                    "calibration_gate": {"passed": True}}
            Path(args.selection_lock).write_text(json.dumps(lock))

            def fake_split(_source, _split, out_dir):
                path = Path(out_dir) / "dummy-test.json"
                path.write_text('{"questions": []}')
                return path

            with mock.patch.object(runner, "verify_frozen_hashes", return_value=hashes), \
                 mock.patch.object(runner, "validate_fixture", return_value={"fixture_cross_split_leaks": 0}), \
                 mock.patch.object(runner, "_split_file", side_effect=fake_split) as split_mock, \
                 mock.patch.object(runner, "_run", return_value={"status": "FAIL"}) as run_mock:
                self.assertEqual(runner.run(args), 1)
                self.assertEqual(run_mock.call_count, 2)
            consumed = json.loads(Path(args.selection_lock).read_text())
            self.assertTrue(consumed["holdout_started"])
            with mock.patch.object(runner, "verify_frozen_hashes", return_value=hashes), \
                 mock.patch.object(runner, "validate_fixture", return_value={"fixture_cross_split_leaks": 0}), \
                 mock.patch.object(runner, "_split_file") as split_mock:
                with self.assertRaisesRegex(ValueError, "selection lock"):
                    runner.run(args)
                split_mock.assert_not_called()

    def test_failed_calibration_removes_stale_selection_lock(self):
        import tempfile
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            corpus = root / "corpus.json"
            retrieval = root / "retrieval.json"
            binary = root / "binary"
            corpus.write_text("synthetic corpus pin")
            retrieval.write_text("synthetic retrieval pin")
            binary.write_text("synthetic executable pin")
            hashes = {"corpus.json": runner.digest(corpus), "retrieval.json": runner.digest(retrieval)}
            out = root / "out"
            out.mkdir()
            stale_lock = out / "selection-lock.json"
            stale_lock.write_text('{"stale":true}')
            args = Namespace(binary=str(binary), corpus=str(corpus), retrieval=str(retrieval),
                             classic="unused-classic", out=str(out), phase="calibrate", selection_lock="",
                             max_items=10, max_bytes=65536, timeout=2,
                             embed_endpoint="", embed_model="", embed_key_env="", embed_dims=0,
                             rerank_url="", rerank_model="", assessor_url="", assessor_model="",
                             fixture_cross_split_leaks=0)

            def fake_split(_source, split, out_dir):
                path = Path(out_dir) / f"{split}.json"
                path.write_text('{"questions": []}')
                return path

            def fake_run(_binary, _args, _split, version, provider, _rerank, _split_name, _out):
                mode = "lexical" if provider == "lexical" else "hybrid"
                return {"status": "PASS", "metrics": {"recall_at_5": .1 if version == "v1" else .2,
                        "mrr_at_10": .1, "false_positive_rate": 0, "false_positive_queries": 0,
                        "no_evidence_denominator": 1, "answerable_denominator": 1,
                        "recall_denominator": 1, "mrr_denominator": 1, "p95_denominator": 1,
                        "p95_ms": 1, "stale_leaks": 0, "ambient_leaks": 0, "scope_leaks": 0,
                        "invalid_refs": 0, "fixture_cross_split_leaks": 0, "assessor_configured": False,
                        "false_sufficient_rate": None, "false_sufficient_denominator": 0,
                        "failure_counts": {"embedding": 0, "rerank": 0, "assessment": 0},
                        "mode": mode}}

            with mock.patch.object(runner, "verify_frozen_hashes", return_value=hashes), \
                 mock.patch.object(runner, "validate_fixture", return_value={"fixture_cross_split_leaks": 0}), \
                 mock.patch.object(runner, "_split_file", side_effect=fake_split), \
                 mock.patch.object(runner, "_run", side_effect=fake_run):
                self.assertEqual(runner.run(args), 0)
            self.assertFalse(stale_lock.exists())


if __name__ == "__main__":
    unittest.main()


class RetrievalQualityV3ProtocolTests(unittest.TestCase):
    """Protocol-level tests for the v3 runner; no model or holdout I/O."""

    def metrics(self, **overrides):
        base = {
            "recall_at_5": 0.9, "mrr_at_10": 0.8, "p95_ms": 100.0,
            "false_positive_rate": 0.0, "none_false_positive_rate": 0.0,
            "false_positive_queries": 0, "no_evidence_denominator": 40,
            "stale_leaks": 0, "ambient_leaks": 0, "scope_leaks": 0, "invalid_refs": 0,
            "fixture_cross_split_leaks": 0, "answerable_denominator": 180,
            "recall_denominator": 180, "mrr_denominator": 180, "p95_denominator": 220,
            "failure_counts": {"embedding": 0, "rerank": 0, "assessment": 0},
            "assessor_configured": True, "false_sufficient_rate": 0.0,
            "false_sufficient_denominator": 40, "forgotten_leakage": 0, "admission_failures": 0,
            "blocked_note": "v3",
        }
        base.update(overrides)
        return base

    def baseline(self):
        return self.metrics(recall_at_5=0.5, mrr_at_10=0.4, p95_ms=100.0)

    def test_v3_gate_requires_recall_fp_lifecycle_and_admission(self):
        gate = runner.quality_gate_v3(self.metrics(), self.baseline())
        self.assertTrue(gate["passed"], gate["checks"])
        for override in ({"recall_at_5": 0.84}, {"false_positive_rate": 0.06},
                         {"forgotten_leakage": 1}, {"admission_failures": 2},
                         {"false_sufficient_rate": 0.2}, {"stale_leaks": 1}):
            failed = runner.quality_gate_v3(self.metrics(**override), self.baseline())
            self.assertFalse(failed["passed"], override)

    def test_v3_selection_orders_recall_mrr_p95_then_stricter_threshold(self):
        def candidate(recall, mrr, p95, threshold):
            return {"backend": "hash", "threshold_hundredths": threshold,
                    "metrics": self.metrics(recall_at_5=recall, mrr_at_10=mrr, p95_ms=p95),
                    "gate": {"passed": True}}

        selected = runner.select_v3_candidate([
            candidate(0.9, 0.8, 90, 30), candidate(0.9, 0.8, 90, 70),
            candidate(0.9, 0.8, 110, 50), candidate(0.91, 0.7, 200, 90),
            candidate(0.95, 0.9, 10, 40),
        ])
        self.assertEqual(selected["threshold_hundredths"], 40, "highest recall must win")
        tie = runner.select_v3_candidate([candidate(0.9, 0.8, 90, 30), candidate(0.9, 0.8, 90, 70)])
        self.assertEqual(tie["threshold_hundredths"], 70)
        self.assertIsNone(runner.select_v3_candidate([{"backend": "hash", "threshold_hundredths": 1,
                                                        "metrics": self.metrics(), "gate": {"passed": False}}]))

    def lock_document(self):
        hashes = {"corpus.json": "a", "retrieval.json": "b", "lifecycle.json": "c"}
        configuration = {"protocol": "v3", "embedding_endpoint": "http://127.0.0.1:1/v1/embeddings",
                         "embedding_model": "embed-model", "embedding_dims": 1024,
                         "embedding_input_type": True, "embedding_key_env": "",
                         "embedding_revision": "rev-7", "rerank_endpoint": "", "rerank_model": "",
                         "assessor_endpoint": "http://127.0.0.1:2/v1/systemone",
                         "assessor_model": "decision-model", "admission_batch_questions": 32,
                         "max_items": 10, "max_bytes": 65536}
        selected = {"version": "v3", "backend": "hash", "threshold_hundredths": 42,
                    "threshold": 0.42, "rerank": False}
        lock = {"schema": runner.V3_LOCK_SCHEMA, "fixture_sha256": hashes,
                "configuration": configuration,
                "configuration_sha256": runner.config_signature(configuration),
                "policy_template_sha256": "tmpl", "binary_sha256": "bin",
                "identity": {"embedding_revision": "rev-7", "assessor_model": "decision-model",
                             "assessor_actual_model": "decision-model-1.2", "binary_sha256": "bin"},
                "budget": {"max_items": 10, "max_bytes": 65536},
                "warmup": {"questions": 1, "source": "train", "cache_flush": True},
                "selected": selected, "calibration_gate": {"passed": True}, "holdout_started": False}
        return hashes, configuration, lock

    def test_v3_lock_rejects_unpinned_or_started_locks(self):
        hashes, configuration, lock = self.lock_document()
        config_hash = runner.config_signature(configuration)
        self.assertEqual(runner.validate_v3_lock(lock, hashes, config_hash, "tmpl", "bin")["threshold_hundredths"], 42)
        for broken in ({**lock, "holdout_started": True},
                       {**lock, "binary_sha256": "other"},
                       {**lock, "selected": {**lock["selected"], "threshold_hundredths": 101}},
                       {**lock, "calibration_gate": {"passed": False}},
                       {**lock, "schema": "retrieval-v3-selection-lock-v1"},
                       {**lock, "identity": {**lock["identity"], "binary_sha256": "other"}},
                       {**lock, "budget": {"max_items": 20, "max_bytes": 65536}},
                       {**lock, "warmup": {"questions": 8}}):
            with self.assertRaises(ValueError):
                runner.validate_v3_lock(broken, hashes, config_hash, "tmpl", "bin")
        with self.assertRaises(ValueError):
            runner.validate_v3_lock(lock, hashes, config_hash, "other-template", "bin")
        with self.assertRaises(ValueError):
            runner.validate_v3_lock(lock, {"corpus.json": "z"}, config_hash, "tmpl", "bin")

    def test_v3_lock_configuration_hash_must_match_its_content(self):
        hashes, configuration, lock = self.lock_document()
        config_hash = runner.config_signature(configuration)
        tampered = {**lock, "configuration": {**configuration, "embedding_input_type": False}}
        with self.assertRaisesRegex(ValueError, "does not match its own hash"):
            runner.validate_v3_lock(tampered, hashes, config_hash, "tmpl", "bin")
        # embedding_input_type is part of the hash: dropping it changes the data.
        without_flag = {key: value for key, value in configuration.items() if key != "embedding_input_type"}
        self.assertNotEqual(runner.config_signature(without_flag), config_hash)

    def test_v3_holdout_claim_is_single_use(self):
        with tempfile.TemporaryDirectory() as tmp:
            lock_path = Path(tmp) / "selection-lock.json"
            runner.claim_holdout(lock_path)
            self.assertTrue(Path(str(lock_path) + ".consumed").exists())
            with self.assertRaises(FileExistsError):
                runner.claim_holdout(lock_path)

    def test_v3_fixture_is_frozen_and_valid(self):
        root = Path(__file__).resolve().parents[1] / "fixtures" / "eval" / "quality-v3-r1"
        hashes = runner.verify_frozen_hashes_v3(root)
        summary = runner.validate_fixture_v3(root / "retrieval.json", root / "corpus.json", root / "lifecycle.json")
        self.assertEqual(sorted(hashes), ["corpus.json", "lifecycle.json", "retrieval.json"])
        self.assertEqual(summary["questions"], 780)
        self.assertEqual(summary["groups"], 330)
        self.assertEqual(summary["splits"], {"train": 110, "calibration": 110, "test": 110})
        self.assertEqual(summary["split_questions"], {"train": 260, "calibration": 260, "test": 260})
        self.assertEqual(summary["lifecycle_sources"], 30)
        self.assertEqual(summary["fixture_cross_split_leaks"], 0)

    def test_v3_policy_document_pins_integer_threshold(self):
        args = Namespace(embed_model="Qwen/Qwen3-Embedding-0.6B", embed_revision="rev-7",
                                  assessor_model="decision-v3-0.8b")
        policy = runner.admission_policy_document(args, "openai", 7, "f" * 64, "calibration")
        self.assertEqual(policy["threshold"], 0.07)
        self.assertEqual(policy["phase"], "calibration")
        self.assertEqual(policy["provider_id"], "openai-compat")
        self.assertEqual(policy["model_revision"], "rev-7")
        self.assertEqual(policy["fixture_sha256"], "f" * 64)


class RetrievalQualityV3BehaviourTests(unittest.TestCase):
    """End-to-end runner behaviour with the binary and services mocked."""

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.fixtures = self.root / "fixtures"
        self.fixtures.mkdir()
        (self.fixtures / "retrieval.json").write_text(json.dumps(
            {"questions": [{"id": "warm-1", "split": "train", "query": "热身"}]}))
        (self.fixtures / "corpus.json").write_text("{}")
        self.binary = self.root / "context-eval"
        self.binary.write_text("synthetic binary pin")
        self.out = self.root / "out"
        self.hashes = {"corpus.json": "a" * 64, "retrieval.json": "b" * 64, "lifecycle.json": "c" * 64}
        real_digest = runner.digest

        def fake_digest(path):
            name = Path(path).name
            if name in self.hashes:
                return self.hashes[name]
            return real_digest(Path(path))

        self.patches = [
            mock.patch.object(runner, "verify_frozen_hashes_v3", return_value=self.hashes),
            mock.patch.object(runner, "digest", side_effect=fake_digest),
            mock.patch.object(runner, "validate_fixture_v3",
                              return_value={"lifecycle_sources": 3, "questions": 1}),
            mock.patch.object(runner, "flush_embed_cache"),
            mock.patch.object(runner, "_split_file", side_effect=self.fake_split),
        ]
        for patch in self.patches:
            patch.start()
        self.addCleanup(self.temp.cleanup)
        self.addCleanup(lambda: [patch.stop() for patch in self.patches])

    @staticmethod
    def fake_split(_source, split, out):
        path = Path(out) / f"retrieval-{split}.json"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps({"questions": [], "split": split}))
        return path

    def args(self, **overrides):
        values = {
            "binary": str(self.binary), "fixture_dir": str(self.fixtures), "lifecycle": "",
            "out": str(self.out), "phase": "calibrate", "selection_lock": "", "max_items": 10,
            "max_bytes": 65536, "timeout": 5, "embed_endpoint": "http://127.0.0.1:9/v1/embeddings",
            "embed_model": "embed-model", "embed_key_env": "", "embed_dims": 1024,
            "embed_input_type": True, "embed_revision": "rev-7",
            "rerank_url": "", "rerank_model": "", "assessor_url": "http://127.0.0.1:8/v1/systemone",
            "assessor_model": "decision-model", "admission_batch_questions": 32,
            "protocol": "v3", "backends": "hash", "flush_embed_cache_url": "",
            "admission_probability_cache": "", "warmup": True,
            "assessor_actual_model": "decision-model-1.2", "fixture_cross_split_leaks": 0,
        }
        values.update(overrides)
        return Namespace(**values)

    def passing_run(self, *, recall=0.9, p95=100.0, engine_p95=50.0, status="PASS"):
        return {"status": status,
                "metrics": {"recall_at_5": recall, "mrr_at_10": 0.8, "p95_ms": p95,
                            "false_positive_rate": 0.0, "none_false_positive_rate": 0.0,
                            "false_positive_queries": 0, "no_evidence_denominator": 40,
                            "stale_leaks": 0, "ambient_leaks": 0, "scope_leaks": 0, "invalid_refs": 0,
                            "fixture_cross_split_leaks": 0, "answerable_denominator": 180,
                            "recall_denominator": 180, "mrr_denominator": 180, "p95_denominator": 220,
                            "failure_counts": {"embedding": 0, "rerank": 0, "assessment": 0},
                            "assessor_configured": True, "false_sufficient_rate": 0.0,
                            "false_sufficient_denominator": 40, "forgotten_leakage": 0,
                            "admission_failures": 0},
                "raw_metrics": {"retrieval_hybrid_latency_ms_p50": p95 / 2,
                                "retrieval_hybrid_latency_ms_p95": p95,
                                "admission_engine_latency_ms_p50": engine_p95 / 2,
                                "admission_engine_latency_ms_p95": engine_p95,
                                "admission_engine_calls": 220, "admission_requests": 220,
                                "admission_request_candidates": 8800,
                                "admission_request_bytes": 8800 * 900,
                                "retrieval_requests": 220,
                                "stage_latencies": {"admission": {"p50": 10, "p95": 20, "n": 220},
                                                    "assessor": {"p50": 5, "p95": 9, "n": 220},
                                                    "rerank": {"p50": 0, "p95": 0, "n": 0}}}}

    def test_v3_flush_url_must_match_the_embedding_service(self):
        with self.assertRaisesRegex(ValueError, "own host and port"):
            runner.run(self.args(flush_embed_cache_url="http://127.0.0.1:8790/admin/flush"))
        runner.validate_flush_target("http://127.0.0.1:8792/v1/embeddings",
                                     "http://127.0.0.1:8792/admin/flush")
        with self.assertRaisesRegex(ValueError, "requires --embed-endpoint"):
            runner.validate_flush_target("", "http://127.0.0.1:8792/admin/flush")

    def test_v3_output_directory_must_be_new_or_empty(self):
        self.out.mkdir()
        (self.out / "manifest.json").write_text("{}")
        with self.assertRaisesRegex(ValueError, "must be new or empty"):
            runner.run(self.args())

    def test_v3_profile_writes_no_lock_and_never_touches_test(self):
        calls = []

        def fake_run(_binary, _args, split_path, version, provider, _rerank, split, out, extra=None, corpus=None):
            calls.append((split, version, tuple(extra or ())))
            return self.passing_run()

        with mock.patch.object(runner, "_run", side_effect=fake_run):
            self.assertEqual(runner.run(self.args(phase="profile")), 0)
        manifest = json.loads((self.out / "manifest.json").read_text())
        self.assertFalse(manifest["protocol_config"]["test_touched"])
        self.assertIsNone(manifest["selection_lock"])
        self.assertFalse((self.out / "selection-lock.json").exists())
        self.assertTrue(all(split == "train" for split, _version, _extra in calls))
        self.assertEqual({version for _split, version, _extra in calls}, {"v1", "v3"})
        self.assertTrue(any("--profile-admission" in extra for _split, _version, extra in calls))
        self.assertNotIn("result", {entry["status"] for entry in manifest["runs"].values()})
        self.assertEqual(manifest["profile"]["conclusion"], "construction-bound")

    def test_v3_profile_reports_a_model_bound_configuration(self):
        def fake_run(_binary, _args, _split, version, _provider, _rerank, _split_name, _out, extra=None, corpus=None):
            if version == "v1":
                return self.passing_run(p95=120.0)
            return self.passing_run(engine_p95=900.0, p95=1200.0)

        with mock.patch.object(runner, "_run", side_effect=fake_run):
            runner.run(self.args(phase="profile"))
        manifest = json.loads((self.out / "manifest.json").read_text())
        self.assertEqual(manifest["profile"]["conclusion"], "model-bound")
        self.assertIn("cannot bring this configuration under budget", manifest["profile"]["reason"])
        self.assertFalse((self.out / "selection-lock.json").exists())

    def test_v3_calibration_executes_every_threshold_and_one_uncached_rerun(self):
        thresholds = []
        uncached = 0

        def fake_run(_binary, _args, _split, version, provider, _rerank, _split_name, out, extra=None, corpus=None):
            nonlocal uncached
            path = Path(out)
            if "--admission-probability-cache" in (extra or ()):
                thresholds.append(path.name)
            elif "uncached" in path.name:
                uncached += 1
            return self.passing_run(recall=0.5 if version == "v1" else 0.9)

        with mock.patch.object(runner, "_run", side_effect=fake_run):
            self.assertEqual(runner.run(self.args()), 0)
        self.assertEqual(len(thresholds), 101)
        self.assertEqual(uncached, 1)
        manifest = json.loads((self.out / "manifest.json").read_text())
        self.assertEqual(manifest["thresholds_executed"], 101)
        lock = json.loads((self.out / "selection-lock.json").read_text())
        self.assertEqual(lock["schema"], runner.V3_LOCK_SCHEMA)
        self.assertEqual(lock["selected"]["threshold_hundredths"], 100)
        self.assertIn("embedding_input_type", lock["configuration"])
        self.assertFalse(lock["holdout_started"])

    def test_v3_final_uncached_failure_writes_no_lock(self):
        def fake_run(_binary, _args, _split, version, provider, _rerank, _split_name, out, extra=None, corpus=None):
            if "uncached" in Path(out).name:
                return self.passing_run(status="FAIL", recall=0.1)
            return self.passing_run(recall=0.5 if version == "v1" else 0.9)

        with mock.patch.object(runner, "_run", side_effect=fake_run):
            self.assertEqual(runner.run(self.args()), 1)
        manifest = json.loads((self.out / "manifest.json").read_text())
        self.assertNotIn("selection_lock", manifest)
        self.assertFalse((self.out / "selection-lock.json").exists())
        self.assertIn("uncached final rerun", manifest["calibration_selection"])

    def test_v3_gate_failure_writes_no_lock(self):
        def fake_run(_binary, _args, _split, version, provider, _rerank, _split_name, out, extra=None, corpus=None):
            return self.passing_run(recall=0.5 if version == "v1" else 0.9,
                                    p95=100.0 if version == "v1" else 1000.0)

        with mock.patch.object(runner, "_run", side_effect=fake_run):
            self.assertEqual(runner.run(self.args()), 0)
        manifest = json.loads((self.out / "manifest.json").read_text())
        self.assertNotIn("selection_lock", manifest)
        self.assertFalse((self.out / "selection-lock.json").exists())
        self.assertIn("No v3 candidate passed every calibration gate", manifest["calibration_selection"])

    def write_lock(self, configuration=None, **overrides):
        configuration = configuration or {
            "protocol": "v3", "embedding_endpoint": "http://127.0.0.1:9/v1/embeddings",
            "embedding_model": "embed-model", "embedding_dims": 1024,
            "embedding_input_type": True, "embedding_key_env": "", "embedding_revision": "rev-7",
            "rerank_endpoint": "", "rerank_model": "",
            "assessor_endpoint": "http://127.0.0.1:8/v1/systemone",
            "assessor_model": "decision-model", "admission_batch_questions": 32,
            "max_items": 10, "max_bytes": 65536,
        }
        lock = {"schema": runner.V3_LOCK_SCHEMA, "fixture_sha256": self.hashes,
                "configuration": configuration,
                "configuration_sha256": runner.config_signature(configuration),
                "policy_template_sha256": "",
                "binary_sha256": runner.digest(self.binary),
                "identity": {"embedding_revision": "rev-7", "assessor_model": "decision-model",
                             "assessor_actual_model": "decision-model-1.2",
                             "binary_sha256": runner.digest(self.binary)},
                "budget": {"max_items": 10, "max_bytes": 65536},
                "warmup": dict(runner.WARMUP),
                "selected": {"version": "v3", "backend": "hash", "threshold_hundredths": 42,
                             "threshold": 0.42, "rerank": False},
                "calibration_gate": {"passed": True}, "holdout_started": False}
        lock.update(overrides)
        lock["policy_template_sha256"] = runner.config_signature(
            runner.admission_policy_document(self.args(backends="hash"), "hash", 0,
                                             self.hashes["retrieval.json"], "selected")).replace("x", "x")
        template = runner.admission_policy_document(self.args(backends="hash"), "hash", 0,
                                                    self.hashes["retrieval.json"], "selected")
        template.pop("threshold", None)
        template.pop("phase", None)
        lock["policy_template_sha256"] = runner.config_signature(template)
        path = self.root / "selection-lock.json"
        path.write_text(json.dumps(lock))
        return path

    def test_v3_holdout_restores_the_locked_configuration(self):
        lock_path = self.write_lock()
        seen = []

        def fake_run(_binary, args, _split, version, _provider, _rerank, split, out, extra=None, corpus=None):
            seen.append((version, args.embed_endpoint, args.embed_model, args.admission_batch_questions,
                         args.max_items, split))
            return self.passing_run()

        with mock.patch.object(runner, "_run", side_effect=fake_run):
            runner.run(self.args(phase="holdout", selection_lock=str(lock_path),
                                 embed_endpoint=None, embed_model=None, embed_dims=None,
                                 embed_input_type=None, embed_revision=None, assessor_url=None,
                                 assessor_model=None, admission_batch_questions=None,
                                 max_items=None, max_bytes=None))
        official = [entry for entry in seen if entry[5] == "test"]
        self.assertEqual(len(official), 2)
        self.assertEqual({entry[1] for entry in official}, {"http://127.0.0.1:9/v1/embeddings"})
        self.assertEqual({entry[2] for entry in official}, {"embed-model"})
        self.assertEqual({entry[3] for entry in official}, {32})
        self.assertEqual({entry[4] for entry in official}, {10})
        self.assertEqual({entry[0] for entry in official}, {"v1", "v3"})
        self.assertTrue(any(entry[5] == "train" for entry in seen), "the warmup must precede the official runs")
        consumed = json.loads(lock_path.read_text())
        self.assertTrue(consumed["holdout_started"])
        self.assertTrue(Path(str(lock_path) + ".consumed").exists())

    def test_v3_holdout_rejects_a_conflicting_explicit_flag_before_any_test_query(self):
        lock_path = self.write_lock()
        runs = []
        for overrides in ({"embed_endpoint": "http://127.0.0.1:7/other"},
                          {"embed_model": "other-model"},
                          {"admission_batch_questions": 1},
                          {"embed_input_type": False}):
            with self.subTest(overrides=overrides):
                self.out = self.root / f"out-{sorted(overrides)[0]}"
                args = self.args(phase="holdout", selection_lock=str(lock_path), **overrides)
                with mock.patch.object(runner, "_run", side_effect=lambda *a, **k: runs.append(a)):
                    with self.assertRaisesRegex(ValueError, "conflicts with the selection lock"):
                        runner.run(args)
                self.assertEqual(runs, [])
                self.assertFalse(json.loads(lock_path.read_text())["holdout_started"])

    def test_v3_holdout_rejects_the_calibration_cache(self):
        lock_path = self.write_lock()
        with mock.patch.object(runner, "_run") as run_mock:
            with self.assertRaisesRegex(ValueError, "must not use the calibration probability cache"):
                runner.run(self.args(phase="holdout", selection_lock=str(lock_path),
                                     admission_probability_cache=str(self.root / "cache.json")))
            run_mock.assert_not_called()

    def test_v3_holdout_cannot_be_replayed_after_a_crash(self):
        lock_path = self.write_lock()
        runner.claim_holdout(lock_path)
        with mock.patch.object(runner, "_run") as run_mock:
            with self.assertRaises((ValueError, FileExistsError)):
                runner.run(self.args(phase="holdout", selection_lock=str(lock_path)))
            run_mock.assert_not_called()
        # The consumed claim is never removed, so the lock cannot be reopened.
        self.assertTrue(Path(str(lock_path) + ".consumed").exists())

    def test_v3_holdout_rejects_an_older_lock_schema(self):
        lock_path = self.write_lock(schema="retrieval-v3-selection-lock-v1")
        with mock.patch.object(runner, "_run") as run_mock:
            with self.assertRaisesRegex(ValueError, "older protocol"):
                runner.run(self.args(phase="holdout", selection_lock=str(lock_path)))
            run_mock.assert_not_called()

    def test_unverifiable_assessor_identity_blocks_the_lock(self):
        identity = runner.v3_identity(self.args(assessor_model="typesafe/jev-latest",
                                                assessor_actual_model=""), "b" * 64, "CONFIGURED")
        ready, reason = runner.identity_is_verifiable(identity)
        self.assertFalse(ready)
        self.assertIn("latest", reason)
        ready, _reason = runner.identity_is_verifiable(
            runner.v3_identity(self.args(assessor_model="typesafe/jev-latest",
                                         assessor_actual_model="jev-1.14.0"), "b" * 64, "CONFIGURED"))
        self.assertTrue(ready)
