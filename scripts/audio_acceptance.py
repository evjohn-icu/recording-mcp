#!/usr/bin/env python3
"""Acceptance manifest and gold-schema validation for the v0.4 audio harness.

The harness never scans for audio: a sample is only opened after the operator
authorizes it in a manifest, and the manifest's paths must stay inside the
manifest's own directory. Real quality metrics are scored against a
human-authored gold document; missing inputs are reported as NOT_RUN or
BLOCKED, never as a fabricated pass.
"""
from __future__ import annotations

import hashlib
import json
from datetime import datetime
from pathlib import Path

MANIFEST_SCHEMA = "context-engine-acceptance-v1"
GOLD_SCHEMA = "context-engine-gold-v1"
CATEGORIES = ("quiet_two_person", "far_field_meeting", "overlapping_speech", "media_interference")


class AcceptanceError(ValueError):
    """A manifest or gold document violates the frozen schema."""

    def __init__(self, code: str, detail: str):
        super().__init__(f"{code}: {detail}")
        self.code = code
        self.detail = detail


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def valid_hash(value: object) -> bool:
    """True for a 64-character hex SHA-256 digest."""
    if not isinstance(value, str) or len(value) != 64:
        return False
    try:
        bytes.fromhex(value)
    except ValueError:
        return False
    return True


def _inside(root: Path, candidate: Path) -> bool:
    try:
        candidate.resolve().relative_to(root.resolve())
        return True
    except ValueError:
        return False


def load_manifest(path: Path) -> dict:
    """Validate the acceptance manifest without opening any audio."""
    payload = json.loads(path.read_text(encoding="utf-8"))
    if payload.get("schema") != MANIFEST_SCHEMA:
        raise AcceptanceError("invalid_manifest", f"schema must be {MANIFEST_SCHEMA}")
    samples = payload.get("samples")
    if not isinstance(samples, list) or not samples:
        raise AcceptanceError("invalid_manifest", "samples must be a non-empty array")
    root = path.parent
    seen: set[str] = set()
    for index, sample in enumerate(samples):
        sample_id = sample.get("id")
        if not isinstance(sample_id, str) or not sample_id or sample_id in seen:
            raise AcceptanceError("invalid_manifest", f"sample {index} has a missing or duplicate id")
        seen.add(sample_id)
        if sample.get("category") not in CATEGORIES:
            raise AcceptanceError("invalid_manifest", f"sample {sample_id} has an unknown category")
        if not valid_hash(sample.get("audio_sha256")):
            raise AcceptanceError("invalid_manifest", f"sample {sample_id} audio_sha256 must be 64 hex")
        authorization = sample.get("authorization")
        if not isinstance(authorization, dict) or authorization.get("allowed") is not True:
            raise AcceptanceError("unauthorized_sample", f"sample {sample_id} is not authorized")
        if not str(authorization.get("reference") or "").strip():
            raise AcceptanceError("unauthorized_sample", f"sample {sample_id} authorization has no reference")
        audio_path = Path(str(sample.get("audio_path") or ""))
        if not audio_path.is_absolute():
            audio_path = root / audio_path
        if not _inside(root, audio_path):
            raise AcceptanceError("path_escape", f"sample {sample_id} audio path escapes the manifest root")
        gold_path = Path(str(sample.get("gold_path") or ""))
        if not gold_path.is_absolute():
            gold_path = root / gold_path
        if not _inside(root, gold_path):
            raise AcceptanceError("path_escape", f"sample {sample_id} gold path escapes the manifest root")
        if not valid_hash(sample.get("gold_sha256")):
            raise AcceptanceError("invalid_manifest", f"sample {sample_id} gold_sha256 must be 64 hex")
        sample["_audio_path"] = str(audio_path)
        sample["_gold_path"] = str(gold_path)
    payload["_root"] = str(root)
    return payload


def verify_sample_files(sample: dict) -> list[str]:
    """Hash the authorized files; returns NOT_RUN/BLOCKED-style failures."""
    problems: list[str] = []
    audio = Path(sample["_audio_path"])
    gold = Path(sample["_gold_path"])
    if not audio.is_file():
        problems.append(f"audio missing: {audio.name}")
    elif sha256_file(audio) != sample["audio_sha256"]:
        problems.append("audio hash differs from the manifest")
    if not gold.is_file():
        problems.append(f"gold missing: {gold.name}")
    elif sha256_file(gold) != sample["gold_sha256"]:
        problems.append("gold hash differs from the manifest")
    return problems


def require_int(value: object) -> bool:
    """True for a real integer; booleans are explicitly rejected."""
    return isinstance(value, int) and not isinstance(value, bool)


