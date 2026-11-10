#!/usr/bin/env python3
"""Voiceprint benchmark with a raw ONNX Runtime backend (CPU or CUDA).

The sherpa-onnx path in ``run_voiceprint.py`` stays the reference implementation;
this script exists to measure the same models under an explicit ONNX Runtime
provider without changing the sherpa numbers.
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
    compute_eer,
    ensure_results,
    file_info,
    gpu_memory_used_mb,
    peak_rss_mb,
    read_wav,
    write_json,
)
from run_voiceprint import l2_normalize, make_fbank

MODEL_SPECS = {
    "campplus": {
        "path": MODELS / "campplus" / "campplus_zh_en_fp32.onnx",
        "input": "x",
        "output": "embedding",
        "engine": "3D-Speaker CAM++",
    },
    "wespeaker": {
        "path": MODELS / "wespeaker" / "cnceleb_resnet34_LM.onnx",
        "input": "feats",
        "output": "embs",
        "engine": "WeSpeaker CN-Celeb ResNet34-LM",
    },
}


def load_session(path: Path, threads: int, device: str):
    import onnxruntime as ort

    available = ort.get_available_providers()
    if device == "cuda":
        if "CUDAExecutionProvider" not in available:
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
    session = ort.InferenceSession(str(path), sess_options=options, providers=providers)
    # A missing CUDA runtime library degrades the session to CPU without an
    # error, which would mislabel CPU numbers as GPU results.
    if device == "cuda" and "CUDAExecutionProvider" not in session.get_providers():
        raise SystemExit(
            f"CUDAExecutionProvider requested but session fell back to {session.get_providers()}"
        )
    return session


def extract(session, input_name: str, output_name: str, audio: np.ndarray) -> np.ndarray:
    fbank = make_fbank()
    fbank.accept_waveform(SAMPLE_RATE, np.asarray(audio, dtype=np.float32))
    fbank.input_finished()
    n_frames = fbank.num_frames_ready
    if n_frames == 0:
        raise ValueError("empty fbank")
    features = np.stack([fbank.get_frame(i) for i in range(n_frames)]).astype(np.float32)
    features = features - features.mean(axis=0, keepdims=True)
    embedding = session.run(None, {input_name: features[None, :, :]})[0][0]
    return l2_normalize(np.asarray(embedding, dtype=np.float32))


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", choices=sorted(MODEL_SPECS), required=True)
    parser.add_argument("--device", choices=["cpu", "cuda"], default="cpu")
    parser.add_argument("--trials", type=Path, default=RESULTS / "trials.json")
    parser.add_argument("--data", type=Path, default=DATA / "aishell4")
    parser.add_argument("--threads", type=int, default=4)
    parser.add_argument("--out", type=Path, default=None)
    args = parser.parse_args()

    ensure_results()
    spec = MODEL_SPECS[args.model]
    trial_data = json.load(open(args.trials, encoding="utf-8"))
    speakers = trial_data["speakers"]
    trials = trial_data["trials"]
    audio_cache: dict[str, np.ndarray] = {}

    def audio_for(session_id: str) -> np.ndarray:
        if session_id not in audio_cache:
            wav, sr = read_wav(args.data / "test" / "wav" / f"{session_id}.wav")
            if sr != SAMPLE_RATE:
                raise ValueError(f"expected {SAMPLE_RATE} Hz, got {sr}")
            audio_cache[session_id] = wav
        return audio_cache[session_id]

    def slice_audio(session_id: str, start: float, end: float) -> np.ndarray:
        audio = audio_for(session_id)
        return audio[int(start * SAMPLE_RATE) : int(end * SAMPLE_RATE)]

    gpu_start_mb = gpu_memory_used_mb() if args.device == "cuda" else None
    started_load = time.perf_counter()
    session = load_session(spec["path"], args.threads, args.device)
    load_seconds = time.perf_counter() - started_load
    providers = list(session.get_providers())

    embedding_cache: dict[tuple[str, float, float], np.ndarray] = {}
    embedding_seconds = 0.0
    audio_seconds = 0.0

    def get_embedding(session_id: str, start: float, end: float) -> np.ndarray:
        nonlocal embedding_seconds, audio_seconds
        key = (session_id, round(start, 4), round(end, 4))
        if key not in embedding_cache:
            chunk = slice_audio(session_id, start, end)
            started = time.perf_counter()
            embedding_cache[key] = extract(session, spec["input"], spec["output"], chunk)
            embedding_seconds += time.perf_counter() - started
            audio_seconds += len(chunk) / SAMPLE_RATE
        return embedding_cache[key]

    enrollment: dict[str, np.ndarray] = {}
    for key, info in speakers.items():
        vectors = [
            get_embedding(info["session"], seg["start"], seg["end"]) for seg in info["enroll"]
        ]
        enrollment[key] = l2_normalize(np.mean(vectors, axis=0))

    labels = np.asarray([trial["label"] for trial in trials], dtype=int)
    scores = np.asarray(
        [
            float(
                np.dot(
                    get_embedding(
                        trial["test"]["session"], trial["test"]["start"], trial["test"]["end"]
                    ),
                    enrollment[trial["enroll_speaker"]],
                )
            )
            for trial in trials
        ],
        dtype=np.float64,
    )
    metrics = compute_eer(labels, scores)
    positive = scores[labels == 1]
    negative = scores[labels == 0]
    gpu_peak_mb = None
    if args.device == "cuda":
        gpu_end_mb = gpu_memory_used_mb()
        if gpu_start_mb is not None and gpu_end_mb is not None:
            gpu_peak_mb = round(max(0.0, gpu_end_mb - gpu_start_mb), 2)

    payload = {
        "engine": spec["engine"],
        "model": file_info(spec["path"]),
        "backend": f"onnxruntime {args.device}, Kaldi 80-dim log-fbank + mean norm",
        "provider": providers,
        "device": args.device,
        "threads": args.threads,
        "embedding_dim": int(next(iter(embedding_cache.values())).shape[0]),
        "load_seconds": round(load_seconds, 4),
        "embedding_seconds": round(embedding_seconds, 4),
        "audio_seconds": round(audio_seconds, 3),
        "rtf": embedding_seconds / audio_seconds if audio_seconds else None,
        "peak_rss_mb": round(peak_rss_mb(), 2),
        "gpu_peak_mb": gpu_peak_mb,
        "metrics": metrics,
        "supplementary": {
            "positive_score_mean": float(positive.mean()),
            "positive_score_std": float(positive.std()),
            "negative_score_mean": float(negative.mean()),
            "negative_score_std": float(negative.std()),
            "n_enrollment_speakers": len(enrollment),
            "n_unique_segments": len(embedding_cache),
        },
        "trials": [
            {
                "label": int(trial["label"]),
                "score": float(score),
                "test": trial["test"],
                "enroll_speaker": trial["enroll_speaker"],
            }
            for trial, score in zip(trials, scores)
        ],
        "notes": [
            "Trials and enrollment segments come from the fixed results/trials*.json file.",
            "Enrollment is the mean of two disjoint segments per speaker; cosine scoring.",
            "gpu_peak_mb is whole-device nvidia-smi memory delta, not per-process allocation.",
            "The frontend is explicit Kaldi fbank + mean subtraction, so tiny EER differences from the sherpa path are expected.",
        ],
    }
    out = args.out or RESULTS / f"voiceprint_{args.model}_{args.device}.json"
    write_json(out, payload)
    print(
        json.dumps(
            {
                "engine": payload["engine"],
                "device": args.device,
                "trials": metrics["n_trials"],
                "EER": metrics["eer"],
                "minDCF": metrics["min_dcf_normalized"],
                "RTF": payload["rtf"],
                "dim": payload["embedding_dim"],
                "gpu_peak_mb": gpu_peak_mb,
            },
            ensure_ascii=False,
        )
    )
    print(f"wrote {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
