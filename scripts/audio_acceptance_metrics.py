#!/usr/bin/env python3
"""Pure audio-acceptance metrics shared by evaluation harnesses.

Every metric reports explicit numerators and denominators plus a status so
callers can distinguish:

- ``OK``: the metric was measured from the given inputs.
- ``NOT_EVALUABLE``: a denominator is zero (no reference speech, no gold
  events, no returned refs, ...). Values are ``None`` instead of a
  misleading 0 or 1.
- ``NOT_RUN``: required human input (e.g. adjudications) is missing; the
  semantic number is unknown, not zero.

All functions are pure and depend on the Python standard library only.
Text normalization and edit distance are shared with scripts/eval-asr-cer.py.
"""
from __future__ import annotations

import math
import re
import unicodedata
from collections import Counter

__all__ = [
    "STATUS_OK", "STATUS_NOT_EVALUABLE", "STATUS_NOT_RUN", "WER_TOKENIZATION",
    "normalize_text", "edit_distance", "cer", "wer", "der", "eer",
    "event_pr", "evidence_accuracy", "decision_precision", "aggregate_error_rates",
    "percentile",
]

STATUS_OK = "OK"
STATUS_NOT_EVALUABLE = "NOT_EVALUABLE"
STATUS_NOT_RUN = "NOT_RUN"

# Tokenization identifier reported by wer() unless the caller passes one.
WER_TOKENIZATION = "cjk-latin-v1"

# --- text normalization (shared with scripts/eval-asr-cer.py) -------------

CN_DIGITS = {"零": 0, "〇": 0, "一": 1, "二": 2, "两": 2, "三": 3, "四": 4,
             "五": 5, "六": 6, "七": 7, "八": 8, "九": 9, "幺": 1}
CN_UNITS = {"十": 10, "百": 100, "千": 1000}
CN_RUN = re.compile(r"[零〇一二三四五六七八九两幺十百千]+")
KEEP = re.compile(r"[^\u4e00-\u9fffA-Za-z0-9]")
_HAN = re.compile(r"[\u4e00-\u9fff]")
_WORD_RUN = re.compile(r"[A-Za-z0-9]+")


def _cn_number(run: str) -> str:
    if not any(ch in CN_UNITS for ch in run):
        return "".join(str(CN_DIGITS[ch]) for ch in run)
    total = 0
    digit = None
    for ch in run:
        if ch in CN_DIGITS:
            digit = CN_DIGITS[ch]
        else:
            total += (1 if digit is None else digit) * CN_UNITS[ch]
            digit = None
    return str(total + (digit or 0))


def normalize_text(text: str) -> str:
    """NFKC-normalize, fold Chinese numerals, keep Han/Latin/digit only."""
    text = unicodedata.normalize("NFKC", text)
    text = CN_RUN.sub(lambda match: _cn_number(match.group(0)), text)
    return KEEP.sub("", text)


def edit_distance(a, b) -> int:
    """Levenshtein distance over sequences (strings or lists of tokens)."""
    if len(a) < len(b):
        a, b = b, a
    previous = list(range(len(b) + 1))
    for i, ca in enumerate(a, 1):
        current = [i]
        for j, cb in enumerate(b, 1):
            current.append(min(previous[j] + 1, current[j - 1] + 1, previous[j - 1] + (ca != cb)))
        previous = current
    return previous[-1]


def _wer_tokens(text: str) -> list[str]:
    """NFKC text -> tokens: one per Han character, one per Latin/digit run."""
    text = unicodedata.normalize("NFKC", text)
    tokens: list[str] = []
    index = 0
    while index < len(text):
        if _HAN.match(text, index):
            tokens.append(text[index])
            index += 1
            continue
        match = _WORD_RUN.match(text, index)
        if match:
            tokens.append(match.group(0))
            index = match.end()
        else:
            index += 1
    return tokens


def _empty_text_result(tokenization: str) -> dict:
    return {"value": None, "numerator": None, "denominator": 0,
            "status": STATUS_NOT_EVALUABLE, "tokenization": tokenization}


