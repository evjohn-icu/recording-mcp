"""Shared utilities for the audio-intelligence PoC.

This package is intentionally self-contained.  Nothing here is imported by the
recording-mcp application; the experiment only writes inside
``experiments/audio-intelligence-poc``.
"""

from __future__ import annotations

import hashlib
import json
import os
import resource
import subprocess
import time
from collections import defaultdict
from pathlib import Path
from typing import Any, Iterable

import numpy as np

ROOT = Path(__file__).resolve().parents[1]
DATA = ROOT / "data"
MODELS = ROOT / "models"
RESULTS = ROOT / "results"

SAMPLE_RATE = 16_000


def ensure_results() -> None:
    RESULTS.mkdir(parents=True, exist_ok=True)


def read_wav(path: str | Path, max_seconds: float | None = None) -> tuple[np.ndarray, int]:
    import soundfile as sf

    audio, sr = sf.read(str(path), dtype="float32", always_2d=False)
    if audio.ndim > 1:
        audio = audio.mean(axis=1)
    if max_seconds is not None:
        audio = audio[: int(max_seconds * sr)]
    return np.ascontiguousarray(audio, dtype=np.float32), int(sr)


def read_rttm(path: str | Path) -> list[dict[str, Any]]:
    """Read the subset of RTTM used by AISHELL-4: SPEAKER records."""
    records: list[dict[str, Any]] = []
    with open(path, "r", encoding="utf-8") as fh:
        for line in fh:
            fields = line.split()
            if len(fields) < 9 or fields[0] != "SPEAKER":
                continue
            start = float(fields[3])
            duration = float(fields[4])
            if duration <= 0:
                continue
            records.append(
                {
                    "file_id": fields[1],
                    "start": round(start, 6),
                    "end": round(start + duration, 6),
                    "speaker": fields[7],
                }
            )
    return records


def merge_intervals(segments: Iterable[dict[str, Any]]) -> list[tuple[float, float]]:
    intervals = sorted((float(s["start"]), float(s["end"])) for s in segments if s["end"] > s["start"])
    merged: list[list[float]] = []
    for start, end in intervals:
        if merged and start <= merged[-1][1] + 1e-9:
            merged[-1][1] = max(merged[-1][1], end)
        else:
            merged.append([start, end])
    return [(a, b) for a, b in merged]


def union_duration(segments: Iterable[dict[str, Any]]) -> float:
    return sum(end - start for start, end in merge_intervals(segments))


def overlap_duration(segments: Iterable[dict[str, Any]]) -> float:
    total = sum(max(0.0, float(s["end"]) - float(s["start"])) for s in segments)
    return max(0.0, total - union_duration(segments))


def frame_speech_labels(
    segments: Iterable[dict[str, Any]],
    duration: float,
    frame_shift: float = 0.032,
) -> tuple[np.ndarray, np.ndarray]:
    """Return (speech, frame_centers) at a fixed frame shift."""
    n_frames = int(np.floor(duration / frame_shift))
    centers = (np.arange(n_frames, dtype=np.float64) + 0.5) * frame_shift
    intervals = merge_intervals(segments)
    if not intervals:
        return np.zeros(n_frames, dtype=bool), centers
    starts = np.asarray([a for a, _ in intervals], dtype=np.float64)
    ends = np.asarray([b for _, b in intervals], dtype=np.float64)
    idx = np.searchsorted(starts, centers, side="right") - 1
    speech = np.zeros(n_frames, dtype=bool)
    valid = idx >= 0
    speech[valid] = centers[valid] < ends[idx[valid]]
    return speech, centers


def annotation_from_segments(segments: Iterable[dict[str, Any]]):
    from pyannote.core import Annotation, Segment

    annotation = Annotation()
    for seg in segments:
        start, end = float(seg["start"]), float(seg["end"])
        if end > start:
            annotation[Segment(start, end), str(seg["speaker"])] = str(seg["speaker"])
    return annotation


