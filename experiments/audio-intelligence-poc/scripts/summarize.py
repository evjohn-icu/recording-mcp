#!/usr/bin/env python3
"""Collect the primary PoC numbers into one auditable JSON file."""

from __future__ import annotations

import json
import sys
from pathlib import Path

from common import RESULTS, write_json


def load(name: str) -> dict:
    return json.load(open(RESULTS / name, encoding="utf-8"))


def main() -> int:
    vad = load("vad_silero.json")
    tagging = {
        "zipformer_cpu": load("audio_tagging_zipformer_cpu.json"),
        "ced_cpu": load("audio_tagging_sherpa_cpu.json"),
        "panns_cpu": load("audio_tagging_panns_cpu.json"),
        "panns_cuda": load("audio_tagging_panns_cuda.json"),
    }
    diarization = {
        "pyannote_cuda_full": load("diarization_pyannote_cuda_L.json"),
        "sherpa_cpu_oracle7": load("diarization_sherpa_cpu_L_c7.json"),
        "sherpa_cpu_default": load("diarization_sherpa_cpu_L.json"),
        "sherpa_cpu_threshold04": load("diarization_sherpa_cpu_L_t04.json"),
        "pyannote_cpu_300s": load("diarization_pyannote_cpu_S_first300s.json"),
    }
    voiceprint = {
        "campplus_cpu": load("voiceprint_campplus_cpu.json"),
        "wespeaker_cpu": load("voiceprint_wespeaker_cpu.json"),
        "campplus_docker_cpu": load("voiceprint_campplus_docker_cpu.json"),
    }
    docker = {
        "vad_cpu": load("vad_silero_docker.json"),
        "zipformer_cpu_42clips": load("audio_tagging_zipformer_docker_smoke.json"),
        "panns_cuda_42clips": load("audio_tagging_panns_docker_gpu_smoke.json"),
        "pyannote_cuda_60s": load("diarization_pyannote_docker_gpu_smoke.json"),
    }
    summary = {
        "vad": {
            "engine": vad["engine"],
            "model": vad["model"],
            "aggregate": vad["aggregate"],
            "load_seconds": vad["load_seconds"],
            "rtf": vad["rtf"],
            "peak_rss_mb": vad["peak_rss_mb"],
        },
        "tagging": {
            key: {
                "engine": value["engine"],
                "metrics": {
                    k: value["metrics"].get(k)
                    for k in ("mAP", "top1_accuracy", "top3_accuracy")
                },
                "rtf": value["rtf"],
                "load_seconds": value["load_seconds"],
                "peak_rss_mb": value["peak_rss_mb"],
                "gpu_peak_mb": value["gpu_peak_mb"],
            }
            for key, value in tagging.items()
        },
        "diarization": {
            key: {
                "engine": value["engine"],
                "scope": value["scope"],
                "metrics": value["metrics"],
                "rtf": value["rtf"],
                "load_seconds": value["load_seconds"],
                "peak_rss_mb": value["peak_rss_mb"],
                "gpu_peak_mb": value["gpu_peak_mb"],
            }
            for key, value in diarization.items()
        },
        "voiceprint": {
            key: {
                "engine": value["engine"],
                "metrics": value["metrics"],
                "rtf": value["rtf"],
                "load_seconds": value["load_seconds"],
                "peak_rss_mb": value["peak_rss_mb"],
            }
            for key, value in voiceprint.items()
        },
        "docker": {
            key: {
                "engine": value["engine"],
                "scope": value.get("scope"),
                "metrics": {
                    k: value.get("metrics", {}).get(k)
                    for k in ("f1", "mAP", "top1_accuracy", "eer", "der")
                },
                "rtf": value["rtf"],
                "load_seconds": value["load_seconds"],
                "peak_rss_mb": value["peak_rss_mb"],
                "gpu_peak_mb": value.get("gpu_peak_mb"),
            }
            for key, value in docker.items()
        },
        "not_run": [
            "official gated pyannote Community-1 repository (no HF token; public mirror used)",
            "GPU execution for Silero VAD, sherpa-onnx diarization, CAM++, WeSpeaker",
            "pyannote CPU full 2227.8 s (only 300 s subset)",
            "self-contained recording-mcp-audio-poc:gpu image (GPU runs used CPU image + host venv)",
            "macOS and RK hardware",
            "AISHELL-4 remaining test sessions",
            "commercial ambient corpus (ESC-50 only)",
            "speaker-count estimator",
            "recording-mcp end-to-end integration and ASR/Evidence/Worldline/MCP regressions",
        ],
    }
    write_json(RESULTS / "summary.json", summary)
    print(f"wrote {RESULTS / 'summary.json'}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
