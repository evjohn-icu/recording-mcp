#!/bin/sh
# M1 demo: ingest a doubao ASR fixture, run the honest pipeline worker with
# the synthetic generator (allowed explicitly for demos), then verify over
# the context-memory MCP stdio server that the raw speech is searchable and
# the index reports ready. Everything lives under data/demo (gitignored).
set -eu

cd "$(dirname "$0")/.."

DEMO_DIR=data/demo
DB="$DEMO_DIR/context-memory-demo.db"

echo "== build =="
make build

echo "== reset $DEMO_DIR =="
rm -rf "$DEMO_DIR"
mkdir -p "$DEMO_DIR"

echo "== 1/4 normalize the doubao fixture =="
bin/adapter-doubao \
	--input examples/asr/doubao-final.jsonl \
	--metadata examples/asr/doubao-metadata.json \
	--output "$DEMO_DIR/normalized.json"
test -s "$DEMO_DIR/normalized.json"

echo "== 2/4 import the transcript =="
CONV_ID=$(bin/import-transcript --db "$DB" --input "$DEMO_DIR/normalized.json" \
	| python3 -c 'import json,sys; print(json.load(sys.stdin)["conversation_id"])')
echo "conversation_id=$CONV_ID"

echo "== 3/4 drain the pipeline (synthetic generator, demo-only) =="
i=0
while [ "$i" -lt 30 ]; do
	out=$(bin/context-worker \
		--db "$DB" \
		--executor pipeline \
		--generator synthetic --allow-synthetic \
		--stages extract,publish,curate,embed \
		--once 2>&1) || true
	case "$out" in
	*"no job available"*)
		echo "queue drained after $i job(s)"
		break
		;;
	esac
	i=$((i + 1))
done
if [ "$i" -ge 30 ]; then
	echo "demo: worker loop did not drain after 30 iterations" >&2
	exit 1
fi

echo "== 4/4 verify over MCP (search / status / evidence) =="
DB="$DB" DEMO_DIR="$DEMO_DIR" CONV_ID="$CONV_ID" python3 scripts/demo-verify.py

echo
echo "=============================================================="
echo " demo used the synthetic generator: placeholder candidates,"
echo " NOT real model analysis. Real distillation example:"
echo "   bin/context-worker --db \"$DB\" \\"
echo "     --generator openai --generator-url https://api.example.com/v1/chat/completions \\"
echo "     --generator-model glm-5.3-flash   # key via GENERATOR_API_KEY"
echo "=============================================================="