def cer(reference: str, hypothesis: str) -> dict:
    """Character error rate: edits / normalized reference characters.

    Emptiness is decided after normalization, so punctuation-only text counts
    as empty. An empty reference is NOT_EVALUABLE; a non-empty reference with
    an empty hypothesis is the full error (value 1.0), never NOT_RUN.
    """
    ref_norm = normalize_text(reference if reference is not None else "")
    hyp_norm = normalize_text(hypothesis if hypothesis is not None else "")
    if not ref_norm:
        return {"value": None, "numerator": None, "denominator": 0,
                "status": STATUS_NOT_EVALUABLE}
    numerator = edit_distance(hyp_norm, ref_norm)
    denominator = len(ref_norm)
    return {"value": numerator / denominator, "numerator": numerator,
            "denominator": denominator, "status": STATUS_OK}


def wer(reference: str, hypothesis: str, tokenization: str = WER_TOKENIZATION) -> dict:
    """Word error rate: NFKC text, Han chars are single tokens, Latin/digit
    runs are word tokens. Chinese numerals are intentionally NOT folded here
    (unlike cer); the tokenization identifier is reported with the result.
    """
    ref_tokens = _wer_tokens(reference if reference is not None else "")
    hyp_tokens = _wer_tokens(hypothesis if hypothesis is not None else "")
    if not ref_tokens:
        result = _empty_text_result(tokenization)
        result["numerator"] = None
        return result
    numerator = edit_distance(hyp_tokens, ref_tokens)
    denominator = len(ref_tokens)
    return {"value": numerator / denominator, "numerator": numerator,
            "denominator": denominator, "status": STATUS_OK,
            "tokenization": tokenization}


# --- interval helpers ------------------------------------------------------

def _merged_intervals(intervals):
    merged: list[tuple[float, float]] = []
    for start, end in sorted((start, end) for start, end in intervals if end > start):
        if merged and start <= merged[-1][1]:
            if end > merged[-1][1]:
                merged[-1] = (merged[-1][0], end)
        else:
            merged.append((start, end))
    return merged


def _subtract_intervals(regions, holes):
    remaining: list[tuple[float, float]] = []
    for start, end in regions:
        cursor = start
        for hole_start, hole_end in holes:
            if hole_end <= cursor or hole_start >= end:
                continue
            if hole_start > cursor:
                remaining.append((cursor, hole_start))
            cursor = max(cursor, hole_end)
            if cursor >= end:
                break
        if cursor < end:
            remaining.append((cursor, end))
    return remaining


def _timed_items(items, name: str, label_key: str):
    parsed: list[tuple[float, float, object]] = []
    for index, item in enumerate(items or []):
        if not isinstance(item, dict):
            raise TypeError(f"{name}[{index}] must be a dict")
        try:
            start = float(item["start_ms"])
            end = float(item["end_ms"])
            label = item[label_key]
        except KeyError as exc:
            raise ValueError(f"{name}[{index}] is missing required field {exc}") from None
        if end < start:
            raise ValueError(f"{name}[{index}] has end_ms < start_ms")
        parsed.append((start, end, label))
    parsed.sort(key=lambda item: (item[0], item[1], str(item[2])))
    return parsed


def _crop_segments(segments, regions):
    cropped: list[tuple[float, float, object]] = []
    for start, end, speaker in segments:
        for region_start, region_end in regions:
            low = max(start, region_start)
            high = min(end, region_end)
            if high > low:
                cropped.append((low, high, speaker))
    cropped.sort(key=lambda item: (item[0], item[1], str(item[2])))
    return cropped


def _assign_max(weights):
    """Maximum-weight assignment (Hungarian, JV variant) over a rectangular
    matrix. Returns sorted (row, col) pairs; every row is assigned."""
    rows = len(weights)
    cols = len(weights[0]) if rows else 0
    if rows == 0 or cols == 0:
        return []
    transposed = rows > cols
    if transposed:
        matrix = [[weights[r][c] for r in range(rows)] for c in range(cols)]
        rows, cols = cols, rows
    else:
        matrix = [list(row) for row in weights]
    infinity = float("inf")
    u = [0.0] * (rows + 1)
    v = [0.0] * (cols + 1)
    matched = [0] * (cols + 1)  # matched[col] = 1-based row
    way = [0] * (cols + 1)
    for row_index in range(1, rows + 1):
        matched[0] = row_index
        col = 0
        minv = [infinity] * (cols + 1)
        used = [False] * (cols + 1)
        while True:
            used[col] = True
            current_row = matched[col]
            delta = infinity
            next_col = 0
            for candidate in range(1, cols + 1):
                if not used[candidate]:
                    cost = -matrix[current_row - 1][candidate - 1] - u[current_row] - v[candidate]
                    if cost < minv[candidate]:
                        minv[candidate] = cost
                        way[candidate] = col
                    if minv[candidate] < delta:
                        delta = minv[candidate]
                        next_col = candidate
            for candidate in range(cols + 1):
                if used[candidate]:
                    u[matched[candidate]] += delta
                    v[candidate] -= delta
                else:
                    minv[candidate] -= delta
            col = next_col
            if matched[col] == 0:
                break
        while col:
            matched[col] = matched[way[col]]
            col = way[col]
    pairs = []
    for col in range(1, cols + 1):
        if matched[col]:
            row, column = matched[col] - 1, col - 1
            if transposed:
                row, column = column, row
            pairs.append((row, column))
    return sorted(pairs)


