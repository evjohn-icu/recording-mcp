#!/usr/bin/env python3
"""Estimate the speaker count from Silero-VAD windows + CAM++ agglomerative clustering.

sherpa-onnx threshold clustering over-segments AISHELL-4, so the edge
deployment needs a speaker-count estimate before diarization.
"""

from __future__ import annotations

import argparse
import json
import sys
import time
from pathlib import Path

import numpy as np

from common import (
    DATA,
    MODELS,
    RESULTS,
    SAMPLE_RATE,
    ensure_results,
    file_info,
    peak_rss_mb,
    read_rttm,
    read_wav,
    write_json,
)
from run_vad_silero import CHUNK, load_session, run_vad
from run_voiceprint import l2_normalize

WINDOW_SECONDS = 1.5
HOP_SECONDS = 0.75
MIN_WINDOW_SPEECH = 0.1
MAX_WINDOWS = 200
THRESHOLDS = tuple(round(float(t), 2) for t in np.arange(0.3, 1.01, 0.1))
SILHOUETTE_SEED = 20261110
MIN_SILHOUETTE = 0.05
MAX_SPEAKERS = 12


def select_windows(probs: np.ndarray, duration: float) -> list[tuple[float, float]]:
    frame_seconds = CHUNK / SAMPLE_RATE
    candidates: list[tuple[float, float]] = []
    start = 0.0
    while start + WINDOW_SECONDS <= duration:
        first = int(start / frame_seconds)
        last = min(int((start + WINDOW_SECONDS) / frame_seconds), probs.size)
        if last > first and float(probs[first:last].mean()) >= MIN_WINDOW_SPEECH:
            candidates.append((start, start + WINDOW_SECONDS))
        start += HOP_SECONDS
    if len(candidates) <= MAX_WINDOWS:
        return candidates
    picks = np.linspace(0, len(candidates) - 1, MAX_WINDOWS).round().astype(int)
    return [candidates[i] for i in sorted(set(picks.tolist()))]


def embed_windows(extractor, audio: np.ndarray, windows: list[tuple[float, float]]) -> np.ndarray:
    vectors = []
    for start, end in windows:
        chunk = audio[int(start * SAMPLE_RATE) : int(end * SAMPLE_RATE)]
        stream = extractor.create_stream()
        stream.accept_waveform(SAMPLE_RATE, chunk)
        stream.input_finished()
        vectors.append(l2_normalize(np.asarray(extractor.compute(stream), dtype=np.float32)))
    return np.stack(vectors)


