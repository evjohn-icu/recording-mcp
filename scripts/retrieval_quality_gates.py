#!/usr/bin/env python3
"""Shared retrieval-quality gates.

``run-retrieval-quality-v2.py`` (the calibration/holdout runner) and
``check_context_engine_release.py`` (the release checker) must apply the exact
same verdict to the same metrics, so the gate implementations live here and
both import them. The legacy v2 gate, the v3 gate and their direct helpers are
kept in one place; runner-side protocol code keeps its own module.
"""
from __future__ import annotations

import hashlib
import json
import math


def metric_number(metrics: dict, key: str, default: float) -> float:
    value = metrics.get(key)
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value):
        return default
    return float(value)


def failure_count(metrics: dict, name: str) -> float:
    """A per-failure counter that tolerates malformed metrics objects.

    A malformed ``failure_counts`` cannot prove zero failures, so it reads as
    a failure instead of raising on attacker-controlled content.
    """
    counts = metrics.get("failure_counts")
    if not isinstance(counts, dict):
        return -1
    value = counts.get(name)
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value):
        return -1
    return value


def recall_eligible_ids(metrics: dict) -> list | None:
    """The lifecycle-eligible id list, or None when it is not usable.

    The list must be a non-empty list of strings and is compared in order:
    a reordered or differently typed list is a different denominator, not an
    equivalent one.
    """
    ids = metrics.get("recall_eligible_ids")
    if not isinstance(ids, list) or not ids or any(not isinstance(item, str) for item in ids):
        return None
    return ids


def config_signature(configuration: dict) -> str:
    canonical = json.dumps(configuration, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(canonical).hexdigest()


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
        "embedding_and_rerank_failures_zero": all(failure_count(metrics, name) == 0 for name in ("embedding", "rerank")),
        "baseline_embedding_and_rerank_failures_zero": all(failure_count(baseline, name) == 0 for name in ("embedding", "rerank")),
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
        "assessment_failures_zero": failure_count(metrics, "assessment") == 0,
        "false_sufficient_rate_at_most_0_05": metric_number(metrics, "false_sufficient_rate", float("inf")) <= .05,
        "insufficient_package_denominator_positive": metric_number(metrics, "false_sufficient_denominator", 0) > 0,
    })
    return {"passed": all(checks.values()), "checks": checks,
            "retrieval_gate": retrieval}


def quality_gate_v3(metrics: dict, baseline: dict) -> dict:
    retrieval = retrieval_gate(metrics, baseline)
    checks = dict(retrieval["checks"])
    candidate_ids = recall_eligible_ids(metrics)
    baseline_ids = recall_eligible_ids(baseline)
    # The lifecycle denominator must be identical on both sides: a one-sided
    # lifecycle would let the candidate drop unforgotten-free questions and
    # inflate its recall without changing a single business result. The lists
    # are compared in order, as non-empty lists of strings.
    ids_match = candidate_ids is not None and baseline_ids is not None and candidate_ids == baseline_ids
    checks.update({
        "assessor_configured": metrics.get("assessor_configured") is True,
        "assessment_failures_zero": failure_count(metrics, "assessment") == 0,
        "false_sufficient_rate_at_most_0_05": metric_number(metrics, "false_sufficient_rate", float("inf")) <= .05,
        "insufficient_package_denominator_positive": metric_number(metrics, "false_sufficient_denominator", 0) > 0,
        "recall_at_5_at_least_0_85": metric_number(metrics, "recall_at_5", -1) >= .85,
        "none_false_positive_rate_at_most_0_05": metric_number(metrics, "none_false_positive_rate", 1) <= .05,
        "forgotten_leakage_zero": metric_number(metrics, "forgotten_leakage", -1) == 0,
        "admission_failures_zero": metric_number(metrics, "admission_failures", -1) == 0,
        "recall_eligible_ids_match": ids_match,
    })
    return {"passed": all(checks.values()), "checks": checks, "retrieval_gate": retrieval}
