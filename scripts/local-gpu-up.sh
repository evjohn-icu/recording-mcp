#!/usr/bin/env bash
# One-command offline GPU startup for the local (Mode B) deployment:
# preflight -> ASR model probe -> optional embedding server -> web+worker ->
# health -> controlled shutdown. Prints one readiness line and writes
# artifacts/local-gpu/startup.json.
set -euo pipefail
umask 077
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
umask 077

PROFILE=${RECORDING_PROFILE:-home}
PORT=8788
PROFILES_ROOT="$ROOT/data/profiles"
BACKEND=qwen
ENV_FILE=""
CHECK=0
BUILD=1
CYCLES=1
WITH_EMBEDDING=0
WORKER_HEALTH_PORT=

usage() {
  cat <<'EOF'
usage: local-gpu-up.sh [--profile NAME] [--port N] [--profiles-root DIR]
                       [--backend ark|qwen] [--env-file PATH] [--check]
                       [--no-build] [--cycles N] [--with-embedding]
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --profile) PROFILE=${2:?}; shift 2 ;;
    --port) PORT=${2:?}; shift 2 ;;
    --profiles-root) PROFILES_ROOT=${2:?}; shift 2 ;;
    --backend) BACKEND=${2:?}; shift 2 ;;
    --env-file) ENV_FILE=${2:?}; shift 2 ;;
    --check) CHECK=1; shift ;;
    --no-build) BUILD=0; shift ;;
    --cycles) CYCLES=${2:?}; shift 2 ;;
    --with-embedding) WITH_EMBEDDING=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "local-gpu-up: unknown argument $1" >&2; usage >&2; exit 2 ;;
  esac
done

case "$PROFILE" in ''|*[!a-zA-Z0-9_-]*) echo "local-gpu-up: invalid profile name" >&2; exit 2 ;; esac
case "$PORT" in ''|*[!0-9]*) echo "local-gpu-up: port must be numeric" >&2; exit 2 ;; esac
case "$CYCLES" in ''|*[!0-9]*) echo "local-gpu-up: cycles must be numeric" >&2; exit 2 ;; esac
if [ "$CYCLES" -lt 1 ]; then echo "local-gpu-up: --cycles must be at least 1" >&2; exit 2; fi
case "$BACKEND" in ark|qwen) ;; *) echo "local-gpu-up: backend must be ark or qwen" >&2; exit 2 ;; esac
WORKER_HEALTH_PORT=${WORKER_HEALTH_PORT:-$((PORT + 1))}

if [ -n "$ENV_FILE" ]; then
  if [ ! -f "$ENV_FILE" ]; then echo "local-gpu-up: env file not found: $ENV_FILE" >&2; exit 1; fi
elif [ -f "$ROOT/.env" ]; then
  ENV_FILE="$ROOT/.env"
fi
if [ -n "$ENV_FILE" ]; then
  env_owner=$(stat -c %u "$ENV_FILE")
  [ "$env_owner" = "$(id -u)" ] || die "env file must be owned by the current user: $ENV_FILE"
  env_mode=$(stat -c %a "$ENV_FILE")
  if [ $(( 0$env_mode & 077 )) -ne 0 ]; then
    die "env file must not be group/world accessible (chmod 600): $ENV_FILE"
  fi
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  set +a
  # Re-validate anything the file may have overridden.
  case "$PROFILE" in ''|*[!a-zA-Z0-9_-]*) die "invalid profile name from env file" ;; esac
  case "$PORT" in ''|*[!0-9]*) die "port must be numeric" ;; esac
  case "$BACKEND" in ark|qwen) ;; *) die "backend must be ark or qwen" ;; esac
fi

