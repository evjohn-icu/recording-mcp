#!/usr/bin/env python3
"""Silero VAD (ONNX) on the same AISHELL-4 sessions used by the other PoCs."""

from __future__ import annotations

import argparse
import sys
import time
from pathlib import Path

import numpy as np

from common import (
    DATA,
    MODELS,
    RESULTS,
    SAMPLE_RATE,
    best_f1_threshold,
    binary_metrics,
    ensure_results,
    file_info,
    gpu_memory_used_mb,
    merge_intervals,
    peak_rss_mb,
    read_rttm,
    read_wav,
    write_json,
)
from prepare_eval import discover_sessions

CHUNK = 512  # 32 ms at 16 kHz for Silero VAD v5
CONTEXT = 64  # Silero v5 prepends the previous 64 samples


def load_session(path: Path, threads: int, provider: str = "cpu"):
    import onnxruntime as ort

    if provider == "cuda":
        if "CUDAExecutionProvider" not in ort.get_available_providers():
            raise SystemExit("CUDAExecutionProvider unavailable")
        # onnxruntime-gpu does not search torch's bundled nvidia/*/lib dirs on
        # its own; preload_dlls() loads them (cuDNN 9, cuBLAS 12) first.
        if hasattr(ort, "preload_dlls"):
            ort.preload_dlls()
        providers = ["CUDAExecutionProvider", "CPUExecutionProvider"]
    else:
        providers = ["CPUExecutionProvider"]
    options = ort.SessionOptions()
    options.intra_op_num_threads = threads
    options.inter_op_num_threads = 1
    options.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
    session = ort.InferenceSession(str(path), sess_options=options, providers=providers)
    # get_available_providers() only lists compiled-in providers; a missing
    # CUDA runtime library makes the session fall back to CPU silently.
    if provider == "cuda" and "CUDAExecutionProvider" not in session.get_providers():
        raise SystemExit(
            f"CUDAExecutionProvider requested but session fell back to {session.get_providers()}"
        )
    return session


def speech_at(centers: np.ndarray, segments: list[dict]) -> np.ndarray:
    intervals = merge_intervals(segments)
    if not intervals:
        return np.zeros(centers.size, dtype=bool)
    starts = np.asarray([a for a, _ in intervals], dtype=np.float64)
    ends = np.asarray([b for _, b in intervals], dtype=np.float64)
    idx = np.searchsorted(starts, centers, side="right") - 1
    result = np.zeros(centers.size, dtype=bool)
    valid = idx >= 0
    result[valid] = centers[valid] < ends[idx[valid]]
    return result


