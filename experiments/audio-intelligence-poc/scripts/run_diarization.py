#!/usr/bin/env python3
"""Speaker diarization: sherpa-onnx (pyannote segmentation + CAM++) vs pyannote Community-1."""

from __future__ import annotations

import argparse
import json
import os
import sys
import tempfile
import time
from pathlib import Path

import numpy as np

from common import (
    DATA,
    MODELS,
    RESULTS,
    SAMPLE_RATE,
    der_metrics,
    ensure_results,
    file_info,
    peak_rss_mb,
    read_rttm,
    read_wav,
    write_json,
)


def clip_segments(segments: list[dict], max_seconds: float | None) -> list[dict]:
    if max_seconds is None:
        return segments
    clipped = []
    for seg in segments:
        if seg["start"] >= max_seconds:
            continue
        clipped.append({**seg, "end": min(seg["end"], max_seconds)})
    return clipped


def write_rttm(path: Path, file_id: str, segments: list[dict]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "w", encoding="utf-8") as fh:
        for seg in segments:
            fh.write(
                f"SPEAKER {file_id} 1 {seg['start']:.4f} {seg['end'] - seg['start']:.4f} "
                f"<NA> <NA> {seg['speaker']} <NA> <NA>\n"
            )


def run_sherpa(args, audio: np.ndarray) -> tuple[list[dict], float, float]:
    import sherpa_onnx

    started_load = time.perf_counter()
    # sherpa-onnx logs a CUDA fallback from C++ to fd 2; capture it so a CPU
    # build cannot be reported under a CUDA label.
    config = sherpa_onnx.OfflineSpeakerDiarizationConfig(
        segmentation=sherpa_onnx.OfflineSpeakerSegmentationModelConfig(
            pyannote=sherpa_onnx.OfflineSpeakerSegmentationPyannoteModelConfig(
                model=str(args.segmentation)
            ),
            num_threads=args.threads,
            provider=args.provider,
        ),
        embedding=sherpa_onnx.SpeakerEmbeddingExtractorConfig(
            model=str(args.embedding),
            num_threads=args.threads,
            provider=args.provider,
        ),
        clustering=sherpa_onnx.FastClusteringConfig(
            num_clusters=args.num_clusters, threshold=args.threshold
        ),
        min_duration_on=args.min_duration_on,
        min_duration_off=args.min_duration_off,
    )
    saved_fd = os.dup(2)
    log = tempfile.TemporaryFile(mode="w+b")
    try:
        os.dup2(log.fileno(), 2)
        # Creating the diarizer is what requests the provider, so its warning
        # is the only evidence of a fallback.
        diarizer = sherpa_onnx.OfflineSpeakerDiarization(config)
    finally:
        os.dup2(saved_fd, 2)
        os.close(saved_fd)
    log.seek(0)
    captured = log.read().decode("utf-8", "replace")
    log.close()
    if args.provider == "cuda" and "Fallback to cpu" in captured:
        raise SystemExit(
            "sherpa-onnx refused CUDAExecutionProvider: "
            + captured.strip().splitlines()[0]
        )
    load_seconds = time.perf_counter() - started_load
    started = time.perf_counter()
    result = diarizer.process(audio)
    inference_seconds = time.perf_counter() - started
    segments = [
        {"start": float(seg.start), "end": float(seg.end), "speaker": f"S{seg.speaker}"}
        for seg in result.sort_by_start_time()
    ]
    return segments, load_seconds, inference_seconds


