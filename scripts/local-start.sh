#!/usr/bin/env bash
# Start the selected local profile's web API and durable processing worker.
set -uo pipefail
umask 077
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
if [ "$#" -lt 1 ] || [ "$#" -gt 3 ]; then echo "usage: $0 PROFILE [port] [profiles-root]" >&2; exit 2; fi
PROFILE=$1
PORT=${2:-8788}
PROFILES_ROOT=${3:-${RECORDING_PROFILES_ROOT:-$ROOT/data/profiles}}
case "$PROFILE" in ''|*[!a-zA-Z0-9_-]*) echo "local-start: invalid profile name" >&2; exit 2;; esac
case "$PORT" in ''|*[!0-9]*) echo "local-start: port must be numeric" >&2; exit 2;; esac
if [ "$PORT" -lt 1 ] || [ "$PORT" -gt 65535 ]; then echo "local-start: port must be between 1 and 65535" >&2; exit 2; fi
for binary in context-web context-worker; do
  if [ ! -x "$ROOT/bin/$binary" ]; then echo "local-start: missing bin/$binary; run make build first" >&2; exit 1; fi
done
if [ ! -f "$ROOT/web/dist/index.html" ]; then echo "local-start: missing web/dist/index.html; run make web first" >&2; exit 1; fi
WORKER_PID=
WEB_PID=
cleanup() {
  trap - EXIT
  [ -z "$WEB_PID" ] || kill "$WEB_PID" 2>/dev/null || true
  [ -z "$WORKER_PID" ] || kill "$WORKER_PID" 2>/dev/null || true
  [ -z "$WEB_PID" ] || wait "$WEB_PID" 2>/dev/null || true
  [ -z "$WORKER_PID" ] || wait "$WORKER_PID" 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cd "$ROOT"
# Generation stays off unless explicitly configured. Secrets are passed through
# the worker's existing key-env mechanism, never expanded onto its command line.
# Embedding stays offline by default. "local" is the launcher-facing name for
# an OpenAI-compatible local server; both processes must agree on the provider.
EMBED_PROVIDER_ARG=${EMBED_PROVIDER:-hash}
case "$EMBED_PROVIDER_ARG" in local) EMBED_PROVIDER_ARG=openai ;; esac
SEMANTIC_PROVIDER_ARG=${SEMANTIC_PROVIDER:-${EMBED_PROVIDER:-hash}}
case "$SEMANTIC_PROVIDER_ARG" in local) SEMANTIC_PROVIDER_ARG=openai ;; esac
WORKER_ARGS=(--profile "$PROFILE" --profiles-root "$PROFILES_ROOT" --generator "${GENERATOR_PROVIDER:-off}" --embed-provider "$EMBED_PROVIDER_ARG" --asr-provider "${ASR_PROVIDER:-off}")
if [ -n "${GENERATOR_ENDPOINT:-}" ]; then WORKER_ARGS+=(--generator-url "$GENERATOR_ENDPOINT"); fi
if [ -n "${GENERATOR_MODEL:-}" ]; then WORKER_ARGS+=(--generator-model "$GENERATOR_MODEL"); fi
if [ -n "${GENERATOR_KEY_ENV:-}" ]; then WORKER_ARGS+=(--generator-key-env "$GENERATOR_KEY_ENV"); fi
WEB_ARGS=(--profile "$PROFILE" --profiles-root "$PROFILES_ROOT" --addr "127.0.0.1:$PORT" --web-dir "$ROOT/web/dist" --semantic-provider "$SEMANTIC_PROVIDER_ARG" --asr-provider "${ASR_PROVIDER:-off}")
for value in EMBED_ENDPOINT EMBED_MODEL EMBED_DIMS; do
  if [ -n "${!value:-}" ]; then WORKER_ARGS+=("--${value,,}" "${!value}"); fi
done
if [ -n "${EMBED_KEY_ENV+set}" ]; then WORKER_ARGS+=(--embed-key-env "$EMBED_KEY_ENV"); fi
for value in SEMANTIC_ENDPOINT SEMANTIC_MODEL SEMANTIC_DIMS; do
  if [ -n "${!value:-}" ]; then WEB_ARGS+=("--${value,,}" "${!value}"); fi
