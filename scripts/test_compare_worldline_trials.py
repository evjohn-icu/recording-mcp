import importlib.util
import os
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).with_name("compare-worldline-trials.py")
SPEC = importlib.util.spec_from_file_location("compare_worldline_trials", SCRIPT)
COMPARE = importlib.util.module_from_spec(SPEC)
assert SPEC and SPEC.loader
SPEC.loader.exec_module(COMPARE)


def make_trial(predictions, *, stale=False):
    state = {
        "left": {"title": "同一合成主题", "summary": "甲在公园散步。", "subject": None, "identity_status": "unconfirmed", "evidence": []},
        "right": {"title": "同一合成主题", "summary": "甲在公园行走。", "subject": None, "identity_status": "unconfirmed", "evidence": []},
    }
    sources = [{
        "title": "<合成主题>",
        "summary": "<script>合成文本</script>",
        "claim_kind": "observation",
        "evidence_refs": [{"turn_id": "turn-synthetic", "revision": "rev-1", "conversation_id": "conv/synthetic", "text": "</blockquote><script>synthetic</script>"}],
    }]
    case = {"id": "alias-test", "task": "alias_topic", "state": state, "sources": sources}
    case["input_hash"] = COMPARE.json_hash({"state": state, "sources": sources})
    prompt_value = {name: {"labels": labels, "instructions": instructions} for name, (labels, instructions) in COMPARE.TASKS.items()}
    prepared = {
        "schema": COMPARE.TRIAL_SCHEMA,
        "api_version": "source-snapshot-v1",
        "decision_runner_sha256": COMPARE.file_hash(COMPARE.DECISION_RUNNER),
        "synthetic_fixture_sha256": COMPARE.file_hash(COMPARE.ROOT / "fixtures/worldline-decision-experiment.json"),
        "task_prompt_sha256": COMPARE.json_hash(prompt_value),
        "page_checks": [{"path": "/api/worldline/threads", "params": {"limit": 10}, "version": "source-snapshot-v1", "digest": "a" * 64}],
        "cases": [case],
        "human_review_required": True,
        "no_gold_labels": True,
    }
    answers = []
    for prediction in predictions:
        labels = set(COMPARE.TASKS["alias_topic"][0])
        distribution = {label: float(label == prediction) for label in labels}
        answers.append({"id": "alias-test", "task": "alias_topic", "input_hash": case["input_hash"], "prediction": prediction, "distribution": distribution})
    result = {
        "schema": COMPARE.RESULT_SCHEMA,
        "prepared_input_hash": COMPARE.prepared_hash(prepared),
        "decision_runner_sha256": prepared["decision_runner_sha256"],
        "synthetic_fixture_sha256": prepared["synthetic_fixture_sha256"],
        "task_prompt_sha256": prepared["task_prompt_sha256"],
        "no_gold_labels": True,
        "valid_predictions": 1,
        "invalid_or_failed": 0,
        "actual_models": ["synthetic-test-model"],
        "weights_versions": ["synthetic-test-weight"],
        "cases": answers,
        "stale": stale,
        "stale_reason": "source changed" if stale else "",
    }
    return prepared, result


