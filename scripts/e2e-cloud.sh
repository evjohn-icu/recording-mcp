#!/bin/sh
# e2e-cloud：Android 录音 → ingest-v1（本地 wrangler dev + D1 + R2）→
# context-sync（fixture ASR）→ pipeline drain → MCP 检索 的全场景验收编排。
# 无 Cloudflare 账号、无真实 ASR key；每个场景使用独立 wrangler 状态目录。
# 用法：sh scripts/e2e-cloud.sh   （PORT 环境变量可覆盖默认 8787）
set -eu

cd "$(dirname "$0")/.."
ROOT=$(pwd)

PORT="${PORT:-8787}"
SERVER="http://127.0.0.1:$PORT"
ADMIN_TOKEN="e2e-admin"
E2E_ROOT=data/e2e
SCENARIOS="normal offline-recover crash-restart duplicate bad-checksum gap r2-after-write d1-manifest d1-event response-drop"

WPID=""

stop_wrangler() {
	# wrangler(node) 派生 workerd 子进程；必须整组终止，否则 workerd 孤儿占端口。
	# dash 内建 kill 不认负 pid，必须用外部 /bin/kill（procps/uutils 均支持 -- -PGID）。
	[ -n "$WPID" ] || return 0
	/bin/kill -TERM -- "-$WPID" 2>/dev/null || kill "$WPID" 2>/dev/null || true
	wait "$WPID" 2>/dev/null || true
	# 双保险：按 entry 端口清掉可能残留的 workerd
	pkill -f "socket-addr=entry=localhost:$PORT" 2>/dev/null || true
	/bin/kill -KILL -- "-$WPID" 2>/dev/null || true
	WPID=""
}

cleanup() {
	stop_wrangler
}
trap cleanup EXIT INT TERM

fail() {
	echo "e2e-cloud: FAIL $*" >&2
	exit 1
}

jget() { # jget <python 取值表达式>：从 stdin JSON 提取字段
	python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"
}

code_of() { # code_of <curl 参数...>：只打印 HTTP 状态码
	curl -s -o /dev/null -w '%{http_code}' "$@"
}

admin_post() { # admin_post <path> <body>：管理面 POST，输出响应体
	curl -sf -X POST "$SERVER$1" \
		-H "Authorization: Bearer $ADMIN_TOKEN" \
		-H "X-Ingest-Version: ingest-v1" \
		-H "Content-Type: application/json" \
		-d "$2"
}

echo "== build =="
make build
for b in record-sim context-sync context-worker context-memory; do
	[ -x "bin/$b" ] || fail "bin/$b 不存在（make build 失败？）"
done

if curl -s -o /dev/null --max-time 2 "$SERVER/healthz" 2>/dev/null; then
	fail "端口 $PORT 已有服务响应（可能是残留的 wrangler dev），请先释放"
fi

if [ ! -d apps/cloud-ingest/node_modules ]; then
	echo "== npm ci (apps/cloud-ingest) =="
	(cd apps/cloud-ingest && npm ci) || fail "npm ci 失败"
fi

# 本地 wrangler dev 的密钥与故障注入开关（apps/cloud-ingest/.dev.vars 不入库）
cat > apps/cloud-ingest/.dev.vars <<EOF
ADMIN_TOKEN=$ADMIN_TOKEN
PAIRING_PEPPER=e2e-pepper
ENABLE_DEBUG_FAULTS=1
EOF

