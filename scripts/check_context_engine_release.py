#!/usr/bin/env python3
"""Validate the v0.4 release decision document.

The checker answers one question: does the recorded evidence actually support
the readiness claims next to it?

- A ``PASS`` row must carry the command that was run and exit code 0, and any
  artifact it names must exist with the recorded SHA-256. An interactive check
  (``verification:"interactive"``, e.g. a browser session) has no exit code;
  it must instead pin an artifact, so the observation stays machine-checkable.
- ``NOT_RUN``, ``BLOCKED`` and ``EXPERIMENTAL`` rows must carry a reason.
- ``FAIL`` evidence can never support a READY decision.
- ``EXPERIMENTAL`` evidence can never support a production recommendation.
- READY requires current (not historical) evidence for every required row and
  no blocker marked as blocking that gate.
- The evidence snapshot must pin the candidate HEAD, the hashed sources and the
  uncommitted diff that the evidence was collected against.

It proves structure and artifact integrity only; it cannot prove that a test
was sufficient or that the run happened as described.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import math
import re
import sys
from pathlib import Path

SCHEMA = "context-engine-v04-release-results-v2"
DECISION_SCHEMA = "context-engine-v04-decision-v1"
THRESHOLDS_SCHEMA = "context-engine-production-thresholds-v1"
GATE_SCHEMA = "context-engine-production-gate-v1"
STATUSES = ("PASS", "FAIL", "NOT_RUN", "BLOCKED", "EXPERIMENTAL")
KINDS = ("current", "historical")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
HEAD_RE = re.compile(r"^[0-9a-f]{40}$")
MERGE_KEYS = ("merge_readiness", "release_readiness", "scope", "blockers", "evidence_snapshot",
              "required_for_merge", "required_for_release", "production_ready_recommendation")
CATEGORIES = ("quiet_two_person", "far_field_meeting", "overlapping_speech", "media_interference")
PRODUCTION_METRICS = ("cer_max", "wer_max", "der_max", "event_precision_min", "event_recall_min",
                      "eer_max", "evidence_accuracy_min", "semantic_accuracy_min",
                      "decision_precision_min")
# A release candidate must carry these three current rows: the audio quality
# gate, the v3 calibration and the independent v3 holdout.
REQUIRED_RELEASE_ROWS = ("audio.production-gate", "v3.calibration", "v3.holdout")


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def canonical_digest(payload) -> str:
    """The digest review-audio-acceptance.py stores for its thresholds."""
    canonical = json.dumps(payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


def _read_json(path: Path, label: str, errors: list[str]) -> dict | None:
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        errors.append(f"{label} is not readable JSON: {exc}")
        return None
    if not isinstance(payload, dict):
        errors.append(f"{label} must be a JSON object")
        return None
    return payload


def _number(value, label: str, errors: list[str], *, integer: bool):
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        errors.append(f"{label} must be a number")
        return None
    if integer and not isinstance(value, int):
        errors.append(f"{label} must be an integer count")
        return None
    if not math.isfinite(float(value)) or value < 0:
        errors.append(f"{label} must be a finite non-negative number")
        return None
    return value


def _production_thresholds(decision: dict, root: Path, document_dir: Path | None,
                           errors: list[str]) -> tuple[dict | None, str]:
    """Verify the approved threshold artifact and return (payload, digest)."""
    threshold = decision.get("production_threshold")
    if not isinstance(threshold, dict) or threshold.get("approved") is not True:
        errors.append("production readiness needs a pre-approved production_threshold")
        return None, ""
    artifact = str(threshold.get("artifact") or "")
    if not artifact:
        errors.append("production_threshold.artifact is required")
        return None, ""
    target = _resolve(root, document_dir, artifact)
    if not target.is_file():
        errors.append(f"production_threshold artifact not found: {artifact}")
        return None, ""
    recorded = str(threshold.get("artifact_sha256") or "")
    if not SHA256_RE.match(recorded):
        errors.append("production_threshold.artifact_sha256 must be 64 hex characters")
        return None, ""
    if sha256_file(target) != recorded:
        errors.append(f"production_threshold artifact hash does not match {artifact}")
        return None, ""
    payload = _read_json(target, "production_threshold artifact", errors)
    if payload is None:
        return None, ""
    if payload.get("schema") != THRESHOLDS_SCHEMA:
        errors.append(f"production_threshold.schema must be {THRESHOLDS_SCHEMA}")
    if payload.get("approved") is not True:
        errors.append("production_threshold artifact is not approved")
    categories = payload.get("categories")
    if not isinstance(categories, list) or sorted(categories) != sorted(CATEGORIES):
        errors.append("production_threshold.categories must be exactly the four acceptance categories")
    metrics_payload = payload.get("metrics")
    if not isinstance(metrics_payload, dict):
        errors.append("production_threshold.metrics must be an object")
    else:
        for key in PRODUCTION_METRICS:
            value = metrics_payload.get(key)
            if isinstance(value, bool) or not isinstance(value, (int, float)) \
                    or not math.isfinite(value) or not 0.0 <= value <= 1.0:
                errors.append(f"production_threshold.metrics.{key} must be a finite number in [0, 1]")
    return payload, canonical_digest(payload)


def _recompute_gate(gate: dict, thresholds: dict, errors: list[str]) -> None:
    """Recompute every ratio from the stored counts and enforce the thresholds."""
    limits = thresholds.get("metrics") or {}
    categories = gate.get("categories")
    if not isinstance(categories, dict) or sorted(categories) != sorted(CATEGORIES):
        errors.append("production_gate.categories must contain exactly the four acceptance categories")
        return
    if gate.get("failures") or gate.get("blocked"):
        errors.append("production_gate carries recorded failures or blocked items")
    for category in CATEGORIES:
        bucket = categories[category]
        if not isinstance(bucket, dict):
            errors.append(f"production_gate.categories.{category} must be an object")
            continue
        prefix = f"production_gate {category}"
        cer = bucket.get("cer") or {}
        wer = bucket.get("wer") or {}
        der = bucket.get("der") or {}
        events = bucket.get("events") or {}
        decision = bucket.get("decision") or {}
        web = bucket.get("web") or {}
        mcp = bucket.get("mcp") or {}
        eer = bucket.get("eer") or {}

        def ratio(numerator, denominator, label):
            if numerator is None:
                return None
            count = _number(denominator, f"{prefix}: {label} denominator", errors, integer=True)
            if count is None:
                return None
            if count <= 0:
                errors.append(f"{prefix}: {label} has a zero denominator")
                return None
            return numerator / count

        def counts(value, label):
            return _number(value, f"{prefix}: {label}", errors, integer=True)

        values = {
            "cer": ratio(counts(cer.get("errors"), "cer.errors"),
                         cer.get("reference"), "cer"),
            "wer": ratio(counts(wer.get("errors"), "wer.errors"),
                         wer.get("reference"), "wer"),
        }
        false_alarm = _number(der.get("false_alarm_ms"), f"{prefix}: der.false_alarm_ms",
                              errors, integer=False)
        missed = _number(der.get("missed_ms"), f"{prefix}: der.missed_ms", errors, integer=False)
        confusion = _number(der.get("confusion_ms"), f"{prefix}: der.confusion_ms",
                            errors, integer=False)
        reference_speech = _number(der.get("reference_speech_ms"),
                                   f"{prefix}: der.reference_speech_ms", errors, integer=False)
        if None in (false_alarm, missed, confusion, reference_speech):
            values["der"] = None
        elif reference_speech <= 0:
            errors.append(f"{prefix}: der has a zero denominator")
            values["der"] = None
        else:
            values["der"] = (false_alarm + missed + confusion) / reference_speech
        tp = counts(events.get("tp"), "events.tp")
        fp = counts(events.get("fp"), "events.fp")
        fn = counts(events.get("fn"), "events.fn")
        values["event_precision"] = (ratio(tp, (tp or 0) + (fp or 0), "event_precision")
                                     if tp is not None and fp is not None else None)
        values["event_recall"] = (ratio(tp, (tp or 0) + (fn or 0), "event_recall")
                                  if tp is not None and fn is not None else None)
        web_returned = counts(web.get("returned_refs"), "web.returned_refs")
        mcp_returned = counts(mcp.get("returned_refs"), "mcp.returned_refs")
        returned = (web_returned or 0) + (mcp_returned or 0) \
            if web_returned is not None and mcp_returned is not None else None
        supported = None
        semantic_accepted = None
        semantic_adjudicated = None
        for source, payload in (("web", web), ("mcp", mcp)):
            source_supported = counts(payload.get("supported_refs"), f"{source}.supported_refs")
            source_accepted = counts(payload.get("semantic_accepted"),
                                     f"{source}.semantic_accepted")
            source_adjudicated = counts(payload.get("semantic_adjudicated"),
                                        f"{source}.semantic_adjudicated")
            source_returned = web_returned if source == "web" else mcp_returned
            if None in (source_supported, source_accepted, source_adjudicated, source_returned):
                supported = semantic_accepted = semantic_adjudicated = None
                continue
            if source_returned <= 0:
                errors.append(f"{prefix}: {source} returned no evidence")
            if source_adjudicated != source_returned:
                errors.append(f"{prefix}: {source}: every returned ref must be adjudicated")
            if source_supported > source_returned:
                errors.append(f"{prefix}: {source}.supported_refs exceeds returned_refs")
            if source_accepted > source_returned:
                errors.append(f"{prefix}: {source}.semantic_accepted exceeds returned_refs")
            supported = (supported or 0) + source_supported
            semantic_accepted = (semantic_accepted or 0) + source_accepted
            semantic_adjudicated = (semantic_adjudicated or 0) + source_adjudicated
        values["evidence_accuracy"] = (ratio(supported, returned, "evidence_accuracy")
                                       if supported is not None else None)
        values["semantic_accuracy"] = (ratio(semantic_accepted, returned, "semantic_accuracy")
                                       if semantic_adjudicated == returned and returned else None)
        accepted = counts(decision.get("accepted"), "decision.accepted")
        total = counts(decision.get("total"), "decision.total")
        adjudicated = counts(decision.get("adjudicated"), "decision.adjudicated")
        if total is not None and adjudicated is not None:
            if total <= 0:
                errors.append(f"{prefix}: decision has no predicted claims")
            if adjudicated != total:
                errors.append(f"{prefix}: every decision must be adjudicated")
        if accepted is not None and total is not None and accepted > total:
            errors.append(f"{prefix}: decision.accepted exceeds total")
        values["decision_precision"] = (ratio(accepted, total, "decision_precision")
                                        if total and adjudicated == total else None)

        for name, value in values.items():
            limit = limits.get(f"{name}_max" if name in ("cer", "wer", "der") else f"{name}_min")
            if name == "event_precision":
                limit = limits.get("event_precision_min")
            elif name == "event_recall":
                limit = limits.get("event_recall_min")
            elif name == "evidence_accuracy":
                limit = limits.get("evidence_accuracy_min")
            elif name == "semantic_accuracy":
                limit = limits.get("semantic_accuracy_min")
            elif name == "decision_precision":
                limit = limits.get("decision_precision_min")
            if not isinstance(limit, (int, float)) or isinstance(limit, bool):
                continue
            if value is None:
                errors.append(f"{prefix}: {name} cannot be recomputed from its counts")
                continue
            if name in ("cer", "wer", "der"):
                if value > limit:
                    errors.append(f"{prefix}: {name} {value:.4f} exceeds {limit}")
            elif value < limit:
                errors.append(f"{prefix}: {name} {value:.4f} is below {limit}")

        eer_value = _number(eer.get("value"), f"{prefix}: eer.value", errors, integer=False)
        same = counts(eer.get("same_trials"), "eer.same_trials")
        different = counts(eer.get("different_trials"), "eer.different_trials")
        if not isinstance(eer.get("experimental"), bool):
            errors.append(f"{prefix}: eer.experimental must be a boolean")
        elif eer.get("experimental"):
            errors.append(f"{prefix}: experimental EER cannot support production readiness")
        if eer_value is not None and same is not None and different is not None:
            if same <= 0 or different <= 0:
                errors.append(f"{prefix}: EER needs at least one same and one different trial")
            elif same < 30 or different < 30:
                errors.append(f"{prefix}: EER has fewer than 30 trials per class")
            limit = limits.get("eer_max")
            if isinstance(limit, (int, float)) and not isinstance(limit, bool) and eer_value > limit:
                errors.append(f"{prefix}: eer {eer_value:.4f} exceeds {limit}")


def _check_audio_gate(gate_id: str, by_id: dict[str, dict], decision: dict,
                      thresholds: dict | None, thresholds_digest: str,
                      root: Path, document_dir: Path | None, errors: list[str]) -> None:
    """Verify the audio gate row's score artifact rather than its PASS string."""
    row = by_id.get(gate_id)
    if row is None:
        errors.append(f"audio quality evidence row does not exist: {gate_id}")
        return
    if row.get("status") != "PASS":
        errors.append(f"audio quality evidence is {row.get('status')}, not PASS")
    if row.get("evidence_kind") != "current":
        errors.append("audio quality evidence must be current")
    artifact = str(row.get("artifact") or "")
    if not artifact:
        errors.append("audio quality evidence needs an artifact score")
        return
    target = _resolve(root, document_dir, artifact)
    if not target.is_file():
        errors.append(f"audio quality artifact not found: {artifact}")
        return
    score = _read_json(target, "audio quality score", errors)
    if score is None:
        return
    gate = score.get("production_gate")
    if not isinstance(gate, dict):
        errors.append(f"audio quality score {artifact} has no production_gate")
        return
    if gate.get("schema") != GATE_SCHEMA:
        errors.append(f"production_gate.schema must be {GATE_SCHEMA}")
    if gate.get("status") != "PASS":
        errors.append(f"audio quality gate is {gate.get('status')}, not PASS")
    for key in ("manifest_sha256", "predictions_sha256", "adjudications_sha256"):
        if not SHA256_RE.match(str(gate.get(key) or "")):
            errors.append(f"production_gate.{key} must be 64 hex characters")
    if gate.get("thresholds_sha256") != thresholds_digest:
        errors.append("production_gate.thresholds_sha256 does not match the approved thresholds")
    if isinstance(thresholds, dict):
        stored = gate.get("thresholds")
        if isinstance(stored, dict):
            for key in PRODUCTION_METRICS:
                if stored.get(key) != (thresholds.get("metrics") or {}).get(key):
                    errors.append(f"production_gate.thresholds.{key} differs from the approved thresholds")
        _recompute_gate(gate, thresholds, errors)


