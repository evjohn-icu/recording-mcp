#!/usr/bin/env python3
"""audio-analysis-v1 adapter CLI.

One analysis run per process, driven by the Go ``internal/audioanalysis``
runner:

    python3 scripts/audio-analysis.py --request REQ.json --pcm PCM --config CONFIG.json --out OUT.json

The managed PCM file contains exactly the analysed window (16 kHz mono S16LE
covering recording-clock ``start_ms..end_ms``); event times are emitted on the
window clock ``0..(end_ms - start_ms)`` per ``docs/engineering/audio-analysis-v1.md``.

Output rules:

- the Result JSON is written to ``--out``; exit status is 0 whenever a valid
  Result document (including a valid failed Result) was written and non-zero
  only for invalid CLI usage or when no Result could be written;
- speaker vectors travel through the side channel ``OUT.json.embeddings.json``
  keyed by event id and never inside the Result;
- stdout stays empty and never carries tracebacks or model paths; stderr may
  carry a short safe error code only;
- the config pins local model files by 64-hex SHA-256; imports run offline
  (``HF_HUB_OFFLINE=1``) and nothing is ever downloaded.

Exit codes: 0 = Result written; 2 = invalid CLI usage (request document
unreadable, wrong argument usage); 1 = the Result could not be written.
"""

from __future__ import annotations

import argparse
import json
import os
import sys

# Force offline imports before anything heavy can load.
os.environ["HF_HUB_OFFLINE"] = "1"
os.environ.setdefault("TRANSFORMERS_OFFLINE", "1")

import audio_analysis_models as models  # noqa: E402  (needs the env vars first)

_UNEXPECTED_ACTION = "the audio analysis adapter failed unexpectedly; retry the run"
_RESULT_ACTION = "the adapter produced events outside the analysed window"


def _parse_args(argv=None):
    parser = argparse.ArgumentParser(
        prog="audio-analysis.py",
        description="Run one audio-analysis-v1 adapter request against local pinned models.",
    )
    parser.add_argument("--request", required=True, help="audio-analysis-v1 request JSON")
    parser.add_argument("--pcm", required=True, help="16 kHz mono S16LE PCM window")
    parser.add_argument("--config", required=True, help="audio-analysis-config-v1 JSON")
    parser.add_argument("--out", required=True, help="Result JSON output path")
    return parser.parse_args(argv)


def _usage_error(code):
    print("audio-analysis: " + code, file=sys.stderr)
    return 2


def _write_error(code):
    print("audio-analysis: " + code, file=sys.stderr)
    return 1


def _load_config(path):
    try:
        with open(path, "r", encoding="utf-8") as handle:
            config = json.load(handle)
    except (OSError, ValueError) as exc:
        raise models.AdapterError(
            "audio_model_unavailable",
            False,
            "the audio analysis configuration could not be read",
        ) from exc
    models.validate_config(config)
    return config


def _window_duration_ms(request):
    start_ms = request.get("start_ms")
    end_ms = request.get("end_ms")
    if not models._finite_int(start_ms) or not models._finite_int(end_ms):
        return -1
    return end_ms - start_ms


def _analyze(args, request):
    """Run the configured capability; returns ``(result, embeddings_or_None)``."""
    models.validate_request(request)
    config = _load_config(args.config)
    capability = request.get("capability")
    # Relative model pins resolve against the config file's own directory.
    base_dir = os.path.dirname(os.path.abspath(args.config))
    resolved = models.resolve_capability(config, capability, base_dir=base_dir)
    # A stale run must never execute against a different model: verify the
    # request's pinned identity against the actually resolved files before any
    # PCM is read or inference starts.
    models.verify_request_identity(request, resolved)
    samples = models.read_pcm(
        args.pcm,
        request.get("start_ms") if models._finite_int(request.get("start_ms")) else 0,
        request.get("end_ms") if models._finite_int(request.get("end_ms")) else 0,
    )
    runner = {
        "vad": models.run_vad,
        "tagging": models.run_tagging,
        "diarization": models.run_diarization,
        "speaker": models.run_speaker,
    }.get(capability)
    if runner is None:  # validate_request already rejected unknown capabilities
        raise models.AdapterError("audio_input_invalid", False, "unknown capability")
    outcome = runner(request, samples, resolved)
    provenance = {
        "adapter_version": models.ADAPTER_VERSION,
        "frontend_id": resolved["frontend_id"],
        "device": outcome.device,
        "input_sha256": request.get("input_sha256") or "",
        "model_files_sha256": dict(request.get("model_files_sha256") or {}),
        "parent_run_ids": [],
        "source_snapshot_sha256": request.get("source_snapshot_sha256") or "",
    }
    result = models.build_result(request, "succeeded", outcome.events, provenance=provenance)
    return result, outcome.embeddings


def main(argv=None):
    args = _parse_args(argv)
    try:
        with open(args.request, "r", encoding="utf-8") as handle:
            request = json.load(handle)
    except (OSError, ValueError):
        # Without the request document no identity fields exist, so no valid
        # Result can be produced: this is invalid CLI usage.
        return _usage_error("audio_input_invalid")
    if not isinstance(request, dict):
        return _usage_error("audio_input_invalid")

    result = None
    embeddings = None
    try:
        result, embeddings = _analyze(args, request)
    except models.AdapterError as exc:
        result = models.build_result(request, "failed", [], exc)
    except Exception:
        result = models.build_result(
            request,
            "failed",
            [],
            models.AdapterError("audio_model_unavailable", True, _UNEXPECTED_ACTION),
        )

    try:
        duration_ms = _window_duration_ms(request)
        if result["status"] == "succeeded":
            try:
                models.validate_result(result, request, duration_ms)
            except models.AdapterError:
                result = models.build_result(
                    request,
                    "failed",
                    [],
                    models.AdapterError("audio_result_invalid", False, _RESULT_ACTION),
                )
                embeddings = None
        if embeddings:
            # Side channel first: a crash between the two writes leaves no
            # Result document instead of speaker events without vectors.
            models.write_json_atomic(args.out + ".embeddings.json", embeddings)
        models.write_json_atomic(args.out, result)
    except (OSError, TypeError, ValueError):
        return _write_error("audio_result_invalid")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
