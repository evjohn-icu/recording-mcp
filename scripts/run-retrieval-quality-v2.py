#!/usr/bin/env python3
"""Run the frozen retrieval-quality-v2 calibration matrix and locked holdout."""
from __future__ import annotations

import argparse
import hashlib
import http.client
import json
import math
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

sys.path.insert(0, str(Path(__file__).resolve().parent))
from retrieval_quality_gates import (config_signature, metric_number, quality_gate,
                                     quality_gate_v3, retrieval_gate)  # noqa: E402,F401

SPLIT_GROUPS = {"train": 80, "calibration": 40, "test": 40}
SPLIT_QUESTIONS = {"train": 120, "calibration": 60, "test": 60}
POSITIVE_BUCKETS = {"synonym", "negation", "time", "cross_session"}
V3_BUCKETS = ("direct_fact", "synonym", "negation", "time_change", "cross_session", "multi_source",
              "revocation", "missing_evidence", "wrong_identity_proposal", "numeric_constraint",
              "decision_commitment")
V3_PARTIAL_BUCKETS = {"time_change", "revocation", "missing_evidence", "wrong_identity_proposal"}
V3_SPLIT_QUESTIONS = {"train": 260, "calibration": 260, "test": 260}
V3_SPLIT_GROUPS = {"train": 110, "calibration": 110, "test": 110}
THRESHOLD_HUNDREDTHS = tuple(range(0, 101))
V3_LOCK_SCHEMA = "retrieval-v3-selection-lock-v3"
V3_LATENCY_COMPARISON = ("migration-budget cap; the v1 baseline is measured in the same run with the same "
                         "query order and budget, while v3 adds admission and evidence assessment")
CJK = re.compile(r"[\u3400-\u9fff]")
LATENCY_COMPARISON = ("migration-budget cap; v1 measures retrieval build without evidence assessor, "
                       "v2 includes assessment when configured; not a matched-stage latency comparison")