def check(document: dict, root: Path, document_dir: Path | None = None) -> tuple[list[str], list[str]]:
    """Return (errors, warnings)."""
    errors: list[str] = []
    warnings: list[str] = []
    if document.get("schema") != SCHEMA:
        errors.append(f"schema must be {SCHEMA}")
        return errors, warnings
    results = document.get("results")
    if not isinstance(results, list) or not results:
        return ["results must be a non-empty array"], warnings
    by_id: dict[str, dict] = {}
    for index, row in enumerate(results):
        if not isinstance(row, dict):
            errors.append(f"results[{index}] must be an object")
            continue
        row_id = row.get("id")
        if not isinstance(row_id, str) or not row_id:
            errors.append(f"results[{index}] has no id")
            continue
        if row_id in by_id:
            errors.append(f"duplicate result id: {row_id}")
        by_id[row_id] = row
        status = row.get("status")
        if status not in STATUSES:
            errors.append(f"{row_id}: status must be one of {list(STATUSES)}")
            continue
        kind = row.get("evidence_kind")
        if kind not in KINDS:
            errors.append(f"{row_id}: evidence_kind must be current or historical")
        if status == "PASS":
            command = row.get("command")
            if not isinstance(command, str) or not command.strip():
                errors.append(f"{row_id}: PASS evidence needs the command that was run")
            interactive = row.get("verification") == "interactive"
            if row.get("verification") not in (None, "command", "interactive"):
                errors.append(f"{row_id}: verification must be command or interactive")
            if not interactive and row.get("exit_code") != 0:
                errors.append(f"{row_id}: PASS evidence needs exit_code 0")
            if interactive and not row.get("artifact"):
                errors.append(f"{row_id}: interactive PASS evidence needs an artifact")
            artifact = row.get("artifact")
            if artifact:
                target = _resolve(root, document_dir, artifact)
                if not target.is_file():
                    errors.append(f"{row_id}: artifact not found: {artifact}")
                elif not SHA256_RE.match(str(row.get("artifact_sha256") or "")):
                    errors.append(f"{row_id}: artifact_sha256 must be 64 hex characters")
                elif sha256_file(target) != row["artifact_sha256"]:
                    errors.append(f"{row_id}: artifact hash does not match {artifact}")
        elif status in ("NOT_RUN", "BLOCKED", "EXPERIMENTAL"):
            if not str(row.get("reason") or "").strip():
                errors.append(f"{row_id}: {status} needs a reason")
        elif status == "FAIL":
            if row.get("exit_code") == 0 and str(row.get("command") or "").strip() and not row.get("reason"):
                warnings.append(f"{row_id}: FAIL recorded with exit code 0 and no reason")

    decision = document.get("decision")
    if not isinstance(decision, dict):
        errors.append("decision object is required")
        return errors, warnings
    if decision.get("schema") != DECISION_SCHEMA:
        errors.append(f"decision.schema must be {DECISION_SCHEMA}")
    missing = [key for key in MERGE_KEYS if key not in decision]
    if missing:
        errors.append(f"decision is missing {', '.join(missing)}")
        return errors, warnings
    merge = decision["merge_readiness"]
    release = decision["release_readiness"]
    if merge not in ("READY", "BLOCKED"):
        errors.append("decision.merge_readiness must be READY or BLOCKED")
    if release not in ("READY", "BLOCKED"):
        errors.append("decision.release_readiness must be READY or BLOCKED")
    if not isinstance(decision["production_ready_recommendation"], bool):
        errors.append("decision.production_ready_recommendation must be a boolean")

    blockers = decision["blockers"]
    if not isinstance(blockers, list):
        errors.append("decision.blockers must be an array")
        blockers = []
    for index, blocker in enumerate(blockers):
        if not isinstance(blocker, dict) or not str(blocker.get("id") or ""):
            errors.append(f"decision.blockers[{index}] needs an id")
            continue
        if not str(blocker.get("detail") or "").strip():
            errors.append(f"decision.blockers[{index}] needs a detail")
        if not isinstance(blocker.get("blocks_merge"), bool):
            errors.append(f"decision.blockers[{index}] needs a boolean blocks_merge")
        if not isinstance(blocker.get("blocks_release"), bool):
            errors.append(f"decision.blockers[{index}] needs a boolean blocks_release")

    snapshot = decision["evidence_snapshot"]
    if not isinstance(snapshot, dict):
        errors.append("decision.evidence_snapshot must be an object")
        snapshot = {}
    head = str(snapshot.get("candidate_head") or "")
    if not HEAD_RE.match(head):
        errors.append("evidence_snapshot.candidate_head must be a 40-character commit SHA")
    if not str(snapshot.get("generated_at") or "").strip():
        errors.append("evidence_snapshot.generated_at is required")
    if not SHA256_RE.match(str(snapshot.get("uncommitted_diff_sha256") or "")):
        errors.append("evidence_snapshot.uncommitted_diff_sha256 must be 64 hex characters")
    for key in ("source_sha256", "config_sha256"):
        entries = snapshot.get(key)
        if not isinstance(entries, dict) or not entries:
            errors.append(f"evidence_snapshot.{key} must be a non-empty object")
            continue
        for relative, recorded in sorted(entries.items()):
            if not SHA256_RE.match(str(recorded)):
                errors.append(f"evidence_snapshot.{key}[{relative}] must be 64 hex characters")
                continue
            target = _resolve(root, document_dir, relative)
            if not target.is_file():
                errors.append(f"evidence_snapshot.{key}: file not found: {relative}")
            elif sha256_file(target) != recorded:
                errors.append(f"evidence_snapshot.{key}: hash does not match {relative}")

    required_merge = _ids(decision.get("required_for_merge"), "required_for_merge", errors)
    required_release = _ids(decision.get("required_for_release"), "required_for_release", errors)
    for row_id in required_merge + required_release:
        if row_id not in by_id:
            errors.append(f"required evidence id does not exist: {row_id}")

    qualifying = [row_id for row_id in required_merge if row_id in by_id]
    if merge == "READY":
        if not qualifying:
            errors.append("merge READY needs at least one required evidence row")
        for row_id in qualifying:
            row = by_id[row_id]
            if row.get("status") != "PASS":
                errors.append(f"merge READY requires PASS evidence; {row_id} is {row.get('status')}")
            if row.get("evidence_kind") != "current":
                errors.append(f"merge READY requires current evidence; {row_id} is historical")
        for blocker in blockers:
            if isinstance(blocker, dict) and blocker.get("blocks_merge") is True:
                errors.append(f"merge READY contradicts blocker {blocker.get('id')}")
    if release == "READY":
        if not required_release:
            errors.append("release READY needs at least one required evidence row")
        for row_id in required_release:
            row = by_id.get(row_id)
            if row is None:
                continue
            if row.get("status") != "PASS":
                errors.append(f"release READY requires PASS evidence; {row_id} is {row.get('status')}")
            if row.get("evidence_kind") != "current":
                errors.append(f"release READY requires current evidence; {row_id} is "
                              f"{row.get('evidence_kind')}")
        for blocker in blockers:
            if isinstance(blocker, dict) and blocker.get("blocks_release") is True:
                errors.append(f"release READY contradicts blocker {blocker.get('id')}")
        missing_anchors = [row_id for row_id in REQUIRED_RELEASE_ROWS if row_id not in required_release]
        if missing_anchors:
            errors.append("release READY requires the current production evidence: "
                          + ", ".join(missing_anchors))
        gate_id = str(decision.get("audio_quality_evidence_id") or "")
        if not gate_id:
            errors.append("release READY needs decision.audio_quality_evidence_id")
        elif gate_id not in required_release:
            errors.append("decision.audio_quality_evidence_id must be a required release row")
        thresholds, thresholds_digest = _production_thresholds(decision, root, document_dir, errors)
        _check_audio_gate(gate_id, by_id, decision, thresholds, thresholds_digest,
                          root, document_dir, errors)
    if release == "READY" and merge != "READY":
        errors.append("release READY requires merge READY")
    if decision["production_ready_recommendation"]:
        if release != "READY":
            errors.append("production_ready_recommendation=true requires release_readiness READY")
        for row_id in required_release:
            row = by_id.get(row_id, {})
            if row.get("status") == "EXPERIMENTAL":
                errors.append(f"EXPERIMENTAL evidence cannot support production readiness: {row_id}")
            if row.get("status") == "BLOCKED" or row.get("status") == "NOT_RUN":
                errors.append(f"incomplete evidence cannot support production readiness: {row_id}")
        threshold = decision.get("production_threshold")
        if not isinstance(threshold, dict) or threshold.get("approved") is not True:
            errors.append("production readiness needs a pre-approved production_threshold")
        for blocker in blockers:
            if isinstance(blocker, dict) and blocker.get("blocks_release") is True:
                errors.append(f"production readiness contradicts blocker {blocker.get('id')}")
    if release != "READY" and not any(
            isinstance(blocker, dict) and blocker.get("blocks_release") is True for blocker in blockers):
        errors.append("release_readiness BLOCKED needs at least one blocker with blocks_release=true")

    for row_id, row in sorted(by_id.items()):
        if row.get("evidence_kind") == "historical" and row_id in required_merge:
            warnings.append(f"{row_id}: historical evidence cannot justify a current merge decision")
    return errors, warnings


