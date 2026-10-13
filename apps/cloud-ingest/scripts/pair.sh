#!/bin/sh
# 本地开发辅助：向运行中的 worker 申请一次性配对码（管理面）。
# 用法：npm run pair [-- owner]；服务地址用 PAIR_SERVER 覆盖，token 优先取环境变量。
set -eu

DIR=$(cd "$(dirname "$0")/.." && pwd)
SERVER="${PAIR_SERVER:-http://127.0.0.1:8787}"
OWNER="${1:-default}"

if [ -z "${ADMIN_TOKEN:-}" ] && [ -f "${DIR}/.dev.vars" ]; then
  ADMIN_TOKEN=$(sed -n 's/^ADMIN_TOKEN=//p' "${DIR}/.dev.vars" | head -n 1)
fi
if [ -z "${ADMIN_TOKEN:-}" ]; then
  echo "ADMIN_TOKEN is not set and ${DIR}/.dev.vars is missing" >&2
  exit 1
fi

curl -sS -X POST "${SERVER}/v1/admin/pairing-codes" \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "X-Ingest-Version: ingest-v1" \
  -H "Content-Type: application/json" \
  -d "{\"owner\":\"${OWNER}\"}"
echo
