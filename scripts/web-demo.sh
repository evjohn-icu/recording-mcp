#!/bin/sh
# Run the local web browser against a disposable, synthetic database only.
# The only accepted argument is an optional loopback port; the database and
# audio directory are always created below /tmp and are never caller-supplied.
set -eu

umask 077
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
cd "$ROOT"

if [ "$#" -gt 1 ]; then
    echo "usage: $0 [port]" >&2
    exit 2
fi
PORT=${1:-8788}
case "$PORT" in
    ''|*[!0-9]*) echo "web-demo: port must be an integer (got: $PORT)" >&2; exit 2 ;;
esac
if [ "$PORT" -lt 1 ] || [ "$PORT" -gt 65535 ]; then
    echo "web-demo: port must be between 1 and 65535" >&2
    exit 2
fi

if ! command -v python3 >/dev/null 2>&1; then
    echo "web-demo: python3 is required for the synthetic PCM fixture" >&2
    exit 1
fi
for binary in bin/context-eval bin/context-web; do
    if [ ! -x "$ROOT/$binary" ]; then
        echo "web-demo: missing $binary; run 'make build' first" >&2
        exit 1
    fi
done
if [ ! -f "$ROOT/web/dist/index.html" ]; then
    echo "web-demo: missing web/dist/index.html; run 'make web' first" >&2
    exit 1
fi

# This directory is freshly allocated by mktemp. In particular, do not turn
# this into a --db/data argument: context-eval intentionally removes its db
# path before populating it, so only this disposable path may be passed.
DEMO_DIR=$(mktemp -d /tmp/recording-mcp-web-demo.XXXXXX)
DB="$DEMO_DIR/synthetic-eval.db"
AUDIO_DIR="$DEMO_DIR/synthetic-audio"
REPORT="$DEMO_DIR/context-eval-report.json"
mkdir -m 700 "$AUDIO_DIR"

if [ -e "$DB" ] || [ -e "$DB-wal" ] || [ -e "$DB-shm" ] || [ -e "$DB-journal" ]; then
    echo "web-demo: refusing to use an existing database path: $DB" >&2
    exit 1
fi

echo "recording-mcp web demo: SYNTHETIC DATA ONLY"
echo "demo directory: $DEMO_DIR"
echo "synthetic database: $DB"
echo "synthetic audio directory: $AUDIO_DIR"
echo "evaluation report: $REPORT"
echo "running deterministic context-eval (this database is disposable)..."
"$ROOT/bin/context-eval" --db "$DB" --out "$REPORT"

# context-eval creates the real fixture records, revisions, notes and source
# attachment provenance. This Python-only step deliberately reads those rows
# rather than guessing IDs, then adds raw 16 kHz mono s16le PCM and the exact
# provider recording/device bridge consumed by internal/app/audio.go.
python3 - "$DB" "$AUDIO_DIR" <<'PY'
import math
import os
import sqlite3
import struct
import sys

DB, AUDIO_DIR = sys.argv[1:]
conn = sqlite3.connect(DB)
try:
    required = {"source_attachments", "attachment_turns", "turns", "sync_recordings"}
    present = {row[0] for row in conn.execute("SELECT name FROM sqlite_master WHERE type='table'")}
    missing = required - present
    if missing:
        raise RuntimeError("synthetic eval database is missing tables: " + ", ".join(sorted(missing)))

    # One row per memory recording/provider pair. The provider IDs and device
    # IDs come from the fixture's source JSON, not from user or real-db input.
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
        raise RuntimeError("no fixture source_attachments with provider recording/device IDs")

    sample_rate = 16000
    amplitude = 6000
    frequency = 440.0
    now = 1
    for index, (memory_recording_id, provider_recording_id, device_id, max_end_ms, started_at) in enumerate(rows):
        if not provider_recording_id or not device_id:
            raise RuntimeError("fixture source attachment has an empty provider recording/device ID")
        # Leave a deterministic one-second tail. Every real fixture turn end
        # is therefore strictly inside the file, including superseded turns.
        duration_ms = max(1000, int(max_end_ms or 0) + 1000)
        sample_count = (duration_ms * sample_rate + 999) // 1000
        pcm_path = os.path.abspath(os.path.join(AUDIO_DIR, "recording-%03d.pcm" % index))
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
            (provider_recording_id, device_id, sample_count, started_at,
             pcm_path, now + index),
        )

    conn.commit()
    count = conn.execute("SELECT COUNT(*) FROM sync_recordings WHERE owner='synthetic-demo'").fetchone()[0]
    if count != len(rows):
        raise RuntimeError("audio bridge row count mismatch")
    print("audio fixture: %d synthetic recordings mapped from source_attachments" % count)
    print("audio format: deterministic 16 kHz mono s16le .pcm; fixture turn offsets covered")
finally:
    conn.close()
PY

if [ ! -s "$DB" ]; then
    echo "web-demo: context-eval did not create a database" >&2
    exit 1
fi

echo "starting context-web on http://127.0.0.1:$PORT"
echo "the next context-web line contains the single-use localhost token (valid for 10 minutes)"
# exec means Ctrl-C/SIGTERM is delivered only to this demo server, never to
# an unrelated service. The disposable directory is intentionally printed and
# retained for inspection after the demo exits.
exec "$ROOT/bin/context-web" \
    --db "$DB" \
    --audio-dir "$AUDIO_DIR" \
    --web-dir "$ROOT/web/dist" \
    --addr "127.0.0.1:$PORT" \
    --semantic-provider hash