# --- diarization error rate ------------------------------------------------

def der(reference_segments, hypothesis_segments, collar_ms: float = 0, skip_overlap: bool = False) -> dict:
    """Diarization error rate over ``{start_ms, end_ms, speaker}`` segments.

    Follows the pyannote.metrics convention: a collar of total width
    ``collar_ms`` is centered on each reference segment boundary, pairwise
    reference overlap regions are excluded when ``skip_overlap`` is true,
    evaluation regions are the union of reference and hypothesis support, and
    one globally optimal speaker mapping (Hungarian assignment maximizing
    co-occurring speaker time) is applied before counting. Per atom the
    numerator is ``max(ref_speakers, hyp_speakers) - correct``, so
    DER = (false_alarm + missed + confusion) / reference speaker time, with
    overlapping reference speakers each contributing their own time.
    """
    if collar_ms < 0:
        raise ValueError("collar_ms must be >= 0")
    reference = _timed_items(reference_segments, "reference", "speaker")
    hypothesis = _timed_items(hypothesis_segments, "hypothesis", "speaker")
    regions = _merged_intervals([(start, end) for start, end, _ in reference]
                                + [(start, end) for start, end, _ in hypothesis])
    if collar_ms:
        holes = []
        for start, end, _ in reference:
            holes.append((start - collar_ms / 2.0, start + collar_ms / 2.0))
            holes.append((end - collar_ms / 2.0, end + collar_ms / 2.0))
        regions = _subtract_intervals(regions, _merged_intervals(holes))
    if skip_overlap:
        holes = []
        for i in range(len(reference)):
            for j in range(i + 1, len(reference)):
                low = max(reference[i][0], reference[j][0])
                high = min(reference[i][1], reference[j][1])
                if high > low:
                    holes.append((low, high))
        regions = _subtract_intervals(regions, _merged_intervals(holes))
    reference = _crop_segments(reference, regions)
    hypothesis = _crop_segments(hypothesis, regions)
    bounds = sorted({bound for start, end, _ in reference + hypothesis for bound in (start, end)})
    atoms = []
    cooccurrence: dict[tuple[object, object], float] = {}
    for start, end in zip(bounds, bounds[1:]):
        duration = end - start
        if duration <= 0:
            continue
        r_labels = [speaker for s, e, speaker in reference if s <= start and e >= end]
        h_labels = [speaker for s, e, speaker in hypothesis if s <= start and e >= end]
        if not r_labels and not h_labels:
            continue
        atoms.append((duration, r_labels, h_labels))
        for r_label in r_labels:
            for h_label in h_labels:
                key = (r_label, h_label)
                cooccurrence[key] = cooccurrence.get(key, 0.0) + duration
    mapping: dict[object, object] = {}
    if cooccurrence:
        ref_labels = sorted({pair[0] for pair in cooccurrence}, key=str)
        hyp_labels = sorted({pair[1] for pair in cooccurrence}, key=str)
        weights = [[cooccurrence.get((r, h), 0.0) for h in hyp_labels] for r in ref_labels]
        for row, col in _assign_max(weights):
            if weights[row][col] > 0:
                mapping[hyp_labels[col]] = ref_labels[row]
    false_alarm = missed = confusion = total = 0.0
    for duration, r_labels, h_labels in atoms:
        total += duration * len(r_labels)
        r_counts = Counter(r_labels)
        h_counts = Counter(mapping[label] for label in h_labels if label in mapping)
        correct = sum(min(count, h_counts.get(label, 0)) for label, count in r_counts.items())
        matched = min(len(r_labels), len(h_labels))
        confusion += duration * (matched - correct)
        missed += duration * (len(r_labels) - matched)
        false_alarm += duration * (len(h_labels) - matched)
    if total <= 0:
        return {"value": None, "false_alarm_ms": false_alarm, "missed_ms": missed,
                "confusion_ms": confusion, "reference_speech_ms": total,
                "status": STATUS_NOT_EVALUABLE, "label_mapping": mapping}
    value = (false_alarm + missed + confusion) / total
    return {"value": value, "false_alarm_ms": false_alarm, "missed_ms": missed,
            "confusion_ms": confusion, "reference_speech_ms": total,
            "status": STATUS_OK, "label_mapping": mapping}