TEST_MODE=${LOCAL_GPU_UP_TEST_MODE:-0}
ARTIFACTS_DIR=$ROOT/artifacts/local-gpu
PYTHON=$ROOT/.venv-asr/bin/python
ASR_RUNNER=$ROOT/scripts/local-asr.py
ASR_PROBE_CMD=
WEB_BIN=$ROOT/bin/context-web
WORKER_BIN=$ROOT/bin/context-worker
PROFILE_BIN=$ROOT/bin/context-profile
START_SCRIPT=$ROOT/scripts/local-start.sh
HEALTH_CMD=
WORKER_HEALTH_CMD=
NVIDIA_SMI=nvidia-smi
HEALTH_TIMEOUT=120
HF_ROOT=${HF_HOME:-$HOME/.cache/huggingface}/hub
# Test-only hooks are inert unless LOCAL_GPU_UP_TEST_MODE=1: they execute
# commands and redirect binaries, so an accidental environment value (or a
# sourced .env) must not be able to reach them.
FFMPEG_BIN=ffmpeg
FFPROBE_BIN=ffprobe
if [ "$TEST_MODE" = 1 ]; then
  ARTIFACTS_DIR=${LOCAL_GPU_UP_ARTIFACTS_DIR:-$ARTIFACTS_DIR}
  PYTHON=${LOCAL_GPU_UP_PYTHON:-$PYTHON}
  ASR_RUNNER=${LOCAL_GPU_UP_ASR_RUNNER:-$ASR_RUNNER}
  ASR_PROBE_CMD=${LOCAL_GPU_UP_ASR_PROBE:-}
  WEB_BIN=${LOCAL_GPU_UP_WEB_BIN:-$WEB_BIN}
  WORKER_BIN=${LOCAL_GPU_UP_WORKER_BIN:-$WORKER_BIN}
  PROFILE_BIN=${LOCAL_GPU_UP_PROFILE_BIN:-$PROFILE_BIN}
  START_SCRIPT=${LOCAL_GPU_UP_START_SCRIPT:-$START_SCRIPT}
  HEALTH_CMD=${LOCAL_GPU_UP_HEALTH_CMD:-}
  WORKER_HEALTH_CMD=${LOCAL_GPU_UP_WORKER_HEALTH_CMD:-}
  NVIDIA_SMI=${LOCAL_GPU_UP_NVIDIA_SMI:-$NVIDIA_SMI}
  HEALTH_TIMEOUT=${LOCAL_GPU_UP_HEALTH_TIMEOUT:-$HEALTH_TIMEOUT}
  HF_ROOT=${LOCAL_GPU_UP_HF_ROOT:-$HF_ROOT}
  FFMPEG_BIN=${LOCAL_GPU_UP_FFMPEG:-ffmpeg}
  FFPROBE_BIN=${LOCAL_GPU_UP_FFPROBE:-ffprobe}
  EMBED_CMD=${LOCAL_GPU_UP_EMBED_CMD:-}
  EMBED_HEALTH_CMD=${LOCAL_GPU_UP_EMBED_HEALTH_CMD:-}
fi

# Model code must not inherit API keys or the profile password, matching the
# Go runner's whitelist (internal/cloudsync.localASREnv).
CLEAN_ENV=(env -i)
for key in PATH HOME TMPDIR TEMP LANG LC_ALL CUDA_VISIBLE_DEVICES CUDA_HOME LD_LIBRARY_PATH \
           HF_HOME HF_HUB_CACHE HF_HUB_OFFLINE TRANSFORMERS_CACHE XDG_CACHE_HOME; do
  if [ -n "${!key+set}" ]; then CLEAN_ENV+=("$key=${!key}"); fi
done

mkdir -p "$ARTIFACTS_DIR"

die() { echo "local-gpu-up: $*" >&2; exit 1; }

pin() {
  python3 - "$ROOT/scripts/model-revisions.json" "$1" "$2" <<'PY'
import json, sys
models = json.load(open(sys.argv[1], encoding="utf-8"))["models"]
print(models[sys.argv[2]][sys.argv[3]])
PY
}

snapshot_dir() {
  local repo=$1 revision=$2
  echo "$HF_ROOT/models--${repo//\//--}/snapshots/$revision"
}

NVIDIA_SMI_BIN=
free_vram_mb() {
  "$NVIDIA_SMI_BIN" --query-gpu=memory.free --format=csv,noheader,nounits -i 0 2>/dev/null | tr -d ' ' | head -n1
}

required_vram() {
  if [ "$BACKEND" = qwen ]; then echo 6000; else echo 4500; fi
}

