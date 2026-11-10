#!/usr/bin/env python3
"""Collect the primary PoC numbers into one auditable JSON file."""

from __future__ import annotations

import json
import sys
from pathlib import Path

from common import RESULTS, write_json


def load(name: str) -> dict:
    return json.load(open(RESULTS / name, encoding="utf-8"))


def optional(name: str) -> dict | None:
    path = RESULTS / name
    return load(name) if path.exists() else None


def has_cuda_provider(payload: dict | None) -> bool:
    return bool(payload) and "CUDAExecutionProvider" in (payload.get("provider") or [])


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

    vad_full_cpu = optional("vad_silero_all_cpu.json")
    vad_gpu = optional("vad_silero_gpu.json")
    diar_pyannote_all = optional("diarization_pyannote_cuda_all.json")
    diar_sherpa_all = optional("diarization_sherpa_cpu_all.json")
    diar_pyannote_cpu_full = optional("diarization_pyannote_cpu_L.json")
    speaker_count = optional("speaker_count_estimator.json")
    official_pyannote = optional("pyannote_official_status.json")
    docker_validation = optional("docker_gpu_validation.json")
    manifest_all = optional("manifest_all.json")

    voiceprint_full = {
        key: value
        for key, value in {
            "campplus_cpu": optional("voiceprint_campplus_all_cpu.json"),
            "wespeaker_cpu": optional("voiceprint_wespeaker_all_cpu.json"),
            "campplus_cuda": optional("voiceprint_campplus_cuda_all.json"),
            "wespeaker_cuda": optional("voiceprint_wespeaker_cuda_all.json"),
            "campplus_onnx_cpu": optional("voiceprint_campplus_onnx_cpu_all.json"),
            "wespeaker_onnx_cpu": optional("voiceprint_wespeaker_onnx_cpu_all.json"),
        }.items()
        if value
    }

    summary = {
        "vad": {
            "engine": vad["engine"],
            "model": vad["model"],
            "aggregate": vad["aggregate"],
            "load_seconds": vad["load_seconds"],
            "rtf": vad["rtf"],
            "peak_rss_mb": vad["peak_rss_mb"],
            "full_cpu": None
            if not vad_full_cpu
            else {
                "engine": vad_full_cpu["engine"],
                "provider": vad_full_cpu["provider"],
                "n_sessions": len(vad_full_cpu["sessions"]),
                "aggregate": vad_full_cpu["aggregate"],
                "audio_seconds": vad_full_cpu["audio_seconds"],
                "rtf": vad_full_cpu["rtf"],
            },
            "gpu": None
            if not vad_gpu
            else {
                "provider": vad_gpu["provider"],
                "n_sessions": len(vad_gpu["sessions"]),
                "aggregate": vad_gpu["aggregate"],
                "rtf": vad_gpu["rtf"],
                "gpu_peak_mb": vad_gpu.get("gpu_peak_mb"),
            },
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
            **{
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
            "pyannote_cuda_all": None
            if not diar_pyannote_all
            else {
                "engine": diar_pyannote_all["engine"],
                "n_sessions": diar_pyannote_all["n_sessions"],
                "audio_seconds": diar_pyannote_all["audio_seconds"],
                "der": diar_pyannote_all["aggregate"]["der"],
                "components": diar_pyannote_all["aggregate"]["components"],
                "rtf": diar_pyannote_all["rtf"],
            },
            "sherpa_cpu_all": None
            if not diar_sherpa_all
            else {
                "engine": diar_sherpa_all["engine"],
                "n_sessions": diar_sherpa_all["n_sessions"],
                "num_clusters_mode": diar_sherpa_all["num_clusters_mode"],
                "audio_seconds": diar_sherpa_all["audio_seconds"],
                "der": diar_sherpa_all["aggregate"]["der"],
                "components": diar_sherpa_all["aggregate"]["components"],
                "rtf": diar_sherpa_all["rtf"],
            },
            "pyannote_cpu_full": None
            if not diar_pyannote_cpu_full
            else {
                "scope": diar_pyannote_cpu_full["scope"],
                "der": diar_pyannote_cpu_full["metrics"]["der"],
                "rtf": diar_pyannote_cpu_full["rtf"],
                "peak_rss_mb": diar_pyannote_cpu_full["peak_rss_mb"],
            },
        },
        "voiceprint": {
            **{
                key: {
                    "engine": value["engine"],
                    "metrics": value["metrics"],
                    "rtf": value["rtf"],
                    "load_seconds": value["load_seconds"],
                    "peak_rss_mb": value["peak_rss_mb"],
                }
                for key, value in voiceprint.items()
            },
            **{
                f"{key}_all": {
                    "engine": value["engine"],
                    "provider": value.get("provider"),
                    "metrics": value["metrics"],
                    "rtf": value["rtf"],
                    "load_seconds": value["load_seconds"],
                    "peak_rss_mb": value["peak_rss_mb"],
                    "gpu_peak_mb": value.get("gpu_peak_mb"),
                }
                for key, value in voiceprint_full.items()
            },
        },
        "speaker_count": None
        if not speaker_count
        else {
            "engine": speaker_count["engine"],
            "sessions": {
                key: {
                    "estimated_speakers": value.get("estimated_speakers"),
                    "reference_speakers": value.get("reference_speakers"),
                    "silhouette": value.get("silhouette"),
                    "threshold": value.get("threshold"),
                    "reliable": value.get("reliable"),
                    "window_count": value.get("window_count"),
                }
                for key, value in speaker_count["sessions"].items()
            },
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
        "docker_gpu": None
        if not docker_validation
        else {
            "self_contained_gpu_image": docker_validation.get("self_contained_gpu_image"),
            "cuda_matmul": docker_validation.get("cuda_matmul"),
        },
        "official_pyannote": None
        if not official_pyannote
        else {
            key: official_pyannote.get(key)
            for key in ("status", "model", "revision", "session", "device", "result", "reason")
        },
        "not_run": [],
    }

    not_run: list[str] = summary["not_run"]
    if not (official_pyannote and official_pyannote.get("status") == "PASS"):
        not_run.append(
            "official gated pyannote Community-1 repository (public mirror used; see pyannote_official_status.json)"
        )
    if not has_cuda_provider(vad_gpu):
        not_run.append("GPU execution for Silero VAD")
    if not (
        has_cuda_provider(voiceprint_full.get("campplus_cuda"))
        and has_cuda_provider(voiceprint_full.get("wespeaker_cuda"))
    ):
        not_run.append("GPU execution for CAM++ / WeSpeaker ONNX")
    not_run.append(
        "GPU execution for sherpa-onnx diarization (CPU is the edge deployment target)"
    )
    if not diar_pyannote_cpu_full:
        not_run.append("pyannote CPU full session (only a 300 s subset)")
    image = (docker_validation or {}).get("self_contained_gpu_image") or {}
    if image.get("status") != "PASS":
        not_run.append("self-contained recording-mcp-audio-poc:gpu image")
    if not (manifest_all and manifest_all.get("n_sessions", 0) >= 20):
        not_run.append("AISHELL-4 remaining test sessions")
    if not speaker_count:
        not_run.append("speaker-count estimator")
    not_run.extend(
        [
            "macOS and RK hardware",
            "commercial ambient corpus (ESC-50 only)",
            "recording-mcp end-to-end integration and ASR/Evidence/Worldline/MCP regressions",
        ]
    )

    write_json(RESULTS / "summary.json", summary)
    print(f"wrote {RESULTS / 'summary.json'}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
