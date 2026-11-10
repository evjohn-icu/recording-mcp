#!/usr/bin/env python3
"""Build the evaluation manifest and fixed voiceprint/verification trials."""

from __future__ import annotations

import argparse
import random
import sys
from collections import Counter, defaultdict
from pathlib import Path

from common import DATA, MODELS, RESULTS, clean_voiceprint_segments, now_iso, overlap_duration, read_rttm, read_wav, speaker_groups, union_duration, write_json

SESSIONS = ("L_R004S03C01", "S_R004S03C01")
TRIAL_SEED = 20261110


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--data", type=Path, default=DATA / "aishell4")
    parser.add_argument("--out", type=Path, default=RESULTS / "manifest.json")
    parser.add_argument("--trials-out", type=Path, default=RESULTS / "trials.json")
    args = parser.parse_args()

    sessions: dict[str, dict] = {}
    all_segments: dict[str, list[dict]] = {}
    for session_id in SESSIONS:
        wav = args.data / "test" / "wav" / f"{session_id}.wav"
        rttm = args.data / "test" / "TextGrid" / f"{session_id}.rttm"
        if not wav.exists() or not rttm.exists():
            raise FileNotFoundError(f"missing {wav} or {rttm}")
        audio, sr = read_wav(wav)
        segments = read_rttm(rttm)
        for seg in segments:
            seg["session"] = session_id
        all_segments[session_id] = segments
        speakers = Counter()
        for seg in segments:
            speakers[seg["speaker"]] += seg["end"] - seg["start"]
        sessions[session_id] = {
            "wav": str(wav.relative_to(DATA)),
            "rttm": str(rttm.relative_to(DATA)),
            "duration_seconds": round(len(audio) / sr, 3),
            "sample_rate": sr,
            "n_segments": len(segments),
            "speakers": {key: round(value, 3) for key, value in sorted(speakers.items())},
            "speech_union_seconds": round(union_duration(segments), 3),
            "overlap_seconds": round(overlap_duration(segments), 3),
        }

    # Fixed verification trials.  We keep enrollment/test speakers inside one
    # session because speaker identifiers are session-local in AISHELL-4.
    rng = random.Random(TRIAL_SEED)
    speakers_out: dict[str, dict] = {}
    trials: list[dict] = []
    clean_stats: dict[str, dict] = {}
    for session_id, segments in all_segments.items():
        clean = clean_voiceprint_segments(segments)
        clean_stats[session_id] = {
            "clean_segments": len(clean),
            "clean_speakers": len({seg["speaker"] for seg in clean}),
            "clean_seconds": round(sum(seg["end"] - seg["start"] for seg in clean), 3),
        }
        groups = speaker_groups(clean)
        usable = {speaker: values for speaker, values in groups.items() if len(values) >= 3}
        for speaker, values in usable.items():
            key = f"{session_id}:{speaker}"
            enroll = values[:2]
            test = values[2:][:20]
            speakers_out[key] = {
                "session": session_id,
                "speaker": speaker,
                "enroll": [{"start": seg["start"], "end": seg["end"]} for seg in enroll],
                "n_enroll": len(enroll),
                "n_test": len(test),
            }
            other_speakers = [k for k in usable if k != speaker]
            for seg in test:
                negative_speaker = rng.choice(other_speakers)
                trials.append(
                    {
                        "test": {
                            "session": session_id,
                            "start": seg["start"],
                            "end": seg["end"],
                        },
                        "enroll_speaker": key,
                        "label": 1,
                    }
                )
                trials.append(
                    {
                        "test": {
                            "session": session_id,
                            "start": seg["start"],
                            "end": seg["end"],
                        },
                        "enroll_speaker": f"{session_id}:{negative_speaker}",
                        "label": 0,
                    }
                )

    payload = {
        "generated_at": now_iso(),
        "dataset": "AISHELL-4 (OpenSLR SLR111, CC BY-SA 4.0), test split",
        "sessions": sessions,
        "voiceprint_clean": clean_stats,
        "voiceprint_speakers": speakers_out,
        "trial_seed": TRIAL_SEED,
        "n_trials": len(trials),
        "trial_positive": sum(t["label"] for t in trials),
    }
    write_json(args.out, payload)
    write_json(
        args.trials_out,
        {
            "generated_at": payload["generated_at"],
            "seed": TRIAL_SEED,
            "speakers": speakers_out,
            "trials": trials,
        },
    )
    print(f"wrote {args.out} and {args.trials_out}")
    for session_id, data in sessions.items():
        print(
            f"{session_id}: {data['duration_seconds']:.1f}s, "
            f"{len(data['speakers'])} speakers, overlap {data['overlap_seconds']:.1f}s"
        )
    print(f"voiceprint: {len(speakers_out)} speakers, {len(trials)} trials")
    print(f"models dir: {MODELS}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
