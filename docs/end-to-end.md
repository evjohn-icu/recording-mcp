# 端到端场景与验证（ingest-v1 链路）

一键回归：`make e2e-cloud`（即 `sh scripts/e2e-cloud.sh`）。脚本对每个场景起独立 `wrangler dev`（本地 miniflare，`--persist-to` 隔离状态），跑完即杀进程；日志与产物在 `data/e2e/<scenario>/`。端口可用 `PORT=8788` 覆盖。

场景编排：`record-sim`（模拟录音 + 契约验证）→ `context-sync --asr fixture`（拉取 + 校验 + 转写入库）→ `context-worker`（extract/publish/curate/embed）→ `e2e-verify.py`（MCP stdio 断言检索命中）。

## 场景矩阵

| 场景 | record-sim 参数 | 断言要点 |
| --- | --- | --- |
| normal | 默认 | 4/4 块上传、`coverage_ok=true`、finish `complete`；MCP `search_memory` 命中 `fixture transcript recording=<id> segment=00`、证据非 stale、`indexing_status=ready` |
| offline-recover | `--offline-for 20s` | 先落 spool 不上传，恢复后补齐，零丢失（`chunks_uploaded == chunks_generated`） |
| crash-restart | `--crash-after-chunks 3` | 首跑 `exit 1` 后续跑续传，不重复上传已提交块，spool 清空 |
| duplicate | 正常后重传首块 | `duplicates ≥ 1` 且云端 chunk 行数 == `total_chunks`（无重复记录） |
| bad-checksum | 错误 sha + 异内容重放 | 观察码 `[validation_failed, conflict_checksum]` |
| gap | 跳过 seq 2 | finish `409 sequence_gap`、`missing_sequences==[2]`、录音 `state=recording`、无转写入库（`turns==0`，MCP verify 按设计跳过） |
| r2-after-write 等 4 种 fault | `--scenario normal --fault <s>` | 500 后重试收敛到 `complete`；`/v1/sync` 事件无 `(type,recording_id,seq)` 重复 |

隔离检查（normal 场景内执行，"场景 F"）：设备 B 跨 owner 读 A 的录音 → `404`；设备 token 调管理面 → `403 forbidden_scope`；sync token 上传 chunk → `403`；撤销后设备 token → `401 token_revoked`。

## 单独运行

```sh
# 模拟器（契约客户端验证器）
bin/record-sim --server http://127.0.0.1:8787 --pair-code <CODE> \
  --duration 120s --chunk-seconds 30 --scenario normal --report-json /tmp/sim.json
cat /tmp/sim.json   # recording_id / chunks_uploaded / expected_error_codes / coverage_ok

# MCP 检索断言（对任意已入库 db）
python3 scripts/e2e-verify.py --db data/e2e/normal/context-memory.db \
  --recording-id <id> --expect-phrase "fixture transcript recording=<id> segment=00"
```

向量漂移检查：`go test ./internal/cloudsync/ -run TestVectors`（重新生成比对 `contracts/ingest-v1/vectors/`）与 `apps/cloud-ingest` 内 `vectors.test.ts`（解码 base64 重算 sha256 + 错误码全表比对）。

## 口径（诚实声明）

- 本文档全部验证基于 **本地 wrangler miniflare**；真实 Cloudflare 云端部署未执行（NOT_RUN）。
- e2e 转写使用 `--asr fixture`（确定性离线文本）；真实豆包 ASR 识别质量未验证（NOT_RUN，需 `VOLC_API_KEY`）。
- Android 仅构建 + JVM 单测；真机后台/锁屏/断网行为未验证（NOT_RUN）。