# --- speaker verification equal error rate ---------------------------------

def eer(same_scores, different_scores) -> dict:
    """Equal error rate from cosine-similarity trials in [-1, 1].

    Thresholds are the midpoints between consecutive unique observed scores.
    FAR (different trials accepted) and FRR (same trials rejected) are swept
    there; the crossing is found by linear interpolation between the two
    closest threshold points. Implicit ROC endpoints (far=1/frr=0 below all
    scores, far=0/frr=1 above) guarantee a crossing on the piecewise-linear
    curve. ``experimental`` is true when either class has fewer than 30
    trials.
    """
    same = [float(score) for score in same_scores or ()]
    different = [float(score) for score in different_scores or ()]
    result = {"value": None, "far": None, "frr": None, "threshold": None,
              "same_trials": len(same), "different_trials": len(different),
              "status": STATUS_NOT_EVALUABLE,
              "experimental": len(same) < 30 or len(different) < 30}
    if not same or not different:
        return result
    unique = sorted(set(same) | set(different))
    if len(unique) == 1:
        thresholds = [unique[0]]
    else:
        thresholds = [(low + high) / 2.0 for low, high in zip(unique, unique[1:])]

    def error_rates(threshold: float) -> tuple[float, float]:
        far = sum(1 for score in different if score >= threshold) / len(different)
        frr = sum(1 for score in same if score < threshold) / len(same)
        return far, frr

    points = [(unique[0] - 1.0, 1.0, 0.0)]
    for threshold in thresholds:
        far, frr = error_rates(threshold)
        points.append((threshold, far, frr))
    points.append((unique[-1] + 1.0, 0.0, 1.0))
    for threshold, far, frr in points[1:-1]:
        if far == frr:
            result.update(value=far, far=far, frr=frr, threshold=threshold, status=STATUS_OK)
            return result
    for (t1, far1, frr1), (t2, far2, frr2) in zip(points, points[1:]):
        if far1 - frr1 > 0 > far2 - frr2:
            denominator = (far2 - far1) - (frr2 - frr1)
            alpha = (frr1 - far1) / denominator
            value = far1 + alpha * (far2 - far1)
            threshold = t1 + alpha * (t2 - t1)
            result.update(value=value, far=value, frr=value, threshold=threshold, status=STATUS_OK)
            return result
    return result  # unreachable: the implicit endpoints guarantee a crossing


# --- acoustic event precision / recall -------------------------------------

def _interval_iou(start_a, end_a, start_b, end_b) -> float:
    intersection = min(end_a, end_b) - max(start_a, start_b)
    if intersection <= 0:
        return 0.0
    union = (end_a - start_a) + (end_b - start_b) - intersection
    return intersection / union if union > 0 else 0.0


def _label_metrics(tp: int, fp: int, fn: int, reference_count: int) -> dict:
    precision = tp / (tp + fp) if (tp + fp) else None
    recall = tp / (tp + fn) if reference_count > 0 else None
    if precision is not None and recall is not None:
        f1 = 2 * precision * recall / (precision + recall) if (precision + recall) > 0 else 0.0
    else:
        f1 = None
    status = STATUS_OK if reference_count > 0 else STATUS_NOT_EVALUABLE
    return {"tp": tp, "fp": fp, "fn": fn, "precision": precision,
            "recall": recall, "f1": f1, "status": status}