class CompareWorldlineTrialsTests(unittest.TestCase):
    def test_compares_predictions_without_gold_or_accuracy(self):
        prepared_a, result_a = make_trial(["same_topic"])
        prepared_b, result_b = make_trial(["related_topic"])
        comparison, report = COMPARE.compare_trials(prepared_a, result_a, prepared_b, result_b, "A", "B")
        self.assertEqual(comparison["agreement"], 0)
        self.assertEqual(comparison["disagreement"], 1)
        self.assertFalse(comparison["accuracy_claimed"])
        self.assertEqual(comparison["pair_labels_by_task"]["alias_topic"]["same_topic -> related_topic"], 1)
        self.assertIn("same_topic", report)
        self.assertIn("related_topic", report)
        self.assertNotIn("<script>合成文本</script>", report)
        self.assertIn("&lt;script&gt;合成文本&lt;/script&gt;", report)
        self.assertNotIn("<script>synthetic</script>", report)
        self.assertIn("http://127.0.0.1:8787/c/conv%2Fsynthetic", report)
        self.assertIn("不计算准确率", report)
        self.assertIn("相关但不同事项", report)

    def test_rejects_stale_trial(self):
        prepared_a, result_a = make_trial(["same_topic"], stale=True)
        prepared_b, result_b = make_trial(["same_topic"])
        with self.assertRaisesRegex(ValueError, "stale"):
            COMPARE.compare_trials(prepared_a, result_a, prepared_b, result_b, "A", "B")

    def test_rejects_result_prepared_hash_mismatch(self):
        prepared_a, result_a = make_trial(["same_topic"])
        prepared_b, result_b = make_trial(["same_topic"])
        result_b["prepared_input_hash"] = "0" * 64
        with self.assertRaisesRegex(ValueError, "prepared_input_hash"):
            COMPARE.compare_trials(prepared_a, result_a, prepared_b, result_b, "A", "B")

    def test_rejects_different_input_hash_or_page_snapshot(self):
        prepared_a, result_a = make_trial(["same_topic"])
        prepared_b, result_b = make_trial(["same_topic"])
        prepared_b["cases"][0]["state"]["left"]["summary"] = "changed source"
        with self.assertRaisesRegex(ValueError, "input hash"):
            COMPARE.compare_trials(prepared_a, result_a, prepared_b, result_b, "A", "B")

        prepared_b, result_b = make_trial(["same_topic"])
        prepared_b["page_checks"][0]["digest"] = "b" * 64
        with self.assertRaisesRegex(ValueError, "page_checks"):
            COMPARE.compare_trials(prepared_a, result_a, prepared_b, result_b, "A", "B")

    def test_rejects_invalid_or_missing_case_prediction(self):
        prepared_a, result_a = make_trial(["same_topic"])
        prepared_b, result_b = make_trial(["same_topic"])
        result_b["cases"][0]["distribution"]["different_topic"] = 0.5
        with self.assertRaisesRegex(ValueError, "normalized"):
            COMPARE.compare_trials(prepared_a, result_a, prepared_b, result_b, "A", "B")

    def test_structural_warnings_preserve_model_labels_and_prioritize_html(self):
        prepared_a, result_a = make_trial(["same_topic"])
        prepared_b, result_b = make_trial(["same_topic"])
        cases = [
            {"id": "z-no-evidence", "task": "summary_support", "state": {"summary": "摘要", "summary_kind": "fact", "evidence": [{"text": "已提供的原话"},], "identity": {}}, "sources": [{"title": "来源", "summary": "原始摘要", "claim_kind": "fact", "evidence_refs": [{"text": "真实合成引文", "conversation_id": "conversation"}]}]},
            {"id": "y-supported-empty", "task": "summary_support", "state": {"summary": "无引用摘要", "summary_kind": "fact", "evidence": [], "identity": {}}, "sources": []},
            {"id": "a-normal", "task": "summary_support", "state": {"summary": "有证据", "summary_kind": "fact", "evidence": [{"text": "支持原文"}], "identity": {}}, "sources": []},
        ]
        pred_a = ["no_evidence", "supported", "supported"]
        pred_b = ["unsupported", "no_evidence", "supported"]

        def install(prepared, results, predictions):
            for case in cases:
                case["input_hash"] = COMPARE.json_hash({"state": case["state"], "sources": case["sources"]})
            prepared["cases"] = cases
            rows = []
            for case, prediction in zip(cases, predictions):
                labels = set(COMPARE.TASKS[case["task"]][0])
                rows.append({"id": case["id"], "task": case["task"], "input_hash": case["input_hash"], "prediction": prediction,
                             "distribution": {label: float(label == prediction) for label in labels}})
            results["cases"] = rows
            results["valid_predictions"] = len(rows)
            results["prepared_input_hash"] = COMPARE.prepared_hash(prepared)

        install(prepared_a, result_a, pred_a)
        install(prepared_b, result_b, pred_b)
        comparison, report = COMPARE.compare_trials(prepared_a, result_a, prepared_b, result_b, "A", "B")
        self.assertEqual(comparison["agreement"], 1)
        self.assertEqual(comparison["structural_inconsistencies"]["count"], 2)
        self.assertEqual(comparison["structural_inconsistencies"]["by_code"], {"no_evidence_with_source": 1, "supported_without_evidence": 1})
        indexed = {row["id"]: row for row in comparison["cases"]}
        self.assertEqual(indexed["z-no-evidence"]["model_a_label"], "no_evidence")
        self.assertEqual(indexed["y-supported-empty"]["model_a_label"], "supported")
        self.assertIn("不能直接采用该判断", report)
        self.assertLess(report.index("z-no-evidence"), report.index("a-normal"))
        self.assertLess(report.index("y-supported-empty"), report.index("a-normal"))

    def test_output_files_are_private(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = COMPARE.private_output_dir(Path(temporary) / "private")
            COMPARE.write_private(output / "result.json", b"synthetic only")
            self.assertEqual(os.stat(output).st_mode & 0o777, 0o700)
            self.assertEqual(os.stat(output / "result.json").st_mode & 0o777, 0o600)

    def test_web_links_only_use_literal_loopback_base(self):
        self.assertEqual(COMPARE.validate_web_base("http://127.0.0.1:8787/"), "http://127.0.0.1:8787")
        for value in ("http://localhost:8787", "http://example.com", "http://127.0.0.1/path"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                COMPARE.validate_web_base(value)


if __name__ == "__main__":
    unittest.main()
