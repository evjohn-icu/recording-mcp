#!/usr/bin/env python3
"""Ambient audio tagging: sherpa-onnx CED vs PANNs CNN14 on ESC-50.

ESC-50 is used only as a labelled, public evaluation set.  It is
CC BY-NC 3.0 (non-commercial); the report records that restriction.
"""

from __future__ import annotations

import argparse
import csv
import json
import sys
import time
from pathlib import Path

import numpy as np

from common import DATA, MODELS, RESULTS, ensure_results, file_info, peak_rss_mb, read_wav, write_json

# ESC-50 category -> AudioSet display name.  The names are resolved against
# each model's own 527-class label table; this keeps the index mapping honest.
ESC_TO_AUDIOSET = {
    "dog": "Dog",
    "cat": "Cat",
    "rooster": "Chicken, rooster",
    "cow": "Cattle, bovinae",
    "frog": "Frog",
    "pig": "Pig",
    "rain": "Rain",
    "sea_waves": "Waves, surf",
    "crackling_fire": "Crackle",
    "crickets": "Cricket",
    "chirping_birds": "Bird vocalization, bird call, bird song",
    "water_drops": "Drip",
    "wind": "Wind",
    "thunder": "Thunder",
    "crying_baby": "Baby cry, infant cry",
    "clapping": "Applause",
    "laughing": "Laughter",
    "snoring": "Snoring",
    "chainsaw": "Chainsaw",
    "helicopter": "Helicopter",
    "siren": "Siren",
}


def resolve_label(name: str, labels: list[str]) -> int | None:
    exact = {label: idx for idx, label in enumerate(labels)}
    if name in exact:
        return exact[name]
    folded = {label.lower(): idx for idx, label in enumerate(labels)}
    if name.lower() in folded:
        return folded[name.lower()]
    lower = name.lower()
    candidates = [(idx, label) for idx, label in enumerate(labels) if lower in label.lower()]
    if candidates:
        return candidates[0][0]
    return None


def load_esc50(root: Path, limit_per_class: int | None = None) -> list[dict]:
    rows = list(csv.DictReader(open(root / "meta" / "esc50.csv", encoding="utf-8")))
    selected = [row for row in rows if row["category"] in ESC_TO_AUDIOSET]
    if limit_per_class is not None:
        counts: dict[str, int] = {}
        limited = []
        for row in selected:
            category = row["category"]
            if counts.get(category, 0) < limit_per_class:
                limited.append(row)
                counts[category] = counts.get(category, 0) + 1
        selected = limited
    return selected


def load_sherpa(root: Path, model_type: str, threads: int, top_k: int = 527):
    import sherpa_onnx

    labels = []
    with open(root / "class_labels_indices.csv", encoding="utf-8") as fh:
        for row in csv.DictReader(fh):
            labels.append(row["display_name"])
    if model_type == "ced":
        model_config = sherpa_onnx.AudioTaggingModelConfig(
            ced=str(root / "model.onnx"), num_threads=threads, provider="cpu"
        )
    else:
        model_config = sherpa_onnx.AudioTaggingModelConfig(
            zipformer=sherpa_onnx.OfflineZipformerAudioTaggingModelConfig(
                model=str(root / "model.onnx")
            ),
            num_threads=threads,
            provider="cpu",
        )
    config = sherpa_onnx.AudioTaggingConfig(
        model=model_config,
        labels=str(root / "class_labels_indices.csv"),
        top_k=top_k,
    )
    return sherpa_onnx.AudioTagging(config), labels


def run_sherpa(sherpa_root: Path, model_type: str, esc_root: Path, rows: list[dict], threads: int) -> tuple[np.ndarray, list[str], float, float]:
    started_load = time.perf_counter()
    tagging, labels = load_sherpa(sherpa_root, model_type, threads)
    load_seconds = time.perf_counter() - started_load
    probs = np.zeros((len(rows), 527), dtype=np.float32)
    started = time.perf_counter()
    for i, row in enumerate(rows):
        audio, sr = read_wav(esc_root / "audio" / row["filename"])
        if sr != 16_000:
            import librosa

            audio = librosa.resample(audio, orig_sr=sr, target_sr=16_000).astype(np.float32)
        stream = tagging.create_stream()
        stream.accept_waveform(16_000, audio)
        events = tagging.compute(stream, top_k=527)
        for event in events:
            probs[i, event.index] = float(event.prob)
    inference_seconds = time.perf_counter() - started
    return probs, labels, load_seconds, inference_seconds


def run_panns(esc_root: Path, rows: list[dict], device: str) -> tuple[np.ndarray, list[str], float, float]:
    import librosa
    import torch
    from panns_inference import AudioTagging, labels

    # The constructor downloads Cnn14 into ~/panns_data when missing.  Warm it
    # once outside the timed load so the reported load is a local model load.
    AudioTagging(device=device)
    started_load = time.perf_counter()
    tagger = AudioTagging(device=device)
    load_seconds = time.perf_counter() - started_load
    if device == "cuda":
        torch.cuda.reset_peak_memory_stats()
    probs = np.zeros((len(rows), 527), dtype=np.float32)
    started = time.perf_counter()
    with torch.no_grad():
        for i, row in enumerate(rows):
            audio, sr = read_wav(esc_root / "audio" / row["filename"])
            audio32 = librosa.resample(audio, orig_sr=sr, target_sr=32_000)
            clipwise, _ = tagger.inference(audio32[None, :])
            probs[i] = clipwise[0]
    inference_seconds = time.perf_counter() - started
    return probs, list(labels), load_seconds, inference_seconds


