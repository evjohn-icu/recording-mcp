#!/usr/bin/env python3
"""Fetch the official gated pyannote Community-1 pipeline and score one session.

Without a Hugging Face token this writes a NOT_RUN status file instead of
silently reusing the public mirror under the official name.
"""

from __future__ import annotations

import argparse
import os
import subprocess
import sys
from pathlib import Path

from common import MODELS, RESULTS, ensure_results, now_iso, write_json

MODEL_ID = "pyannote/speaker-diarization-community-1"
STATUS = RESULTS / "pyannote_official_status.json"


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--repo", default=MODEL_ID)
    parser.add_argument("--model-dir", type=Path, default=MODELS / "pyannote-official")
    parser.add_argument("--session", default="L_R004S03C01")
    parser.add_argument("--device", choices=["cpu", "cuda"], default="cuda")
    args = parser.parse_args()

    ensure_results()
    token = os.environ.get("HF_TOKEN") or os.environ.get("HUGGING_FACE_HUB_TOKEN")
    if not token:
        write_json(
            STATUS,
            {
                "status": "NOT_RUN",
                "reason": "no HF token",
                "model": args.repo,
                "checked_at": now_iso(),
                "note": "The gated repository requires accepting the terms and an access token; "
                "the public mirror result stands as the pyannote measurement.",
            },
        )
        print(f"no HF token; wrote {STATUS}")
        return 0

    from huggingface_hub import snapshot_download

    path = snapshot_download(args.repo, local_dir=args.model_dir, token=token)
    # Session ids are <L|S>_<room...>; the L/S prefix is the AISHELL-4 subset label.
    label = args.session.split("_")[0]
    out = RESULTS / f"diarization_pyannote_official_{args.device}_{label}.json"
    argv = [
        sys.executable,
        str(Path(__file__).resolve().parent / "run_diarization.py"),
        "--engine",
        "pyannote",
        "--device",
        args.device,
        "--session",
        args.session,
        "--pipeline",
        path,
        "--out",
        str(out),
    ]
    proc = subprocess.run(argv, capture_output=True, text=True)
    write_json(
        STATUS,
        {
            "status": "PASS" if proc.returncode == 0 else "FAIL",
            "model": args.repo,
            "path": path,
            "session": args.session,
            "device": args.device,
            "result": str(out),
            "returncode": proc.returncode,
            "stdout": proc.stdout.strip(),
            "stderr": proc.stderr.strip(),
            "checked_at": now_iso(),
        },
    )
    print(proc.stdout.strip())
    print(f"status written to {STATUS}")
    return proc.returncode


if __name__ == "__main__":
    sys.exit(main())