def _matched_pairs(edges, ref_count: int, hyp_count: int) -> set[tuple[int, int]]:
    """Maximum-cardinality bipartite matching over same-label edges.

    ``edges`` holds ``(ref_index, hyp_index)`` pairs that satisfy the label and
    overlap test. The pair set comes from the same Hungarian assignment the
    diarization metric uses, with unit weights on the allowed edges, so the
    cardinality is optimal instead of greedy. Rows and columns keep their
    canonical order, which makes the result deterministic.
    """
    if not edges or ref_count == 0 or hyp_count == 0:
        return set()
    allowed = set(edges)
    weights = [[1 if (row, col) in allowed else 0 for col in range(hyp_count)]
               for row in range(ref_count)]
    return {(row, col) for row, col in _assign_max(weights) if weights[row][col]}


def event_pr(reference_events, hypothesis_events, iou_threshold: float = 0.5) -> dict:
    """Acoustic event precision/recall with same-label one-to-one matching.

    Candidate pairs are matched by maximum cardinality per label (IoU >=
    threshold), never greedily, so two reference events that each overlap a
    single hypothesis event still count as two true positives. A label with
    zero reference events keeps its precision-side false positives but reports
    ``recall: null`` and status NOT_EVALUABLE. With no reference events at all
    the overall values are null (never 0 or 1).
    """
    if not 0.0 <= iou_threshold <= 1.0:
        raise ValueError("iou_threshold must be within [0, 1]")
    reference = _timed_items(reference_events, "reference", "label")
    hypothesis = _timed_items(hypothesis_events, "hypothesis", "label")
    labels = sorted({label for _, _, label in reference} | {label for _, _, label in hypothesis}, key=str)
    per_label = {}
    tp = 0
    for label in labels:
        ref_indices = [i for i, (_, _, item_label) in enumerate(reference) if item_label == label]
        hyp_indices = [j for j, (_, _, item_label) in enumerate(hypothesis) if item_label == label]
        edges = []
        for row, i in enumerate(ref_indices):
            r_start, r_end, _ = reference[i]
            for col, j in enumerate(hyp_indices):
                h_start, h_end, _ = hypothesis[j]
                if _interval_iou(r_start, r_end, h_start, h_end) >= iou_threshold:
                    edges.append((row, col))
        matched = _matched_pairs(edges, len(ref_indices), len(hyp_indices))
        label_tp = len(matched)
        tp += label_tp
        per_label[label] = _label_metrics(label_tp, len(hyp_indices) - label_tp,
                                          len(ref_indices) - label_tp, len(ref_indices))
    fp = len(hypothesis) - tp
    fn = len(reference) - tp
    if not reference:
        return {"precision": None, "recall": None, "f1": None, "tp": tp, "fp": fp,
                "fn": fn, "per_label": per_label, "status": STATUS_NOT_EVALUABLE}
    metrics_ = _label_metrics(tp, fp, fn, len(reference))
    return {"precision": metrics_["precision"], "recall": metrics_["recall"],
            "f1": metrics_["f1"], "tp": tp, "fp": fp, "fn": fn,
            "per_label": per_label, "status": STATUS_OK}


# --- evidence accuracy ------------------------------------------------------

def _ref_key(ref: dict):
    """The stable ref identifier used by adjudications (``ref_id`` or ``id``)."""
    return ref.get("ref_id") or ref.get("id")