# 场景 F（隔离）：第二个设备 / 不同 owner 的凭据边界。在 normal 场景内执行。
run_isolation_checks() {
	rid=$1
	echo "== [normal/F] 隔离检查 =="
	resp=$(admin_post /v1/admin/pairing-codes '{"owner":"device-b-owner","ttl_seconds":900}') ||
		fail "[F] 为设备 B 创建配对码失败"
	PC_B=$(printf '%s' "$resp" | jget 'd["pairing_code"]')
	pair_json=$(curl -sf -X POST "$SERVER/v1/devices/pair" \
		-H "X-Ingest-Version: ingest-v1" -H "Content-Type: application/json" \
		-d "{\"protocol_version\":\"ingest-v1\",\"pairing_code\":\"$PC_B\",\"device_name\":\"device-b\"}") ||
		fail "[F] 设备 B 配对失败"
	B_TOKEN=$(printf '%s' "$pair_json" | jget 'd["device_token"]')
	B_ID=$(printf '%s' "$pair_json" | jget 'd["device_id"]')

	c=$(code_of -X GET "$SERVER/v1/recordings/$rid" \
		-H "Authorization: Bearer $B_TOKEN" -H "X-Ingest-Version: ingest-v1")
	[ "$c" = "404" ] || fail "[F] 设备 B 读其他 owner 录音期望 404，实际 $c"

	c=$(code_of -X POST "$SERVER/v1/admin/pairing-codes" \
		-H "Authorization: Bearer $B_TOKEN" -H "X-Ingest-Version: ingest-v1" \
		-H "Content-Type: application/json" -d '{"owner":"device-b-owner"}')
	[ "$c" = "403" ] || fail "[F] 设备 token 调管理面期望 403，实际 $c"

	c=$(code_of -X PUT "$SERVER/v1/recordings/$rid/chunks/0" \
		-H "Authorization: Bearer $CF_SYNC_TOKEN" -H "X-Ingest-Version: ingest-v1" \
		-H "Content-Type: application/octet-stream" --data-binary 'x')
	[ "$c" = "403" ] || fail "[F] sync token PUT chunk 期望 403，实际 $c"

	c=$(code_of -X POST "$SERVER/v1/admin/devices/$B_ID/revoke" \
		-H "Authorization: Bearer $ADMIN_TOKEN" -H "X-Ingest-Version: ingest-v1")
	[ "$c" = "200" ] || fail "[F] 撤销设备 B 期望 200，实际 $c"

	c=$(code_of -X PUT "$SERVER/v1/recordings/$rid/chunks/0" \
		-H "Authorization: Bearer $B_TOKEN" -H "X-Ingest-Version: ingest-v1" \
		-H "Content-Type: application/octet-stream" --data-binary 'x')
	[ "$c" = "401" ] || fail "[F] 已撤销设备 token PUT 期望 401 token_revoked，实际 $c"

	echo "[normal/F] 隔离检查 OK：跨 owner 404 / 管理面 403 / sync PUT 403 / 撤销后 401"
}

