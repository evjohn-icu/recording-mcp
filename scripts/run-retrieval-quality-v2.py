#!/usr/bin/env python3
"""Run the frozen retrieval-quality-v2 calibration matrix and locked holdout."""
from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import re
import subprocess
import time

SPLIT_GROUPS = {"train": 80, "calibration": 40, "test": 40}
SPLIT_QUESTIONS = {"train": 120, "calibration": 60, "test": 60}
POSITIVE_BUCKETS = {"synonym", "negation", "time", "cross_session"}
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


def metric_number(metrics: dict, key: str, default: float) -> float:
    value = metrics.get(key)
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value):
        return default
    return float(value)


def config_signature(configuration: dict) -> str:
    canonical = json.dumps(configuration, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(canonical).hexdigest()


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
    for row in rows:
        if any(key not in row for key in required_question_fields):
            missing = [key for key in required_question_fields if key not in row]
            raise ReportProtocolError(f"question {row.get('id')} is missing fields: {', '.join(missing)}")
        if row["split"] != split or not isinstance(row["unanswerable"], bool):
            raise ReportProtocolError(f"question {row['id']} has an invalid split or answerability label")
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
        if row["expected_sufficiency"] not in ("sufficient", "insufficient"):
            raise ReportProtocolError(f"question {row['id']} has invalid expected sufficiency")
        complete_final_evidence = (not row["unanswerable"] and row["gold_evidence_count"] > 0 and
                                   row["final_gold_evidence_found"] == row["gold_evidence_count"])
        expected_status = "sufficient" if complete_final_evidence else "insufficient"
        if row["expected_sufficiency"] != expected_status:
            raise ReportProtocolError(f"question {row['id']} expected sufficiency disagrees with final evidence coverage")
        if version == "v2" and row.get("sufficiency_status") not in ("sufficient", "insufficient", "unknown"):
            raise ReportProtocolError(f"question {row['id']} is missing a valid sufficiency status")
    answerable = [q for q in rows if not q["unanswerable"]]
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
    assessed = [q for q in rows if q["assessor_calls"] > 0]
    false_sufficient_denominator = sum(q["expected_sufficiency"] == "insufficient" for q in assessed)
    false_sufficient = sum(q["expected_sufficiency"] == "insufficient" and q.get("sufficiency_status") == "sufficient" for q in assessed)
    assessor_configured = m["evidence_assessor_configured"]
    false_sufficient_rate = None
    if assessor_configured and false_sufficient_denominator:
        false_sufficient_rate = false_sufficient / false_sufficient_denominator
    failures = {"embedding": m["semantic_provider_embed_errors"],
                "rerank": sum(q["rerank_failed"] for q in rows),
                "assessment": sum(q["assessment_failed"] for q in rows)}
    return {"split": split, "mode": mode, "recall_at_5": selected_recall, "mrr_at_10": selected_mrr,
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
            "sufficiency": {"status_counts": {status: sum(q.get("sufficiency_status") == status for q in assessed)
                                                for status in ("sufficient", "insufficient", "unknown")},
                            "scored": len(assessed),
                            "matches": sum(q["expected_sufficiency"] == q.get("sufficiency_status") for q in assessed)},
            "embedding_invocations": _required_number(m, "semantic_provider_embed_requests", integer=True),
            "model_calls": sum(q["model_calls"] for q in rows),
            "rerank_calls": sum(q["rerank_calls"] for q in rows),
            "assessor_calls": sum(q["assessor_calls"] for q in rows),
            "failure_counts": failures, "bucket_metrics": buckets}


def retrieval_gate(metrics: dict, baseline: dict) -> dict:
    fp_denom = metric_number(metrics, "no_evidence_denominator", 0)
    fp_queries = metric_number(metrics, "false_positive_queries", -1)
    fp_rate = metric_number(metrics, "false_positive_rate", 1)
    fp_consistent = fp_denom > 0 and 0 <= fp_queries <= fp_denom and math.isclose(fp_rate, fp_queries / fp_denom, abs_tol=1e-12)
    values = {
        "recall_at_5_strictly_improves": metric_number(metrics, "recall_at_5", -1) > metric_number(baseline, "recall_at_5", 1),
        "mrr_at_10_not_regressed": metric_number(metrics, "mrr_at_10", -1) >= metric_number(baseline, "mrr_at_10", 1),
        "false_positive_rate_at_most_0_05": metric_number(metrics, "false_positive_rate", 1) <= .05,
        "stale_leaks_zero": metric_number(metrics, "stale_leaks", -1) == 0,
        "ambient_leaks_zero": metric_number(metrics, "ambient_leaks", -1) == 0,
        "scope_leaks_zero": metric_number(metrics, "scope_leaks", -1) == 0,
        "invalid_refs_zero": metric_number(metrics, "invalid_refs", -1) == 0,
        "fixture_cross_split_leaks_zero": metric_number(metrics, "fixture_cross_split_leaks", -1) == 0,
        "p95_at_most_1_5x_same_provider_v1": metric_number(metrics, "p95_ms", float("inf")) <= metric_number(baseline, "p95_ms", 0) * 1.5,
        "embedding_and_rerank_failures_zero": all(metrics.get("failure_counts", {}).get(name, -1) == 0 for name in ("embedding", "rerank")),
        "baseline_embedding_and_rerank_failures_zero": all(baseline.get("failure_counts", {}).get(name, -1) == 0 for name in ("embedding", "rerank")),
        "baseline_p95_denominator_positive": metric_number(baseline, "p95_denominator", 0) > 0,
        "baseline_answerable_denominator_positive": metric_number(baseline, "answerable_denominator", 0) > 0,
        "answerable_denominator_positive": metric_number(metrics, "answerable_denominator", 0) > 0,
        "no_evidence_denominator_positive": metric_number(metrics, "no_evidence_denominator", 0) > 0,
        "p95_denominator_positive": metric_number(metrics, "p95_denominator", 0) > 0,
        "recall_and_mrr_denominators_positive": metric_number(metrics, "recall_denominator", 0) > 0 and metric_number(metrics, "mrr_denominator", 0) > 0,
        "false_positive_query_rate_matches_denominators": fp_consistent,
    }
    return {"passed": all(values.values()), "checks": values}


def quality_gate(metrics: dict, baseline: dict) -> dict:
    retrieval = retrieval_gate(metrics, baseline)
    checks = dict(retrieval["checks"])
    checks.update({
        "assessor_configured": metrics.get("assessor_configured") is True,
        "assessment_failures_zero": metrics.get("failure_counts", {}).get("assessment", -1) == 0,
        "false_sufficient_rate_at_most_0_05": metric_number(metrics, "false_sufficient_rate", float("inf")) <= .05,
        "insufficient_package_denominator_positive": metric_number(metrics, "false_sufficient_denominator", 0) > 0,
    })
    return {"passed": all(checks.values()), "checks": checks,
            "retrieval_gate": retrieval}


def _split_file(source: Path, split: str, out: Path) -> Path:
    payload = json.loads(source.read_text(encoding="utf-8"))
    payload["questions"] = [q for q in payload["questions"] if q["split"] == split]
    path = out / f"retrieval-{split}.json"
    path.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return path


def _run(binary: str, args: argparse.Namespace, split_path: Path, version: str,
         provider: str, rerank: bool, split: str, out: Path) -> dict:
    cmd = [binary, "--corpus", str(args.corpus), "--questions", "", "--links", "",
           "--retrieval", str(split_path), "--projection", "", "--max-items", "10", "--max-bytes", "65536",
           "--retrieval-version", version, "--embed-provider", "off" if provider == "lexical" else provider,
           "--out", str(out), "--timeout", f"{args.timeout}s"]
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
    result = {"status": "PASS" if proc.returncode == 0 and Path(out).exists() else "FAIL",
              "command": safe_command(cmd), "exit_code": proc.returncode, "elapsed_seconds": time.monotonic() - start,
              "stdout": proc.stdout[-3000:], "stderr": proc.stderr[-3000:]}
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
    return result


def run(args: argparse.Namespace) -> int:
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


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, help="built context-eval executable")
    parser.add_argument("--corpus", default="fixtures/eval/quality-v2-r3/corpus.json")
    parser.add_argument("--retrieval", default="fixtures/eval/quality-v2-r3/retrieval.json")
    parser.add_argument("--out", required=True)
    parser.add_argument("--phase", choices=("calibrate", "holdout"), default="calibrate")
    parser.add_argument("--selection-lock", default="")
    parser.add_argument("--max-items", type=int, default=10)
    parser.add_argument("--max-bytes", type=int, default=65536)
    parser.add_argument("--timeout", type=int, default=300)
    parser.add_argument("--embed-endpoint", default="")
    parser.add_argument("--embed-model", default="")
    parser.add_argument("--embed-key-env", default="")
    parser.add_argument("--embed-dims", type=int, default=0,
                        help="required with --embed-endpoint/--embed-model; keeps real embeddings NOT_RUN when omitted")
    parser.add_argument("--embed-input-type", action="store_true",
                        help="send input_type (needed by the bundled Qwen embedding server)")
    parser.add_argument("--rerank-url", default="")
    parser.add_argument("--rerank-model", default="")
    parser.add_argument("--assessor-url", default="")
    parser.add_argument("--assessor-model", default="")
    args = parser.parse_args()
    args.fixture_cross_split_leaks = 0
    return run(args)


if __name__ == "__main__":
    raise SystemExit(main())
