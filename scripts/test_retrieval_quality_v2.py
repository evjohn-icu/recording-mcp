import importlib.util
import json
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