def valid_timestamp(value: object) -> bool:
    """A parseable ISO-8601 instant that carries an explicit timezone."""
    if not isinstance(value, str) or not value.strip():
        return False
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return False
    return parsed.tzinfo is not None


def _check_span(span: object, label: str, duration_ms: int | None = None) -> dict:
    if not isinstance(span, dict):
        raise AcceptanceError("invalid_gold", f"{label} must be an object")
    start, end = span.get("start_ms"), span.get("end_ms")
    if not require_int(start) or not require_int(end):
        raise AcceptanceError("invalid_gold", f"{label} start_ms/end_ms must be integers")
    if start < 0 or end <= start:
        raise AcceptanceError("invalid_gold", f"{label} needs 0 <= start_ms < end_ms")
    if duration_ms is not None and end > duration_ms:
        raise AcceptanceError("invalid_gold", f"{label} ends after audio_duration_ms")
    return span


def _check_list(payload: dict, key: str) -> list | None:
    """Omitted means unannotated; an explicit empty array means confirmed empty."""
    if key not in payload:
        return None
    value = payload[key]
    if not isinstance(value, list):
        raise AcceptanceError("invalid_gold", f"gold {key} must be an array when present")
    return value


def _check_identifiers(items: list, key: str, label: str) -> None:
    seen: set[str] = set()
    for item in items:
        identifier = item.get(key) if isinstance(item, dict) else None
        if not isinstance(identifier, str) or not identifier:
            raise AcceptanceError("invalid_gold", f"{label} item needs a string {key}")
        if identifier in seen:
            raise AcceptanceError("invalid_gold", f"{label} has duplicate {key} {identifier}")
        seen.add(identifier)


def load_gold(path: Path, sample_id: str, known_sample_ids: set[str] | None = None) -> dict:
    """Read and validate one human-authored gold document."""
    return validate_gold(json.loads(path.read_text(encoding="utf-8")), sample_id, known_sample_ids)