def run_vad(session, audio: np.ndarray, threads: int) -> tuple[np.ndarray, np.ndarray, float]:
    state = np.zeros((2, 1, 128), dtype=np.float32)
    context = np.zeros((1, CONTEXT), dtype=np.float32)
    sr = np.asarray(SAMPLE_RATE, dtype=np.int64)
    probs: list[float] = []
    started = time.perf_counter()
    for offset in range(0, len(audio), CHUNK):
        chunk = audio[offset : offset + CHUNK]
        if chunk.size < CHUNK:
            chunk = np.pad(chunk, (0, CHUNK - chunk.size))
        # Silero's ONNX contract is [64 samples of context, current 512 samples].
        # Omitting the context silently degrades the model instead of failing.
        model_input = np.concatenate([context, chunk.reshape(1, -1)], axis=1).astype(np.float32)
        feed = {"input": model_input, "state": state, "sr": sr}
        output, state = session.run(None, feed)
        # The graph returns a speech probability in [0, 1]; do not apply sigmoid.
        probs.append(float(output.reshape(-1)[0]))
        context = model_input[:, -CONTEXT:].copy()
    elapsed = time.perf_counter() - started
    centers = (np.arange(len(probs), dtype=np.float64) + 0.5) * CHUNK / SAMPLE_RATE
    return np.asarray(probs, dtype=np.float32), centers, elapsed


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", type=Path, default=MODELS / "silero-official" / "model.onnx")
    parser.add_argument("--data", type=Path, default=DATA / "aishell4")
    parser.add_argument(
        "--sessions",
        nargs="*",
        default=None,
        help="session ids; default discovers every wav with a matching rttm",
    )
    parser.add_argument("--threads", type=int, default=4)
    parser.add_argument("--provider", choices=["cpu", "cuda"], default="cpu")
    parser.add_argument("--out", type=Path, default=RESULTS / "vad_silero.json")
    args = parser.parse_args()

    ensure_results()
    session_ids = args.sessions if args.sessions else discover_sessions(args.data)
    if not session_ids:
        raise SystemExit(f"no sessions with wav+rttm found under {args.data / 'test'}")
    load_started = time.perf_counter()
    session = load_session(args.model, args.threads, args.provider)
    load_seconds = time.perf_counter() - load_started
    providers = list(session.get_providers())

    # Warm up on a short slice so the first measured pass excludes lazy init.
    gpu_start_mb = gpu_memory_used_mb() if args.provider == "cuda" else None
    warm, _ = read_wav(args.data / "test" / "wav" / f"{session_ids[0]}.wav", max_seconds=5.0)
    run_vad(session, warm, args.threads)

    all_true: list[np.ndarray] = []
    all_score: list[np.ndarray] = []
    per_session: dict[str, dict] = {}
    probs_out: dict[str, np.ndarray] = {}
    for session_id in session_ids:
        audio, sr = read_wav(args.data / "test" / "wav" / f"{session_id}.wav")
        if sr != SAMPLE_RATE:
            raise ValueError(f"expected {SAMPLE_RATE} Hz, got {sr}")
        segments = read_rttm(args.data / "test" / "TextGrid" / f"{session_id}.rttm")
        probs, centers, elapsed = run_vad(session, audio, args.threads)
        truth = speech_at(centers, segments)
        metrics = binary_metrics(truth, probs, threshold=0.5)
        metrics["best_f1"] = best_f1_threshold(truth, probs)
        metrics["audio_seconds"] = round(len(audio) / sr, 3)
        metrics["inference_seconds"] = round(elapsed, 4)
        metrics["rtf"] = elapsed / (len(audio) / sr)
        per_session[session_id] = metrics
        all_true.append(truth)
        all_score.append(probs)
        probs_out[session_id] = probs.astype(np.float16)
        print(
            f"{session_id}: F1={metrics['f1']:.4f} AUC={metrics['roc_auc']:.4f} "
            f"RTF={metrics['rtf']:.5f} speech={metrics['positive_frames']}/{metrics['n_frames']}"
        )

    truth = np.concatenate(all_true)
    score = np.concatenate(all_score)
    aggregate = binary_metrics(truth, score, threshold=0.5)
    aggregate["best_f1"] = best_f1_threshold(truth, score)
    total_audio = sum(v["audio_seconds"] for v in per_session.values())
    total_inference = sum(v["inference_seconds"] for v in per_session.values())
    gpu_peak_mb = None
    if args.provider == "cuda":
        gpu_end_mb = gpu_memory_used_mb()
        if gpu_start_mb is not None and gpu_end_mb is not None:
            gpu_peak_mb = round(max(0.0, gpu_end_mb - gpu_start_mb), 2)
    payload = {
        "engine": "silero-vad-onnx",
        "model": file_info(args.model),
        "sample_rate": SAMPLE_RATE,
        "chunk_samples": CHUNK,
        "context_samples": CONTEXT,
        "frame_shift_seconds": CHUNK / SAMPLE_RATE,
        "threads": args.threads,
        "provider": providers,
        "load_seconds": round(load_seconds, 4),
        "audio_seconds": round(total_audio, 3),
        "inference_seconds": round(total_inference, 4),
        "rtf": total_inference / total_audio if total_audio else None,
        "peak_rss_mb": round(peak_rss_mb(), 2),
        "gpu_peak_mb": gpu_peak_mb,
        "sessions": per_session,
        "aggregate": aggregate,
        "notes": [
            "32 ms frames; ground truth is the union of AISHELL-4 RTTM speaker segments.",
            "Threshold 0.5 is the primary operating point; best_f1 is an optimistic threshold sweep.",
            "gpu_peak_mb is whole-device nvidia-smi memory delta across the measured sessions, not per-process allocation.",
            "The CPUExecutionProvider remains the deployment target; the GPU row only measures resource headroom.",
        ],
    }
    write_json(args.out, payload)
    np.savez_compressed(RESULTS / "vad_silero_probs.npz", **probs_out)
    print(f"wrote {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