def run_pyannote(args, audio: np.ndarray):
    import torch
    from pyannote.audio import Pipeline

    # pyannote.audio 4.0.0 predates torch's weights_only default flip.  The
    # local checkpoint is a trusted public mirror (CC-BY-4.0), so opt out for
    # this process only instead of allowlisting every Lightning global.
    _torch_load = torch.load

    def _load(*args, **kwargs):
        kwargs["weights_only"] = False
        return _torch_load(*args, **kwargs)

    torch.load = _load

    started_load = time.perf_counter()
    pipeline = Pipeline.from_pretrained(str(args.pipeline))
    if args.device == "cuda" and torch.cuda.is_available():
        pipeline.to(torch.device("cuda"))
    # Reduce the batch size for an 8 GiB card; this is a deployment parameter,
    # not an accuracy parameter.
    if hasattr(pipeline, "segmentation_batch_size"):
        pipeline.segmentation_batch_size = args.batch_size
    if hasattr(pipeline, "embedding_batch_size"):
        pipeline.embedding_batch_size = args.batch_size
    load_seconds = time.perf_counter() - started_load
    if args.device == "cuda":
        torch.cuda.reset_peak_memory_stats()
    waveform = torch.from_numpy(audio).unsqueeze(0)
    started = time.perf_counter()
    with torch.no_grad():
        output = pipeline({"waveform": waveform, "sample_rate": SAMPLE_RATE})
    inference_seconds = time.perf_counter() - started
    annotation = getattr(output, "speaker_diarization", output)
    segments = [
        {"start": float(segment.start), "end": float(segment.end), "speaker": str(label)}
        for segment, _, label in annotation.itertracks(yield_label=True)
    ]
    gpu_peak_mb = None
    if args.device == "cuda":
        gpu_peak_mb = round(torch.cuda.max_memory_allocated() / 2**20, 2)
    return segments, load_seconds, inference_seconds, gpu_peak_mb


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--engine", choices=["sherpa", "pyannote"], required=True)
    parser.add_argument("--session", default="L_R004S03C01")
    parser.add_argument("--data", type=Path, default=DATA / "aishell4")
    parser.add_argument("--device", choices=["cpu", "cuda"], default="cpu")
    parser.add_argument("--provider", choices=["cpu", "cuda"], default="cpu")
    parser.add_argument("--threads", type=int, default=4)
    parser.add_argument("--threshold", type=float, default=0.5)
    parser.add_argument("--num-clusters", type=int, default=-1)
    parser.add_argument("--min-duration-on", type=float, default=0.3)
    parser.add_argument("--min-duration-off", type=float, default=0.5)
    parser.add_argument("--max-seconds", type=float, default=None)
    parser.add_argument("--batch-size", type=int, default=8)
    parser.add_argument("--segmentation", type=Path, default=MODELS / "sherpa-seg" / "model.onnx")
    parser.add_argument("--embedding", type=Path, default=MODELS / "campplus" / "campplus_zh_en_fp32.onnx")
    parser.add_argument("--pipeline", type=Path, default=MODELS / "pyannote-community-1")
    parser.add_argument("--out", type=Path, default=None)
    args = parser.parse_args()

    ensure_results()
    wav = args.data / "test" / "wav" / f"{args.session}.wav"
    rttm = args.data / "test" / "TextGrid" / f"{args.session}.rttm"
    audio, sr = read_wav(wav, max_seconds=args.max_seconds)
    if sr != SAMPLE_RATE:
        raise ValueError(f"expected {SAMPLE_RATE} Hz, got {sr}")
    reference = clip_segments(read_rttm(rttm), args.max_seconds)

    if args.engine == "sherpa":
        hypothesis, load_seconds, inference_seconds = run_sherpa(args, audio)
        gpu_peak_mb = None
        model_info = {
            "segmentation": file_info(args.segmentation),
            "embedding": file_info(args.embedding),
        }
        provider = args.provider
    else:
        hypothesis, load_seconds, inference_seconds, gpu_peak_mb = run_pyannote(args, audio)
        model_info = {"pipeline": str(args.pipeline.resolve())}
        provider = args.device

    metrics = der_metrics(reference, hypothesis, collar=0.25, skip_overlap=False)
    suffix = f"_{args.device}" if args.engine == "pyannote" else ""
    stem = f"diarization_{args.engine}{suffix}_{args.session}"
    if args.max_seconds:
        stem += f"_first{int(args.max_seconds)}s"
    out = args.out or RESULTS / f"{stem}.json"
    hyp_rttm = RESULTS / f"{stem}.rttm"
    write_rttm(hyp_rttm, args.session, hypothesis)
    payload = {
        "engine": "sherpa-onnx" if args.engine == "sherpa" else "pyannote-community-1",
        "session": args.session,
        "audio_seconds": round(len(audio) / sr, 3),
        "scope": "full" if args.max_seconds is None else f"first {args.max_seconds:.0f}s",
        "provider": provider,
        "threads": args.threads if args.engine == "sherpa" else None,
        "threshold": args.threshold if args.engine == "sherpa" else None,
        "num_clusters": args.num_clusters if args.engine == "sherpa" else None,
        "batch_size": args.batch_size if args.engine == "pyannote" else None,
        "models": model_info,
        "load_seconds": round(load_seconds, 4),
        "inference_seconds": round(inference_seconds, 4),
        "rtf": inference_seconds / (len(audio) / sr),
        "peak_rss_mb": round(peak_rss_mb(), 2),
        "gpu_peak_mb": gpu_peak_mb,
        "metrics": metrics,
        "n_hypothesis_segments": len(hypothesis),
        "notes": [
            "DER uses pyannote.metrics 4.2.0 with a 0.25 s collar; overlap is scored (skip_overlap=False).",
            "sherpa-onnx clustering is threshold-based with an unknown speaker count; pyannote Community-1 is end-to-end.",
        ],
    }
    write_json(out, payload)
    print(
        json.dumps(
            {
                "engine": payload["engine"],
                "scope": payload["scope"],
                "DER": metrics["der"],
                "speakers_ref": metrics["ref_speakers"],
                "speakers_hyp": metrics["hyp_speakers"],
                "rtf": payload["rtf"],
                "load_s": payload["load_seconds"],
                "gpu_peak_mb": gpu_peak_mb,
            },
            ensure_ascii=False,
        )
    )
    print(f"wrote {out} and {hyp_rttm}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
