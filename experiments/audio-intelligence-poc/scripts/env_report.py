#!/usr/bin/env python3
"""Capture the exact environment, model inventory, and Docker/WSL2 GPU probes."""

from __future__ import annotations

import json
import platform
import subprocess
import sys
from importlib import metadata
from pathlib import Path

from common import MODELS, RESULTS, ROOT, file_info, gpu_query, now_iso, run_command, write_json


def package_versions() -> dict[str, str]:
    names = [
        "numpy",
        "scipy",
        "scikit-learn",
        "soundfile",
        "librosa",
        "onnxruntime",
        "sherpa-onnx",
        "psutil",
        "pandas",
        "pyannote-metrics",
        "kaldi-native-fbank",
        "torch",
        "torchaudio",
        "pyannote.audio",
        "panns-inference",
    ]
    versions = {}
    for name in names:
        try:
            versions[name] = metadata.version(name)
        except metadata.PackageNotFoundError:
            versions[name] = "NOT_INSTALLED"
    return versions


def model_inventory() -> dict:
    items = {}
    for path in sorted(MODELS.rglob("*")):
        if path.is_file() and path.name != ".gitattributes":
            try:
                items[str(path.relative_to(MODELS))] = file_info(path)
            except OSError:
                continue
    panns = Path.home() / "panns_data" / "Cnn14_mAP=0.431.pth"
    if panns.exists():
        items["panns_data/Cnn14_mAP=0.431.pth"] = {
            "path": str(panns),
            "bytes": panns.stat().st_size,
        }
    return items


def dataset_inventory() -> dict:
    base = ROOT / "data" / "aishell4" / "test"
    sessions = {}
    for wav in sorted((base / "wav").glob("*.wav")):
        rttm = base / "TextGrid" / f"{wav.stem}.rttm"
        sessions[wav.stem] = {
            "wav_bytes": wav.stat().st_size,
            "rttm_bytes": rttm.stat().st_size if rttm.exists() else None,
        }
    esc_csv = ROOT / "data" / "esc50" / "meta" / "esc50.csv"
    esc_count = None
    if esc_csv.exists():
        esc_count = max(0, sum(1 for _ in open(esc_csv, encoding="utf-8")) - 1)
    return {
        "aishell4_test": sessions,
        "esc50": {
            "clips": esc_count,
            "audio_dir": str((ROOT / "data" / "esc50" / "audio").relative_to(ROOT)),
        },
    }


def docker_probes() -> dict:
    base = "debian:bookworm-slim"
    probes = {}
    probes["host_nvidia_smi"] = run_command(["nvidia-smi"], timeout=30)
    probes["compose_gpu_all"] = run_command(
        [
            "docker",
            "run",
            "--rm",
            "--gpus",
            "all",
            "--network=host",
            base,
            "sh",
            "-c",
            "ls -l /dev/nvidia* 2>&1; env | grep NVIDIA",
        ],
        timeout=120,
    )
    probes["gpu_all_wsl_libs"] = run_command(
        [
            "docker",
            "run",
            "--rm",
            "--gpus",
            "all",
            "-v",
            "/usr/lib/wsl/lib:/usr/lib/wsl/lib:ro",
            "-e",
            "LD_LIBRARY_PATH=/usr/lib/wsl/lib",
            "-v",
            "/usr/lib/wsl/lib/nvidia-smi:/usr/bin/nvidia-smi:ro",
            "--network=host",
            base,
            "nvidia-smi",
        ],
        timeout=120,
    )
    probes["privileged_dxg"] = run_command(
        [
            "docker",
            "run",
            "--rm",
            "--privileged",
            "--device",
            "/dev/dxg",
            "--device",
            "/dev/dri",
            "-v",
            "/usr/lib/wsl/lib:/usr/lib/wsl/lib:ro",
            "-e",
            "LD_LIBRARY_PATH=/usr/lib/wsl/lib",
            "-v",
            "/usr/lib/wsl/lib/nvidia-smi:/usr/bin/nvidia-smi:ro",
            "--network=host",
            base,
            "nvidia-smi",
        ],
        timeout=120,
    )
    probes["cdi_nvidia"] = run_command(
        ["docker", "run", "--rm", "--device", "nvidia.com/gpu=all", base, "true"], timeout=120
    )
    return probes


def main() -> int:
    os_release = {}
    if Path("/etc/os-release").exists():
        for line in open("/etc/os-release", encoding="utf-8"):
            if "=" in line:
                key, value = line.rstrip().split("=", 1)
                os_release[key] = value.strip('"')
    payload = {
        "generated_at": now_iso(),
        "git": {
            "branch": run_command(["git", "branch", "--show-current"]),
            "head": run_command(["git", "rev-parse", "HEAD"]),
            "status": run_command(["git", "status", "--short"]),
        },
        "host": {
            "uname": platform.uname().__dict__,
            "os_release": os_release,
            "python": platform.python_version(),
            "cpu": run_command(["lscpu"]),
            "memory": run_command(["free", "-b"]),
        },
        "gpu": gpu_query(),
        "gpu_full": run_command(["nvidia-smi", "-q"], timeout=60),
        "docker": {
            "version": run_command(["docker", "version"]),
            "info": run_command(["docker", "info"]),
            "context": run_command(["docker", "context", "ls"]),
        },
        "wsl": run_command(["wsl.exe", "--version"], timeout=30),
        "python_packages": package_versions(),
        "models": model_inventory(),
        "datasets": dataset_inventory(),
    }
    write_json(RESULTS / "environment.json", payload)
    probes = docker_probes()
    write_json(
        RESULTS / "docker_gpu_probe.json",
        {
            "generated_at": now_iso(),
            "docker_desktop_settings": {
                "EngineTypeWindows": "wsl2",
                "ProxyHTTPMode": "system (dead manual proxy removed during validation)",
                "GPU CDI devices": "docker.com/gpu=webgpu only; CUDA works via --gpus all + /usr/lib/wsl/lib bind mount",
            },
            "probes": probes,
        },
    )
    print(f"wrote {RESULTS / 'environment.json'} and {RESULTS / 'docker_gpu_probe.json'}")
    for name, probe in probes.items():
        print(name, "rc=", probe["returncode"], "stdout=", probe["stdout"][:120].replace("\n", " | "))
    return 0


if __name__ == "__main__":
    sys.exit(main())