preflight() {
  NVIDIA_SMI_BIN=$(command -v "$NVIDIA_SMI" 2>/dev/null || true)
  [ -n "$NVIDIA_SMI_BIN" ] || die "nvidia-smi not found; this launcher requires a local NVIDIA GPU"
  local free
  free=$(free_vram_mb || true)
  case "$free" in ''|*[!0-9]*) die "cannot read GPU memory from nvidia-smi" ;; esac
  local need
  need=$(required_vram)
  [ "$free" -ge "$need" ] || die "GPU has ${free} MiB free, ${need} MiB required for --backend $BACKEND"
  command -v "$FFMPEG_BIN" >/dev/null 2>&1 || die "ffmpeg not found"
  command -v "$FFPROBE_BIN" >/dev/null 2>&1 || die "ffprobe not found"
  [ -x "$PYTHON" ] || die "ASR interpreter not found: $PYTHON (see docs/productization/local-asr.md)"
  local repo revision dir
  repo=$(pin asr_"$BACKEND" repo_id)
  revision=$(pin asr_"$BACKEND" revision)
  dir=$(snapshot_dir "$repo" "$revision")
  [ -d "$dir" ] || die "pinned ASR model missing: $dir; run scripts/fetch-local-models.sh"
  if embedding_enabled; then
    repo=$(pin embed_qwen repo_id)
    revision=$(pin embed_qwen revision)
    dir=$(snapshot_dir "$repo" "$revision")
    [ -d "$dir" ] || die "pinned embedding model missing: $dir; run scripts/fetch-local-models.sh"
  fi
  [ -x "$WEB_BIN" ] || die "missing $WEB_BIN; run make build"
  [ -x "$WORKER_BIN" ] || die "missing $WORKER_BIN; run make build"
  [ -f "$ROOT/web/dist/index.html" ] || die "missing web/dist/index.html; run make web"
}

embedding_enabled() {
  [ "$WITH_EMBEDDING" = 1 ] || [ "${EMBED_PROVIDER:-}" = local ]
}

maybe_build() {
  [ "$BUILD" = 1 ] || return 0
  [ "$TEST_MODE" = 1 ] && return 0
  if [ ! -x "$WEB_BIN" ] || [ ! -x "$WORKER_BIN" ]; then
    echo "local-gpu-up: building Go binaries" >&2
    (cd "$ROOT" && make build)
  fi
  if [ ! -f "$ROOT/web/dist/index.html" ]; then
    echo "local-gpu-up: building web bundle" >&2
    (cd "$ROOT" && make web)
  fi
}

ensure_profile() {
  if "$PROFILE_BIN" list --profiles-root "$PROFILES_ROOT" 2>/dev/null | grep -qx "$PROFILE"; then
    return 0
  fi
  if [ -n "${MEMORY_PROFILE_PASSWORD:-}" ]; then
    "$PROFILE_BIN" create -name "$PROFILE" --profiles-root "$PROFILES_ROOT" >/dev/null
    echo "local-gpu-up: created profile $PROFILE" >&2
    return 0
  fi
  die "profile $PROFILE not found under $PROFILES_ROOT and MEMORY_PROFILE_PASSWORD is not set"
}

run_probe() {
  local out="$ARTIFACTS_DIR/probe.json"
  local started ended
  started=$(date +%s%3N)
  if [ -n "$ASR_PROBE_CMD" ]; then
    # Test-only: a stub that prints the same JSON contract.
    sh -c "$ASR_PROBE_CMD" >"$out"
  else
    "${CLEAN_ENV[@]}" "$PYTHON" "$ASR_RUNNER" --backend "$BACKEND" --probe --device cuda:0 --dtype float16 \
      --load-timeout "${ASR_PROBE_TIMEOUT:-900}" >"$out"
  fi
  ended=$(date +%s%3N)
  python3 - "$out" "$((ended - started))" <<'PY'
import json, sys
payload = json.load(open(sys.argv[1], encoding="utf-8"))
elapsed = int(sys.argv[2])
print(f"local-gpu-up: ASR probe ok backend={payload['backend']} model={payload['model']}@{payload['revision']} "
      f"load={payload['model_load_ms']}ms vram_total={payload['vram_total_mb']}MiB "
      f"vram_free={payload['vram_free_mb']}MiB vram_peak={payload['vram_peak_mb']}MiB probe={elapsed}ms")
PY
}

probe_value() {
  python3 - "$ARTIFACTS_DIR/probe.json" "$1" <<'PY'
import json, sys
print(json.load(open(sys.argv[1], encoding="utf-8")).get(sys.argv[2], ""))
PY
}

EMBED_PID=
STACK_PID=
SHUTDOWN_DONE=0

stop_embedding() {
  [ -n "$EMBED_PID" ] || return 0
  kill -TERM "$EMBED_PID" 2>/dev/null || true
  wait "$EMBED_PID" 2>/dev/null || true
  EMBED_PID=
}

