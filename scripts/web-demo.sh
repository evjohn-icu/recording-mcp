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
python3 "$ROOT/scripts/fixture-audio-bridge.py" --db "$DB" --audio-dir "$AUDIO_DIR"

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