def estimate(embeddings: np.ndarray) -> dict:
    from sklearn.cluster import AgglomerativeClustering
    from sklearn.metrics import silhouette_score

    sample = embeddings
    if sample.shape[0] > 2000:
        rng = np.random.default_rng(SILHOUETTE_SEED)
        sample = sample[np.sort(rng.choice(sample.shape[0], 2000, replace=False))]

    best: dict | None = None
    tried: list[dict] = []
    for threshold in THRESHOLDS:
        labels = AgglomerativeClustering(
            n_clusters=None,
            metric="cosine",
            linkage="average",
            distance_threshold=threshold,
        ).fit_predict(sample)
        n_clusters = int(labels.max()) + 1
        # A single cluster has no silhouette value; it cannot be the best
        # threshold even if it is the "correct" count by luck.
        if 1 < n_clusters < sample.shape[0]:
            score = float(silhouette_score(sample, labels, metric="cosine"))
        else:
            score = None
        tried.append({"threshold": threshold, "clusters": n_clusters, "silhouette": score})
        if score is not None and (best is None or score > best["silhouette"]):
            best = {"threshold": threshold, "silhouette": score, "clusters": n_clusters}

    if best is None:
        return {
            "estimated_speakers": None,
            "silhouette": None,
            "threshold": None,
            "reliable": False,
            "sweep": tried,
        }
    k = max(1, min(int(best["clusters"]), MAX_SPEAKERS))
    return {
        "estimated_speakers": k,
        "raw_clusters": int(best["clusters"]),
        "silhouette": best["silhouette"],
        "threshold": best["threshold"],
        "reliable": bool(best["silhouette"] >= MIN_SILHOUETTE and 1 <= k <= MAX_SPEAKERS),
        "sweep": tried,
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--session", nargs="+", required=True)
    parser.add_argument("--data", type=Path, default=DATA / "aishell4")
    parser.add_argument("--max-seconds", type=float, default=None)
    parser.add_argument("--threads", type=int, default=4)
    parser.add_argument("--vad-model", type=Path, default=MODELS / "silero-official" / "model.onnx")
    parser.add_argument(
        "--embedding", type=Path, default=MODELS / "campplus" / "campplus_zh_en_fp32.onnx"
    )
    parser.add_argument("--out", type=Path, default=RESULTS / "speaker_count_estimator.json")
    args = parser.parse_args()

    import sherpa_onnx

    ensure_results()
    load_started = time.perf_counter()
    vad = load_session(args.vad_model, args.threads, "cpu")
    extractor = sherpa_onnx.SpeakerEmbeddingExtractor(
        sherpa_onnx.SpeakerEmbeddingExtractorConfig(
            model=str(args.embedding), num_threads=args.threads, provider="cpu"
        )
    )
    load_seconds = time.perf_counter() - load_started

    sessions: dict[str, dict] = {}
    for session_id in args.session:
        audio, sr = read_wav(
            args.data / "test" / "wav" / f"{session_id}.wav", max_seconds=args.max_seconds
        )
        if sr != SAMPLE_RATE:
            raise ValueError(f"expected {SAMPLE_RATE} Hz, got {sr}")
        duration = len(audio) / sr
        probs, _, vad_seconds = run_vad(vad, audio, args.threads)
        windows = select_windows(probs, duration)
        if len(windows) < 2:
            sessions[session_id] = {
                "audio_seconds": round(duration, 3),
                "window_count": len(windows),
                "estimated_speakers": None,
                "silhouette": None,
                "threshold": None,
                "reference_speakers": None,
                "reliable": False,
                "error": "fewer than 2 speech windows",
            }
            print(f"{session_id}: fewer than 2 speech windows")
            continue
        started = time.perf_counter()
        embeddings = embed_windows(extractor, audio, windows)
        embedding_seconds = time.perf_counter() - started
        result = estimate(embeddings)
        reference = read_rttm(args.data / "test" / "TextGrid" / f"{session_id}.rttm")
        # Compare against the reference speakers that actually appear inside the
        # measured scope, otherwise a truncated run is scored against the full session.
        if args.max_seconds is not None:
            reference = [seg for seg in reference if seg["start"] < args.max_seconds]
        reference_speakers = len({seg["speaker"] for seg in reference})
        sessions[session_id] = {
            "audio_seconds": round(duration, 3),
            "window_count": len(windows),
            "vad_seconds": round(vad_seconds, 4),
            "embedding_seconds": round(embedding_seconds, 4),
            "embedding_dim": int(embeddings.shape[1]),
            "reference_speakers": reference_speakers,
            **result,
        }
        print(
            f"{session_id}: estimated={result['estimated_speakers']} "
            f"reference={reference_speakers} silhouette={result['silhouette']} "
            f"threshold={result['threshold']} reliable={result['reliable']}"
        )

    payload = {
        "engine": "silero-vad + CAM++ agglomerative (cosine, average linkage)",
        "vad_model": file_info(args.vad_model),
        "embedding_model": file_info(args.embedding),
        "window_seconds": WINDOW_SECONDS,
        "hop_seconds": HOP_SECONDS,
        "min_window_speech_probability": MIN_WINDOW_SPEECH,
        "max_windows": MAX_WINDOWS,
        "thresholds": list(THRESHOLDS),
        "min_silhouette": MIN_SILHOUETTE,
        "max_speakers": MAX_SPEAKERS,
        "load_seconds": round(load_seconds, 4),
        "peak_rss_mb": round(peak_rss_mb(), 2),
        "n_sessions": len(sessions),
        "sessions": sessions,
        "notes": [
            "Windows are 1.5 s with a 0.75 s hop, kept when the mean Silero probability is at least 0.1.",
            "The threshold with the highest cosine silhouette wins; a single-cluster split is never eligible.",
            "reliable requires silhouette >= 0.05 and 1 <= estimated_speakers <= 12; unreliable estimates must not drive sherpa num_clusters.",
        ],
    }
    write_json(args.out, payload)
    print(f"wrote {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