def evidence_accuracy(returned_refs, gold_spans, scope_ids, ambient_ids, forgotten_ids,
                      tolerance_ms: float = 250, semantic_judgments=None) -> dict:
    """Accuracy of returned evidence refs against human gold support spans.

    The accuracy denominator is every returned ref, not only the structurally
    legal ones: an invalid, stale, ambient, forgotten, out-of-scope or
    wrong-recording/wrong-revision ref is a returned error and lowers the
    number. ``scope_ids`` empty means no scope restriction. Zero returned refs
    is NOT_EVALUABLE with a null accuracy; a non-empty set of refs that are all
    illegal is 0.0, never null.

    A ref supports the answer when it is legal and its time span lies inside a
    gold span expanded by ``tolerance_ms`` on both endpoints (conversation,
    recording and revision ids must match when both sides carry them, and turn
    ids likewise; a ref without a turn id can still match by span).

    ``legal_rate`` is the independent structural number (legal refs / returned
    refs). ``semantic_accuracy`` needs a human judgment per returned ref given
    as ``semantic_judgments`` (a mapping from ref id to bool): it is NOT_RUN
    until every returned ref is judged. ``coverage`` is the fraction of
    required gold spans covered by at least one supported ref.
    """
    if tolerance_ms < 0:
        raise ValueError("tolerance_ms must be >= 0")
    scope = set(scope_ids or ())
    ambient = set(ambient_ids or ())
    forgotten = set(forgotten_ids or ())
    refs = list(returned_refs or [])
    gold = list(gold_spans or [])
    for index, span in enumerate(gold):
        if not isinstance(span, dict):
            raise TypeError(f"gold_spans[{index}] must be a dict")
        if "start_ms" not in span or "end_ms" not in span:
            raise ValueError(f"gold_spans[{index}] needs start_ms and end_ms")
    covered = [False] * len(gold)
    errors = Counter()
    supported = 0
    legal_refs = 0
    for ref in refs:
        if not isinstance(ref, dict):
            raise TypeError("each returned ref must be a dict")
        turn_id = ref.get("turn_id")
        conversation_id = ref.get("conversation_id")
        recording_id = ref.get("recording_id")
        revision = ref.get("transcript_revision")
        illegal = None
        if ref.get("valid", True) is False:
            illegal = "invalid"
        elif ref.get("current", True) is False:
            illegal = "stale"
        elif (turn_id is not None and turn_id in ambient) or \
                (conversation_id is not None and conversation_id in ambient):
            illegal = "ambient"
        elif (turn_id is not None and turn_id in forgotten) or \
                (conversation_id is not None and conversation_id in forgotten):
            illegal = "forgotten"
        elif scope and conversation_id not in scope:
            illegal = "out_of_scope"
        start = ref.get("start_ms")
        end = ref.get("end_ms")
        if illegal is None and (start is None or end is None or end <= start):
            illegal = "no_span"
        if illegal is not None:
            errors[illegal] += 1
            continue
        legal_refs += 1
        matched_span = None
        failure = None
        for index, span in enumerate(gold):
            gold_conversation = span.get("conversation_id")
            if gold_conversation is not None and conversation_id != gold_conversation:
                failure = failure or "wrong_conversation"
                continue
            gold_recording = span.get("recording_id")
            if gold_recording is not None and recording_id is not None and recording_id != gold_recording:
                failure = failure or "wrong_recording"
                continue
            gold_revision = span.get("transcript_revision")
            if gold_revision is not None and revision is not None and revision != gold_revision:
                failure = failure or "wrong_revision"
                continue
            gold_turn = span.get("turn_id")
            if turn_id is not None and gold_turn is not None and turn_id != gold_turn:
                continue
            if start < span["start_ms"] - tolerance_ms or end > span["end_ms"] + tolerance_ms:
                continue
            matched_span = index
            break
        if matched_span is None:
            if failure is not None:
                errors[failure] += 1
            continue
        supported += 1
        covered[matched_span] = True
    required_total = sum(1 for span in gold if span.get("required"))
    required_covered = sum(1 for index, span in enumerate(gold)
                           if span.get("required") and covered[index])
    returned = len(refs)
    judged = semantic_judgments or {}
    judged_refs = [ref for ref in refs if isinstance(ref, dict) and _ref_key(ref) in judged]
    semantic_accepted = sum(1 for ref in judged_refs if bool(judged[_ref_key(ref)]))
    semantic_complete = returned > 0 and len(judged_refs) == returned
    return {"accuracy": supported / returned if returned else None,
            "accuracy_status": STATUS_OK if returned else STATUS_NOT_EVALUABLE,
            "supported_refs": supported, "returned_refs": returned,
            "legal_refs": legal_refs, "legal_ref_rate": legal_refs / returned if returned else None,
            "error_counts": dict(sorted(errors.items())),
            "semantic_accuracy": semantic_accepted / len(judged_refs) if semantic_complete else None,
            "semantic_status": STATUS_OK if semantic_complete else STATUS_NOT_RUN,
            "semantic_accepted": semantic_accepted, "semantic_adjudicated": len(judged_refs),
            "coverage": required_covered / required_total if required_total else None,
            "required_total": required_total, "required_covered": required_covered,
            "status": STATUS_OK if returned else STATUS_NOT_EVALUABLE}


# --- decision / commitment precision ----------------------------------------