run_scenario() {
	s=$1
	dir="$E2E_ROOT/$s"
	rm -rf "$dir"
	mkdir -p "$dir"

	echo "== [$s] 1/8 d1 migrations =="
	(cd apps/cloud-ingest && npx wrangler d1 migrations apply recording-mcp-ingest \
		--local --persist-to "$ROOT/$dir/.wrangler-state") \
		> "$dir/migrate.log" 2>&1 ||
		{ tail -n 30 "$dir/migrate.log" >&2; fail "[$s] d1 migrations 失败"; }

	echo "== [$s] 2/8 wrangler dev =="
	if curl -s -o /dev/null --max-time 2 "$SERVER/healthz" 2>/dev/null; then
		fail "[$s] 端口 $PORT 仍被占用（上一场景 server 未退出）"
	fi
	(cd apps/cloud-ingest &&
		exec setsid npx wrangler dev --port "$PORT" --persist-to "$ROOT/$dir/.wrangler-state") \
		> "$dir/wrangler.log" 2>&1 &
	WPID=$!
	ready=""
	i=0
	while [ "$i" -lt 60 ]; do
		if curl -sf "$SERVER/healthz" 2>/dev/null |
			grep -Eq '"status"[[:space:]]*:[[:space:]]*"ok"'; then
			ready=1
			break
		fi
		kill -0 "$WPID" 2>/dev/null || break
		sleep 1
		i=$((i + 1))
	done
	if [ -z "$ready" ]; then
		tail -n 40 "$dir/wrangler.log" >&2 || true
		fail "[$s] wrangler dev 60s 内未就绪（见 $dir/wrangler.log）"
	fi

	echo "== [$s] 3/8 admin 凭据 =="
	resp=$(admin_post /v1/admin/pairing-codes '{"owner":"default","ttl_seconds":900}') ||
		fail "[$s] 创建 pairing code 失败"
	PAIRING_CODE=$(printf '%s' "$resp" | jget 'd["pairing_code"]')
	resp=$(admin_post /v1/admin/sync-tokens '{"owner":"default","label":"e2e"}') ||
		fail "[$s] 创建 sync token 失败"
	CF_SYNC_TOKEN=$(printf '%s' "$resp" | jget 'd["sync_token"]')
	export CF_SYNC_TOKEN

	echo "== [$s] 4/8 record-sim =="
	scen=$s
	fault=""
	case "$s" in
	r2-after-write | d1-manifest | d1-event | response-drop)
		scen=normal
		fault=$s
		;;
	esac
	# shellcheck disable=SC2086  # 值均不含空白
	sim_flags="--server $SERVER --pair-code $PAIRING_CODE --scenario $scen \
--spool $dir/spool --report-json $dir/sim.json"
	if [ -n "$fault" ]; then
		sim_flags="$sim_flags --fault $fault"
	fi
	sim_rc=0
	# shellcheck disable=SC2086
	bin/record-sim $sim_flags >"$dir/sim.log" 2>&1 || sim_rc=$?
	if [ "$s" = crash-restart ]; then
		[ "$sim_rc" -eq 1 ] ||
			{ cat "$dir/sim.log" >&2; fail "[$s] 首次运行应因崩溃退出码 1，实际 $sim_rc"; }
		echo "   crash run 1 OK (exit 1)，续传 run 2..."
		# shellcheck disable=SC2086
		bin/record-sim $sim_flags >"$dir/sim2.log" 2>&1 ||
			{ cat "$dir/sim2.log" >&2; fail "[$s] 续传运行失败"; }
	elif [ "$sim_rc" -ne 0 ]; then
		cat "$dir/sim.log" >&2
		fail "[$s] record-sim 退出码 $sim_rc"
	fi

	DEVICE_TOKEN=$(sed -n 's/^device_token=//p' "$dir/sim.log" | tail -n 1)
	[ -n "$DEVICE_TOKEN" ] || fail "[$s] sim 输出缺少 device_token"
	printf '%s\n' "$DEVICE_TOKEN" >"$dir/device-token"
	RID=$(jget 'd["recording_id"]' <"$dir/sim.json")

	echo "== [$s] 5/8 context-sync =="
	i=0
	ev=1
	while [ "$i" -lt 20 ]; do
		if ! out=$(bin/context-sync --db "$dir/context-memory.db" --server "$SERVER" \
			--sync-token-env CF_SYNC_TOKEN --asr fixture --audio-dir "$dir/audio" --once 2>&1); then
			printf '%s\n' "$out" >>"$dir/context-sync.log"
			tail -n 20 "$dir/context-sync.log" >&2
			fail "[$s] context-sync 失败"
		fi
		printf '%s\n' "$out" >>"$dir/context-sync.log"
		ev=$(printf '%s\n' "$out" | python3 -c '
import json, sys
n = 1
for line in sys.stdin:
    line = line.strip()
    if line.startswith("{"):
        try:
            n = int(json.loads(line).get("events_processed", 0))
        except Exception:
            pass
print(n)')
		if [ "$ev" = "0" ]; then
			break
		fi
		i=$((i + 1))
	done
	[ "$ev" = "0" ] || fail "[$s] context-sync 20 轮后仍有未处理事件"

	echo "== [$s] 6/8 pipeline drain =="
	i=0
	while [ "$i" -lt 300 ]; do
		out=$(bin/context-worker --db "$dir/context-memory.db" \
			--executor pipeline --generator synthetic --allow-synthetic \
			--stages extract,publish,curate,embed --once 2>&1) || true
		case "$out" in
		*"no job available"*)
			echo "queue drained after $i job(s)"
			break
			;;
		esac
		printf '%s\n' "$out" >>"$dir/worker.log"
		i=$((i + 1))
	done
	[ "$i" -lt 300 ] || fail "[$s] worker 300 轮未排空"

	echo "== [$s] 7/8 MCP verify =="
	# gap 场景按契约永不 complete → 无转写入库 → 无可检索内容，跳过 MCP 断言
	if [ "$s" = gap ]; then
		echo "[$s] 跳过 MCP verify（录音未完成属预期）"
	elif python3 scripts/e2e-verify.py --db "$dir/context-memory.db" \
		--recording-id "$RID" \
		--expect-phrase "fixture transcript recording=$RID segment=00" \
		>"$dir/verify.log" 2>&1; then
		cat "$dir/verify.log"
	else
		cat "$dir/verify.log" >&2
		fail "[$s] MCP 验证失败"
	fi

	echo "== [$s] 8/8 断言 =="
	python3 - "$s" "$dir/sim.json" <<'PYEOF' || fail "[$s] sim.json 断言失败"
import json, sys

