#!/usr/bin/env python3
"""Voiceprint benchmark on AISHELL-4: 3D-Speaker CAM++ vs WeSpeaker CN-Celeb."""

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
    peak_rss_mb,
    read_wav,
    write_json,
)


def l2_normalize(vector: np.ndarray) -> np.ndarray:
    norm = float(np.linalg.norm(vector))
    return vector / norm if norm > 0 else vector


def extract_campplus(extractor, audio: np.ndarray) -> np.ndarray:
    stream = extractor.create_stream()
    stream.accept_waveform(SAMPLE_RATE, audio)
    stream.input_finished()
    return l2_normalize(np.asarray(extractor.compute(stream), dtype=np.float32))


def extract_wespeaker(session, audio: np.ndarray) -> np.ndarray:
    fbank = make_fbank()
    fbank.accept_waveform(SAMPLE_RATE, audio)
    fbank.input_finished()
    n_frames = fbank.num_frames_ready
    if n_frames == 0:
        raise ValueError("empty fbank")
    features = np.stack([fbank.get_frame(i) for i in range(n_frames)]).astype(np.float32)
    features = features - features.mean(axis=0, keepdims=True)
    embedding = session.run(None, {"feats": features[None, :, :]})[0][0]
    return l2_normalize(np.asarray(embedding, dtype=np.float32))


def make_fbank():
    import kaldi_native_fbank as knf

    opts = knf.FbankOptions()
    opts.frame_opts.samp_freq = SAMPLE_RATE
    opts.frame_opts.dither = 0.0
    opts.frame_opts.frame_length_ms = 25.0
    opts.frame_opts.frame_shift_ms = 10.0
    opts.frame_opts.snip_edges = True
    opts.mel_opts.num_bins = 80
    opts.mel_opts.low_freq = 20.0
    opts.mel_opts.high_freq = 0.0
    opts.energy_floor = 0.0
    opts.use_log_fbank = True
    opts.use_power = True
    return knf.OnlineFbank(opts)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", choices=["campplus", "wespeaker"], required=True)
    parser.add_argument("--trials", type=Path, default=RESULTS / "trials.json")
    parser.add_argument("--data", type=Path, default=DATA / "aishell4")
    parser.add_argument("--device", choices=["cpu"], default="cpu")
    parser.add_argument("--threads", type=int, default=4)
    parser.add_argument("--out", type=Path, default=None)
    args = parser.parse_args()

    ensure_results()
    trial_data = json.load(open(args.trials, encoding="utf-8"))
    speakers = trial_data["speakers"]
    trials = trial_data["trials"]
    audio_cache: dict[str, np.ndarray] = {}

    def audio_for(session: str) -> np.ndarray:
        if session not in audio_cache:
            path = args.data / "test" / "wav" / f"{session}.wav"
            wav, sr = read_wav(path)
            if sr != SAMPLE_RATE:
                raise ValueError(f"expected {SAMPLE_RATE} Hz, got {sr}")
            audio_cache[session] = wav
        return audio_cache[session]

    def slice_audio(session: str, start: float, end: float) -> np.ndarray:
        audio = audio_for(session)
        return audio[int(start * SAMPLE_RATE) : int(end * SAMPLE_RATE)]

    started_load = time.perf_counter()
    if args.model == "campplus":
        import sherpa_onnx

        model_path = MODELS / "campplus" / "campplus_zh_en_fp32.onnx"
        extractor = sherpa_onnx.SpeakerEmbeddingExtractor(
            sherpa_onnx.SpeakerEmbeddingExtractorConfig(
                model=str(model_path), num_threads=args.threads, provider="cpu"
            )
        )
        fbank = None
        backend = "sherpa-onnx SpeakerEmbeddingExtractor (CPU)"
        embed = lambda audio: extract_campplus(extractor, audio)
    else:
        import onnxruntime as ort

        model_path = MODELS / "wespeaker" / "cnceleb_resnet34_LM.onnx"
        options = ort.SessionOptions()
        options.intra_op_num_threads = args.threads
        options.inter_op_num_threads = 1
        session = ort.InferenceSession(
            str(model_path), sess_options=options, providers=["CPUExecutionProvider"]
        )
        backend = "onnxruntime (CPU), Kaldi 80-dim log-fbank + mean norm"
        embed = lambda audio: extract_wespeaker(session, audio)
    load_seconds = time.perf_counter() - started_load

    embedding_cache: dict[tuple[str, float, float], np.ndarray] = {}
    embedding_seconds = 0.0
    audio_seconds = 0.0

    def get_embedding(session: str, start: float, end: float) -> np.ndarray:
        nonlocal embedding_seconds, audio_seconds
        key = (session, round(start, 4), round(end, 4))
        if key not in embedding_cache:
            chunk = slice_audio(session, start, end)
            started = time.perf_counter()
            embedding_cache[key] = embed(chunk)
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
    payload = {
        "engine": "3D-Speaker CAM++" if args.model == "campplus" else "WeSpeaker CN-Celeb ResNet34-LM",
        "model": file_info(model_path),
        "backend": backend,
        "provider": "CPUExecutionProvider",
        "threads": args.threads,
        "embedding_dim": int(next(iter(embedding_cache.values())).shape[0]),
        "load_seconds": round(load_seconds, 4),
        "embedding_seconds": round(embedding_seconds, 4),
        "audio_seconds": round(audio_seconds, 3),
        "rtf": embedding_seconds / audio_seconds if audio_seconds else None,
        "peak_rss_mb": round(peak_rss_mb(), 2),
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
            "Trials are fixed in results/trials.json: one positive and one same-session negative per test segment.",
            "Enrollment is the mean of two disjoint segments per speaker; cosine scoring.",
            "AISHELL-4 is far-field meeting audio; this is a harder condition than VoxCeleb/CN-Celeb test sets.",
        ],
    }
    out = args.out or RESULTS / f"voiceprint_{args.model}_cpu.json"
    write_json(out, payload)
    print(
        json.dumps(
            {
                "engine": payload["engine"],
                "EER": metrics["eer"],
                "minDCF": metrics["min_dcf_normalized"],
                "RTF": payload["rtf"],
                "load_s": payload["load_seconds"],
                "dim": payload["embedding_dim"],
            },
            ensure_ascii=False,
        )
    )
    print(f"wrote {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