def validate_gold(payload: dict, sample_id: str, known_sample_ids: set[str] | None = None) -> dict:
    """Validate an already-decoded gold document.

    ``known_sample_ids`` is the set of samples authorized by the containing
    manifest; speaker trials and required spans may only reference those.
    """
    if not isinstance(payload, dict):
        raise AcceptanceError("invalid_gold", "gold must be a JSON object")
    if payload.get("schema") != GOLD_SCHEMA:
        raise AcceptanceError("invalid_gold", f"schema must be {GOLD_SCHEMA}")
    if payload.get("provenance") != "human":
        raise AcceptanceError("invalid_gold", "gold provenance must be human")
    if payload.get("sample_id") != sample_id:
        raise AcceptanceError("invalid_gold", "gold sample_id does not match the manifest")
    if not str(payload.get("annotator") or "").strip():
        raise AcceptanceError("invalid_gold", "gold annotator is required")
    reviewed_at = payload.get("reviewed_at")
    if not str(reviewed_at or "").strip():
        raise AcceptanceError("invalid_gold", "gold reviewed_at is required")
    if not valid_timestamp(reviewed_at):
        raise AcceptanceError("invalid_gold", "gold reviewed_at must be an ISO-8601 timestamp with a timezone")
    duration = payload.get("audio_duration_ms")
    if not require_int(duration) or duration <= 0:
        raise AcceptanceError("invalid_gold", "gold audio_duration_ms must be a positive integer")
    regions = payload.get("scored_regions")
    if not isinstance(regions, list) or not regions:
        raise AcceptanceError("invalid_gold", "gold scored_regions must be a non-empty array")
    ordered: list[tuple[int, int]] = []
    for region in regions:
        _check_span(region, "gold scored_regions", duration)
        ordered.append((region["start_ms"], region["end_ms"]))
    ordered.sort()
    for (_, previous_end), (start, _) in zip(ordered, ordered[1:]):
        if start < previous_end:
            raise AcceptanceError("invalid_gold", "gold scored_regions must not overlap")

    transcript = _check_list(payload, "transcript")
    if transcript is not None:
        _check_identifiers(transcript, "id", "transcript")
        for index, turn in enumerate(transcript):
            _check_span(turn, f"transcript[{index}]", duration)
            if not isinstance(turn.get("text"), str):
                raise AcceptanceError("invalid_gold", f"transcript[{index}] needs a string text")
            if not isinstance(turn.get("speaker"), str) or not turn["speaker"]:
                raise AcceptanceError("invalid_gold", f"transcript[{index}] needs a speaker label")

    events = _check_list(payload, "acoustic_events")
    if events is not None:
        _check_identifiers(events, "id", "acoustic_events")
        for index, event in enumerate(events):
            _check_span(event, f"acoustic_events[{index}]", duration)
            if not isinstance(event.get("label"), str) or not event["label"]:
                raise AcceptanceError("invalid_gold", f"acoustic_events[{index}] needs a label")

    trials = _check_list(payload, "speaker_trials")
    if trials is not None:
        _check_identifiers(trials, "id", "speaker_trials")
        for index, trial in enumerate(trials):
            label = f"speaker_trials[{index}]"
            if trial.get("split") not in ("calibration", "evaluation"):
                raise AcceptanceError("invalid_gold", "speaker trial split must be calibration or evaluation")
            if not isinstance(trial.get("same_speaker"), bool):
                raise AcceptanceError("invalid_gold", "speaker trial needs same_speaker boolean")
            for side in ("left", "right"):
                span = trial.get(side)
                _check_span(span, f"{label}.{side}")
                side_sample = span.get("sample_id")
                if not isinstance(side_sample, str) or not side_sample:
                    raise AcceptanceError("invalid_gold", f"{label}.{side} needs a sample_id")
                if known_sample_ids is not None and side_sample not in known_sample_ids:
                    raise AcceptanceError("invalid_gold",
                                          f"{label}.{side} references an unauthorized sample {side_sample}")
                if not isinstance(span.get("speaker"), str) or not span["speaker"]:
                    raise AcceptanceError("invalid_gold", f"{label}.{side} needs a speaker label")

    queries = _check_list(payload, "queries")
    if queries is not None:
        _check_identifiers(queries, "id", "queries")
        for index, query in enumerate(queries):
            label = f"queries[{index}]"
            if not isinstance(query.get("query"), str) or not query["query"].strip():
                raise AcceptanceError("invalid_gold", f"{label} needs a non-empty query")
            if query.get("expected_sufficiency") not in ("sufficient", "insufficient"):
                raise AcceptanceError("invalid_gold", f"{label} expected_sufficiency must be sufficient or insufficient")
            completeness = query.get("evidence_completeness")
            if completeness not in ("complete", "partial", "none"):
                raise AcceptanceError("invalid_gold", f"{label} evidence_completeness is invalid")
            spans = query.get("required_spans")
            if not isinstance(spans, list):
                raise AcceptanceError("invalid_gold", f"{label} required_spans must be an array")
            if completeness == "none" and spans:
                raise AcceptanceError("invalid_gold", f"{label} with no evidence must not require spans")
            if completeness == "complete" and not spans:
                raise AcceptanceError("invalid_gold", f"{label} with complete evidence must require spans")
            for span_index, span in enumerate(spans):
                _check_span(span, f"{label}.required_spans[{span_index}]")
                if known_sample_ids is not None and span.get("sample_id") not in known_sample_ids:
                    raise AcceptanceError("invalid_gold",
                                          f"{label}.required_spans[{span_index}] references an unauthorized sample")
            scope = query.get("scope_sample_ids")
            if scope is not None:
                if not isinstance(scope, list) or any(not isinstance(item, str) for item in scope):
                    raise AcceptanceError("invalid_gold", f"{label} scope_sample_ids must be string IDs")
                if known_sample_ids is not None:
                    unknown = set(scope) - known_sample_ids
                    if unknown:
                        raise AcceptanceError("invalid_gold",
                                              f"{label} scope references unauthorized samples {sorted(unknown)}")

    claims = _check_list(payload, "claims")
    if claims is not None:
        _check_identifiers(claims, "id", "claims")
        for index, claim in enumerate(claims):
            label = f"claims[{index}]"
            if claim.get("kind") not in ("decision", "commitment"):
                raise AcceptanceError("invalid_gold", f"{label} kind must be decision or commitment")
            if not isinstance(claim.get("text"), str) or not claim["text"].strip():
                raise AcceptanceError("invalid_gold", f"{label} needs a non-empty text")
            if not isinstance(claim.get("source_spans"), list):
                raise AcceptanceError("invalid_gold", f"{label} source_spans must be an array")
            for span_index, span in enumerate(claim["source_spans"]):
                _check_span(span, f"{label}.source_spans[{span_index}]")
    return payload


def no_manifest_entries() -> list[dict]:
    """The four category placeholders used when no manifest is authorized."""
    return [{"id": f"v04.real.{category}", "category": category, "status": "NOT_RUN",
             "reason": "no authorized acceptance manifest; real audio was not accessed"}
            for category in CATEGORIES]