scenario, path = sys.argv[1], sys.argv[2]
sim = json.load(open(path))
codes = sim.get("expected_error_codes") or []
missing = sim.get("missing_sequences") or []
if scenario in ("normal", "offline-recover", "crash-restart"):
    assert sim["chunks_uploaded"] == sim["chunks_generated"], sim
    assert sim["duplicates"] == 0, sim
    assert sim["coverage_ok"] is True, sim
    assert sim["state"] == "complete", sim
elif scenario == "duplicate":
    assert sim["duplicates"] >= 1, sim
    assert sim["state"] == "complete", sim
elif scenario == "bad-checksum":
    assert codes == ["validation_failed", "conflict_checksum"], codes
    assert sim["state"] == "complete", sim
elif scenario == "gap":
    assert codes == ["sequence_gap"], codes
    assert missing == [2], missing
    assert sim["state"] == "recording", sim
elif scenario in ("r2-after-write", "d1-manifest", "d1-event", "response-drop"):
    assert sim["state"] == "complete", sim
    assert sim["chunks_uploaded"] == sim["chunks_generated"], sim
else:
    raise AssertionError("unknown scenario %s" % scenario)
print("sim.json OK: uploaded=%s/%s duplicates=%s codes=%s state=%s" % (
    sim["chunks_uploaded"], sim["chunks_generated"], sim["duplicates"],
    codes, sim["state"]))
PYEOF

	if [ "$s" = crash-restart ]; then
		if find "$dir/spool" -name '*.pcm' 2>/dev/null | grep -q .; then
			fail "[$s] 续传完成后 spool 仍有残留块文件"
		fi
		echo "spool 已清空"
	fi

	if [ "$s" = gap ]; then
		# 录音未 complete → 不得有任何转写入库（context-sync 不推进游标、不拼接、不摄入）
		turns=$(sqlite3 "$dir/context-memory.db" "select count(*) from turns" 2>/dev/null || echo 0)
		[ "$turns" = "0" ] || fail "[$s] 录音未完成却产生了 $turns 条 turns"
		echo "gap 无转写入库（turns=0，符合预期）"
	fi

	case "$s" in
	r2-after-write | d1-manifest | d1-event | response-drop)
		detail=$(curl -sf "$SERVER/v1/recordings/$RID" \
			-H "Authorization: Bearer $DEVICE_TOKEN" \
			-H "X-Ingest-Version: ingest-v1") ||
			fail "[$s] 读取云端录音详情失败"
		printf '%s' "$detail" | python3 -c '
import json, sys
d = json.load(sys.stdin)
assert d["state"] == "complete", d["state"]
assert len(d["chunks"]) == d["total_chunks"], (len(d["chunks"]), d["total_chunks"])
print("cloud detail OK: state=%s chunks=%d" % (d["state"], len(d["chunks"])))' ||
			fail "[$s] 云端录音详情断言失败"
		# 事件表无重复（经公开 sync API 观测 (type, recording_id, seq) 唯一性）
		curl -sf "$SERVER/v1/sync?cursor=0&limit=500" \
			-H "Authorization: Bearer $CF_SYNC_TOKEN" \
			-H "X-Ingest-Version: ingest-v1" | python3 -c '
import json, sys
d = json.load(sys.stdin)
keys = set()
mine = 0
for e in d["events"]:
    k = (e["type"], e["recording_id"], e.get("seq"))
    assert k not in keys, "duplicate event %r" % (k,)
    keys.add(k)
    if e["recording_id"] == sys.argv[1]:
        mine += 1
assert mine >= 3, "events for recording: %d" % mine
print("sync events OK: %d events for %s, no duplicate identity" % (mine, sys.argv[1]))' "$RID" ||
			fail "[$s] 事件表唯一性断言失败"
		;;
	esac

	if [ "$s" = normal ]; then
		run_isolation_checks "$RID"
	fi

	stop_wrangler
	n=0
	while curl -s -o /dev/null --max-time 2 "$SERVER/healthz" 2>/dev/null && [ "$n" -lt 25 ]; do
		sleep 0.2
		n=$((n + 1))
	done
}

if [ ! -d apps/cloud-ingest ]; then
	fail "apps/cloud-ingest 不存在"
fi

for s in $SCENARIOS; do
	run_scenario "$s"
	echo "PASS $s"
done
echo "e2e-cloud: 全部场景通过（$SCENARIOS）"