def digest(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()


def verify_frozen_hashes(root: Path) -> dict[str, str]:
    manifest = json.loads((root / "SHA256SUMS.json").read_text(encoding="utf-8"))
    expected_files = {"corpus.json", "retrieval.json"}
    if set(manifest) != expected_files:
        raise ValueError("frozen hash manifest must pin corpus.json and retrieval.json")
    actual = {name: digest(root / name) for name in sorted(expected_files)}
    if actual != manifest:
        raise ValueError(f"frozen fixture hash mismatch: expected {manifest}, got {actual}")
    return actual


def validate_fixture(retrieval_path: Path, corpus_path: Path | None = None) -> dict:
    payload = json.loads(retrieval_path.read_text(encoding="utf-8"))
    questions = payload.get("questions", [])
    if len(questions) != 240:
        raise ValueError(f"expected 240 questions, got {len(questions)}")
    corpus_path = corpus_path or retrieval_path.with_name("corpus.json")
    corpus = json.loads(corpus_path.read_text(encoding="utf-8"))
    sources: dict[tuple[str, str, str], str] = {}
    for conv in corpus.get("conversations", []):
        for segment in conv.get("segments", []):
            src = segment.get("source", {})
            seen_indices = set()
            for turn in segment.get("turns", []):
                index = turn.get("original_index")
                if isinstance(index, bool) or not isinstance(index, int) or index <= 0 or index in seen_indices:
                    raise ValueError("source turn original_index must be positive and unique within its segment")
                seen_indices.add(index)
                sources[(src.get("segment_id"), src.get("transcript_revision"), turn.get("id"))] = src.get("conversation_id")
    if len(sources) < 80:
        raise ValueError("v2 corpus must contain at least 80 independently authored source turns")

    ids: set[str] = set()
    normalized: set[str] = set()
    group_splits: dict[str, str] = {}
    group_questions: dict[str, list[dict]] = {}
    source_splits: dict[str, str] = {}
    split_questions = {name: 0 for name in SPLIT_QUESTIONS}
    positive_counts = {bucket: 0 for bucket in POSITIVE_BUCKETS}
    bucket_split_groups = {bucket: {split: set() for split in SPLIT_GROUPS} for bucket in POSITIVE_BUCKETS}
    referenced_source_splits: dict[tuple[str, str, str], str] = {}
    positive = negative = 0
    for q in questions:
        qid, group, split = q.get("id"), q.get("group_id"), q.get("split")
        if not qid or qid in ids:
            raise ValueError(f"missing or duplicate query id: {qid!r}")
        ids.add(qid)
        if not group or split not in split_questions:
            raise ValueError(f"query {qid} requires a concept group and valid split")
        if group in group_splits and group_splits[group] != split:
            raise ValueError(f"concept group {group} crosses splits")
        group_splits[group] = split
        group_questions.setdefault(group, []).append(q)
        split_questions[split] += 1
        query = "".join(q.get("query", "").split()).casefold()
        if not query or not CJK.search(query) or query in normalized:
            raise ValueError(f"query {qid} is empty, non-Chinese, or duplicated")
        normalized.add(query)
        source_group = q.get("source_group_id")
        if not source_group:
            raise ValueError(f"query {qid} lacks source_group_id")
        if source_group in source_splits and source_splits[source_group] != split:
            raise ValueError(f"source group {source_group} crosses splits")
        source_splits[source_group] = split
        if q.get("unanswerable", False):
            negative += 1
            if q.get("evidence_completeness") != "none":
                raise ValueError(f"no-evidence query {qid} requires a none evidence-completeness label")
            if q.get("gold_turn_refs") or q.get("gold_evidence_turn_ids"):
                raise ValueError(f"no-evidence query {qid} has gold evidence")
        else:
            positive += 1
            if q.get("evidence_completeness") != "complete":
                raise ValueError(f"answerable query {qid} must label all required evidence complete")
            bucket = q.get("bucket")
            if bucket not in POSITIVE_BUCKETS:
                raise ValueError(f"positive query {qid} has unknown bucket {bucket!r}")
            positive_counts[bucket] += 1
            refs = q.get("gold_turn_refs") or []
            if not refs:
                raise ValueError(f"positive query {qid} lacks source references")
            for ref in refs:
                key = (ref.get("segment_id"), ref.get("transcript_revision"), ref.get("turn_id"))
                if key not in sources:
                    raise ValueError(f"query {qid} references missing source {key}")
                if key in referenced_source_splits and referenced_source_splits[key] != split:
                    raise ValueError(f"referenced source {key} crosses splits")
                referenced_source_splits[key] = split
            bucket_split_groups[bucket][split].add(group)
            if bucket == "cross_session":
                conversations = {sources[(r.get("segment_id"), r.get("transcript_revision"), r.get("turn_id"))] for r in refs}
                if len(conversations) < 2:
                    raise ValueError(f"cross-session query {qid} must reference at least two sessions")
    if (positive, negative) != (160, 80):
        raise ValueError(f"expected 160 positive / 80 no-evidence queries, got {positive} / {negative}")
    if positive_counts != {"synonym": 40, "negation": 40, "time": 40, "cross_session": 40}:
        raise ValueError(f"positive bucket counts are invalid: {positive_counts}")
    wanted_bucket_groups = {"train": 10, "calibration": 5, "test": 5}
    for bucket, splits in bucket_split_groups.items():
        counts = {split: len(groups) for split, groups in splits.items()}
        if counts != wanted_bucket_groups:
            raise ValueError(f"positive bucket {bucket} is not independently balanced by split: {counts}")
    for group, items in group_questions.items():
        no_evidence = {bool(item.get("unanswerable", False)) for item in items}
        wanted = 1 if no_evidence == {True} else 2
        if len(no_evidence) != 1 or len(items) != wanted:
            raise ValueError(f"concept group {group} has invalid composition")
        if len({item.get("source_group_id") for item in items}) != 1:
            raise ValueError(f"concept group {group} maps to multiple source groups")
        if no_evidence != {True}:
            required_groups = [tuple(sorted((ref.get("segment_id"), ref.get("transcript_revision"), ref.get("turn_id"))
                                            for ref in item.get("gold_turn_refs", []))) for item in items]
            if len(set(required_groups)) != 1:
                raise ValueError(f"positive concept group {group} has inconsistent required evidence")
    group_counts = {split: sum(value == split for value in group_splits.values()) for split in SPLIT_GROUPS}
    if group_counts != SPLIT_GROUPS or split_questions != SPLIT_QUESTIONS:
        raise ValueError(f"expected group/question splits {SPLIT_GROUPS}/{SPLIT_QUESTIONS}, got {group_counts}/{split_questions}")
    return {"questions": len(questions), "positive": positive, "no_evidence": negative,
            "split_group_counts": group_counts, "split_question_counts": split_questions,
            "positive_bucket_counts": positive_counts, "source_groups": len(source_splits),
            "source_turns": len(sources), "complete_positive_evidence_groups": positive,
            "fixture_cross_split_leaks": 0}


def verify_frozen_hashes_v3(root: Path) -> dict[str, str]:
    manifest = json.loads((root / "SHA256SUMS.json").read_text(encoding="utf-8"))
    expected_files = {"corpus.json", "retrieval.json", "lifecycle.json"}
    if set(manifest) != expected_files:
        raise ValueError("v3 frozen hash manifest must pin corpus.json, retrieval.json and lifecycle.json")
    actual = {name: digest(root / name) for name in sorted(expected_files)}
    if actual != manifest:
        raise ValueError(f"v3 frozen fixture hash mismatch: expected {manifest}, got {actual}")
    return actual


def validate_fixture_v3(retrieval_path: Path, corpus_path: Path, lifecycle_path: Path) -> dict:
    payload = json.loads(retrieval_path.read_text(encoding="utf-8"))
    questions = payload.get("questions", [])
    if len(questions) != 780:
        raise ValueError(f"expected 780 v3 questions, got {len(questions)}")
    corpus = json.loads(corpus_path.read_text(encoding="utf-8"))
    sources: dict[tuple, str] = {}
    conversations: dict[str, str] = {}
    for conv in corpus.get("conversations", []):
        conversation_id = conv.get("conversation_id")
        for segment in conv.get("segments", []):
            src = segment.get("source", {})
            for turn in segment.get("turns", []):
                key = (src.get("segment_id"), src.get("transcript_revision"), turn.get("id"))
                if key in sources:
                    raise ValueError(f"duplicate source turn {key}")
                sources[key] = conversation_id
                conversations[conversation_id] = src.get("recording_id") or conversation_id
    if not sources:
        raise ValueError("v3 corpus has no source turns")
    ids: set[str] = set()
    normalized: set[str] = set()
    group_splits: dict[str, str] = {}
    source_group_splits: dict[str, str] = {}
    segment_splits: dict[str, str] = {}
    split_questions = {name: 0 for name in V3_SPLIT_QUESTIONS}
    split_groups = {name: 0 for name in V3_SPLIT_GROUPS}
    bucket_groups = {bucket: {split: set() for split in V3_SPLIT_GROUPS} for bucket in V3_BUCKETS}
    group_questions: dict[str, list[dict]] = {}
    lifecycle_turns: set[tuple] = set()
    lifecycle = json.loads(lifecycle_path.read_text(encoding="utf-8"))
    if lifecycle.get("schema") != "retrieval-lifecycle-v1":
        raise ValueError("lifecycle fixture schema must be retrieval-lifecycle-v1")
    for item in lifecycle.get("sources", []):
        key = (item.get("segment_id"), item.get("transcript_revision"), item.get("turn_id"))
        if key not in sources:
            raise ValueError(f"lifecycle source {key} is not in the corpus")
        lifecycle_turns.add(key)
    if not lifecycle_turns:
        raise ValueError("lifecycle fixture forgets no source")
    forgotten_groups: set[str] = set()
    for q in questions:
        qid, group, split = q.get("id"), q.get("group_id"), q.get("split")
        if not qid or qid in ids:
            raise ValueError(f"missing or duplicate v3 query id: {qid!r}")
        ids.add(qid)
        if not group or split not in V3_SPLIT_QUESTIONS:
            raise ValueError(f"v3 query {qid} requires a group and a valid split")
        if group_splits.setdefault(group, split) != split:
            raise ValueError(f"v3 group {group} crosses splits")
        group_questions.setdefault(group, []).append(q)
        split_questions[split] += 1
        query = "".join(q.get("query", "").split()).casefold()
        if not query or not CJK.search(query) or query in normalized:
            raise ValueError(f"v3 query {qid} is empty, non-Chinese, or duplicated")
        normalized.add(query)
        source_group = q.get("source_group_id")
        if not source_group or source_group_splits.setdefault(source_group, split) != split:
            raise ValueError(f"v3 query {qid} has a missing or cross-split source group")
        bucket = q.get("bucket")
        if bucket not in V3_BUCKETS:
            raise ValueError(f"v3 query {qid} has unknown bucket {bucket!r}")
        bucket_groups[bucket][split].add(group)
        completeness = q.get("evidence_completeness")
        expected = q.get("expected_sufficiency")
        refs = q.get("gold_turn_refs") or []
        for ref in refs:
            key = (ref.get("segment_id"), ref.get("transcript_revision"), ref.get("turn_id"))
            if key not in sources:
                raise ValueError(f"v3 query {qid} references missing source {key}")
            segment_splits.setdefault(ref.get("segment_id"), split)
            if segment_splits[ref.get("segment_id")] != split:
                raise ValueError(f"segment {ref.get('segment_id')} crosses splits")
            if key in lifecycle_turns:
                forgotten_groups.add(group)
                if not q.get("expect_forgotten_excluded"):
                    raise ValueError(f"v3 query {qid} cites a forgotten source without expect_forgotten_excluded")
        if completeness == "complete":
            if q.get("unanswerable") or expected != "sufficient" or not refs:
                raise ValueError(f"v3 complete query {qid} must be answerable, sufficient and cite evidence")
            if bucket == "cross_session":
                if len({sources[(r.get("segment_id"), r.get("transcript_revision"), r.get("turn_id"))] for r in refs}) < 2:
                    raise ValueError(f"v3 cross-session query {qid} must cite two sessions")
            if bucket == "multi_source" and len(refs) < 2:
                raise ValueError(f"v3 multi-source query {qid} must cite at least two turns")
        elif completeness == "none":
            if not q.get("unanswerable") or refs or expected != "insufficient":
                raise ValueError(f"v3 none query {qid} must be unanswerable, insufficient and cite nothing")
        elif completeness == "partial":
            if q.get("unanswerable") or expected != "insufficient":
                raise ValueError(f"v3 partial query {qid} must be answerable with insufficient expectation")
            if bucket not in V3_PARTIAL_BUCKETS:
                raise ValueError(f"v3 partial query {qid} is in a bucket that must not have partial questions")
        else:
            raise ValueError(f"v3 query {qid} has invalid evidence_completeness {completeness!r}")
    for group, items in group_questions.items():
        bucket = items[0].get("bucket")
        wanted = 3 if bucket in V3_PARTIAL_BUCKETS else 2
        if len(items) != wanted:
            raise ValueError(f"v3 group {group} has {len(items)} questions, want {wanted}")
        labels = sorted(item.get("evidence_completeness") for item in items)
        if labels.count("complete") != 1 or labels.count("none") != 1 or labels.count("partial") != (wanted - 2):
            raise ValueError(f"v3 group {group} has invalid completeness composition {labels}")
        if len({item.get("source_group_id") for item in items}) != 1:
            raise ValueError(f"v3 group {group} maps to multiple source groups")
    for bucket in V3_BUCKETS:
        for split in V3_SPLIT_GROUPS:
            if len(bucket_groups[bucket][split]) != 10:
                raise ValueError(f"v3 bucket {bucket}/{split} has {len(bucket_groups[bucket][split])} groups, want 10")
    for split in V3_SPLIT_GROUPS:
        groups = {group for group, value in group_splits.items() if value == split}
        split_groups[split] = len(groups)
    if split_groups != V3_SPLIT_GROUPS or split_questions != V3_SPLIT_QUESTIONS:
        raise ValueError(f"v3 split totals are invalid: groups={split_groups} questions={split_questions}")
    if not forgotten_groups:
        raise ValueError("lifecycle sources are not owned by any question group")
    return {"questions": len(questions), "groups": len(group_questions), "splits": split_groups,
            "split_questions": split_questions, "bucket_counts": {b: len(bucket_groups[b]["calibration"]) for b in V3_BUCKETS},
            "lifecycle_sources": len(lifecycle_turns), "partial_buckets": sorted(V3_PARTIAL_BUCKETS),
            "fixture_cross_split_leaks": 0}


def embedding_identity(backend: str, args: argparse.Namespace) -> tuple[str, str]:
    if backend == "hash":
        return ("hash-ngram-v1", "hash-ngram-v1")
    return ("openai-compat", args.embed_model or "")


def admission_policy_document(args: argparse.Namespace, backend: str, threshold_hundredths: int,
                              fixture_sha256: str, phase: str) -> dict:
    provider_id, model_id = embedding_identity(backend, args)
    return {"schema_version": "evidence-admission-policy-v1", "retrieval_version": "v3",
            "provider_id": provider_id, "model_id": model_id, "model_revision": args.embed_revision,
            "assessor_model": args.assessor_model, "fixture_sha256": fixture_sha256,
            "threshold": threshold_hundredths / 100, "phase": phase}


def provider_status(provider: str, config: dict) -> str:
    if provider in ("lexical", "hash"):
        return "CONFIGURED"
    if provider == "openai":
        required = (config.get("endpoint"), config.get("model"), config.get("dims"))
        if all(required) and int(config["dims"]) > 0:
            key_env = config.get("key_env", "")
            if not key_env or config.get("key_configured", False):
                return "CONFIGURED"
    return "NOT_RUN"


def validate_selection_lock(lock: dict, fixture_hashes: dict, configuration_hash: str,
                            binary_hash: str) -> dict:
    selected = lock.get("selected") if isinstance(lock, dict) else None
    if (not isinstance(lock, dict) or lock.get("fixture_sha256") != fixture_hashes or
            lock.get("configuration_sha256") != configuration_hash or lock.get("binary_sha256") != binary_hash or
            lock.get("holdout_started") or not isinstance(selected, dict) or selected.get("version") != "v2" or
            selected.get("backend") not in ("lexical", "hash", "openai") or
            not lock.get("calibration_gate", {}).get("passed")):
        raise ValueError("selection lock does not pin this fixture, configuration, binary and eligible candidate")
    if not isinstance(selected.get("rerank"), bool):
        raise ValueError("selection lock has no valid reranker setting")
    return selected


def safe_command(command: list[str]) -> list[str]:
    sensitive_flags = {"--embed-endpoint", "--rerank-decision-url", "--assessor-decision-url"}
    safe = list(command)
    for index, value in enumerate(safe[:-1]):
        if value in sensitive_flags:
            safe[index + 1] = "[configured endpoint]"
    return safe


class ReportProtocolError(ValueError):
    pass


def _required_number(data: dict, key: str, *, integer: bool = False) -> float | int:
    if key not in data:
        raise ReportProtocolError(f"report is missing required metric {key}")
    value = data[key]
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or value < 0:
        raise ReportProtocolError(f"report metric {key} must be a finite non-negative number")
    if integer and (not isinstance(value, int) or value < 0):
        raise ReportProtocolError(f"report metric {key} must be a non-negative integer")
    return value


def extract_run_metrics(report: dict, split_path: Path, split: str, version: str, provider: str,
                        args: argparse.Namespace) -> dict:
    if not isinstance(report, dict) or not isinstance(report.get("metrics"), dict):
        raise ReportProtocolError("report has no metrics object")
    m = report["metrics"]
    mode = "lexical" if provider == "lexical" else "hybrid"
    recall_key = "lexical_recall_at_5" if mode == "lexical" else "hybrid_recall_at_5"
    mrr_key = f"retrieval_{mode}_mrr_at_10"
    p95_key = f"retrieval_{mode}_latency_ms_p95"
    selected_recall = _required_number(m, recall_key)
    selected_mrr = _required_number(m, mrr_key)
    p95 = _required_number(m, p95_key)
    for key in ("retrieval_requests", "retrieval_invalid_evidence_refs", "semantic_provider_embed_errors",
                "semantic_provider_embed_requests", "retrieval_rerank_failures", "evidence_assessment_failures",
                "model_calls", "scope_escape_count"):
        _required_number(m, key, integer=True)
    mode_invalid = {name: _required_number(m, f"retrieval_{name}_invalid_evidence_refs", integer=True)
                    for name in ("lexical", "hybrid")}
    if sum(mode_invalid.values()) != m["retrieval_invalid_evidence_refs"]:
        raise ReportProtocolError("retrieval invalid-ref total does not match its mode counters")
    if "evidence_assessor_configured" not in m or not isinstance(m["evidence_assessor_configured"], bool):
        raise ReportProtocolError("report is missing boolean evidence_assessor_configured")
    questions_payload = json.loads(split_path.read_text(encoding="utf-8"))
    expected = questions_payload.get("questions")
    if not isinstance(expected, list) or not expected:
        raise ReportProtocolError("split fixture has no questions")
    expected_ids = {q.get("id") for q in expected}
    if None in expected_ids or len(expected_ids) != len(expected):
        raise ReportProtocolError("split fixture query ids are missing or duplicated")
    all_results = report.get("per_question")
    if not isinstance(all_results, list):
        raise ReportProtocolError("report is missing per_question results")
    if any(not isinstance(q, dict) for q in all_results):
        raise ReportProtocolError("per_question contains a malformed entry")
    retrieval_results = [q for q in all_results if q.get("split") == split]
    rows = [q for q in retrieval_results if q.get("mode") == mode]
    if len(rows) != len(expected) or {q.get("id") for q in rows} != expected_ids:
        raise ReportProtocolError(f"report has {len(rows)} {mode} query rows, expected {len(expected)}")
    expected_modes = {mode} if provider == "lexical" else {"lexical", "hybrid"}
    if {q.get("mode") for q in retrieval_results} != expected_modes:
        raise ReportProtocolError("report contains unexpected retrieval modes")
    if m["retrieval_requests"] != len(retrieval_results):
        raise ReportProtocolError("retrieval_requests does not match retrieval per_question rows")
    if sum(_required_number(q, "model_calls", integer=True) for q in all_results) != m["model_calls"]:
        raise ReportProtocolError("model_calls does not match per_question call counts")
    if sum(bool(q.get("rerank_failed")) for q in retrieval_results) != m["retrieval_rerank_failures"]:
        raise ReportProtocolError("rerank failure count does not match per_question flags")
    if sum(bool(q.get("assessment_failed")) for q in retrieval_results) != m["evidence_assessment_failures"]:
        raise ReportProtocolError("assessment failure count does not match per_question flags")
    if {q.get("mode") for q in retrieval_results} == {"lexical", "hybrid"} and len(retrieval_results) != 2 * len(expected):
        raise ReportProtocolError("hybrid evaluation must report both lexical and hybrid rows for every query")
    required_question_fields = ("split", "bucket", "unanswerable", "returned_items", "stale_items",
                                "ambient_items", "scope_escapes", "latency_ms", "reciprocal_rank_at_10",
                                "recall_at_5", "gold_evidence_count", "gold_evidence_found",
                                "final_gold_evidence_found", "rerank_calls",
                                "assessor_calls", "model_calls", "assessment_failed", "rerank_failed",
                                "expected_sufficiency", "evidence_completeness")
    sufficiency_expectations: dict[str, str] = {}
    for row in rows:
        if any(key not in row for key in required_question_fields):
            missing = [key for key in required_question_fields if key not in row]
            raise ReportProtocolError(f"question {row.get('id')} is missing fields: {', '.join(missing)}")
        if row["split"] != split or not isinstance(row["unanswerable"], bool):
            raise ReportProtocolError(f"question {row['id']} has an invalid split or answerability label")
        if "expect_forgotten_excluded" in row and not isinstance(row["expect_forgotten_excluded"], bool):
            raise ReportProtocolError(f"question {row['id']} has an invalid forgotten-exclusion label")
        if "recall_eligible" in row and not isinstance(row["recall_eligible"], bool):
            raise ReportProtocolError(f"question {row['id']} has an invalid recall eligibility label")
        authored = next(q for q in expected if q.get("id") == row["id"])
        if row["bucket"] != authored.get("bucket") or row["unanswerable"] != bool(authored.get("unanswerable", False)):
            raise ReportProtocolError(f"question {row['id']} has mismatched fixture labels")
        for key in ("returned_items", "stale_items", "ambient_items", "scope_escapes", "gold_evidence_count",
                    "gold_evidence_found", "final_gold_evidence_found", "rerank_calls", "assessor_calls", "model_calls"):
            _required_number(row, key, integer=True)
        if row["final_gold_evidence_found"] > row["gold_evidence_count"]:
            raise ReportProtocolError(f"question {row['id']} over-counts final required evidence")
        for key in ("latency_ms", "reciprocal_rank_at_10", "recall_at_5"):
            _required_number(row, key)
        if not isinstance(row["assessment_failed"], bool) or not isinstance(row["rerank_failed"], bool):
            raise ReportProtocolError(f"question {row['id']} has invalid model failure flags")
        authored_expected = authored.get("expected_sufficiency")
        expected_status = row["expected_sufficiency"]
        legacy_unknown = (getattr(args, "protocol", "v2") == "v2" and row["unanswerable"]
                          and authored_expected == expected_status == "unknown")
        if expected_status not in ("sufficient", "insufficient") and not legacy_unknown:
            raise ReportProtocolError(f"question {row['id']} has invalid expected sufficiency")
        complete_final_evidence = (not row["unanswerable"] and row["gold_evidence_count"] > 0 and
                                   row["final_gold_evidence_found"] == row["gold_evidence_count"])
        if authored_expected:
            if row["expected_sufficiency"] != authored_expected:
                raise ReportProtocolError(f"question {row['id']} expected sufficiency disagrees with the fixture label")
        else:
            expected_status = "sufficient" if complete_final_evidence else "insufficient"
            if row["expected_sufficiency"] != expected_status:
                raise ReportProtocolError(f"question {row['id']} expected sufficiency disagrees with final evidence coverage")
        # The final visible evidence decides the sufficiency accounting, not the
        # authored label: an authored "sufficient" question that dropped a
        # required ref is judged by its final coverage. Explicit "insufficient"
        # labels are retained, and the v2 legacy "unknown" label is retained but
        # never counted as a match.
        sufficiency_expectation = "sufficient" if complete_final_evidence else "insufficient"
        if authored_expected in ("insufficient", "unknown"):
            sufficiency_expectation = authored_expected
        sufficiency_expectations[row["id"]] = sufficiency_expectation
        if version == "v2" and row.get("sufficiency_status") not in ("sufficient", "insufficient", "unknown"):
            raise ReportProtocolError(f"question {row['id']} is missing a valid sufficiency status")
    answerable = [q for q in rows if not q["unanswerable"] and q.get("recall_eligible", True)]
    negatives = [q for q in rows if q["unanswerable"]]
    false_positive_queries = sum(q["returned_items"] > 0 for q in negatives)
    if len(negatives) == 0 or len(answerable) == 0:
        raise ReportProtocolError("split must contain positive and no-evidence queries")
    if not isinstance(m.get("retrieval_buckets"), dict):
        raise ReportProtocolError("report is missing retrieval_buckets")
    expected_buckets = {q.get("bucket") for q in expected}
    buckets: dict[str, dict] = {}
    for bucket in expected_buckets:
        key = f"{split}/{mode}/{bucket}"
        metric = m["retrieval_buckets"].get(key)
        if not isinstance(metric, dict):
            raise ReportProtocolError(f"report is missing bucket metrics for {key}")
        for field in ("questions", "answerable_questions", "no_evidence_questions", "false_positive_queries",
                      "stale_items", "ambient_items", "scope_escapes"):
            _required_number(metric, field, integer=True)
        for field in ("recall_at_5", "mrr_at_10", "false_positive_rate", "latency_ms_p50", "latency_ms_p95"):
            _required_number(metric, field)
        expected_bucket_count = sum(q.get("bucket") == bucket for q in expected)
        if metric["questions"] != expected_bucket_count:
            raise ReportProtocolError(f"bucket {key} has {metric['questions']} questions, expected {expected_bucket_count}")
        buckets[key] = metric
    assessed = [(q, sufficiency_expectations[q["id"]]) for q in rows if q["assessor_calls"] > 0]
    false_sufficient_denominator = sum(expectation == "insufficient" for _q, expectation in assessed)
    false_sufficient = sum(expectation == "insufficient" and q.get("sufficiency_status") == "sufficient"
                           for q, expectation in assessed)
    assessor_configured = m["evidence_assessor_configured"]
    false_sufficient_rate = None
    if assessor_configured and false_sufficient_denominator:
        false_sufficient_rate = false_sufficient / false_sufficient_denominator
    failures = {"embedding": m["semantic_provider_embed_errors"],
                "rerank": sum(q["rerank_failed"] for q in rows),
                "assessment": sum(q["assessment_failed"] for q in rows)}
    forgotten_leakage = 0
    admission_failures = 0
    if version == "v3":
        if any("recall_eligible" not in q for q in rows):
            raise ReportProtocolError("v3 question rows must declare recall_eligible")
        forgotten_leakage = _required_number(m, "forgotten_leakage", integer=True)
        admission_failures = _required_number(m, "admission_failures", integer=True)
        if sum(_required_number(q, "forgotten_leakage", integer=True) for q in rows) != forgotten_leakage:
            raise ReportProtocolError("forgotten_leakage total does not match per_question rows")
    metrics = {"split": split, "mode": mode, "recall_at_5": selected_recall, "mrr_at_10": selected_mrr,
            "false_positive_rate": false_positive_queries / len(negatives),
            "false_positive_queries": false_positive_queries, "no_evidence_denominator": len(negatives),
            "answerable_denominator": len(answerable), "recall_denominator": len(answerable),
            "mrr_denominator": len(answerable), "p95_denominator": len(rows), "p95_ms": p95,
            "stale_leaks": sum(q["stale_items"] for q in rows),
            "ambient_leaks": sum(q["ambient_items"] for q in rows),
            "returned_items_denominator": sum(q["returned_items"] for q in rows),
            "scope_leaks": sum(q["scope_escapes"] for q in rows),
            "invalid_refs": m[f"retrieval_{mode}_invalid_evidence_refs"],
            "fixture_cross_split_leaks": args.fixture_cross_split_leaks,
            "provider": provider, "rerank_configured": bool(args.rerank_url and args.rerank_model),
            "assessor_configured": assessor_configured, "false_sufficient": false_sufficient,
            "false_sufficient_denominator": false_sufficient_denominator,
            "false_sufficient_rate": false_sufficient_rate,
            "sufficiency": {"status_counts": {status: sum(q.get("sufficiency_status") == status for q, _expectation in assessed)
                                                for status in ("sufficient", "insufficient", "unknown")},
                            "scored": len(assessed),
                            "matches": sum(expectation != "unknown"
                                           and expectation == q.get("sufficiency_status")
                                           for q, expectation in assessed)},
            "embedding_invocations": _required_number(m, "semantic_provider_embed_requests", integer=True),
            "model_calls": sum(q["model_calls"] for q in rows),
            "rerank_calls": sum(q["rerank_calls"] for q in rows),
            "assessor_calls": sum(q["assessor_calls"] for q in rows),
            "failure_counts": failures, "bucket_metrics": buckets,
            "forgotten_leakage": forgotten_leakage, "admission_failures": admission_failures,
            "none_false_positive_rate": false_positive_queries / len(negatives),
            "all_unanswerable_false_positive_rate": false_positive_queries / len(negatives),
            "stage_latencies": m.get("stage_latencies") if isinstance(m.get("stage_latencies"), dict) else {}}
    # The recall denominator lives in the report rows, not in this aggregate:
    # v3-protocol runs (the v1 baseline included) publish the exact eligible id
    # set so the gate can prove both sides scored the same unforgotten
    # questions. Legacy v2-protocol runs keep their shape.
    if getattr(args, "protocol", "v2") == "v3":
        metrics["recall_eligible_ids"] = sorted(q["id"] for q in rows if q.get("recall_eligible") is True)
    return metrics


def select_v3_candidate(candidates: list[dict]) -> dict | None:
    """Recall@5 desc, MRR@10 desc, p95 asc, stricter threshold first."""
    eligible = [candidate for candidate in candidates if candidate.get("gate", {}).get("passed")]
    if not eligible:
        return None
    eligible.sort(key=lambda candidate: (
        -metric_number(candidate["metrics"], "recall_at_5", 0),
        -metric_number(candidate["metrics"], "mrr_at_10", 0),
        metric_number(candidate["metrics"], "p95_ms", float("inf")),
        -candidate["threshold_hundredths"],
    ))
    return eligible[0]


def atomic_write_json(path: Path, payload: dict) -> None:
    tmp = path.with_name(path.name + ".tmp")
    with tmp.open("w", encoding="utf-8") as handle:
        json.dump(payload, handle, ensure_ascii=False, indent=2)
        handle.write("\n")
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(tmp, path)
    directory = os.open(path.parent, os.O_RDONLY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)


def claim_holdout(lock_path: Path) -> None:
    claim = lock_path.with_name(lock_path.name + ".consumed")
    handle = os.open(claim, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    try:
        os.write(handle, b"claimed\n")
        os.fsync(handle)
    finally:
        os.close(handle)


def prepare_out_dir(path: Path) -> None:
    """Require a new or empty output directory for every v3 phase.

    Overwriting a directory that already contains a calibration, a lock or a
    holdout would silently reuse or destroy consumed evidence, so it is refused
    instead of cleaned.
    """
    if path.exists():
        if not path.is_dir():
            raise ValueError(f"output path exists and is not a directory: {path}")
        if any(path.iterdir()):
            raise ValueError(f"output directory must be new or empty; refusing to overwrite {path}")
    path.mkdir(parents=True, exist_ok=True)


CONFIG_FLAGS = {
    "embed_endpoint": "embedding_endpoint",
    "embed_model": "embedding_model",
    "embed_dims": "embedding_dims",
    "embed_input_type": "embedding_input_type",
    "embed_key_env": "embedding_key_env",
    "embed_revision": "embedding_revision",
    "rerank_url": "rerank_endpoint",
    "rerank_model": "rerank_model",
    "assessor_url": "assessor_endpoint",
    "assessor_model": "assessor_model",
}


def flag_defaults() -> dict:
    """Values used when a v3 flag is absent, so an absent flag is not explicit."""
    return {"embed_endpoint": "", "embed_model": "", "embed_dims": 0, "embed_input_type": False,
            "embed_key_env": "", "embed_revision": "", "rerank_url": "", "rerank_model": "",
            "assessor_url": "", "assessor_model": "", "admission_batch_questions": 0,
            "flush_embed_cache_url": "", "max_items": 10, "max_bytes": 65536, "backends": "",
            "admission_probability_cache": "", "assessor_actual_model": ""}


def apply_flag_defaults(args: argparse.Namespace) -> dict:
    """Fill absent flags and return the values that were explicitly provided."""
    explicit = {}
    for flag, config_key in CONFIG_FLAGS.items():
        value = getattr(args, flag, None)
        if value is None:
            continue
        explicit[config_key] = value
    if getattr(args, "admission_batch_questions", None) is not None:
        explicit["admission_batch_questions"] = args.admission_batch_questions
    for name, value in flag_defaults().items():
        if getattr(args, name, None) is None:
            setattr(args, name, value)
    return explicit


def v3_configuration(args: argparse.Namespace) -> dict:
    """The complete calibration/selection configuration.

    It is the single source of truth for the configuration hash, the lock and
    the holdout restore, so a flag that is not listed here cannot silently
    drift between calibration and holdout.
    """
    return {
        "protocol": "v3",
        "embedding_endpoint": args.embed_endpoint,
        "embedding_model": args.embed_model,
        "embedding_dims": args.embed_dims,
        "embedding_input_type": bool(args.embed_input_type),
        "embedding_key_env": args.embed_key_env,
        "embedding_revision": args.embed_revision,
        "rerank_endpoint": args.rerank_url,
        "rerank_model": args.rerank_model,
        "assessor_endpoint": args.assessor_url,
        "assessor_model": args.assessor_model,
        "admission_batch_questions": args.admission_batch_questions,
        "max_items": args.max_items,
        "max_bytes": args.max_bytes,
    }


LOCK_BUDGET = {"max_items": 10, "max_bytes": 65536}
WARMUP = {"questions": 1, "source": "train", "cache_flush": True}
PROMPT_IDENTITY = "internal/contextbuild/admission.go:admissionQuestion"


def v3_identity(args: argparse.Namespace, binary_hash: str, embed_status: str) -> dict:
    """The actual model/runtime identity the lock pins."""
    actual = str(getattr(args, "assessor_actual_model", "") or "")
    configured = str(args.assessor_model or "")
    return {
        "embedding_provider": "openai-compat" if embed_status == "CONFIGURED" else "hash-ngram-v1",
        "embedding_model": args.embed_model or "",
        "embedding_revision": args.embed_revision or "",
        "embedding_dims": args.embed_dims,
        "embedding_input_type": bool(args.embed_input_type),
        "assessor_model": configured,
        "assessor_actual_model": actual,
        "binary_sha256": binary_hash,
        "prompt_identity": PROMPT_IDENTITY,
        "prompt_note": ("the admission prompt lives in the pinned binary; the policy template hash "
                        "pins the rubric and threshold document"),
    }


def identity_is_verifiable(identity: dict) -> tuple[bool, str]:
    """A floating alias cannot prove which model answered."""
    configured = str(identity.get("assessor_model") or "")
    actual = str(identity.get("assessor_actual_model") or "")
    if configured.endswith("latest") and not actual:
        return False, ("the assessor model is a floating 'latest' alias and no --assessor-actual-model "
                       "was supplied; the actual identity cannot be verified")
    if not str(identity.get("embedding_revision") or "").strip():
        return False, "the embedding revision is not pinned"
    return True, ""


def _selected_policy_signature(lock: dict, backend: str, threshold_hundredths: int,
                               fixture_sha256: str) -> str:
    """Rebuild the selected-phase admission policy document from the lock."""
    configuration = lock.get("configuration") if isinstance(lock, dict) else None
    configuration = configuration if isinstance(configuration, dict) else {}
    args = argparse.Namespace(embed_model=str(configuration.get("embedding_model") or ""),
                              embed_revision=str(configuration.get("embedding_revision") or ""),
                              assessor_model=str(configuration.get("assessor_model") or ""))
    document = admission_policy_document(args, backend, threshold_hundredths, fixture_sha256, "selected")
    return config_signature(document)


def validate_v3_lock(lock: dict, fixture_hashes: dict, configuration_hash: str, template_hash: str,
                     binary_hash: str) -> dict:
    selected = lock.get("selected") if isinstance(lock, dict) else None
    if (not isinstance(lock, dict) or lock.get("schema") != V3_LOCK_SCHEMA or
            lock.get("fixture_sha256") != fixture_hashes or
            lock.get("configuration_sha256") != configuration_hash or
            lock.get("policy_template_sha256") != template_hash or
            lock.get("binary_sha256") != binary_hash or lock.get("holdout_started") or
            not isinstance(selected, dict) or selected.get("version") != "v3" or
            selected.get("backend") not in ("hash", "openai") or
            not isinstance(selected.get("threshold_hundredths"), int) or
            isinstance(selected.get("threshold_hundredths"), bool) or
            not 0 <= selected["threshold_hundredths"] <= 100 or
            not lock.get("calibration_gate", {}).get("passed")):
        raise ValueError("selection lock does not pin this fixture, configuration, template, binary and eligible v3 candidate")
    if selected.get("threshold_hundredths") / 100 != selected.get("threshold"):
        raise ValueError("selection lock threshold does not match its hundredths grid value")
    if lock.get("selected_sha256") != config_signature(selected):
        raise ValueError("selection lock selected candidate does not match its own digest")
    if lock.get("selected_policy_sha256") != _selected_policy_signature(
            lock, selected["backend"], selected["threshold_hundredths"],
            fixture_hashes.get("retrieval.json", "")):
        raise ValueError("selection lock selected policy does not match its own digest")
    configuration = lock.get("configuration")
    if not isinstance(configuration, dict) or config_signature(configuration) != lock["configuration_sha256"]:
        raise ValueError("selection lock configuration is missing or does not match its own hash")
    budget = lock.get("budget")
    if (not isinstance(budget, dict) or
            (budget.get("max_items"), budget.get("max_bytes")) != (
                LOCK_BUDGET["max_items"], LOCK_BUDGET["max_bytes"]) or
            (configuration.get("max_items"), configuration.get("max_bytes")) != (
                LOCK_BUDGET["max_items"], LOCK_BUDGET["max_bytes"])):
        raise ValueError("selection lock does not pin the fixed retrieval budget")
    if configuration.get("protocol") != "v3" or "embedding_input_type" not in configuration:
        raise ValueError("selection lock configuration is incomplete")
    identity = lock.get("identity")
    if not isinstance(identity, dict) or identity.get("binary_sha256") != binary_hash:
        raise ValueError("selection lock identity is missing or names another binary")
    if identity.get("embedding_provider") != "openai-compat":
        # A hash-embedding calibration is diagnostic only: it cannot prove which
        # real model answered, so it never mints a production lock.
        raise ValueError("selection lock must pin a real openai-compat embedding provider")
    if lock.get("final_rerun_uncached") is not True:
        raise ValueError("selection lock must record a verified uncached final rerun")
    warmup = lock.get("warmup")
    if not isinstance(warmup, dict) or warmup.get("questions") != WARMUP["questions"]:
        raise ValueError("selection lock does not record the warmup rule")
    if not isinstance(selected.get("rerank"), bool):
        raise ValueError("selection lock has no valid reranker setting")
    if selected["rerank"] != bool(configuration.get("rerank_endpoint") and configuration.get("rerank_model")):
        raise ValueError("selection lock reranker setting does not match the restored configuration")
    return selected


def lock_configuration(lock: dict) -> dict:
    """Restore the calibrated configuration from a lock without guessing."""
    configuration = lock.get("configuration") if isinstance(lock, dict) else None
    if not isinstance(configuration, dict):
        raise ValueError("selection lock has no configuration to restore")
    if config_signature(configuration) != lock.get("configuration_sha256"):
        raise ValueError("selection lock configuration does not match its hash")
    missing = [key for key in CONFIG_FLAGS.values() if key not in configuration]
    if missing:
        raise ValueError(f"selection lock configuration is missing {sorted(missing)}")
    return configuration


def restore_lock_arguments(args: argparse.Namespace, lock: dict, explicit: dict) -> dict:
    """Apply the locked configuration, rejecting any conflicting explicit flag."""
    configuration = lock_configuration(lock)
    conflicts = {key: (explicit[key], configuration[key]) for key in explicit
                 if key != "embedding_key_env" and explicit[key] != configuration[key]}
    if conflicts:
        raise ValueError(f"explicit configuration conflicts with the selection lock: {sorted(conflicts)}")
    for flag, config_key in CONFIG_FLAGS.items():
        setattr(args, flag, configuration[config_key])
    setattr(args, "max_items", configuration["max_items"])
    setattr(args, "max_bytes", configuration["max_bytes"])
    args.admission_batch_questions = configuration["admission_batch_questions"]
    return configuration


def _split_file(source: Path, split: str, out: Path) -> Path:
    out.mkdir(parents=True, exist_ok=True)
    payload = json.loads(source.read_text(encoding="utf-8"))
    payload["questions"] = [q for q in payload["questions"] if q["split"] == split]
    path = out / f"retrieval-{split}.json"
    path.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return path


def flush_embed_cache(url: str) -> None:
    if not url:
        return
    request = urllib.request.Request(url, data=b"{}", method="POST",
                                     headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=10) as response:
        if response.status != 200:
            raise RuntimeError(f"embedding cache flush failed: HTTP {response.status}")
        payload = json.loads(response.read())
    if payload.get("status") != "ok":
        raise RuntimeError("embedding cache flush did not confirm")


def flush_embed_cache_safe(url: str) -> str:
    """Run the configured cache flush; return "" on success, else a safe reason.

    Expected transport/protocol failures become recorded evidence instead of an
    unhandled exception; anything outside that set still propagates.
    """
    if not url:
        return ""
    try:
        flush_embed_cache(url)
    except (RuntimeError, OSError, urllib.error.URLError, http.client.HTTPException) as exc:
        return f"{type(exc).__name__}: {exc}"
    return ""


def _run(binary: str, args: argparse.Namespace, split_path: Path, version: str,
         provider: str, rerank: bool, split: str, out: Path, extra: list[str] | None = None,
         corpus: Path | None = None, warmup: bool = False) -> dict:
    cmd = [binary, "--corpus", str(corpus or args.corpus), "--questions", "", "--links", "",
           "--retrieval", str(split_path), "--projection", "", "--max-items", "10", "--max-bytes", "65536",
           "--retrieval-version", version, "--embed-provider", "off" if provider == "lexical" else provider,
           "--out", str(out), "--timeout", f"{args.timeout}s"]
    if getattr(args, "admission_batch_questions", 0):
        cmd += ["--admission-batch-questions", str(args.admission_batch_questions)]
    if extra:
        cmd += extra
    if provider == "lexical":
        cmd += ["--lexical-only"]
    if provider == "openai":
        cmd += ["--embed-endpoint", args.embed_endpoint, "--embed-model", args.embed_model,
                "--embed-key-env", args.embed_key_env, "--embed-dims", str(args.embed_dims)]
        if args.embed_input_type:
            cmd += ["--embed-input-type"]
    if rerank:
        cmd += ["--rerank-decision-url", args.rerank_url, "--rerank-decision-model", args.rerank_model]
    if args.assessor_url and args.assessor_model:
        cmd += ["--assessor-decision-url", args.assessor_url, "--assessor-decision-model", args.assessor_model]
    Path(out).unlink(missing_ok=True)
    start = time.monotonic()
    try:
        proc = subprocess.run(cmd, check=False, capture_output=True, text=True, timeout=args.timeout + 10)
    except subprocess.TimeoutExpired as exc:
        return {"status": "TIMEOUT", "command": safe_command(cmd), "timeout_seconds": args.timeout,
                "elapsed_seconds": time.monotonic() - start,
                "stdout": str(exc.stdout or "")[-3000:], "stderr": str(exc.stderr or "")[-3000:]}
    except OSError as exc:
        # An expected spawn failure (for example the binary lost its exec bit
        # between the digest and the spawn, after a holdout claim was already
        # consumed): record the safe original reason as a FAIL result instead
        # of crashing without a manifest.
        return {"status": "FAIL", "command": safe_command(cmd), "exit_code": None,
                "elapsed_seconds": time.monotonic() - start, "stdout": "",
                "stderr": f"{type(exc).__name__}: {exc}", "spawn_error": True}
    result = {"status": "PASS" if proc.returncode == 0 and Path(out).exists() else "FAIL",
              "command": safe_command(cmd), "exit_code": proc.returncode, "elapsed_seconds": time.monotonic() - start,
              "stdout": proc.stdout[-3000:], "stderr": proc.stderr[-3000:]}
    if warmup:
        # A warmup is excluded from every metric: only the tool exit code and
        # the report's existence decide its status, so a single-question
        # warmup report can never be read as a full-eval result.
        return {**result, "warmup": True, "questions": 1}
    if result["status"] == "PASS":
        try:
            report = json.loads(Path(out).read_text(encoding="utf-8"))
            result["metrics"] = extract_run_metrics(report, split_path, split, version, provider, args)
        except (OSError, json.JSONDecodeError, ReportProtocolError, KeyError, TypeError, ValueError) as exc:
            result["status"] = "FAIL"
            result["protocol_error"] = str(exc)
            return result
        result["report_path"] = str(out)
        result["report_sha256"] = digest(Path(out))
        result["raw_metrics"] = report["metrics"]
    return result


def validate_flush_target(embed_endpoint: str, flush_url: str) -> None:
    """The cache flush must hit the embedding service the run actually uses.

    Copying a historical flush URL that names another port would claim an
    uncached measurement while the real service kept its cache.
    """
    if not flush_url:
        return
    endpoint = urllib.parse.urlsplit(embed_endpoint or "")
    flush = urllib.parse.urlsplit(flush_url)
    if not endpoint.hostname:
        raise ValueError("--flush-embed-cache-url requires --embed-endpoint")
    if (endpoint.hostname, endpoint.port) != (flush.hostname, flush.port):
        raise ValueError("--flush-embed-cache-url must target the embedding service's own host and port")


def split_stage_metrics(result: dict) -> dict:
    """Extract the latency and request accounting a profile run reports."""
    if result.get("status") != "PASS":
        return {}
    raw = result.get("raw_metrics") or {}
    stages = raw.get("stage_latencies") or {}
    return {
        "end_to_end_p50_ms": raw.get("retrieval_hybrid_latency_ms_p50"),
        "end_to_end_p95_ms": raw.get("retrieval_hybrid_latency_ms_p95"),
        "admission_p50_ms": (stages.get("admission") or {}).get("p50"),
        "admission_p95_ms": (stages.get("admission") or {}).get("p95"),
        "admission_n": (stages.get("admission") or {}).get("n"),
        "assessor_p50_ms": (stages.get("assessor") or {}).get("p50"),
        "assessor_p95_ms": (stages.get("assessor") or {}).get("p95"),
        "assessor_n": (stages.get("assessor") or {}).get("n"),
        "rerank_p50_ms": (stages.get("rerank") or {}).get("p50"),
        "rerank_p95_ms": (stages.get("rerank") or {}).get("p95"),
        "engine_p50_ms": raw.get("admission_engine_latency_ms_p50"),
        "engine_p95_ms": raw.get("admission_engine_latency_ms_p95"),
        "engine_calls": raw.get("admission_engine_calls"),
        "assessor_requests": raw.get("admission_requests"),
        "assessor_request_candidates": raw.get("admission_request_candidates"),
        "assessor_request_bytes": raw.get("admission_request_bytes"),
        "p95_denominator": raw.get("retrieval_requests"),
    }


def warmup_split(retrieval_path: Path, out: Path) -> Path:
    """The fixed first train question, used only to warm the model/runtime."""
    payload = json.loads(retrieval_path.read_text(encoding="utf-8"))
    train = [q for q in payload.get("questions", []) if q.get("split") == "train"]
    if not train:
        raise ValueError("the fixture has no train question for the warmup")
    payload["questions"] = [train[0]]
    path = out / "retrieval-warmup.json"
    path.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return path


def _warmup_run(args: argparse.Namespace, retrieval_path: Path, out: Path, version: str, backend: str,
                rerank: bool, corpus: Path, extra: list[str] | None = None) -> dict:
    """One warmup invocation on the train head, excluded from every metric.

    ``extra`` carries the real v3 policy and lifecycle of the measured run that
    follows, so the warmup exercises the same admission path and forget state.
    """
    path = warmup_split(retrieval_path, out)
    flush_error = flush_embed_cache_safe(args.flush_embed_cache_url)
    if flush_error:
        # The cache state is unproven, so the warmup question is not sent; the
        # failure becomes recorded evidence on the warmup entry.
        return {"version": version, "backend": backend, "status": "NOT_RUN",
                "reason": f"embedding cache flush failed before the warmup: {flush_error}",
                "flush_error": flush_error,
                "cache_flushed": bool(args.flush_embed_cache_url), "questions": 0}
    result = _run(args.binary, args, path, version, backend, rerank, "train",
                  out / f"warmup-{version}-{backend}.json", extra=extra, corpus=corpus, warmup=True)
    entry = {"version": version, "backend": backend, "status": result.get("status"),
             "exit_code": result.get("exit_code"), "command": result.get("command"),
             "stderr": (result.get("stderr") or "")[-800:],
             "cache_flushed": bool(args.flush_embed_cache_url), "questions": 1}
    post_error = flush_embed_cache_safe(args.flush_embed_cache_url)
    if post_error:
        entry["flush_error"] = post_error
    return entry


def run_v3(args: argparse.Namespace) -> int:
    root = Path(args.fixture_dir)
    corpus_path = root / "corpus.json"
    retrieval_path = root / "retrieval.json"
    lifecycle_path = Path(args.lifecycle) if args.lifecycle else root / "lifecycle.json"
    explicit = apply_flag_defaults(args)
    hashes = verify_frozen_hashes_v3(root)
    if digest(retrieval_path) != hashes["retrieval.json"]:
        raise ValueError("v3 retrieval fixture must be the pinned frozen bytes")
    if digest(lifecycle_path) != hashes["lifecycle.json"]:
        raise ValueError("v3 lifecycle fixture must be the pinned frozen bytes")
    fixture_summary = validate_fixture_v3(retrieval_path, corpus_path, lifecycle_path)
    if (args.max_items, args.max_bytes) != (LOCK_BUDGET["max_items"], LOCK_BUDGET["max_bytes"]):
        raise ValueError("quality protocol fixes --max-items=10 and --max-bytes=65536")
    if args.phase != "holdout" and not args.embed_revision:
        # A holdout restores the revision from the lock instead of guessing.
        raise ValueError("v3 requires --embed-revision to pin the embedding model revision")
    out = Path(args.out)
    prepare_out_dir(out)
    binary_hash = digest(Path(args.binary))
    fixture_sha = hashes["retrieval.json"]
    embed_config = {"endpoint": args.embed_endpoint, "model": args.embed_model, "dims": args.embed_dims,
                    "key_env": args.embed_key_env,
                    "key_configured": bool(os.environ.get(args.embed_key_env, "")) if args.embed_key_env else True}
    embedding_status = provider_status("openai", embed_config)
    backends = ["hash"] + (["openai"] if embedding_status == "CONFIGURED" else [])
    if args.backends:
        wanted = {part.strip() for part in args.backends.split(",") if part.strip()}
        unknown = wanted - {"hash", "openai"}
        if unknown:
            raise ValueError(f"unknown backends: {sorted(unknown)}")
        backends = [backend for backend in backends if backend in wanted]
        if not backends:
            raise ValueError("no configured backend matches --backends")
    validate_flush_target(args.embed_endpoint, args.flush_embed_cache_url)
    configuration = v3_configuration(args)
    configuration_hash = config_signature(configuration)
    identity = v3_identity(args, binary_hash, "CONFIGURED" if "openai" in backends else "NOT_RUN")

    def template_hash_for(backend: str) -> str:
        template = admission_policy_document(args, backend, 0, fixture_sha, "selected")
        template.pop("threshold", None)
        template.pop("phase", None)
        return config_signature(template)

    def policy_file(directory: Path, backend: str, threshold_hundredths: int, phase: str) -> Path:
        directory.mkdir(parents=True, exist_ok=True)
        path = directory / f"policy-{backend}-{threshold_hundredths:03d}-{phase}.json"
        path.write_text(json.dumps(admission_policy_document(args, backend, threshold_hundredths, fixture_sha, phase),
                                   ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        return path

    results = {"protocol": "v3", "phase": args.phase, "fixture_sha256": hashes, "fixture": fixture_summary,
               "protocol_config": {"max_items": 10, "max_bytes": 65536, "test_touched": False,
                                   "threshold_grid": [0, 100], "latency_comparison": V3_LATENCY_COMPARISON,
                                   "lifecycle_sources": fixture_summary["lifecycle_sources"],
                                   "warmup": WARMUP, "cache_flush": args.flush_embed_cache_url or "",
                                   "configuration": configuration, "configuration_sha256": configuration_hash},
               "provider_status": {"openai_embedding": embedding_status,
                                   "reranker": "CONFIGURED" if args.rerank_url and args.rerank_model else "NOT_RUN",
                                   "assessor": "CONFIGURED" if args.assessor_url and args.assessor_model else "NOT_RUN"},
               "identity": identity, "binary_sha256": binary_hash, "runs": {},
               "quality_gate_passed": False, "default_changed": False}
    assessor_ready = bool(args.assessor_url and args.assessor_model)
    rerank = bool(args.rerank_url and args.rerank_model)
    policy_dir = out / "policies"

    if args.phase == "holdout":
        if not args.selection_lock:
            raise ValueError("holdout requires a calibration selection lock")
        if args.admission_probability_cache:
            raise ValueError("holdout must not use the calibration probability cache")
        lock_path = Path(args.selection_lock)
        lock = json.loads(lock_path.read_text(encoding="utf-8"))
        if lock.get("schema") != V3_LOCK_SCHEMA:
            raise ValueError("the selection lock was written by an older protocol; run a new calibration")
        restore_lock_arguments(args, lock, explicit)
        configuration = v3_configuration(args)
        if not configuration["embedding_revision"]:
            raise ValueError("the selection lock has no embedding revision to restore")
        validate_flush_target(configuration["embedding_endpoint"], args.flush_embed_cache_url)
        configuration_hash = config_signature(configuration)
        selected = validate_v3_lock(lock, hashes, configuration_hash, template_hash_for(selected_backend(lock)),
                                    binary_hash)
        backend, threshold = selected["backend"], selected["threshold_hundredths"]
        # Everything below is derived from the restored configuration, never
        # from a CLI default that the lock overrode.
        rerank = bool(selected["rerank"])
        embed_config = {"endpoint": args.embed_endpoint, "model": args.embed_model,
                        "dims": args.embed_dims, "key_env": args.embed_key_env,
                        "key_configured": (bool(os.environ.get(args.embed_key_env, ""))
                                           if args.embed_key_env else True)}
        embedding_status = provider_status("openai", embed_config)
        assessor_ready = bool(args.assessor_url and args.assessor_model)
        if backend == "openai" and embedding_status != "CONFIGURED":
            raise ValueError("locked OpenAI candidate lacks configured credentials")
        if not assessor_ready:
            raise ValueError("locked v3 candidate requires the assessor endpoint and model")
        ready, reason = identity_is_verifiable(lock.get("identity") or {})
        if not ready:
            # No test question is sent for an identity check; the lock must
            # already prove which model produced the calibration.
            raise ValueError(f"locked identity cannot be verified: {reason}")
        claim_holdout(lock_path)
        lock["holdout_started"] = True
        lock["holdout_started_utc"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        atomic_write_json(lock_path, lock)
        # The manifest pins the exact consumed lock bytes, so any later
        # tampering with the lock breaks the holdout evidence.
        results["selection_lock_sha256"] = digest(lock_path)
        test_file = _split_file(retrieval_path, "test", out)
        baseline_file = out / "retrieval-test-v1.json"
        baseline_file.write_text(test_file.read_text(encoding="utf-8"), encoding="utf-8")
        policy_path = policy_file(policy_dir, backend, threshold, "selected")
        extra = ["--admission-policy", str(policy_path), "--lifecycle", str(lifecycle_path)]
        execution_error = ""

        def holdout_flush(step: str) -> bool:
            """Record an expected flush failure instead of crashing post-claim."""
            nonlocal execution_error
            reason = flush_embed_cache_safe(args.flush_embed_cache_url)
            if reason and not execution_error:
                execution_error = f"{step}: {reason}"
            return not reason

        warmups = []
        if args.warmup:
            warmups.append(_warmup_run(args, retrieval_path, out, "v1", backend, rerank, corpus_path,
                                       extra=["--lifecycle", str(lifecycle_path)]))
            warmups.append(_warmup_run(args, retrieval_path, out, "v3", backend, rerank, corpus_path,
                                       extra=extra))
            for entry in warmups:
                if entry.get("flush_error") and not execution_error:
                    execution_error = f"warmup-{entry.get('version')}: {entry['flush_error']}"
        holdout_flush("before the v1 baseline")
        if execution_error:
            base = {"status": "NOT_RUN", "reason": execution_error}
        else:
            base = _run(args.binary, args, baseline_file, "v1", backend, rerank, "test",
                        out / "v1-test.json", extra=["--lifecycle", str(lifecycle_path)],
                        corpus=corpus_path)
        holdout_flush("before the v3 candidate")
        if execution_error:
            candidate = {"status": "NOT_RUN", "reason": execution_error}
        else:
            candidate = _run(args.binary, args, test_file, "v3", backend, rerank, "test",
                             out / "v3-test.json", extra=extra, corpus=corpus_path)
        results["runs"] = {"v1_test": base, "v3_test": candidate}
        results["warmup_runs"] = warmups
        results["selected_candidate"] = selected
        if execution_error:
            results["cache_flush_error"] = {"reason": execution_error}
        warmup_gate_failed = bool(args.warmup) and any(entry.get("status") != "PASS" for entry in warmups)
        results["warmup_gate"] = {"required": bool(args.warmup), "passed": not warmup_gate_failed}
        if warmup_gate_failed:
            results["warmup_gate"]["reason"] = ("a configured warmup failed; the holdout ran cold and its "
                                                "numbers are not gate evidence")
        if base["status"] == candidate["status"] == "PASS" and not warmup_gate_failed and not execution_error:
            gate = quality_gate_v3(candidate["metrics"], base["metrics"])
            results["holdout_gate"] = gate
            results["quality_gate_passed"] = gate["passed"]
        else:
            results["quality_gate_passed"] = False
        results["protocol_config"]["test_touched"] = True
    elif args.phase == "profile":
        split_file = _split_file(retrieval_path, "train", out / "profile")
        if not assessor_ready:
            results["runs"]["profile"] = {
                "status": "NOT_RUN",
                "reason": "the bounded v3 profile needs --assessor-url and --assessor-model",
            }
        else:
            backend = "openai" if "openai" in backends else backends[0]
            policy_path = policy_file(policy_dir, backend, 0, "calibration")
            profile_extra = ["--admission-policy", str(policy_path), "--calibration-mode",
                             "--lifecycle", str(lifecycle_path), "--profile-admission"]
            warmup_extra = ["--admission-policy", str(policy_path), "--lifecycle", str(lifecycle_path)]
            warmups = []
            if args.warmup:
                warmups.append(_warmup_run(args, retrieval_path, out, "v1", backend, rerank, corpus_path,
                                           extra=["--lifecycle", str(lifecycle_path)]))
                warmups.append(_warmup_run(args, retrieval_path, out, "v3", backend, rerank, corpus_path,
                                           extra=warmup_extra))
            flush_embed_cache(args.flush_embed_cache_url)
            baseline = _run(args.binary, args, split_file, "v1", backend, rerank, "train",
                            out / "profile-v1.json", corpus=corpus_path)
            flush_embed_cache(args.flush_embed_cache_url)
            candidate = _run(args.binary, args, split_file, "v3", backend, rerank, "train",
                             out / "profile-v3.json", extra=profile_extra, corpus=corpus_path)
            results["runs"] = {"v1_profile": baseline, "v3_profile": candidate}
            results["warmup_runs"] = warmups
            results["selected_backend"] = backend
            results["profile"] = profile_verdict(baseline, candidate)
        results["protocol_config"]["test_touched"] = False
        results["selection_lock"] = None
        results["calibration_selection"] = ("profile only: no threshold grid, no lock and no test question; "
                                            "the measured numbers are a lower bound for this train configuration")
    else:
        split = "calibration"
        split_file = _split_file(retrieval_path, split, out)
        for backend in backends:
            warmups_backend = []
            if args.warmup:
                warmups_backend.append(_warmup_run(args, retrieval_path, out, "v1", backend, rerank,
                                                   corpus_path,
                                                   extra=["--lifecycle", str(lifecycle_path)]))
            flush_embed_cache(args.flush_embed_cache_url)
            results["runs"][f"{split}/v1-{backend}"] = _run(
                args.binary, args, split_file, "v1", backend, rerank, split,
                out / f"{split}-v1-{backend}.json", extra=["--lifecycle", str(lifecycle_path)],
                corpus=corpus_path)
            results.setdefault("warmup_runs", []).extend(warmups_backend)
        candidates = []
        if not assessor_ready:
            results["runs"][f"{split}/v3-grid"] = {
                "status": "NOT_RUN",
                "reason": "admission requires --assessor-url and --assessor-model",
            }
            results["calibration_selection"] = "No assessor configured; the threshold grid and holdout lock were not produced."
        else:
            cache_path = Path(args.admission_probability_cache) if args.admission_probability_cache else out / "admission-probability-cache.json"
            for backend in backends:
                # Every threshold in the frozen grid is executed; the grid is
                # never inferred from the threshold-0 latency.
                for threshold in THRESHOLD_HUNDREDTHS:
                    key = f"{split}/v3-{backend}-t{threshold:03d}"
                    policy_path = policy_file(policy_dir, backend, threshold, "calibration")
                    extra = ["--admission-policy", str(policy_path), "--calibration-mode",
                             "--lifecycle", str(lifecycle_path), "--admission-probability-cache", str(cache_path)]
                    result = _run(args.binary, args, split_file, "v3", backend, rerank, split,
                                  out / f"{split}-v3-{backend}-t{threshold:03d}.json", extra=extra,
                                  corpus=corpus_path)
                    results["runs"][key] = result
                    if result.get("status") != "PASS":
                        continue
                    baseline = results["runs"].get(f"{split}/v1-{backend}")
                    if baseline is None or baseline.get("status") != "PASS":
                        continue
                    gate = quality_gate_v3(result["metrics"], baseline["metrics"])
                    result["calibration_gate"] = gate
                    candidates.append({"backend": backend, "threshold_hundredths": threshold,
                                       "metrics": result["metrics"], "gate": gate})
            results["thresholds_executed"] = len(candidates)
            selected = select_v3_candidate(candidates)
            results["selected_calibration_candidate"] = None if selected is None else {
                "backend": selected["backend"], "threshold_hundredths": selected["threshold_hundredths"]}
            if selected is None:
                results["calibration_selection"] = ("No v3 candidate passed every calibration gate; no holdout lock was created.")
            else:
                threshold = selected["threshold_hundredths"]
                backend = selected["backend"]
                policy_path = policy_file(policy_dir, backend, threshold, "calibration")
                extra = ["--admission-policy", str(policy_path), "--calibration-mode",
                         "--lifecycle", str(lifecycle_path)]
                warmups_final = []
                if args.warmup:
                    warmups_final.append(_warmup_run(args, retrieval_path, out, "v3", backend, rerank,
                                                     corpus_path, extra=extra))
                flush_embed_cache(args.flush_embed_cache_url)
                rerun = _run(args.binary, args, split_file, "v3", backend, rerank, split,
                             out / f"{split}-v3-{backend}-t{threshold:03d}-uncached.json", extra=extra,
                             corpus=corpus_path)
                results["runs"][f"{split}/v3-final-uncached"] = rerun
                results.setdefault("warmup_runs", []).extend(warmups_final)
                baseline = results["runs"].get(f"{split}/v1-{backend}")
                if rerun.get("status") == "PASS" and baseline is not None and baseline.get("status") == "PASS":
                    gate = quality_gate_v3(rerun["metrics"], baseline["metrics"])
                    ready, reason = identity_is_verifiable(identity)
                    if not ready:
                        results["calibration_selection"] = f"The candidate is not lockable: {reason}."
                    elif identity.get("embedding_provider") != "openai-compat":
                        # The hash embedding stays diagnostic: it cannot prove
                        # which real model answered, so no production lock.
                        results["calibration_selection"] = (
                            "The selected candidate used a diagnostic embedding provider; only a real "
                            "openai-compat embedding with a verifiable assessor can mint a production lock.")
                    elif not args.flush_embed_cache_url:
                        # final_rerun_uncached would be an unprovable claim:
                        # nothing ever flushed the cache before this rerun.
                        results["calibration_selection"] = (
                            "No verifiable --flush-embed-cache-url was configured, so the final rerun "
                            "cannot be proven uncached; no production lock was created.")
                    elif args.warmup and any(entry.get("status") != "PASS"
                                             for entry in (results.get("warmup_runs") or [])):
                        # Every configured warmup of a production gate must
                        # pass; a failed warmup means the gate ran cold and its
                        # numbers cannot back a lock.
                        results["calibration_selection"] = (
                            "A configured warmup failed; no production lock was created.")
                    elif gate["passed"]:
                        selected_payload = {"version": "v3", "backend": backend,
                                            "threshold_hundredths": threshold, "threshold": threshold / 100,
                                            "rerank": rerank}
                        lock = {"schema": V3_LOCK_SCHEMA,
                                "created_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                                "fixture_sha256": hashes, "configuration": configuration,
                                "configuration_sha256": configuration_hash,
                                "policy_template_sha256": template_hash_for(backend),
                                "identity": identity, "budget": dict(LOCK_BUDGET),
                                "warmup": dict(WARMUP), "binary_sha256": binary_hash,
                                "selected": selected_payload,
                                "selected_sha256": config_signature(selected_payload),
                                "selected_policy_sha256": _selected_policy_signature(
                                    {"configuration": configuration}, backend, threshold,
                                    hashes["retrieval.json"]),
                                "calibration_metrics": rerun["metrics"],
                                "calibration_baseline_metrics": baseline["metrics"],
                                "calibration_gate": gate,
                                "selection_basis": ("calibration only; recall@5 desc, mrr@10 desc, p95 asc, "
                                                    "stricter threshold first; final gate from one uncached rerun"),
                                "cache_used_for_screening": True, "final_rerun_uncached": True,
                                "holdout_started": False}
                        atomic_write_json(out / "selection-lock.json", lock)
                        results["selection_lock"] = str(out / "selection-lock.json")
                        results["selected_calibration_candidate"] = selected_payload
                    else:
                        results["calibration_selection"] = ("The selected candidate failed the uncached final gate; "
                                                            "no holdout lock was created.")
                else:
                    results["calibration_selection"] = ("The selected candidate failed its uncached final rerun; "
                                                        "no holdout lock was created.")
        results["protocol_config"]["test_touched"] = False
    manifest = out / "manifest.json"
    manifest.write_text(json.dumps(results, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(manifest)
    if getattr(args, "protocol", "v2") == "v3" and args.phase == "holdout":
        # A real holdout may only exit 0 when the recorded gate itself passed;
        # calibration/profile keep their diagnostic rc semantics.
        runs_ok = all(item.get("status") in ("PASS", "NOT_RUN") for item in results["runs"].values())
        warmup_ok = (results.get("warmup_gate") or {}).get("passed", True)
        return 0 if runs_ok and warmup_ok and results.get("quality_gate_passed") is True else 1
    return 0 if all(item.get("status") in ("PASS", "NOT_RUN") for item in results["runs"].values()) else 1


def selected_backend(lock: dict) -> str:
    selected = lock.get("selected") if isinstance(lock, dict) else None
    return selected.get("backend", "") if isinstance(selected, dict) else ""


def profile_verdict(baseline: dict, candidate: dict) -> dict:
    """Report whether the assessor engine alone already exceeds the budget.

    The comparison is a lower bound: deleting all client-side request
    construction cannot make a model-bound configuration faster than its own
    engine time. It says nothing about other thresholds or configurations.
    """
    base_metrics = baseline.get("metrics") or {}
    cand_metrics = candidate.get("metrics") or {}
    base_p95 = metric_number(base_metrics, "p95_ms", float("nan"))
    limit = base_p95 * 1.5 if math.isfinite(base_p95) else float("nan")
    engine_p95 = metric_number((candidate.get("raw_metrics") or {}), "admission_engine_latency_ms_p95", float("nan"))
    v3_p95 = metric_number(cand_metrics, "p95_ms", float("nan"))
    stages = split_stage_metrics(candidate)
    verdict = {
        "v1_p95_ms": base_p95 if math.isfinite(base_p95) else None,
        "v1_p95_limit_ms": limit if math.isfinite(limit) else None,
        "v3_p95_ms": v3_p95 if math.isfinite(v3_p95) else None,
        "engine_p95_ms": engine_p95 if math.isfinite(engine_p95) else None,
        "stage_metrics": stages,
        "conclusion": "NOT_RUN",
        "scope": "current train configuration, this provider and this binary only",
    }
    if baseline.get("status") != "PASS" or candidate.get("status") != "PASS":
        verdict["reason"] = "the profile needs one PASS v1 and one PASS v3 run"
        return verdict
    if not math.isfinite(engine_p95) or not math.isfinite(limit) or engine_p95 <= 0 or limit <= 0:
        verdict["reason"] = ("engine latency or the v1 limit was not reported (a zero engine time means "
                             "--profile-admission did not run); no comparison is made")
        return verdict
    if engine_p95 > limit:
        verdict["conclusion"] = "model-bound"
        verdict["reason"] = ("the assessor engine alone already exceeds the v1 limit, so client-side request "
                             "construction cannot bring this configuration under budget")
    else:
        verdict["conclusion"] = "construction-bound"
        verdict["reason"] = ("the assessor engine stays under the v1 limit, so client-side request construction "
                             "is worth one semantics-preserving optimisation")
    return verdict


def run_v2(args: argparse.Namespace) -> int:
    apply_flag_defaults(args)
    fixture_root = Path(args.retrieval).parent
    hashes = verify_frozen_hashes(fixture_root)
    if digest(Path(args.corpus)) != hashes["corpus.json"] or digest(Path(args.retrieval)) != hashes["retrieval.json"]:
        raise ValueError("evaluation corpus/retrieval paths must contain the pinned frozen bytes")
    fixture_summary = validate_fixture(Path(args.retrieval), Path(args.corpus))
    if (args.max_items, args.max_bytes) != (10, 65536):
        raise ValueError("quality protocol fixes --max-items=10 and --max-bytes=65536")
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    binary_hash = digest(Path(args.binary))
    results = {"fixture_sha256": hashes, "fixture": fixture_summary,
               "protocol": {"max_items": 10, "max_bytes": 65536, "test_touched": False,
                            "latency_comparison": LATENCY_COMPARISON},
               "provider_status": {"openai_embedding": provider_status("openai", {
                                       "endpoint": args.embed_endpoint, "model": args.embed_model,
                                       "dims": args.embed_dims, "key_env": args.embed_key_env,
                                       "key_configured": bool(os.environ.get(args.embed_key_env, "")) if args.embed_key_env else True}),
                                   "reranker": "CONFIGURED" if args.rerank_url and args.rerank_model else "NOT_RUN",
                                   "assessor": "CONFIGURED" if args.assessor_url and args.assessor_model else "NOT_RUN"},
               "binary_sha256": binary_hash, "runs": {}, "quality_gate_passed": False, "default_changed": False}
    configuration = {"embedding_endpoint": args.embed_endpoint, "embedding_model": args.embed_model,
                     "embedding_dims": args.embed_dims, "embedding_key_env": args.embed_key_env,
                     "rerank_endpoint": args.rerank_url, "rerank_model": args.rerank_model,
                     "assessor_endpoint": args.assessor_url, "assessor_model": args.assessor_model}
    configuration_hash = config_signature(configuration)
    if args.phase == "holdout":
        if not args.selection_lock:
            raise ValueError("holdout requires a calibration selection lock")
        lock = json.loads(Path(args.selection_lock).read_text(encoding="utf-8"))
        selection = validate_selection_lock(lock, hashes, configuration_hash, binary_hash)
        if selection["backend"] == "openai" and results["provider_status"]["openai_embedding"] != "CONFIGURED":
            raise ValueError("locked OpenAI candidate lacks configured credentials")
        lock["holdout_started"] = True
        lock["holdout_started_utc"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        Path(args.selection_lock).write_text(json.dumps(lock, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        version, provider = "v2", selection["backend"]
        test_file = _split_file(Path(args.retrieval), "test", out)
        baseline_file = out / "retrieval-test-v1.json"
        baseline_file.write_text(test_file.read_text(encoding="utf-8"), encoding="utf-8")
        binary = args.binary
        base = _run(binary, args, baseline_file, "v1", provider, selection["rerank"], "test", out / "v1-test.json")
        candidate = _run(binary, args, test_file, version, provider, selection["rerank"], "test", out / "v2-test.json")
        results["runs"] = {"v1_test": base, "v2_test": candidate}
        if base["status"] == candidate["status"] == "PASS":
            gate = quality_gate(candidate["metrics"], base["metrics"])
            results["holdout_gate"] = gate
            results["quality_gate_passed"] = gate["passed"]
        results["protocol"]["test_touched"] = True
    else:
        stale_lock = out / "selection-lock.json"
        stale_lock.unlink(missing_ok=True)
        for split in ("train", "calibration"):
            split_file = _split_file(Path(args.retrieval), split, out)
            configs = [("v1-lexical", "v1", "lexical", False), ("v1-hash", "v1", "hash", False),
                       ("v2-lexical", "v2", "lexical", False), ("v2-hash", "v2", "hash", False)]
            embed_config = {"endpoint": args.embed_endpoint, "model": args.embed_model,
                            "dims": args.embed_dims, "key_env": args.embed_key_env,
                            "key_configured": bool(os.environ.get(args.embed_key_env, "")) if args.embed_key_env else True}
            if provider_status("openai", embed_config) == "CONFIGURED":
                configs += [("v1-openai", "v1", "openai", False), ("v2-openai", "v2", "openai", False)]
            if args.rerank_url and args.rerank_model:
                configs += [(f"{name}-rerank", version, provider, True) for name, version, provider, _ in list(configs)]
            for name, version, provider, rerank in configs:
                key = f"{split}/{name}"
                report_file = out / f"{split}-{name}.json"
                if provider == "openai" and provider_status(provider, embed_config) != "CONFIGURED":
                    results["runs"][key] = {"status": "NOT_RUN", "reason": "embedding key environment variable is unset"}
                    continue
                results["runs"][key] = _run(args.binary, args, split_file, version, provider, rerank, split, report_file)
        results["protocol"]["test_touched"] = False
        if split == "calibration":
            candidates = []
            for name, version, backend, rerank in configs:
                if version != "v2":
                    continue
                key = f"calibration/{name}"
                baseline_name = f"calibration/v1-{backend}" + ("-rerank" if rerank else "")
                candidate_result = results["runs"].get(key, {})
                baseline_result = results["runs"].get(baseline_name, {})
                if candidate_result.get("status") != "PASS" or baseline_result.get("status") != "PASS":
                    continue
                candidate_result["retrieval_gate"] = retrieval_gate(candidate_result["metrics"], baseline_result["metrics"])
                gate = quality_gate(candidate_result["metrics"], baseline_result["metrics"])
                candidate_result["calibration_gate"] = gate
                if gate["passed"]:
                    candidates.append((candidate_result["metrics"]["recall_at_5"], candidate_result["metrics"]["mrr_at_10"],
                                       {"version": "v2", "backend": backend, "rerank": rerank}, gate))
            if candidates:
                _, _, selected, gate = max(candidates, key=lambda item: (item[0], item[1]))
                lock = {"fixture_sha256": hashes, "configuration_sha256": configuration_hash,
                        "binary_sha256": binary_hash, "selected": selected,
                        "calibration_gate": gate, "selection_basis": "calibration only; maximum recall@5 then MRR@10 among candidates passing all gates"}
                lock_path = out / "selection-lock.json"
                lock_path.write_text(json.dumps(lock, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
                results["selection_lock"] = str(lock_path)
                results["selected_calibration_candidate"] = selected
            else:
                results["selected_calibration_candidate"] = None
                results["calibration_selection"] = "No v2 candidate passed every calibration gate; no holdout lock was created."
    manifest = out / "manifest.json"
    manifest.write_text(json.dumps(results, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(manifest)
    return 0 if all(item.get("status") in ("PASS", "NOT_RUN") for item in results["runs"].values()) else 1


def run(args: argparse.Namespace) -> int:
    return run_v3(args) if getattr(args, "protocol", "v2") == "v3" else run_v2(args)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, help="built context-eval executable")
    parser.add_argument("--corpus", default="fixtures/eval/quality-v2-r3/corpus.json")
    parser.add_argument("--retrieval", default="fixtures/eval/quality-v2-r3/retrieval.json")
    parser.add_argument("--out", required=True)
    parser.add_argument("--phase", choices=("calibrate", "holdout", "profile"), default="calibrate",
                        help="profile runs the bounded v3 latency comparison; it writes no lock")
    parser.add_argument("--warmup", action=argparse.BooleanOptionalAction, default=True,
                        help="run one train-head warmup before each measured run and flush the embed cache")
    parser.add_argument("--selection-lock", default="")
    parser.add_argument("--max-items", type=int, default=None)
    parser.add_argument("--max-bytes", type=int, default=None)
    parser.add_argument("--timeout", type=int, default=300)
    parser.add_argument("--embed-endpoint", default=None)
    parser.add_argument("--embed-model", default=None)
    parser.add_argument("--embed-key-env", default=None)
    parser.add_argument("--embed-dims", type=int, default=None,
                        help="required with --embed-endpoint/--embed-model; keeps real embeddings NOT_RUN when omitted")
    parser.add_argument("--embed-input-type", action="store_true", default=None,
                        help="send input_type (needed by the bundled Qwen embedding server)")
    parser.add_argument("--rerank-url", default=None)
    parser.add_argument("--rerank-model", default=None)
    parser.add_argument("--assessor-url", default=None)
    parser.add_argument("--assessor-model", default=None)
    parser.add_argument("--protocol", choices=("v2", "v3"), default="v2",
                        help="v2 keeps the frozen v2 protocol; v3 adds the admission grid and lifecycle")
    parser.add_argument("--fixture-dir", default="",
                        help="v3 frozen fixture directory (default fixtures/eval/quality-v3-r1)")
    parser.add_argument("--lifecycle", default="", help="v3 lifecycle fixture override")
    parser.add_argument("--embed-revision", default=None, help="embedding model revision pinned by the v3 policy")
    parser.add_argument("--backends", default=None,
                        help="v3 screening backends, comma-separated (hash,openai); empty runs both")
    parser.add_argument("--admission-batch-questions", type=int, default=None,
                        help="candidates per assessor request (0 keeps the client default); max 32")
    parser.add_argument("--flush-embed-cache-url", default=None,
                        help="local embedding service /admin/flush URL used before uncached latency runs")
    parser.add_argument("--admission-probability-cache", default=None,
                        help="calibration-only probability cache path (never used for the final gate)")
    parser.add_argument("--assessor-actual-model", default=None,
                        help="the identity the assessor endpoint actually served, when the configured name is an alias")
    args = parser.parse_args()
    if args.protocol == "v3" and not args.fixture_dir:
        args.fixture_dir = "fixtures/eval/quality-v3-r1"
    args.fixture_cross_split_leaks = 0
    return run(args)


if __name__ == "__main__":
    raise SystemExit(main())