def evaluate(rows: list[dict], probs: np.ndarray, labels: list[str]) -> dict:
    from sklearn.metrics import average_precision_score

    mapping: dict[str, int] = {}
    missing = []
    for esc_name, audioset_name in ESC_TO_AUDIOSET.items():
        idx = resolve_label(audioset_name, labels)
        if idx is None:
            missing.append({"esc50": esc_name, "audioset": audioset_name})
        else:
            mapping[esc_name] = idx
    categories = np.asarray([row["category"] for row in rows])
    mapped_classes = list(mapping)
    mapped_idx = np.asarray([mapping[c] for c in mapped_classes], dtype=int)
    subset = probs[:, mapped_idx]
    per_class: dict[str, dict] = {}
    for j, category in enumerate(mapped_classes):
        truth = (categories == category).astype(int)
        scores = subset[:, j]
        if truth.sum() == 0 or truth.sum() == truth.size:
            continue
        per_class[category] = {
            "audioset_label": labels[mapped_idx[j]],
            "audioset_index": int(mapped_idx[j]),
            "average_precision": float(average_precision_score(truth, scores)),
            "n": int(truth.size),
            "n_positive": int(truth.sum()),
        }
    aps = [v["average_precision"] for v in per_class.values()]
    top1_idx = np.argmax(subset, axis=1)
    top1 = float((np.asarray(mapped_classes)[top1_idx] == categories).mean())
    k = min(3, len(mapped_classes))
    top3_idx = np.argsort(subset, axis=1)[:, -k:][:, ::-1]
    top3_hits = np.any(np.asarray(mapped_classes)[top3_idx] == categories[:, None], axis=1)
    return {
        "mapping": {k: {"index": int(v), "label": labels[v]} for k, v in mapping.items()},
        "missing_labels": missing,
        "classes": len(per_class),
        "mAP": float(np.mean(aps)) if aps else None,
        "top1_accuracy": top1,
        "top3_accuracy": float(top3_hits.mean()),
        "n_clips": int(len(rows)),
        "per_class": per_class,
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--engine", choices=["sherpa", "panns"], required=True)
    parser.add_argument("--device", choices=["cpu", "cuda"], default="cpu")
    parser.add_argument("--esc50", type=Path, default=DATA / "esc50")
    parser.add_argument("--sherpa-model", choices=["ced", "zipformer"], default="ced")
    parser.add_argument("--sherpa-dir", type=Path, default=None)
    parser.add_argument("--threads", type=int, default=4)
    parser.add_argument("--limit-per-class", type=int, default=None)
    parser.add_argument("--out", type=Path, default=None)
    args = parser.parse_args()

    ensure_results()
    rows = load_esc50(args.esc50, args.limit_per_class)
    if args.engine == "sherpa":
        if args.device != "cpu":
            raise SystemExit("sherpa-onnx wheel in this PoC exposes CPUExecutionProvider only")
        sherpa_dir = args.sherpa_dir or MODELS / ("ced-base" if args.sherpa_model == "ced" else "zipformer-tagging")
        probs, labels, load_seconds, inference_seconds = run_sherpa(
            sherpa_dir, args.sherpa_model, args.esc50, rows, args.threads
        )
        model_path = sherpa_dir / "model.onnx"
        provider = "CPUExecutionProvider"
        gpu_peak_mb = None
    else:
        probs, labels, load_seconds, inference_seconds = run_panns(args.esc50, rows, args.device)
        model_path = Path.home() / "panns_data" / "Cnn14_mAP=0.431.pth"
        provider = "cuda" if args.device == "cuda" else "cpu"
        gpu_peak_mb = None
        if args.device == "cuda":
            import torch

            gpu_peak_mb = round(torch.cuda.max_memory_allocated() / 2**20, 2)
    metrics = evaluate(rows, probs, labels)
    clip_seconds = 5.0
    total_audio = len(rows) * clip_seconds
    out = args.out or RESULTS / f"audio_tagging_{args.engine}_{args.device}.json"
    payload = {
        "engine": (
            f"sherpa-onnx-{args.sherpa_model}" if args.engine == "sherpa" else "PANNs-CNN14"
        ),
        "model": file_info(model_path),
        "dataset": {
            "name": "ESC-50",
            "license": "CC BY-NC 3.0 (dataset); individual clips vary",
            "n_clips": len(rows),
            "clip_seconds": clip_seconds,
            "mapped_classes": len(ESC_TO_AUDIOSET),
        },
        "provider": provider,
        "load_seconds": round(load_seconds, 4),
        "inference_seconds": round(inference_seconds, 4),
        "audio_seconds": total_audio,
        "rtf": inference_seconds / total_audio,
        "peak_rss_mb": round(peak_rss_mb(), 2),
        "gpu_peak_mb": gpu_peak_mb,
        "metrics": metrics,
        "notes": [
            "Top-1/top-3 are closed-set over the 21 mapped ESC-50 classes, not over all 527 AudioSet labels.",
            "PANNs resamples 44.1 kHz ESC-50 clips to 32 kHz; CED consumes 16 kHz directly.",
            "ESC-50 is non-commercial; this evaluation does not grant redistribution rights.",
        ],
    }
    write_json(out, payload)
    print(
        json.dumps(
            {
                "engine": payload["engine"],
                "mAP": metrics["mAP"],
                "top1": metrics["top1_accuracy"],
                "top3": metrics["top3_accuracy"],
                "rtf": payload["rtf"],
                "load_s": payload["load_seconds"],
                "gpu_peak_mb": gpu_peak_mb,
            },
            ensure_ascii=False,
        )
    )
    print(f"wrote {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