def _ids(value, label: str, errors: list[str]) -> list[str]:
    if not isinstance(value, list):
        errors.append(f"decision.{label} must be an array")
        return []
    ids = []
    for item in value:
        if not isinstance(item, str) or not item:
            errors.append(f"decision.{label} entries must be non-empty ids")
            continue
        ids.append(item)
    if len(set(ids)) != len(ids):
        errors.append(f"decision.{label} contains duplicates")
    return ids


def _resolve(root: Path, document_dir: Path | None, relative: str) -> Path:
    path = Path(relative)
    if path.is_absolute():
        return path
    for base in (root, document_dir or root):
        candidate = base / path
        if candidate.is_file():
            return candidate
    return root / path


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("document", type=Path)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1],
                        help="repository root used to resolve relative evidence paths")
    parser.add_argument("--json", action="store_true", help="emit a machine-readable verdict")
    args = parser.parse_args()
    document = json.loads(args.document.read_text(encoding="utf-8"))
    errors, warnings = check(document, args.root, args.document.parent)
    verdict = {"status": "PASS" if not errors else "FAIL", "errors": errors, "warnings": warnings,
               "merge_readiness": (document.get("decision") or {}).get("merge_readiness"),
               "release_readiness": (document.get("decision") or {}).get("release_readiness")}
    if args.json:
        print(json.dumps(verdict, ensure_ascii=False, indent=2))
    else:
        print(f"release decision: {verdict['status']} ({verdict['merge_readiness']}/"
              f"{verdict['release_readiness']})")
        for error in errors:
            print(f"- error: {error}", file=sys.stderr)
        for warning in warnings:
            print(f"- warning: {warning}")
        print("Mapped anchors and artifacts prove integrity only, not sufficiency or execution grounds.")
    return 0 if not errors else 1


if __name__ == "__main__":
    raise SystemExit(main())
