import importlib.util
import json
import random
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).with_name("worldline-decision-experiment.py")
SPEC = importlib.util.spec_from_file_location("worldline_decision_experiment", SCRIPT)
EXPERIMENT = importlib.util.module_from_spec(SPEC)
assert SPEC and SPEC.loader
SPEC.loader.exec_module(EXPERIMENT)


class ExperimentFixtureTests(unittest.TestCase):
    def test_fixture_is_synthetic_and_has_both_tasks_and_negative_cases(self):
        fixture = EXPERIMENT.load_fixture(EXPERIMENT.DEFAULT_FIXTURE)
        self.assertTrue(fixture["synthetic_only"])
        self.assertGreaterEqual(len(fixture["cases"]), 20)
        tasks = {case["task"] for case in fixture["cases"]}
        self.assertEqual(tasks, {"alias_topic", "summary_support"})
        alias = [case for case in fixture["cases"] if case["task"] == "alias_topic"]
        self.assertTrue(any(case["expected"] == "different_topic" for case in alias))
        self.assertTrue(any(case["expected"] == "insufficient_evidence" for case in alias))
        self.assertTrue(any(case["expected"] == "wrong_subject" for case in fixture["cases"]))
        self.assertTrue(any(case["expected"] == "wrong_time" for case in fixture["cases"]))
        summary_cases = [case for case in fixture["cases"] if case["task"] == "summary_support"]
        self.assertTrue(all(item["kind"] == "transcript" for case in summary_cases for item in case["state"]["evidence"]))
        self.assertTrue(any(case["expected"] == "insufficient_evidence" for case in summary_cases))

    def test_distribution_schema_is_strict(self):
        labels = set(EXPERIMENT.TASKS["alias_topic"][0])
        answer = {
            "type": "choice",
            "choice": "same_topic",
            "confidence": 0.7,
            "probabilities": {"same_topic": 0.7, "related_topic": 0.1, "different_topic": 0.1, "insufficient_evidence": 0.1},
        }
        choice, distribution, error = EXPERIMENT.validate_answer(answer, labels)
        self.assertEqual(choice, "same_topic")
        self.assertEqual(set(distribution or {}), labels)
        self.assertIsNone(error)
        answer["probabilities"]["different_topic"] = 0.5
        self.assertIsNotNone(EXPERIMENT.validate_answer(answer, labels)[2])

    def test_fixture_rejects_gold_label_fields_inside_model_state(self):
        fixture = {"synthetic_only": True, "cases": [
            {"id": "leak", "task": "alias_topic", "expected": "same_topic", "state": {"gold_label": "same_topic"}},
        ]}
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "fixture.json"
            path.write_text(json.dumps(fixture), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "gold-label"):
                EXPERIMENT.load_fixture(path)

    def test_endpoint_requires_literal_loopback(self):
        self.assertEqual(EXPERIMENT.validate_loopback_endpoint("http://127.0.0.1:8790/v1/systemone"), "http://127.0.0.1:8790/v1/systemone")
        for endpoint in ("http://localhost:8790/v1/systemone", "http://example.com/api", "https://127.0.0.1/api"):
            with self.subTest(endpoint=endpoint), self.assertRaises(ValueError):
                EXPERIMENT.validate_loopback_endpoint(endpoint)
        self.assertIsNone(EXPERIMENT.NoRedirectHandler().redirect_request(None, None, 302, "found", {}, "http://127.0.0.1/new"))

    def test_runner_reports_confusion_and_latency(self):
        fixture = {"fixture_version": "test", "cases": [
            {"id": "alias-test", "task": "alias_topic", "expected": "same_topic", "state": {}},
            {"id": "support-test", "task": "summary_support", "expected": "no_evidence", "state": {}},
        ]}
        answers = {
            "alias-test": {"type": "choice", "choice": "same_topic", "confidence": 1.0,
                           "probabilities": {"same_topic": 1.0, "related_topic": 0.0, "different_topic": 0.0, "insufficient_evidence": 0.0}},
            "support-test": {"type": "choice", "choice": "supported", "confidence": 1.0,
                             "probabilities": {"supported": 1.0, "plan_not_fact": 0.0, "wrong_subject": 0.0, "wrong_time": 0.0, "no_evidence": 0.0, "unsupported": 0.0, "insufficient_evidence": 0.0}},
        }
        original = EXPERIMENT.request_one
        try:
            def fake_request(endpoint, model, case, timeout):
                answer = answers.get(case["id"])
                if answer is None:
                    labels = list(EXPERIMENT.TASKS[case["task"]][0])
                    choice = labels[0]
                    answer = {"type": "choice", "choice": choice, "confidence": 1.0,
                              "probabilities": {label: float(label == choice) for label in labels}}
                return ({"model": "synthetic-test", "weights_version": "test-v1", "answers": {case["id"]: answer}}, 5.0)

            EXPERIMENT.request_one = fake_request
            report = EXPERIMENT.run(fixture, "http://127.0.0.1:1/v1/systemone", None, 1.0, None)
        finally:
            EXPERIMENT.request_one = original
        self.assertEqual(report["valid_answers"], 2)
        self.assertEqual(report["accuracy"], 0.5)
        self.assertEqual(report["confusion"]["summary_support"]["no_evidence -> supported"], 1)
        self.assertEqual(report["latency_ms"]["mean"], 5.0)
        self.assertEqual(report["warmup"]["id"], "worldline-synthetic-warmup")
        expected_order = list(fixture["cases"])
        random.Random(20261023).shuffle(expected_order)
        self.assertEqual(report["case_order"], [case["id"] for case in expected_order])


if __name__ == "__main__":
    unittest.main()
