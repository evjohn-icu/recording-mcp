#!/usr/bin/env python3
"""Run calibration and untouched holdout retrieval evaluations on authored synthetic data."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

SPLIT_GROUPS = {"train": 50, "calibration": 25, "test": 25}
SPLIT_QUESTIONS = {"train": 80, "calibration": 40, "test": 40}
CJK = re.compile(r"[\u3400-\u9fff]")

def digest(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for block in iter(lambda: f.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()

def validate_quality_questions(path: Path, corpus_path: Path | None = None) -> dict:
    payload = json.loads(path.read_text(encoding="utf-8"))
    questions = payload.get("questions", [])
    if len(questions) != 160:
        raise ValueError(f"quality corpus must contain exactly 160 questions, got {len(questions)}")
    corpus_path = corpus_path or path.with_name("corpus.json")
    corpus = json.loads(corpus_path.read_text(encoding="utf-8"))
    available = set()
    for conv in corpus.get("conversations", []):
        for segment in conv.get("segments", []):
            source = segment.get("source", {})
            for turn in segment.get("turns", []):
                available.add((source.get("segment_id"), source.get("transcript_revision"), turn.get("id")))
    if not available:
        raise ValueError("quality corpus has no resolvable source turns")
    positives = [q for q in questions if not q.get("unanswerable", False)]
    negatives = [q for q in questions if q.get("unanswerable", False)]
    if len(positives) != 120 or len(negatives) != 40:
        raise ValueError(f"expected 120 positive / 40 no-evidence, got {len(positives)} / {len(negatives)}")
    groups: dict[str, str] = {}
    group_items: dict[str, list[dict]] = {}
    split_counts = {s: 0 for s in SPLIT_GROUPS}
    question_counts = {s: 0 for s in SPLIT_QUESTIONS}
    bucket_counts: dict[str, dict[str, int]] = {}
    ids: set[str] = set()
    normalized_queries: set[str] = set()
    for q in questions:
        qid, group, split = q.get("id"), q.get("group_id"), q.get("split")
        if not qid or qid in ids:
            raise ValueError(f"missing or duplicate question id: {qid!r}")
        ids.add(qid)
        if not group or split not in split_counts:
            raise ValueError(f"question {qid} requires group_id and train/calibration/test split")
        if group in groups and groups[group] != split:
            raise ValueError(f"near-duplicate group {group} crosses {groups[group]} and {split}")
        groups[group] = split
        group_items.setdefault(group, []).append(q)
        question_counts[split] += 1
        query = q.get("query", "").strip()
        if not query or not CJK.search(query):
            raise ValueError(f"question {qid} must contain an authored non-empty Chinese query")
        normalized = re.sub(r"\s+", "", query).casefold()
        if normalized in normalized_queries:
            raise ValueError(f"duplicate normalized query: {query}")
        normalized_queries.add(normalized)
        bucket = q.get("bucket")
        if not bucket:
            raise ValueError(f"question {qid} requires a retrieval bucket")
        bucket_counts.setdefault(bucket, {}).setdefault(split, 0)
        bucket_counts[bucket][split] += 1
        if q.get("unanswerable", False):
            if any(q.get(k) for k in ("gold_turn_refs", "gold_evidence_turn_ids", "gold_topic_refs", "gold_topic_ids")):
                raise ValueError(f"no-evidence question {qid} must not carry gold evidence")
        else:
            refs = q.get("gold_turn_refs") or []
            if not refs and not q.get("gold_evidence_turn_ids"):
                raise ValueError(f"positive question {qid} has no gold evidence")
            for ref in refs:
                key = (ref.get("segment_id"), ref.get("transcript_revision"), ref.get("turn_id"))
                if key not in available:
                    raise ValueError(f"positive question {qid} refers to absent source turn {key}")
            if not q.get("notes", "").strip():
                raise ValueError(f"positive question {qid} requires annotation notes")
    for group, items in group_items.items():
        labels = {bool(q.get("unanswerable", False)) for q in items}
        if len(labels) != 1:
            raise ValueError(f"group {group} mixes answerable and no-evidence questions")
        expected_size = 1 if True in labels else 2
        if len(items) != expected_size:
            raise ValueError(f"group {group} has {len(items)} questions, expected {expected_size}")
        if len({re.sub(r"\s+", "", q["query"]).casefold() for q in items}) != len(items):
            raise ValueError(f"group {group} repeats a query")
        if False not in labels:
            refs = {tuple((r.get("segment_id"), r.get("transcript_revision"), r.get("turn_id"))
                          for r in (q.get("gold_turn_refs") or [])) for q in items}
            if len(refs) != 1:
                raise ValueError(f"positive group {group} must test one shared evidence concept")
    split_group_counts = {s: sum(1 for gsplit in groups.values() if gsplit == s) for s in SPLIT_GROUPS}
    if split_group_counts != SPLIT_GROUPS:
        raise ValueError(f"expected split concept groups {SPLIT_GROUPS}, got {split_group_counts}")
    if question_counts != SPLIT_QUESTIONS:
        raise ValueError(f"expected split question counts {SPLIT_QUESTIONS}, got {question_counts}")
    return {"questions": len(questions), "positive": len(positives), "no_evidence": len(negatives),
            "groups": len(groups), "split_group_counts": split_group_counts,
            "split_question_counts": question_counts, "buckets": bucket_counts,
            "source_turns": len(available)}

def _run_one(binary: str, go_root: Path, fixtures: dict[str, Path], split_questions: Path,
             report: Path, threshold: float | None, timeout: int) -> dict:
    cmd = ([binary, "run", "./cmd/context-eval"] if binary == "go" else [binary])
    cmd += ["--corpus", str(fixtures["corpus"]), "--questions", str(fixtures["classic"]),
            "--links", str(fixtures["links"]), "--retrieval", str(split_questions),
            "--projection", "", "--out", str(report), "--timeout", f"{timeout}s"]
    if threshold is not None:
        cmd += ["--min-similarity", str(threshold)]
    started = time.monotonic()
    try:
        proc = subprocess.run(cmd, cwd=go_root, check=False, timeout=timeout + 10,
                              capture_output=True, text=True,
                              env={**os.environ, "GOTOOLCHAIN": "local"})
    except subprocess.TimeoutExpired as exc:
        return {"status": "TIMEOUT", "command": cmd, "timeout_seconds": timeout,
                "elapsed_seconds": time.monotonic() - started,
                "stdout": _decode(exc.stdout)[-4000:], "stderr": _decode(exc.stderr)[-4000:]}
    result = {"status": "PASS" if proc.returncode == 0 and report.exists() else "FAIL",
              "command": cmd, "exit_code": proc.returncode,
              "elapsed_seconds": time.monotonic() - started,
              "stdout": proc.stdout[-4000:], "stderr": proc.stderr[-4000:]}
    if result["status"] == "PASS":
        parsed = json.loads(report.read_text(encoding="utf-8"))
        result["report"] = str(report)
        result["report_sha256"] = digest(report)
        result["metrics"] = parsed["metrics"]
    return result

def _decode(value) -> str:
    if value is None: return ""
    return value.decode(errors="replace") if isinstance(value, bytes) else str(value)

def _split_fixture(source: Path, split: str, out: Path) -> Path:
    payload = json.loads(source.read_text(encoding="utf-8"))
    payload["questions"] = [q for q in payload["questions"] if q["split"] == split]
    path = out / f"retrieval-{split}.json"
    path.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return path

def _gate(metrics: dict, baseline: dict) -> dict:
    recall = metrics.get("hybrid_recall_at_5", 0)
    baseline_recall = baseline.get("hybrid_recall_at_5", 0)
    fp = metrics.get("unanswerable_hybrid_false_positive_rate", 1)
    stale = metrics.get("stale_leakage", 1)
    ambient = metrics.get("ambient_leakage", 1)
    escapes = metrics.get("scope_escape_count", 1)
    invalid = metrics.get("retrieval_invalid_evidence_refs", 1)
    checks = {"recall_at_least_baseline": recall >= baseline_recall,
              "no_evidence_false_positive_rate_at_most_0_05": fp <= .05,
              "stale_leakage_zero": stale == 0,
              "ambient_leakage_zero": ambient == 0,
              "scope_escapes_zero": escapes == 0,
              "invalid_evidence_refs_zero": invalid == 0}
    return {"passed": all(checks.values()), "checks": checks,
            "hybrid_recall_at_5": recall, "baseline_hybrid_recall_at_5": baseline_recall,
            "hybrid_false_positive_rate": fp, "stale_leakage": stale,
            "ambient_leakage": ambient, "scope_escape_count": escapes,
            "invalid_evidence_refs": invalid}

def run(args: argparse.Namespace) -> int:
    paths = {"corpus": Path(args.corpus), "classic": Path(args.questions),
             "links": Path(args.links), "quality": Path(args.retrieval)}
    for path in paths.values():
        if not path.is_file(): raise ValueError(f"fixture not found: {path}")
    quality = validate_quality_questions(paths["quality"], paths["corpus"])
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    manifests = {str(p): digest(p) for p in paths.values()}
    split_files = {split: _split_fixture(paths["quality"], split, out) for split in SPLIT_QUESTIONS}
    manifests.update({str(p): digest(p) for p in split_files.values()})
    results = {"quality_corpus": quality, "fixture_sha256": manifests,
               "embedding_provider": "NOT_RUN: real embedding provider was not configured; no external provider calls are made",
               "threshold_policy": "calibration split only; test split is held untouched until selection is complete",
               "runs": {}, "calibration": {}, "holdout": {}}
    base = args.baseline_threshold
    baseline_cal = _run_one(args.binary, Path(args.repo_root), paths, split_files["calibration"],
                             out / "baseline-calibration.json", None, args.timeout)
    results["runs"]["baseline_calibration"] = baseline_cal
    if baseline_cal["status"] != "PASS":
        (out / "manifest.json").write_text(json.dumps(results, ensure_ascii=False, indent=2)+"\n", encoding="utf-8")
        return 1
    baseline_cal_metrics = baseline_cal["metrics"]
    candidates = []
    for threshold in args.thresholds:
        if abs(threshold - base) < 1e-12:
            continue
        label = f"calibration-threshold-{threshold:g}"
        result = _run_one(args.binary, Path(args.repo_root), paths, split_files["calibration"],
                          out / f"{label}.json", threshold, args.timeout)
        results["runs"][label] = result
        if result["status"] == "PASS":
            gate = _gate(result["metrics"], baseline_cal_metrics)
            candidates.append({"threshold": threshold, "gate": gate,
                               "mrr": result["metrics"].get("retrieval_hybrid_mrr_at_10", 0),
                               "report": result["report"]})
    eligible = [c for c in candidates if c["gate"]["passed"]]
    selected = max(eligible, key=lambda c: (c["gate"]["hybrid_recall_at_5"], c["mrr"], c["threshold"])) if eligible else None
    safe_candidates = [c for c in candidates
                       if c["gate"]["checks"]["no_evidence_false_positive_rate_at_most_0_05"]
                       and c["gate"]["checks"]["stale_leakage_zero"]
                       and c["gate"]["checks"]["ambient_leakage_zero"]
                       and c["gate"]["checks"]["scope_escapes_zero"]
                       and c["gate"]["checks"]["invalid_evidence_refs_zero"]]
    diagnostic = max(safe_candidates,
                     key=lambda c: (c["gate"]["hybrid_recall_at_5"], c["mrr"], -c["threshold"])) if safe_candidates else None
    results["calibration"] = {"baseline": baseline_cal_metrics,
                               "candidate_thresholds": candidates,
                               "selected_threshold": selected["threshold"] if selected else None,
                               "diagnostic_threshold": diagnostic["threshold"] if diagnostic else None,
                               "selection": "maximum recall, then MRR@10, then higher threshold among candidates passing all calibration gates"}
    # Exercise the training split as a development diagnostic without using it
    # to select the threshold.
    train = _run_one(args.binary, Path(args.repo_root), paths, split_files["train"],
                     out / "baseline-train.json", None, args.timeout)
    results["runs"]["baseline_train"] = train
    baseline_test = _run_one(args.binary, Path(args.repo_root), paths, split_files["test"],
                             out / "baseline-test.json", None, args.timeout)
    results["runs"]["baseline_test"] = baseline_test
    results["holdout"]["baseline"] = baseline_test.get("metrics")
    holdout_threshold = selected or diagnostic
    if holdout_threshold and baseline_test["status"] == "PASS":
        candidate_test = _run_one(args.binary, Path(args.repo_root), paths, split_files["test"],
                                  out / "candidate-test.json", holdout_threshold["threshold"], args.timeout)
        results["runs"]["candidate_test"] = candidate_test
        results["holdout"]["candidate"] = candidate_test.get("metrics")
        results["holdout"]["candidate_threshold"] = holdout_threshold["threshold"]
        results["holdout"]["candidate_is_diagnostic_only"] = selected is None
        if candidate_test["status"] == "PASS":
            results["holdout"]["gate"] = _gate(candidate_test["metrics"], baseline_test["metrics"])
    else:
        results["holdout"]["gate"] = {"passed": False, "reason": "no calibration candidate passed all gates"}
    results["quality_gate_passed"] = bool(results["holdout"].get("gate", {}).get("passed", False))
    results["default_threshold_changed"] = False
    manifest = out / "manifest.json"
    manifest.write_text(json.dumps(results, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(manifest)
    print(json.dumps({"selected_threshold": results["calibration"]["selected_threshold"],
                      "quality_gate_passed": results["quality_gate_passed"],
                      "holdout": results["holdout"]}, ensure_ascii=False, indent=2))
    return 0 if all(r.get("status") == "PASS" for r in results["runs"].values()) else 1

def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", default="bin/context-eval", help="built context-eval binary; use 'go' to invoke go run")
    parser.add_argument("--repo-root", default=".", help="repository root used for fixture-relative source references")
    parser.add_argument("--corpus", default="fixtures/eval/quality/corpus.json")
    parser.add_argument("--questions", default="fixtures/eval/quality/questions.json", help="classic checks matching this synthetic corpus")
    parser.add_argument("--links", default="fixtures/eval/quality/links.json")
    parser.add_argument("--retrieval", default="fixtures/eval/quality/retrieval.json")
    parser.add_argument("--out", required=True)
    parser.add_argument("--timeout", type=int, default=300, help="maximum seconds for each evaluation invocation")
    parser.add_argument("--baseline-threshold", type=float, default=.05)
    parser.add_argument("--thresholds", default="0.05,0.1,0.15,0.2,0.25,0.3,0.4,0.5,0.65,0.8,1.0")
    args = parser.parse_args()
    try:
        args.thresholds = sorted({float(v) for v in args.thresholds.split(",")})
    except ValueError:
        parser.error("thresholds must be comma-separated numbers")
    if args.timeout <= 0 or not 0 <= args.baseline_threshold <= 1 or any(not 0 <= v <= 1 for v in args.thresholds):
        parser.error("timeout must be positive and all thresholds must be in [0,1]")
    args.repo_root = str(Path(args.repo_root).resolve())
    try:
        return run(args)
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"retrieval-quality: {exc}", file=sys.stderr)
        return 2

if __name__ == "__main__":
    raise SystemExit(main())