stop_stack() {
  [ -n "$STACK_PID" ] || return 0
  kill -TERM "$STACK_PID" 2>/dev/null || true
  local waited=0
  while kill -0 "$STACK_PID" 2>/dev/null && [ "$waited" -lt 120 ]; do
    sleep 0.25
    waited=$((waited + 1))
  done
  if kill -0 "$STACK_PID" 2>/dev/null; then
    # Escalate to the whole session: the wrapper's children must not survive
    # as orphans holding the profile DB or the web port.
    kill -KILL -- "-$STACK_PID" 2>/dev/null || kill -KILL "$STACK_PID" 2>/dev/null || true
  fi
  wait "$STACK_PID" 2>/dev/null || true
  STACK_PID=
}

cleanup() {
  [ "$SHUTDOWN_DONE" = 1 ] && return 0
  SHUTDOWN_DONE=1
  stop_stack
  stop_embedding
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

start_embedding() {
  embedding_enabled || return 0
  local port=${EMBED_PORT:-8790}
  local device=${EMBED_DEVICE:-cpu}
  local revision
  revision=$(pin embed_qwen revision)
  local args=(--serve --host 127.0.0.1 --port "$port" --model Qwen/Qwen3-Embedding-0.6B
              --revision "$revision" --device "$device" --dtype auto --preload --verify-model)
  if [ "$device" != cpu ]; then
    args+=(--gpu-lock "$PROFILES_ROOT/$PROFILE/gpu.lock")
  fi
  if [ -n "$EMBED_CMD" ]; then
    sh -c "$EMBED_CMD" &
  else
    "${CLEAN_ENV[@]}" "$PYTHON" "$ROOT/scripts/local-embed.py" "${args[@]}" &
  fi
  EMBED_PID=$!
  local deadline=$((SECONDS + HEALTH_TIMEOUT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if ! kill -0 "$EMBED_PID" 2>/dev/null; then die "embedding server exited during startup"; fi
    if [ -n "$EMBED_HEALTH_CMD" ]; then
      sh -c "$EMBED_HEALTH_CMD" >/dev/null 2>&1
      healthy=$?
    else
      curl -fsS --noproxy '*' --connect-timeout 2 --max-time 5 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1
      healthy=$?
    fi
    if [ "$healthy" -eq 0 ]; then
      echo "local-gpu-up: embedding server ready on 127.0.0.1:$port (device=$device)" >&2
      export EMBED_PROVIDER=local
      export EMBED_ENDPOINT="http://127.0.0.1:$port/v1/embeddings"
      export EMBED_MODEL=Qwen/Qwen3-Embedding-0.6B
      export EMBED_DIMS=1024
      export EMBED_KEY_ENV=
      export SEMANTIC_PROVIDER=openai
      export SEMANTIC_ENDPOINT="$EMBED_ENDPOINT"
      export SEMANTIC_MODEL="$EMBED_MODEL"
      export SEMANTIC_DIMS="$EMBED_DIMS"
      export SEMANTIC_KEY_ENV=
      return 0
    fi
    sleep 0.25
  done
  die "embedding server did not become healthy"
}

health_ok() {
  if [ -n "$HEALTH_CMD" ]; then
    sh -c "$HEALTH_CMD" >/dev/null 2>&1 || return 1
  else
    curl -fsS --noproxy '*' --connect-timeout 2 --max-time 5 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 || return 1
  fi
  if [ -n "$WORKER_HEALTH_CMD" ]; then
    sh -c "$WORKER_HEALTH_CMD" >/dev/null 2>&1 || return 1
  else
    curl -fsS --noproxy '*' --connect-timeout 2 --max-time 5 "http://127.0.0.1:$WORKER_HEALTH_PORT/healthz" >/dev/null 2>&1 || return 1
  fi
  return 0
}

start_stack() {
  local started
  started=$(date +%s%3N)
  ASR_PROVIDER="$BACKEND" ASR_BACKEND="$BACKEND" \
  ASR_PYTHON="$PYTHON" ASR_SCRIPT="$ASR_RUNNER" ASR_DEVICE=cuda:0 ASR_DTYPE=float16 \
  ASR_TIMEOUT=2h ASR_LOAD_TIMEOUT=5m ASR_WINDOW_TIMEOUT=5m \
  ASR_LOCK="$PROFILES_ROOT/$PROFILE/gpu.lock" ASR_LOCK_TIMEOUT=10m \
  WORKER_HEALTH_PORT="$WORKER_HEALTH_PORT" \
    setsid "$START_SCRIPT" "$PROFILE" "$PORT" "$PROFILES_ROOT" &
  STACK_PID=$!
  local deadline=$((SECONDS + HEALTH_TIMEOUT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if ! kill -0 "$STACK_PID" 2>/dev/null; then
      wait "$STACK_PID" 2>/dev/null || true
      STACK_PID=
      die "web/worker exited before becoming healthy"
    fi
    if health_ok; then
      local now
      now=$(date +%s%3N)
      COLD_START_MS=$((now - started))
      return 0
    fi
    sleep 0.25
  done
  die "stack did not become healthy within ${HEALTH_TIMEOUT}s"
}

write_artifacts() {
  python3 - "$ARTIFACTS_DIR/startup.json" "$PROFILE" "$PORT" "$BACKEND" <<PY
import json, sys
payload = {
    "profile": sys.argv[2],
    "port": int(sys.argv[3]),
    "backend": sys.argv[4],
    "cycles": [json.loads(line) for line in """$CYCLES_JSON""".strip().splitlines() if line.strip()],
}
json.dump(payload, open(sys.argv[1], "w", encoding="utf-8"), ensure_ascii=False, indent=2)
PY
}

maybe_build
preflight
ensure_profile
run_probe
if [ "$CHECK" = 1 ]; then
  echo "local-gpu-up: check passed (probe only)"
  exit 0
fi

cycle_json() {
  python3 - "$1" "$COLD_START_MS" "$(probe_value model_load_ms)" "$(probe_value vram_peak_mb)" "$2" <<'CYCLE'
import json, sys
print(json.dumps({
    "cycle": int(sys.argv[1]),
    "cold_start_ms": int(sys.argv[2]),
    "model_load_ms": int(sys.argv[3] or 0),
    "vram_peak_mb": int(sys.argv[4] or 0),
    "shutdown_ms": int(sys.argv[5]),
}))
CYCLE
}

CYCLES_JSON=""
finish_launcher() {
  local shutdown_started shutdown_ms
  shutdown_started=$(date +%s%3N)
  stop_stack
  stop_embedding
  shutdown_ms=$(( $(date +%s%3N) - shutdown_started ))
  if [ "$CYCLES" -eq 1 ] && [ -n "$CYCLES_JSON" ]; then
    CYCLES_JSON=$(cycle_json 1 "$shutdown_ms")
  fi
  write_artifacts
  SHUTDOWN_DONE=1
  echo "local-gpu-up: stopped (artifacts: $ARTIFACTS_DIR/startup.json)"
  if [ "$CYCLES" -eq 1 ]; then
    exit 0
  fi
  # Multi-cycle mode is an acceptance run: an interrupt before every cycle
  # completed must not look like success.
  completed=$(python3 - "$ARTIFACTS_DIR/startup.json" <<'COUNT'
import json, sys
try:
    print(len(json.load(open(sys.argv[1], encoding="utf-8")).get("cycles") or []))
except Exception:
    print(0)
COUNT
)
  if [ "$completed" -lt "$CYCLES" ]; then
    echo "local-gpu-up: interrupted after $completed/$CYCLES cycles" >&2
    exit 130
  fi
  exit 0
}
trap finish_launcher INT TERM

start_embedding
if [ "$CYCLES" -eq 1 ]; then
  # One command, one long-lived stack: stay in the foreground until TERM/INT so
  # the caller can use the advertised address.
  start_stack
  echo "local-gpu-up: cycle 1/1 ready in ${COLD_START_MS}ms"
  CYCLES_JSON=$(cycle_json 1 0)
  echo "Recording-MCP ready: http://127.0.0.1:$PORT"
  echo "profile: $PROFILE"
  while true; do sleep 1; done
fi
cycle=1
while [ "$cycle" -le "$CYCLES" ]; do
  start_stack
  echo "local-gpu-up: cycle $cycle/$CYCLES ready in ${COLD_START_MS}ms"
  shutdown_started=$(date +%s%3N)
  stop_stack
  shutdown_ms=$(( $(date +%s%3N) - shutdown_started ))
  CYCLES_JSON="${CYCLES_JSON}$(cycle_json "$cycle" "$shutdown_ms")
"
  cycle=$((cycle + 1))
done
stop_embedding
write_artifacts
SHUTDOWN_DONE=1
echo "Recording-MCP ready: http://127.0.0.1:$PORT"
echo "profile: $PROFILE"
echo "artifacts: $ARTIFACTS_DIR/startup.json"