done
if [ -n "${SEMANTIC_KEY_ENV+set}" ]; then WEB_ARGS+=(--semantic-key-env "$SEMANTIC_KEY_ENV"); fi
# Local ASR settings must reach both processes so provider/config agree.
if [ -n "${ASR_BACKEND:-}" ]; then WORKER_ARGS+=(--asr-backend "$ASR_BACKEND"); WEB_ARGS+=(--asr-backend "$ASR_BACKEND"); fi
if [ -n "${ASR_MODEL:-}" ]; then WORKER_ARGS+=(--asr-model "$ASR_MODEL"); WEB_ARGS+=(--asr-model "$ASR_MODEL"); fi
if [ -n "${ASR_PYTHON:-}" ]; then WORKER_ARGS+=(--asr-python "$ASR_PYTHON"); WEB_ARGS+=(--asr-python "$ASR_PYTHON"); fi
if [ -n "${ASR_DEVICE:-}" ]; then WORKER_ARGS+=(--asr-device "$ASR_DEVICE"); WEB_ARGS+=(--asr-device "$ASR_DEVICE"); fi
if [ -n "${ASR_REVISION:-}" ]; then WORKER_ARGS+=(--asr-revision "$ASR_REVISION"); WEB_ARGS+=(--asr-revision "$ASR_REVISION"); fi
if [ -n "${ASR_TIMEOUT:-}" ]; then WORKER_ARGS+=(--asr-timeout "$ASR_TIMEOUT"); fi
if [ -n "${ASR_LOAD_TIMEOUT:-}" ]; then WORKER_ARGS+=(--asr-load-timeout "$ASR_LOAD_TIMEOUT"); fi
if [ -n "${ASR_WINDOW_TIMEOUT:-}" ]; then WORKER_ARGS+=(--asr-window-timeout "$ASR_WINDOW_TIMEOUT"); fi
if [ -n "${ASR_LOCK:-}" ]; then WORKER_ARGS+=(--asr-lock "$ASR_LOCK"); fi
if [ -n "${ASR_LOCK_TIMEOUT:-}" ]; then WORKER_ARGS+=(--asr-lock-timeout "$ASR_LOCK_TIMEOUT"); fi
# Brief polishing is independent of memory generation and only reaches Web.
if [ -n "${BRIEF_POLISH_PI_PROVIDER:-}" ]; then WEB_ARGS+=(--brief-polish-pi-provider "$BRIEF_POLISH_PI_PROVIDER"); fi
if [ -n "${BRIEF_POLISH_PI_MODELS_FILE:-}" ]; then WEB_ARGS+=(--brief-polish-pi-models-file "$BRIEF_POLISH_PI_MODELS_FILE"); fi
if [ -n "${BRIEF_POLISH_ENDPOINT:-}" ]; then WEB_ARGS+=(--brief-polish-endpoint "$BRIEF_POLISH_ENDPOINT"); fi
if [ -n "${BRIEF_POLISH_MODEL:-}" ]; then WEB_ARGS+=(--brief-polish-model "$BRIEF_POLISH_MODEL"); fi
if [ -n "${BRIEF_POLISH_KEY_ENV:-}" ]; then WEB_ARGS+=(--brief-polish-key-env "$BRIEF_POLISH_KEY_ENV"); fi
if [ -n "${WORLDLINE_DECISION_URL:-}" ]; then WORKER_ARGS+=(--worldline-decision-url "$WORLDLINE_DECISION_URL"); fi
if [ -n "${WORLDLINE_DECISION_MODEL:-}" ]; then WORKER_ARGS+=(--worldline-decision-model "$WORLDLINE_DECISION_MODEL"); fi
if [ -n "${WORLDLINE_DECISION_URL:-}" ]; then WEB_ARGS+=(--worldline-decision-url "$WORLDLINE_DECISION_URL"); fi
if [ -n "${WORLDLINE_DECISION_MODEL:-}" ]; then WEB_ARGS+=(--worldline-decision-model "$WORLDLINE_DECISION_MODEL"); fi
if [ -n "${WORLDLINE_EVENTS_DB:-}" ]; then WORKER_ARGS+=(--worldline-events-db "$WORLDLINE_EVENTS_DB"); fi
WORKER_ARGS+=(--health-addr "127.0.0.1:${WORKER_HEALTH_PORT:-$((PORT + 1))}")
"$ROOT/bin/context-worker" "${WORKER_ARGS[@]}" &
WORKER_PID=$!
"$ROOT/bin/context-web" "${WEB_ARGS[@]}" &
WEB_PID=$!
set +e
EXITED_PID=
wait -n -p EXITED_PID "$WEB_PID" "$WORKER_PID"
STATUS=$?
set -e
if [ "$EXITED_PID" = "$WORKER_PID" ]; then echo "local-start: processing worker exited (status $STATUS)" >&2; else echo "local-start: web server exited (status $STATUS)" >&2; fi
exit "$STATUS"
