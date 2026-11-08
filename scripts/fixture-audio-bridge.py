#!/usr/bin/env python3
"""Create deterministic synthetic PCM plus sync_recording rows for a fixture DB.

Extracted from scripts/web-demo.sh so the Beta demo and the existing synthetic
demo share one implementation. Only temporary databases are accepted.
"""
from __future__ import annotations

import argparse
import math
import os
import sqlite3
import struct
import sys
import tempfile


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--db", required=True)
    parser.add_argument("--audio-dir", required=True)
    args = parser.parse_args()
    db_path = os.path.abspath(args.db)
    audio_dir = os.path.abspath(args.audio_dir)
    temp_root = os.path.abspath(tempfile.gettempdir())
    if not db_path.startswith(temp_root + os.sep):
        raise SystemExit("fixture-audio-bridge: refusing to modify a database outside the temporary directory")
    if not os.path.exists(db_path):
        raise SystemExit("fixture-audio-bridge: database does not exist")
    if os.path.commonpath([audio_dir, temp_root]) != temp_root:
        raise SystemExit("fixture-audio-bridge: refusing to write audio outside the temporary directory")
    os.makedirs(audio_dir, exist_ok=True)

    conn = sqlite3.connect(db_path)
    try:
        present = {row[0] for row in conn.execute("SELECT name FROM sqlite_master WHERE type='table'")}
        missing = {"source_attachments", "attachment_turns", "turns", "sync_recordings"} - present
        if missing:
            raise SystemExit("fixture-audio-bridge: missing tables: " + ", ".join(sorted(missing)))
        rows = conn.execute(
            """
            SELECT a.recording_id,
                   json_extract(a.source_json, '$.recording_id'),
                   json_extract(a.source_json, '$.device_id'),
                   COALESCE(MAX(t.global_end_ms), 0),
                   MIN(json_extract(a.source_json, '$.recording_started_at'))
              FROM source_attachments AS a
              LEFT JOIN attachment_turns AS at ON at.attachment_id = a.id
              LEFT JOIN turns AS t ON t.id = at.turn_id
             WHERE json_extract(a.source_json, '$.recording_id') IS NOT NULL
               AND json_extract(a.source_json, '$.device_id') IS NOT NULL
             GROUP BY a.recording_id,
                      json_extract(a.source_json, '$.recording_id'),
                      json_extract(a.source_json, '$.device_id')
             ORDER BY a.recording_id
            """
        ).fetchall()
        if not rows:
            raise SystemExit("fixture-audio-bridge: no fixture source_attachments")

        sample_rate, amplitude, frequency = 16000, 6000, 440.0
        for index, (_, provider_recording_id, device_id, max_end_ms, started_at) in enumerate(rows):
            if not provider_recording_id or not device_id:
                raise SystemExit("fixture-audio-bridge: empty provider recording/device id")
            duration_ms = max(1000, int(max_end_ms or 0) + 1000)
            sample_count = (duration_ms * sample_rate + 999) // 1000
            pcm_path = os.path.join(audio_dir, "recording-%03d.pcm" % index)
            phase = 0
            with open(pcm_path, "wb") as pcm:
                remaining = sample_count
                while remaining:
                    count = min(16000, remaining)
                    values = [int(amplitude * math.sin(2.0 * math.pi * frequency * (phase + n) / sample_rate)) for n in range(count)]
                    pcm.write(struct.pack("<%dh" % count, *values))
                    phase += count
                    remaining -= count
            conn.execute(
                """
                INSERT OR REPLACE INTO sync_recordings
                  (recording_id, owner, device_id, state, total_chunks,
                   total_samples, started_at, stitched_path, transcript_revision,
                   transcription_state, last_error, acked_at, updated_at)
                VALUES (?, 'synthetic-demo', ?, 'downloaded', 1, ?, ?, ?,
                        'asr-v1', 'done', NULL, NULL, ?)
                """,
                (provider_recording_id, device_id, sample_count, started_at, os.path.abspath(pcm_path), index + 1),
            )
        conn.commit()
        count = conn.execute("SELECT COUNT(*) FROM sync_recordings WHERE owner='synthetic-demo'").fetchone()[0]
        if count != len(rows):
            raise SystemExit("fixture-audio-bridge: sync_recording row count mismatch")
        print(f"audio fixture: {count} synthetic recordings mapped from source_attachments")
    finally:
        conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
