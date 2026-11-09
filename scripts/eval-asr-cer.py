#!/usr/bin/env python3
"""Transcribe a 16 kHz PCM file with a local model and score CER against a reference.

The reference may be the repository's transcript.json (turns[].text) or plain
text. Only aggregate metrics are printed; transcript text is never included in
the JSON output, so results can be committed without leaking recording content.

    scripts/eval-asr-cer.py --pcm recording.pcm --reference transcript.json \
        --backend qwen --out artifacts/asr-cer.json
"""
from __future__ import annotations

import argparse
import json
import pathlib
import re
import subprocess
import sys

DIGITS = str.maketrans({"零": "0", "〇": "0", "一": "1", "二": "2", "三": "3", "四": "4",
                        "五": "5", "六": "6", "七": "7", "八": "8", "九": "9",
                        "幺": "1", "两": "2"})
KEEP = re.compile(r"[^\u4e00-\u9fffA-Za-z0-9]")


def normalize(text: str) -> str:
    return KEEP.sub("", text.translate(DIGITS))


def load_reference(path: pathlib.Path) -> str:
    if path.suffix.lower() == ".json":
        data = json.loads(path.read_text(encoding="utf-8"))
        if isinstance(data, dict) and "turns" in data:
            turns = sorted(data["turns"], key=lambda t: t.get("global_start_ms") or 0)
            return "".join(t.get("text", "") for t in turns)
        raise SystemExit("reference JSON has no turns[]")
    return path.read_text(encoding="utf-8")


def levenshtein(a: str, b: str) -> int:
    if len(a) < len(b):
        a, b = b, a
    previous = list(range(len(b) + 1))
    for i, ca in enumerate(a, 1):
        current = [i]
        for j, cb in enumerate(b, 1):
            current.append(min(previous[j] + 1, current[j - 1] + 1, previous[j - 1] + (ca != cb)))
        previous = current
    return previous[-1]


def run_runner(args) -> list[dict]:
    cmd = [
        args.python, args.script, "--backend", args.backend, "--model", args.model,
        "--pcm", args.pcm, "--dtype", "float16", "--device", args.device,
        "--window-ms", str(args.window_ms), "--overlap-ms", str(args.overlap_ms),
        "--max-new-tokens", str(args.max_new_tokens),
    ]
    if args.revision:
        cmd += ["--revision", args.revision]
    if args.language:
        cmd += ["--language", args.language]
    proc = subprocess.run(cmd, capture_output=True, text=True, check=False)
    if proc.returncode != 0:
        raise SystemExit(f"local-asr runner failed ({proc.returncode}): {proc.stderr[-800:]}")
    payload = json.loads(proc.stdout)
    return payload["windows"]


def main() -> int:
    p = argparse.ArgumentParser(description="local ASR CER evaluation")
    p.add_argument("--pcm", required=True)
    p.add_argument("--reference", required=True, help="transcript.json or plain text")
    p.add_argument("--backend", choices=["ark", "qwen"], required=True)
    p.add_argument("--model", default="")
    p.add_argument("--revision", default="")
    p.add_argument("--python", default=str(pathlib.Path(".venv-asr/bin/python")))
    p.add_argument("--script", default=str(pathlib.Path("scripts/local-asr.py")))
    p.add_argument("--device", default="cuda:0")
    p.add_argument("--language", default="")
    p.add_argument("--window-ms", type=int, default=30000)
    p.add_argument("--overlap-ms", type=int, default=0)
    p.add_argument("--max-new-tokens", type=int, default=512)
    p.add_argument("--out", default="")
    args = p.parse_args()

    if not args.model:
        args.model = "Edge0/ARK-ASR-0.6B" if args.backend == "ark" else "Qwen/Qwen3-ASR-1.7B"
    windows = run_runner(args)
    hypothesis = "".join(w.get("text", "") for w in windows)
    reference = load_reference(pathlib.Path(args.reference))
    ref_norm, hyp_norm = normalize(reference), normalize(hypothesis)
    distance = levenshtein(hyp_norm, ref_norm)
    result = {
        "backend": args.backend,
        "model": args.model,
        "revision": args.revision,
        "duration_ms": windows[-1]["end_ms"] if windows else 0,
        "windows": len(windows),
        "reference_chars": len(ref_norm),
        "hypothesis_chars": len(hyp_norm),
        "edit_distance": distance,
        "cer": round(distance / max(1, len(ref_norm)), 4),
        "note": "digits normalized; punctuation and whitespace ignored; text not included",
    }
    text = json.dumps(result, ensure_ascii=False, indent=2)
    print(text)
    if args.out:
        pathlib.Path(args.out).parent.mkdir(parents=True, exist_ok=True)
        pathlib.Path(args.out).write_text(text + "\n", encoding="utf-8")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        raise SystemExit(130)
