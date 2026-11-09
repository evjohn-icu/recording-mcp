#!/usr/bin/env python3
"""Fixtures and committed evidence for the productization retrieval report."""
from __future__ import annotations

import json
import re
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
FIXTURE = ROOT / "fixtures/eval/product-review-v1"
REPORT = ROOT / "docs/productization/retrieval-quality-report.md"
RESULTS = sorted((ROOT / "docs/evals").glob("productization-retrieval-*.json"))


class ProductReviewFixtureTest(unittest.TestCase):
    def test_review_fixture_resolves_and_is_unique(self) -> None:
        corpus = json.loads((FIXTURE / "corpus.json").read_text(encoding="utf-8"))
        retrieval = json.loads((FIXTURE / "retrieval.json").read_text(encoding="utf-8"))
        sources: set[tuple[str, str, str]] = set()
        for conversation in corpus["conversations"]:
            for segment in conversation["segments"]:
                source = segment["source"]
                for turn in segment["turns"]:
                    self.assertIsInstance(turn["original_index"], int)
                    self.assertGreater(turn["original_index"], 0)
                    sources.add((source["segment_id"], source["transcript_revision"], turn["id"]))
        self.assertGreaterEqual(len(corpus["conversations"]), 4)

        ids = [q["id"] for q in retrieval["questions"]]
        self.assertEqual(len(ids), len(set(ids)), "question ids must be unique")
        self.assertEqual(len(ids), 24)
        buckets: dict[str, int] = {}
        answerable = 0
        for question in retrieval["questions"]:
            self.assertEqual(question["split"], "calibration")
            if question["unanswerable"]:
                self.assertEqual(question["gold_turn_refs"], [])
                self.assertIsNone(question["gold_top1_turn_ref"])
                continue
            answerable += 1
            buckets[question["bucket"]] = buckets.get(question["bucket"], 0) + 1
            refs = question["gold_turn_refs"]
            self.assertTrue(refs)
            for ref in refs:
                key = (ref["segment_id"], ref["transcript_revision"], ref["turn_id"])
                self.assertIn(key, sources, f"{question['id']} references a missing source")
            self.assertEqual(question["gold_top1_turn_ref"], refs[0])
        self.assertEqual(answerable, 16)
        self.assertEqual(buckets, {"synonym": 4, "negation": 4, "time_change": 4, "cross_session": 4})
        manifest = json.loads((FIXTURE / "manifest.json").read_text(encoding="utf-8"))
        self.assertIn("corpus.json", manifest)
        self.assertIn("retrieval.json", manifest)
        self.assertTrue((FIXTURE / "REVIEW.md").is_file())

    def test_report_contains_required_metrics(self) -> None:
        text = REPORT.read_text(encoding="utf-8")
        for needle in (
            "Recall@5", "MRR@10", "Precision@5", "p50", "p95", "不可回答",
            "v1 lexical", "v1 hybrid", "v2 lexical", "v2 hybrid",
            "stale", "ambient", "scope", "invalid evidence",
            "NOT_RUN", "默认检索版本保持 v1",
        ):
            self.assertIn(needle, text, f"report is missing {needle!r}")
        self.assertIn("分母", text)
        self.assertEqual(len(RESULTS), 4, "all four matrix results must be committed")

    def test_leakage_counts_are_zero(self) -> None:
        for path in RESULTS:
            metrics = json.loads(path.read_text(encoding="utf-8"))["metrics"]
            self.assertEqual(metrics["stale_leakage"], 0, path.name)
            self.assertEqual(metrics["ambient_leakage"], 0, path.name)
            self.assertEqual(metrics["scope_escape_count"], 0, path.name)
            self.assertEqual(metrics["invalid_evidence_refs"], 0, path.name)
        lexical = json.loads((ROOT / "docs/evals/productization-retrieval-v1lex.json").read_text(encoding="utf-8"))["metrics"]
        self.assertEqual(lexical["unanswerable_lexical_false_positive_items"], 0)


if __name__ == "__main__":
    unittest.main()