def der_metrics(
    ref_segments: Iterable[dict[str, Any]],
    hyp_segments: Iterable[dict[str, Any]],
    collar: float = 0.25,
    skip_overlap: bool = False,
) -> dict[str, float]:
    from pyannote.metrics.diarization import DiarizationErrorRate

    ref = annotation_from_segments(ref_segments)
    hyp = annotation_from_segments(hyp_segments)
    metric = DiarizationErrorRate(collar=collar, skip_overlap=skip_overlap)
    value = float(metric(ref, hyp))
    components: dict[str, float] = {}
    for key, raw in getattr(metric, "accumulated_", {}).items():
        try:
            components[str(key)] = float(np.asarray(raw).sum())
        except (TypeError, ValueError):
            continue
    return {
        "der": value,
        "collar": collar,
        "skip_overlap": skip_overlap,
        "components": components,
        "ref_speakers": len(ref.labels()),
        "hyp_speakers": len(hyp.labels()),
        "ref_speech_seconds": union_duration(ref_segments),
        "hyp_speech_seconds": union_duration(hyp_segments),
    }


def binary_metrics(y_true: np.ndarray, y_score: np.ndarray, threshold: float = 0.5) -> dict[str, Any]:
    from sklearn.metrics import (
        average_precision_score,
        confusion_matrix,
        f1_score,
        precision_score,
        recall_score,
        roc_auc_score,
    )

    y_true = np.asarray(y_true).astype(bool)
    y_score = np.asarray(y_score, dtype=np.float64)
    y_pred = y_score >= threshold
    result: dict[str, Any] = {
        "n_frames": int(y_true.size),
        "positive_frames": int(y_true.sum()),
        "threshold": float(threshold),
        "precision": float(precision_score(y_true, y_pred, zero_division=0)),
        "recall": float(recall_score(y_true, y_pred, zero_division=0)),
        "f1": float(f1_score(y_true, y_pred, zero_division=0)),
        "accuracy": float((y_true == y_pred).mean()),
    }
    if len(np.unique(y_true)) > 1:
        result["roc_auc"] = float(roc_auc_score(y_true, y_score))
        result["pr_auc"] = float(average_precision_score(y_true, y_score))
    else:
        result["roc_auc"] = None
        result["pr_auc"] = None
    tn, fp, fn, tp = confusion_matrix(y_true, y_pred, labels=[False, True]).ravel()
    result["confusion"] = {"tn": int(tn), "fp": int(fp), "fn": int(fn), "tp": int(tp)}
    return result


def best_f1_threshold(y_true: np.ndarray, y_score: np.ndarray) -> dict[str, float]:
    from sklearn.metrics import f1_score

    y_true = np.asarray(y_true).astype(bool)
    y_score = np.asarray(y_score, dtype=np.float64)
    best = (-1.0, 0.5)
    for threshold in np.linspace(0.01, 0.99, 99):
        score = float(f1_score(y_true, y_score >= threshold, zero_division=0))
        if score > best[0]:
            best = (score, float(threshold))
    return {"threshold": best[1], "f1": best[0]}


def compute_eer(labels: np.ndarray, scores: np.ndarray) -> dict[str, float]:
    from sklearn.metrics import roc_curve

    labels = np.asarray(labels).astype(int)
    scores = np.asarray(scores, dtype=np.float64)
    fpr, tpr, thresholds = roc_curve(labels, scores, pos_label=1)
    fnr = 1.0 - tpr
    idx = int(np.nanargmin(np.abs(fpr - fnr)))
    eer = float((fpr[idx] + fnr[idx]) / 2.0)
    threshold = float(thresholds[idx]) if idx < len(thresholds) else 0.0
    pred = scores >= threshold
    accuracy = float((pred.astype(int) == labels).mean())
    p_target, c_miss, c_fa = 0.01, 1.0, 1.0
    dcf = c_miss * p_target * fnr + c_fa * (1.0 - p_target) * fpr
    min_dcf = float(np.min(dcf))
    norm = min(c_miss * p_target, c_fa * (1.0 - p_target))
    return {
        "eer": eer,
        "eer_threshold": threshold,
        "accuracy_at_eer": accuracy,
        "min_dcf": min_dcf,
        "min_dcf_normalized": min_dcf / norm,
        "n_trials": int(labels.size),
        "n_positive": int(labels.sum()),
        "n_negative": int((1 - labels).sum()),
    }


