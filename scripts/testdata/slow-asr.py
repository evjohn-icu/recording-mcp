#!/usr/bin/env python3
"""Fault-injection chunk runner for long-audio reliability tests.

It speaks the same stdout protocol as scripts/local-asr.py (--stream, --skip,
one JSON line per window) but never loads a model: it sleeps a fixed delay per
window and can fail or report CUDA OOM on a chosen window. Every line it emits
is a fault-injection artifact, not real ASR.
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import time

BYTES_PER_MS = 32


def plan_windows(duration_ms: int, window_ms: int, overlap_ms: int) -> list[tuple[int, int]]:
    if window_ms <= 0:
        raise ValueError("window-ms must be positive")
    if overlap_ms < 0 or overlap_ms >= window_ms:
        raise ValueError("overlap-ms must be in [0, window-ms)")
    step = window_ms - overlap_ms
    out: list[tuple[int, int]] = []
    start = 0
    while start < duration_ms:
        end = min(start + window_ms, duration_ms)
        out.append((start, end))
        if end >= duration_ms:
            break
        start += step
    return out


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="fault-injection chunk runner (no model)")
    p.add_argument("--backend", default="ark")
    p.add_argument("--pcm", required=True)
    p.add_argument("--model", default="test-stub")
    p.add_argument("--revision", default="test")
    p.add_argument("--dtype", default="float16")
    p.add_argument("--device", default="cpu")
    p.add_argument("--language", default="")
    p.add_argument("--window-ms", type=int, default=30000)
    p.add_argument("--overlap-ms", type=int, default=2000)
    p.add_argument("--max-new-tokens", type=int, default=0)
    p.add_argument("--stream", action="store_true")
    p.add_argument("--skip", default="")
    p.add_argument("--load-timeout", type=int, default=300)
    p.add_argument("--window-timeout", type=int, default=300)
    p.add_argument("--delay", type=float, default=float(os.environ.get("SLOW_ASR_DELAY", "0.05")))
    p.add_argument("--fail-index", type=int, default=int(os.environ.get("SLOW_ASR_FAIL_INDEX", "0")))
    p.add_argument("--oom-index", type=int, default=int(os.environ.get("SLOW_ASR_OOM_INDEX", "0")))
    args, _unknown = p.parse_known_args()
    return args


def main() -> int:
    args = parse_args()
    duration = os.path.getsize(args.pcm) // BYTES_PER_MS
    if duration <= 0:
        print("slow-asr: empty PCM", file=sys.stderr)
        return 2
    windows = plan_windows(duration, args.window_ms, args.overlap_ms)
    skip = {int(part) for part in args.skip.split(",") if part.strip()}
    for index in skip:
        if index < 1 or index > len(windows):
            print(f"slow-asr: skip index {index} outside plan", file=sys.stderr)
            return 2
    print(f"slow-asr: fault-injection runner, {len(windows)} window(s), delay={args.delay}s", file=sys.stderr, flush=True)
    for index, (start, end) in enumerate(windows, 1):
        if index in skip:
            continue
        if args.oom_index and index == args.oom_index:
            print("torch.OutOfMemoryError: CUDA out of memory (fault-injection)", file=sys.stderr, flush=True)
            return 8
        if args.fail_index and index == args.fail_index:
            print(f"slow-asr: injected failure on window {index}", file=sys.stderr, flush=True)
            return 7
        time.sleep(args.delay)
        if args.stream:
            print(json.dumps({
                "index": index,
                "start_ms": start,
                "end_ms": end,
                "text": f"fault-injection window {index}",
                "language": args.language or "Chinese",
            }, ensure_ascii=False), flush=True)
    if not args.stream:
        print(json.dumps({
            "backend": args.backend,
            "model": args.model,
            "language": args.language,
            "windows": [
                {"index": i, "start_ms": s, "end_ms": e, "text": f"fault-injection window {i}"}
                for i, (s, e) in enumerate(windows, 1) if i not in skip
            ],
        }, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
