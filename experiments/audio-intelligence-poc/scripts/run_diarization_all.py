#!/usr/bin/env python3
"""Run one diarization engine over every AISHELL-4 test session and aggregate DER."""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
from collections import Counter
from pathlib import Path

from common import DATA, RESULTS, ensure_results, read_rttm, write_json
from prepare_eval import discover_sessions

COMPONENTS = ("correct", "false alarm", "missed detection", "confusion", "total")


def reference_speakers(rttm: Path) -> int:
    return len(Counter(seg["speaker"] for seg in read_rttm(rttm)))


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--engine", choices=["sherpa", "pyannote"], required=True)
    parser.add_argument("--device", choices=["cpu", "cuda"], default="cpu")
    parser.add_argument("--data", type=Path, default=DATA / "aishell4")
    parser.add_argument("--sessions", nargs="*", default=None)
    parser.add_argument("--num-clusters-mode", choices=["oracle", "auto"], default="oracle")
    parser.add_argument("--threshold", type=float, default=0.5)
    parser.add_argument("--threads", type=int, default=4)
    parser.add_argument("--batch-size", type=int, default=8)
    parser.add_argument("--max-seconds", type=float, default=None)
    parser.add_argument(
        "--resume",
        action="store_true",
        help="reuse an existing per-session JSON instead of re-running it",
    )
    parser.add_argument("--out", type=Path, default=None)
    args = parser.parse_args()

    ensure_results()
    session_ids = args.sessions if args.sessions else discover_sessions(args.data)
    if not session_ids:
        raise SystemExit(f"no sessions found under {args.data / 'test'}")

    script = Path(__file__).resolve().parent / "run_diarization.py"
    totals: Counter = Counter()
    per_session: dict[str, dict] = {}
    for session_id in session_ids:
        # One JSON per session so an aborted run keeps completed evidence.
        session_stem = f"diarization_{args.engine}_{args.device}_{session_id}"
        if args.max_seconds:
            session_stem += f"_first{int(args.max_seconds)}s"
        session_out = RESULTS / f"{session_stem}.json"
        argv = [
            sys.executable,
            str(script),
            "--engine",
            args.engine,
            "--session",
            session_id,
            "--data",
            str(args.data),
            "--device",
            args.device,
            "--threads",
            str(args.threads),
            "--batch-size",
            str(args.batch_size),
            "--out",
            str(session_out),
        ]
        expected_speakers = reference_speakers(
            args.data / "test" / "TextGrid" / f"{session_id}.rttm"
        )
        if args.engine == "sherpa":
            argv += ["--provider", args.device]
            if args.num_clusters_mode == "oracle":
                argv += ["--num-clusters", str(expected_speakers)]
            else:
                argv += ["--threshold", str(args.threshold)]
        if args.max_seconds:
            argv += ["--max-seconds", str(args.max_seconds)]
        if args.resume and session_out.exists():
            reused = json.load(open(session_out, encoding="utf-8"))
            # Reusing a file produced with different clustering parameters
            # would silently mix incomparable rows into the aggregate.
            stale = []
            if args.engine == "sherpa" and args.num_clusters_mode == "oracle":
                if reused.get("num_clusters") != expected_speakers:
                    stale.append(f"num_clusters={reused.get('num_clusters')} != {expected_speakers}")
            elif args.engine == "sherpa":
                # Threshold clustering leaves num_clusters at the sentinel -1;
                # an oracle file reused here would be relabelled as "auto".
                if reused.get("num_clusters") != -1:
                    stale.append(f"num_clusters={reused.get('num_clusters')} != -1 (auto mode)")
                if reused.get("threshold") != args.threshold:
                    stale.append(f"threshold={reused.get('threshold')} != {args.threshold}")
            if args.max_seconds is None and reused.get("scope") != "full":
                stale.append(f"scope={reused.get('scope')} != full")
            if stale:
                raise SystemExit(f"{session_out.name}: cannot resume ({', '.join(stale)})")
            print(f"{session_id}: reusing {session_out.name}")
        else:
            # Let a sherpa CUDA failure propagate: a CUDA row must never silently
            # report CPU numbers.
            subprocess.run(argv, check=True)

        payload = json.load(open(session_out, encoding="utf-8"))
        components = payload["metrics"]["components"]
        for key in COMPONENTS:
            totals[key] += float(components.get(key, 0.0))
        per_session[session_id] = {
            "der": payload["metrics"]["der"],
            "components": {key: float(components.get(key, 0.0)) for key in COMPONENTS},
            "ref_speakers": payload["metrics"]["ref_speakers"],
            "hyp_speakers": payload["metrics"]["hyp_speakers"],
            "expected_speakers": expected_speakers,
            "num_clusters": payload.get("num_clusters"),
            "audio_seconds": payload["audio_seconds"],
            "inference_seconds": payload["inference_seconds"],
            "load_seconds": payload["load_seconds"],
            "rtf": payload["rtf"],
            "peak_rss_mb": payload["peak_rss_mb"],
            "gpu_peak_mb": payload.get("gpu_peak_mb"),
            "n_hypothesis_segments": payload["n_hypothesis_segments"],
        }
        print(
            f"{session_id}: DER={payload['metrics']['der']:.4f} "
            f"speakers={payload['metrics']['ref_speakers']}/{payload['metrics']['hyp_speakers']} "
            f"RTF={payload['rtf']:.4f}"
        )

    total = totals["total"]
    # pyannote's `total` excludes false alarms (it equals correct + missed +
    # confusion), so (total - correct) / total would drop the FA term.
    errors = totals["false alarm"] + totals["missed detection"] + totals["confusion"]
    aggregate_der = errors / total if total else None
    audio_seconds = sum(v["audio_seconds"] for v in per_session.values())
    inference_seconds = sum(v["inference_seconds"] for v in per_session.values())
    payload = {
        "engine": "sherpa-onnx" if args.engine == "sherpa" else "pyannote-community-1",
        "session": "all",
        "device": args.device,
        "provider": args.device,
        "num_clusters_mode": args.num_clusters_mode if args.engine == "sherpa" else None,
        "threshold": args.threshold if args.engine == "sherpa" and args.num_clusters_mode == "auto" else None,
        "scope": "full" if args.max_seconds is None else f"first {args.max_seconds:.0f}s",
        "n_sessions": len(session_ids),
        "audio_seconds": round(audio_seconds, 3),
        "inference_seconds": round(inference_seconds, 4),
        "rtf": inference_seconds / audio_seconds if audio_seconds else None,
        "aggregate": {
            "der": aggregate_der,
            "components": {key: totals[key] for key in COMPONENTS},
            "ref_speakers": sum(v["ref_speakers"] for v in per_session.values()),
            "hyp_speakers": sum(v["hyp_speakers"] for v in per_session.values()),
        },
        "per_session": per_session,
        "notes": [
            "Aggregate DER is (false alarm + missed detection + confusion) / total over the summed pyannote.metrics components at a 0.25 s collar.",
            "Each session is scored independently; speaker counts are compared per session, not merged.",
        ],
    }
    out = args.out or RESULTS / f"diarization_{args.engine}_{args.device}_all.json"
    write_json(out, payload)
    print(f"aggregate DER={aggregate_der:.4f} over {len(session_ids)} sessions")
    print(f"wrote {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
