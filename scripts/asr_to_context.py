#!/usr/bin/env python3
"""Load a completed `asr_full.py` run into the Context Layer.

Each segment of the run is one upstream ASR request. This script feeds the
segment's final parsed response frames through `adapter-doubao` and then
`import-transcript`, using a per-segment source envelope (offset/duration from
the run manifest). It never talks to the ASR service and never reads audio.

Usage:
    make build
    python scripts/asr_to_context.py \
        --run-dir data/asr/<recording> \
        --db data/asr/<recording>/context-memory.db \
        --conversation-id conversation:<recording> \
        --recording-id recording:<recording>

Notes:
    - Segments with status != "complete" abort the run; fix the transcription
      first (see run.json) instead of partially importing.
    - Blank utterances are kept in the raw source but are not stored as
      searchable turns by the Context Layer; segment text is never deduped
      across segments.
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def load_frames(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]


def main() -> int:
    ap = argparse.ArgumentParser(description="Load an asr_full run into the Context Layer")
    ap.add_argument("--run-dir", required=True, type=Path)
    ap.add_argument("--db", required=True, type=Path)
    ap.add_argument("--conversation-id", required=True)
    ap.add_argument("--recording-id", required=True)
    ap.add_argument("--device-id", default="device:local")
    ap.add_argument("--transcript-revision", default="asr-v1")
    args = ap.parse_args()

    adapter = ROOT / "bin" / "adapter-doubao"
    importer = ROOT / "bin" / "import-transcript"
    if not adapter.exists() or not importer.exists():
        print("missing binaries; run `make build` first", file=sys.stderr)
        return 1

    manifest = json.loads((args.run_dir / "run.json").read_text(encoding="utf-8"))
    segments = manifest.get("segments") or []
    if not segments:
        print("run.json has no segments", file=sys.stderr)
        return 1

    total_turns = 0
    for seg in segments:
        idx = seg["index"]
        if seg.get("status") != "complete":
            print(f"segment {idx:02d}: status={seg.get('status')} — aborting", file=sys.stderr)
            return 1
        attempt = seg["attempts"][-1]
        frames_path = args.run_dir / "segments" / attempt["parsed"]
        frames = load_frames(frames_path)
        server_frames = [f for f in frames if f.get("message_type") == 9 and not f.get("code")]
        if not server_frames:
            print(f"segment {idx:02d}: no server response frames in {frames_path}", file=sys.stderr)
            return 1

        clean = args.run_dir / "segments" / f"segment-{idx:02d}-server-only.jsonl"
        with clean.open("w", encoding="utf-8") as fo:
            for f in server_frames:
                fo.write(json.dumps(f, ensure_ascii=False) + "\n")
        os.chmod(clean, 0o600)

        envelope = {
            "provider": "doubao",
            "provider_request_id": attempt["request_id"],
            "device_id": args.device_id,
            "recording_id": args.recording_id,
            "conversation_id": args.conversation_id,
            "segment_id": f"segment-{idx:02d}",
            "segment_offset_ms": int(seg["start_ms"]),
            "segment_duration_ms": int(seg["end_ms"]) - int(seg["start_ms"]),
            "transcript_revision": args.transcript_revision,
            "completion_status": "final",
            "model": "bigmodel",
        }
        if manifest.get("resource_id"):
            envelope["resource_id"] = manifest["resource_id"]
        env_path = args.run_dir / "segments" / f"segment-{idx:02d}-envelope.json"
        env_path.write_text(json.dumps(envelope, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        os.chmod(env_path, 0o600)

        normalized = args.run_dir / "segments" / f"segment-{idx:02d}-normalized.json"
        r = subprocess.run([str(adapter), "--input", str(clean), "--metadata", str(env_path),
                            "--output", str(normalized)], capture_output=True, text=True)
        if r.returncode:
            print(f"segment {idx:02d}: adapter failed: {r.stderr.strip()}", file=sys.stderr)
            return 1
        os.chmod(normalized, 0o600)

        r = subprocess.run([str(importer), "--db", str(args.db), "--input", str(normalized)],
                           capture_output=True, text=True)
        if r.returncode:
            print(f"segment {idx:02d}: import failed: {r.stderr.strip()}", file=sys.stderr)
            return 1
        report = json.loads(r.stdout)
        total_turns += report["turns"]
        print(f"segment {idx:02d}: turns={report['turns']} conversation={report['conversation_id']}")

    print(f"ASR_TO_CONTEXT_DONE segments={len(segments)} turns={total_turns} db={args.db}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