def decision_precision(claims, adjudications) -> dict:
    """Precision of predicted claims against human adjudications.

    A claim counts as adjudicated only when an adjudication carries the same
    ``claim_id`` and the same ``prediction_digest``; everything else stays
    unadjudicated, is never counted as accepted or rejected, and keeps the
    formal precision unknown. A kind's precision is reported only when every
    predicted claim of that kind has been adjudicated, and the overall
    precision only when every predicted claim has been; a partially reviewed
    set reports null with the review counts instead of the selected subset's
    score. Zero predicted claims is NOT_EVALUABLE, not NOT_RUN.
    """
    claim_items = list(claims or [])
    adjudication_items = list(adjudications or [])
    for index, claim in enumerate(claim_items):
        if not isinstance(claim, dict) or "claim_id" not in claim or "kind" not in claim:
            raise ValueError(f"claims[{index}] needs claim_id and kind")
    by_kind: dict[str, dict] = {}
    accepted_total = 0
    adjudicated_total = 0
    for claim in claim_items:
        kind = str(claim["kind"])
        bucket = by_kind.setdefault(kind, {"precision": None, "total": 0, "accepted": 0,
                                           "adjudicated": 0, "unadjudicated": 0,
                                           "status": STATUS_NOT_RUN})
        bucket["total"] += 1
        match = None
        for adjudication in adjudication_items:
            if (isinstance(adjudication, dict)
                    and adjudication.get("claim_id") == claim["claim_id"]
                    and adjudication.get("prediction_digest") == claim.get("prediction_digest")):
                match = adjudication
                break
        if match is None:
            bucket["unadjudicated"] += 1
            continue
        bucket["adjudicated"] += 1
        adjudicated_total += 1
        if match.get("accepted"):
            bucket["accepted"] += 1
            accepted_total += 1
    for kind in sorted(by_kind):
        bucket = by_kind[kind]
        if bucket["total"] and bucket["adjudicated"] == bucket["total"]:
            bucket["precision"] = bucket["accepted"] / bucket["total"]
            bucket["status"] = STATUS_OK
    total = len(claim_items)
    if total == 0:
        overall = {"precision": None, "total": 0, "accepted": 0, "adjudicated": 0,
                   "unadjudicated": 0, "status": STATUS_NOT_EVALUABLE}
    elif adjudicated_total == total:
        overall = {"precision": accepted_total / total, "total": total, "accepted": accepted_total,
                   "adjudicated": adjudicated_total, "unadjudicated": 0, "status": STATUS_OK}
    else:
        overall = {"precision": None, "total": total, "accepted": accepted_total,
                   "adjudicated": adjudicated_total, "unadjudicated": total - adjudicated_total,
                   "status": STATUS_NOT_RUN}
    return {**overall, "by_kind": {kind: by_kind[kind] for kind in sorted(by_kind)}}


def aggregate_error_rates(results) -> dict:
    """Pooled micro-average of ``cer``/``wer``-style results.

    Numerators and denominators are summed, never averaged over rates. A
    non-empty reference with an empty hypothesis contributes its full
    deletion count, which ``cer``/``wer`` already report.
    """
    numerator = 0
    denominator = 0
    for result in results or ():
        if not isinstance(result, dict) or result.get("status") != STATUS_OK:
            continue
        if result.get("numerator") is None or not result.get("denominator"):
            continue
        numerator += int(result["numerator"])
        denominator += int(result["denominator"])
    if denominator == 0:
        return {"value": None, "numerator": None, "denominator": 0,
                "status": STATUS_NOT_EVALUABLE}
    return {"value": numerator / denominator, "numerator": numerator,
            "denominator": denominator, "status": STATUS_OK}


# --- percentile ---------------------------------------------------------------

def percentile(values, p: float):
    """Linear-interpolation percentile: sorted, index = (n - 1) * p.

    Returns None for an empty list; raises ValueError for p outside [0, 1].
    """
    if not 0.0 <= p <= 1.0:
        raise ValueError("p must be within [0, 1]")
    if not values:
        return None
    ordered = sorted(float(value) for value in values)
    index = (len(ordered) - 1) * p
    low = math.floor(index)
    high = math.ceil(index)
    if low == high:
        return ordered[low]
    fraction = index - low
    return ordered[low] + (ordered[high] - ordered[low]) * fraction


if __name__ == "__main__":  # pragma: no cover - smoke check
    print(cer("十五分钟", "15分钟"))
    print(percentile([1, 2, 3, 4], 0.5))
