import importlib.util
import json
import os
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).with_name("worldline-private-trial.py")
SPEC = importlib.util.spec_from_file_location("worldline_private_trial", SCRIPT)
trial = importlib.util.module_from_spec(SPEC)
assert SPEC and SPEC.loader
SPEC.loader.exec_module(trial)


def thread(thread_id, title, macro="macro-work", count=1):
    return {
        "id": thread_id,
        "title": title,
        "macro_topic_id": macro,
        "macro_topic_title": "工作与项目",
        "entry_count": count,
    }


def claim(claim_id, summary, kind="observation", text="原文提及。", evidence=True):
    refs = [{
        "turn_id": f"turn-{claim_id}",
        "revision": "rev-1",
        "conversation_id": f"conv-{claim_id}",
        "recording_id": f"rec-{claim_id}",
        "text": text,
    }] if evidence else []
    return {
        "id": claim_id,
        "version": "v1",
        "summary": summary,
        "kind": kind,
        "evidence": refs,
    }


def detail(summary, claims):
    return {
        "version": "snapshot-1",
        "thread": {"title": summary["title"]},
        "entries": [{"claim": item} for item in claims],
        "unknown_entries": [],
    }


class WorldlinePrivateTrialTests(unittest.TestCase):
    def test_loopback_only_web_url(self):
        self.assertEqual(trial.validate_loopback_url("http://127.0.0.1:8790/"), "http://127.0.0.1:8790")
        for value in ("https://127.0.0.1:8790", "http://example.com:8790", "http://user@127.0.0.1:8790", "http://127.0.0.1:8790/api"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                trial.validate_loopback_url(value)

    def test_login_sends_same_origin_header(self):
        client = trial.LocalWebClient("http://127.0.0.1:8790")
        seen = {}
        def fake_request(method, path, payload=None, extra_headers=None):
            seen.update({"method": method, "path": path, "payload": payload, "headers": extra_headers})
            return {"profile": "synthetic", "csrf_token": "synthetic-token"}
        client._request_json = fake_request
        client.login("synthetic", "not-a-real-password")
        self.assertEqual(seen["method"], "POST")
        self.assertEqual(seen["path"], "/api/profiles/login")
        self.assertEqual(seen["headers"], {"Origin": "http://127.0.0.1:8790"})

    def test_chinese_bigram_shortlist_respects_macro_degree_and_cap(self):
        values = [thread("a", "城市公园计划"), thread("b", "城市公园安排"), thread("c", "城市公园变化"), thread("other", "城市公园活动", macro="macro-life")]
        pairs = trial.shortlist_alias_pairs(values)
        self.assertEqual(len(pairs), 3)
        self.assertEqual({item for pair in pairs for item in (pair["left_thread"]["id"], pair["right_thread"]["id"])}, {"a", "b", "c"})
        many = [thread(f"t{i}", f"城市公园{i}") for i in range(12)]
        shortlist = trial.shortlist_alias_pairs(many, cap=16, per_thread=2)
        self.assertLessEqual(len(shortlist), 16)
        degrees = {}
        for pair in shortlist:
            for side in ("left_thread", "right_thread"):
                key = pair[side]["id"]
                degrees[key] = degrees.get(key, 0) + 1
        self.assertTrue(all(degree <= 2 for degree in degrees.values()))

    def test_export_caps_cases_uses_transcript_evidence_and_unknown_identity(self):
        summaries = [thread("t1", "城市公园计划"), thread("t2", "城市公园安排")]
        details = {
            "t1": detail(summaries[0], [claim("c1", "公园在城市北边", "observation", "原文一"), claim("c2", "准备周日散步", "plan", evidence=False)]),
            "t2": detail(summaries[1], [claim("c3", "公园靠近城市北边", "observation", "原文二"), claim("c4", "申请了公园活动", "request", "原文三")]),
        }
        cases, stats = trial.make_trial_cases(summaries, details)
        self.assertEqual(stats["alias_cases_exported"], 1)
        self.assertEqual(stats["summary_support_cases_exported"], 4)
        self.assertTrue(all("expected" not in item and "gold" not in item for item in cases))
        alias = next(item for item in cases if item["task"] == "alias_topic")
        self.assertIsNone(alias["state"]["left"]["subject"])
        self.assertEqual(alias["state"]["left"]["identity_status"], "unconfirmed")
        self.assertEqual(alias["state"]["left"]["evidence"][0]["kind"], "transcript")
        support = next(item for item in cases if item["task"] == "summary_support")
        self.assertEqual(support["state"]["identity"], {"summary_subject": None, "summary_confirmed": False, "evidence_subject": None, "evidence_confirmed": False})
        self.assertEqual(support["state"]["evidence"][0]["kind"], "transcript")
        self.assertEqual(support["state"]["evidence"][0]["mentioned_at"], None)
        self.assertNotIn("plan", json.dumps(support["state"]["evidence"], ensure_ascii=False))

    def test_evidence_over_cap_is_skipped_not_silently_truncated(self):
        item = claim("many", "摘要", evidence=False)
        item["evidence"] = [{"turn_id": str(i), "revision": "r", "text": f"原文{i}"} for i in range(3)]
        visible, refs, complete = trial.source_evidence(item)
        self.assertFalse(complete)
        self.assertEqual(visible, [])
        self.assertEqual(refs, [])
        for malformed in (
            [{"turn_id": "t", "revision": "r", "text": "ok"}, "broken-row"],
            [{"turn_id": "", "revision": "r", "text": "ok"}],
            [{"turn_id": "t", "revision": "  ", "text": "ok"}],
            [{"turn_id": "t", "revision": "r", "text": "   "}],
        ):
            with self.subTest(malformed=malformed):
                visible, refs, complete = trial.source_evidence({"evidence": malformed})
                self.assertFalse(complete)
                self.assertEqual((visible, refs), ([], []))
        self.assertEqual(trial.source_evidence({"evidence": []}), ([], [], True))

    def test_frozen_runner_prompt_and_unknown_identity_abstention_are_used(self):
        case = {
            "id": "support-test",
            "task": "summary_support",
            "state": {"summary": "不确定陈述", "summary_kind": "observation", "evidence": [{"kind": "transcript", "text": "转写不完整", "mentioned_at": None}], "identity": {"summary_subject": None, "summary_confirmed": False, "evidence_subject": None, "evidence_confirmed": False}},
            "input_hash": "hash",
            "sources": [],
        }
        captured = []
        labels = set(trial.TASKS["summary_support"][0])
        distribution = {label: (1.0 if label == "insufficient_evidence" else 0.0) for label in labels}
        original = trial.request_one
        def fake_request(endpoint, model, submitted, timeout):
            captured.append((submitted, trial.decision_runner.TASKS[submitted["task"]][1]))
            return ({"model": "JEV-test", "weights_version": "weights-1", "answers": {submitted["id"]: {"type": "choice", "choice": "insufficient_evidence", "confidence": 1.0, "probabilities": distribution}}}, 1.0)
        trial.request_one = fake_request
        try:
            result = trial.evaluate_cases({"api_version": "v1", "cases": [case]}, "http://127.0.0.1:8790/v1/systemone", None, 1.0)
        finally:
            trial.request_one = original
        submitted, instructions = captured[0]
        self.assertIsNone(submitted["state"]["identity"]["summary_subject"])
        self.assertEqual(instructions, trial.decision_runner.TASKS["summary_support"][1])
        self.assertIn("未知主体或模糊转写选 insufficient_evidence", instructions)
        self.assertEqual(result["cases"][0]["prediction"], "insufficient_evidence")
        self.assertEqual(result["decision_runner_sha256"], trial.DECISION_RUNNER_SHA256)
        self.assertEqual(result["synthetic_fixture_sha256"], trial.SYNTHETIC_FIXTURE_SHA256)

    def test_support_claims_are_deduplicated_and_round_robin_by_kind(self):
        summaries = [thread("t1", "项目甲"), thread("t2", "项目乙")]
        rows1 = [claim(f"a{i}", f"摘要{i}", "fact") for i in range(12)]
        rows2 = [claim(f"b{i}", f"摘要{i}", "plan") for i in range(12)]
        details = {"t1": detail(summaries[0], rows1), "t2": detail(summaries[1], rows2)}
        cases, stats = trial.make_trial_cases(summaries, details)
        supports = [item for item in cases if item["task"] == "summary_support"]
        self.assertEqual(len(supports), trial.MAX_SUMMARY_CASES)
        kinds = [item["state"]["summary_kind"] for item in supports]
        self.assertLessEqual(abs(kinds.count("fact") - kinds.count("plan")), 1)
        self.assertEqual(stats["summary_support_candidates_deduplicated"], 24)

    def test_page_snapshot_detects_any_content_change(self):
        response = {"version": "v1", "thread": {"title": "主题"}, "entries": [{"claim": {"summary": "原摘要"}}]}

        class FakeClient:
            def __init__(self, current):
                self.current = current

            def reset_budget(self):
                return None

            def get(self, path, params):
                return self.current

        page = {"path": "/api/worldline/threads/t1", "params": {"limit": 10}, "version": "v1", "digest": trial.content_hash(response)}
        prepared = {"page_checks": [page]}
        self.assertEqual(trial.verify_prepared_snapshot(FakeClient(response), prepared), (True, ""))
        changed = {"version": "v1", "thread": {"title": "新标题"}, "entries": [{"claim": {"summary": "原摘要"}}]}
        valid, reason = trial.verify_prepared_snapshot(FakeClient(changed), prepared)
        self.assertFalse(valid)
        self.assertIn("changed", reason)

    def test_prepared_input_rechecks_case_and_runner_hashes(self):
        case = {"id": "alias-1", "task": "alias_topic", "state": {"left": {"title": "A"}, "right": {"title": "B"}}, "sources": []}
        case["input_hash"] = trial.content_hash({"state": case["state"], "sources": case["sources"]})
        prepared = {
            "schema": "worldline-private-trial-v1", "no_gold_labels": True, "cases": [case],
            "decision_runner_sha256": trial.DECISION_RUNNER_SHA256,
            "synthetic_fixture_sha256": trial.SYNTHETIC_FIXTURE_SHA256,
            "task_prompt_sha256": trial.TASK_PROMPT_SHA256,
        }
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "trial-input.json"
            path.write_text(json.dumps(prepared), encoding="utf-8")
            self.assertEqual(trial.load_prepared(path)["cases"][0]["input_hash"], case["input_hash"])
            broken = json.loads(path.read_text(encoding="utf-8"))
            broken["cases"][0]["state"]["left"]["title"] = "changed"
            path.write_text(json.dumps(broken), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "case hash"):
                trial.load_prepared(path)

    def test_stale_report_suppresses_predictions(self):
        prepared = {"stats": {}, "cases": [{"id": "alias-01", "task": "alias_topic", "sources": [{"title": "<主题>", "summary": "<script>alert(1)</script>", "claim_kind": "<kind>", "evidence_refs": [{"text": "</blockquote><script>x</script>", "conversation_id": "a/b"}]}]}]}
        results = {"cases": [{"id": "alias-01", "prediction": "same_topic"}], "valid_predictions": 1, "invalid_or_failed": 0}
        html = trial.render_report(prepared, "http://127.0.0.1:8790", results, stale=True, stale_reason="a source changed")
        self.assertIn("源数据已变化，结果作废", html)
        self.assertNotIn("待人工核对的模型输出：same_topic", html)
        self.assertNotIn("<script>alert(1)</script>", html)
        self.assertIn("&lt;script&gt;alert(1)&lt;/script&gt;", html)
        self.assertIn("http://127.0.0.1:8790/c/a%2Fb", html)

    def test_private_file_modes(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = trial.private_output_dir(Path(temporary) / "private")
            target = directory / "data.json"
            trial.write_private(target, b"synthetic")
            self.assertEqual(os.stat(directory).st_mode & 0o777, 0o700)
            self.assertEqual(os.stat(target).st_mode & 0o777, 0o600)


if __name__ == "__main__":
    unittest.main()