def peak_rss_mb() -> float:
    return resource.getrusage(resource.RUSAGE_SELF).ru_maxrss / 1024.0


def sha256(path: str | Path) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def file_info(path: str | Path) -> dict[str, Any]:
    p = Path(path).resolve()
    try:
        rel = str(p.relative_to(ROOT))
    except ValueError:
        rel = str(p)
    return {"path": rel, "bytes": p.stat().st_size, "sha256": sha256(p)}


def write_json(path: str | Path, payload: Any) -> None:
    p = Path(path)
    p.parent.mkdir(parents=True, exist_ok=True)
    with open(p, "w", encoding="utf-8") as fh:
        json.dump(payload, fh, ensure_ascii=False, indent=2, sort_keys=True)
        fh.write("\n")


def run_command(argv: list[str], timeout: float = 60) -> dict[str, Any]:
    started = time.perf_counter()
    try:
        proc = subprocess.run(argv, capture_output=True, text=True, timeout=timeout)
        return {
            "argv": argv,
            "returncode": proc.returncode,
            "stdout": proc.stdout.strip(),
            "stderr": proc.stderr.strip(),
            "seconds": round(time.perf_counter() - started, 4),
        }
    except Exception as exc:  # pragma: no cover - environment probe
        return {
            "argv": argv,
            "returncode": None,
            "stdout": "",
            "stderr": f"{type(exc).__name__}: {exc}",
            "seconds": round(time.perf_counter() - started, 4),
        }


def gpu_query() -> dict[str, Any]:
    out = run_command(
        [
            "nvidia-smi",
            "--query-gpu=name,memory.total,memory.used,driver_version,utilization.gpu",
            "--format=csv,noheader,nounits",
        ]
    )
    fields: dict[str, Any] = {"raw": out}
    if out["returncode"] == 0 and out["stdout"]:
        row = [part.strip() for part in out["stdout"].splitlines()[0].split(",")]
        if len(row) >= 5:
            fields.update(
                {
                    "name": row[0],
                    "memory_total_mib": float(row[1]),
                    "memory_used_mib": float(row[2]),
                    "driver_version": row[3],
                    "utilization_gpu_pct": float(row[4]),
                }
            )
    return fields


def speaker_groups(segments: Iterable[dict[str, Any]]) -> dict[str, list[dict[str, Any]]]:
    groups: dict[str, list[dict[str, Any]]] = defaultdict(list)
    for seg in segments:
        groups[str(seg["speaker"])].append(seg)
    for values in groups.values():
        values.sort(key=lambda item: (item["start"], item["end"]))
    return dict(groups)


def clean_voiceprint_segments(
    segments: Iterable[dict[str, Any]],
    min_duration: float = 2.0,
    max_duration: float = 12.0,
    overlap_tolerance: float = 0.05,
) -> list[dict[str, Any]]:
    """Drop touching segments, then remove any segment that overlaps another speaker."""
    raw = [dict(seg) for seg in segments if seg["end"] - seg["start"] >= min_duration]
    clean: list[dict[str, Any]] = []
    for seg in raw:
        conflict = False
        for other in raw:
            if other is seg or other["speaker"] == seg["speaker"]:
                continue
            intersection = min(seg["end"], other["end"]) - max(seg["start"], other["start"])
            if intersection > overlap_tolerance:
                conflict = True
                break
        if conflict:
            continue
        end = min(float(seg["end"]), float(seg["start"]) + max_duration)
        if end - float(seg["start"]) >= min_duration:
            clean.append({"start": float(seg["start"]), "end": end, "speaker": str(seg["speaker"]), "session": seg["session"]})
    return clean


def now_iso() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%S%z")


def set_thread_env(threads: int) -> None:
    for key in ("OMP_NUM_THREADS", "MKL_NUM_THREADS", "OPENBLAS_NUM_THREADS", "NUMEXPR_NUM_THREADS"):
        os.environ[key] = str(threads)
