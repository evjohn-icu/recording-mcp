#!/usr/bin/env python3
"""Download the complete AISHELL-4 test split and convert channel 0 to 16 kHz mono WAV."""

from __future__ import annotations

import argparse
import subprocess
import sys
from pathlib import Path

from common import DATA, RESULTS, ensure_results, read_rttm, write_json

EXPECTED_SESSIONS = 20
MAX_DURATION_DELTA = 2.0
LICENSE = "CC BY-SA 4.0"


def convert(flac: Path, wav: Path) -> None:
    subprocess.run(
        [
            "ffmpeg",
            "-hide_banner",
            "-loglevel",
            "error",
            "-y",
            "-i",
            str(flac),
            "-af",
            "pan=mono|c0=c0",
            "-ar",
            "16000",
            "-sample_fmt",
            "s16",
            str(wav),
        ],
        check=True,
    )


def main() -> int:
    import soundfile as sf
    from huggingface_hub import hf_hub_download, list_repo_files

    parser = argparse.ArgumentParser()
    parser.add_argument("--data", type=Path, default=DATA / "aishell4")
    parser.add_argument("--repo", default="AISHELL/AISHELL-4")
    args = parser.parse_args()

    ensure_results()
    files = set(list_repo_files(args.repo, repo_type="dataset"))
    flacs = sorted(f for f in files if f.startswith("test/wav/") and f.endswith(".flac"))
    pairs: list[tuple[str, str]] = []
    for flac in flacs:
        session_id = Path(flac).stem
        rttm = f"test/TextGrid/{session_id}.rttm"
        if rttm not in files:
            print(f"skip {session_id}: missing {rttm}")
            continue
        pairs.append((flac, rttm))

    # The official test split has exactly 20 sessions; a different count means
    # the mirror changed, so refuse to write a partial "full split" result.
    if len(pairs) != EXPECTED_SESSIONS:
        raise RuntimeError(
            f"expected {EXPECTED_SESSIONS} wav/rttm pairs, found {len(pairs)} on {args.repo}"
        )

    sessions: dict[str, dict] = {}
    for flac_rel, rttm_rel in pairs:
        flac = Path(hf_hub_download(args.repo, flac_rel, repo_type="dataset", local_dir=args.data))
        rttm = Path(hf_hub_download(args.repo, rttm_rel, repo_type="dataset", local_dir=args.data))
        session_id = flac.stem
        wav = args.data / "test" / "wav" / f"{session_id}.wav"
        convert(flac, wav)

        info = sf.info(str(wav))
        duration = float(info.frames) / float(info.samplerate)
        max_end = max((seg["end"] for seg in read_rttm(rttm)), default=0.0)
        # AISHELL-4 meetings end with non-speech: the existing S_R004S03C01 has
        # a 21.0 s tail gap, so a symmetric 2 s check would reject valid data.
        # The failure that matters is the reference pointing past the audio.
        if max_end > duration + MAX_DURATION_DELTA:
            raise RuntimeError(
                f"{session_id}: rttm end {max_end:.3f}s exceeds wav {duration:.3f}s "
                f"by more than {MAX_DURATION_DELTA}s"
            )

        sessions[session_id] = {
            "wav": str(wav.relative_to(args.data)),
            "rttm": str(rttm.relative_to(args.data)),
            "duration_seconds": round(duration, 3),
            "rttm_max_end_seconds": round(max_end, 3),
            "rttm_tail_gap_seconds": round(duration - max_end, 3),
            "sample_rate": int(info.samplerate),
            "channels": int(info.channels),
            "wav_bytes": wav.stat().st_size,
            "rttm_bytes": rttm.stat().st_size,
        }
        print(
            f"{session_id}: {duration:.1f}s, wav {sessions[session_id]['wav_bytes'] / 2**20:.1f} MiB"
        )

    payload = {
        "dataset": "AISHELL-4 (OpenSLR SLR111), test split",
        "repo": args.repo,
        "license": LICENSE,
        "n_sessions": len(sessions),
        "sessions": sessions,
        "conversion": "ffmpeg pan=mono|c0=c0 -ar 16000 -sample_fmt s16",
    }
    write_json(RESULTS / "aishell4_download.json", payload)
    print(f"{len(sessions)} sessions written to {RESULTS / 'aishell4_download.json'}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
