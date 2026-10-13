# 交付报告：android-cloud-sync-v1

日期：2026-10-14 · 分支 `feat/android-cloud-sync`（基于 main `2febf1c`，共 10 个分阶段 commit，未 push）· 计划：`local://android-cloud-sync-v1-plan.md`

只记录真实执行过的命令与观察到的结果。"本地 Wrangler 通过"≠"真实 Cloudflare 已部署"；"fixture ASR 通过"≠"真实识别合格"；"APK 构建通过"≠"真机行为已验证"。

## PASS（已验证）

| # | 验证项 | 命令 | 结果 |
| --- | --- | --- | --- |
| 1 | Go 全量回归 | `go build ./... && go vet ./... && go test -race -count=1 ./...` | 全部包 ok（含新增 `internal/cloudsync`、v13 后的 `internal/migrations`） |
| 2 | 构建 | `make build` | 13 个二进制，含 `bin/context-sync`、`bin/record-sim` |
| 3 | hermetic 演示 | `make demo` | PASS（data/demo，与 e2e 隔离） |
| 4 | 契约向量 | `go run ./cmd/ingest-vectors`（连续两次） | 11 个文件逐字节一致；python 解码 base64 复算 sha256 与 `content_sha256`/`chunk_meta_ok` 一致；`openapi.yaml` 经 `yaml.safe_load` 解析成功 |
| 5 | 向量漂移门 | `go test ./internal/cloudsync/ -run TestVectors`（含于 #1） | `ingestvectors.Build()` 与已提交文件逐字节相等 |
| 6 | Worker 单测 | `npm run typecheck && npm test --prefix apps/cloud-ingest`（实际 `cd apps/cloud-ingest`） | tsc 0 错误；7 文件 **74/74 通过**（修复后连续多轮全绿） |
| 7 | 本地云端冒烟 | `npx wrangler d1 migrations apply --local` + `wrangler dev` + curl | 迁移 0001 OK；`/healthz` → `{"status":"ok","version":"ingest-v1"}`；admin 签发配对码/sync token 成功 |
| 8 | e2e 全场景 | `sh scripts/e2e-cloud.sh` | **10/10 场景 PASS**（详见下） |
| 9 | 场景 F 隔离 | 同上（normal 场景内 4 条断言） | 跨 owner 404 / 设备 token 调管理面 403 `forbidden_scope` / sync token PUT 403 / 撤销后 401 `token_revoked` |
| 10 | Android 单测+构建 | `cd apps/android && ANDROID_HOME=$HOME/Android/Sdk ./gradlew :app:testDebugUnitTest :app:assembleDebug` | **34/34**；APK `app/build/outputs/apk/debug/app-debug.apk` 12,896,644 B，sha256 `d4e702811e0e32e69dfa3262729ba4edc3c8a078e487f90e71a18d56f4f49ff9` |
| 11 | Android-契约一致性 | `PcmChunkWriterTest`/`IngestApiTest`（含于 #10） | 分块字节与 `vectors/pcm_1s_440hz.json` 一致；`X-Chunk-Meta` 与 `chunk_meta_ok.json` 字面量一致 |
| 12 | 最终验收目标 | #8 的 `normal` 场景 | 模拟录音 → 本地 Cloudflare 持久化 → `context-sync` 下载校验 → fixture ASR → 真实 `IngestTranscript` → pipeline drain → MCP `search_memory` 逐字命中 + `get_evidence_context` 非 stale + `indexing_status=ready` |

口径说明（诚实声明）：e2e 的 pipeline drain 按计划使用 `--generator synthetic --allow-synthetic`（与 `make demo` 同口径）——extract/publish/curate/embed 各阶段的执行、任务队列、索引与检索路径是真实代码，但候选记忆的生成文本是确定性占位（非真实模型提炼；真实 `GENERATOR_API_KEY` 路径本轮 NOT_RUN）。原话（fixture 转写文本）的摄入、证据绑定与逐字检索不受此影响。

### e2e 场景明细（每场景独立 miniflare，日志在 `data/e2e/<s>/`）

| 场景 | 关键观测 |
| --- | --- |
| normal | 4/4 块、`state=complete`、67 jobs 排空、MCP 证据命中 |
| offline-recover | 4/4 块零丢失、duplicates=0 |
| crash-restart | 首跑 exit 1（3 块 durable）、续跑补齐、spool 清空 |
| duplicate | `duplicates=1` 且云端 chunk 行数 == total_chunks（事件唯一索引生效） |
| bad-checksum | 观察码 `[validation_failed, conflict_checksum]`，最终仍 complete |
| gap | `409 sequence_gap`、`missing_sequences=[2]`、`state=recording`（永不 complete） |
| r2-after-write / d1-manifest / d1-event / response-drop | 500 注入后重试收敛 complete；`/v1/sync` 断言事件 `(type,recording_id,seq)` 无重复 |

## 集成期发现并已修复（原 FAIL，现为 PASS）

1. **管理面角色越权返回 401**：有效设备 token 调 admin 路由应为 `403 forbidden_scope`（契约 §3）。修复 `apps/cloud-ingest` admin 门（valid token→403，revoked→`token_revoked`，未知→401），补 3 个测试。commit `c58cfe8`。
2. **e2e 排空上限过低**：`RunOnce` 每进程恰好处理 1 个 job，24-turn fixture 需 ~65 jobs，30 轮必超。上限提到 300。commit `9a2431f`。
3. **workerd 孤儿占端口导致场景串数据**：dash 内建 `kill` 不认负 pid，进程组杀不掉；下一场景的 `wrangler dev` 绑定失败而 healthz 由残留 server 应答，normal 的数据泄入后续场景库。改为 `setsid` + `/bin/kill -- -PGID` + 按端口 pkill 兜底 + 每场景启动前端口占用即 fail。commit `bcbf82d`。
4. **verify 会话匹配脆弱**：`list_conversations` 的 `recording_id` 是内部哈希形式，多会话库匹配失败。补 title 匹配（`Recording <id>`）。
5. **gap 场景误跑 MCP verify**：gap 按契约永不 complete → 无转写 → 无可检索内容。verify 对 gap 条件跳过（脚本注明"录音未完成属预期"）。

## NOT_RUN（范围决定，非缺陷）

| 项 | 原因 |
| --- | --- |
| Scenario H：真实 Cloudflare 部署（`wrangler deploy`/远端 D1/R2/secrets/cron） | 用户决定本轮仅本地 wrangler miniflare |
| Scenario G：真机后台/锁屏/断网/进程死亡 | 无设备（`adb devices` 为空）；Android 验收=构建+JVM 单测。手工步骤已写入 `docs/android.md` |
| 真实豆包 ASR 识别质量 | 用户决定 e2e 仅用 fixture ASR；`--asr doubao` 未执行，零费用；`DoubaoTranscriber` 的帧过滤/envelope 组装有单测覆盖 |
| CI 四 job 实际运行 | 需 push；`.github/workflows/ci.yml`（test + cloud-ingest + android + e2e-cloud）已提交并通过 YAML 解析 |
| 生产备份/恢复演练（`wrangler d1 export` 等） | 依赖真实云端；流程写入 `docs/cloud-deploy.md` |

## BLOCKED

无。没有遗留阻塞项。

## 实现偏差（相对计划，均有因）

- **Android**：AGP 9.4.0 内置 Kotlin（独立 `org.jetbrains.kotlin.android` 插件被移除）；KSP 2.3.0→2.3.4；compileSdk 36→37（Compose ui 1.12.1 要求，targetSdk 仍 36）。JDK 26 + Gradle 9.8.1 直接可用，未启用 JDK 21 回退。详见 `docs/android.md`。
- **Go sync**：契约事件 payload 无 owner/device_id 字段 → 引擎接受可选回显并回退 `default`/`--device-id`；`sync_chunks` 无 sample_count 列 → 完备性用 `bytes/2`（服务端 durable-ACK 已保证 bytes==sample_count*2）；新增 `transcription_pending` 补转写扫描（`--asr off` 后重跑 `--asr fixture` 的唯一实现路径）；校验和失配记 `failed` 并放行后续事件（不堵塞流）。
- **e2e**：`X-Debug-Fault` 仅随每块首次 PUT 发送（重试不带头），否则 500 注入永不收敛；场景 F 设备 B 用不同 owner 配对（同 owner 共享 recording 属合法行为）；gap 场景 `coverage_ok=false` 为设计使然。
- **文档语言**：沿用仓库现状（中文叙述 + 英文标识符），未执行旧蓝图的"英文 README 为主"。

## 评审轮（2026-10-14 自查 + 外部意见过滤后采纳项）

- **契约补钉**：已删除 recording 的后续语义此前未冻结——实现为 `PUT chunk`/`finish`/音频下载 → `404 recording_not_found`，`POST /v1/recordings` 同元数据重放 → `200 duplicate:true` 且 `state=deleted`（不复活、无新事件），详情返回 tombstone。已写入 `contracts/ingest-v1/README.md` §5 幂等矩阵，并新增 2 个测试锁定（`chunks.test.ts`，含 `eventCount` 断言无复活/无 finished 事件）。
- **gap 场景加严**：e2e 新增断言 `turns==0`（录音未 complete 时 context-sync 不得有任何转写入库），实测通过。
- **报告修正**：commit 计数 9→10；补充"e2e pipeline 使用 `--generator synthetic --allow-synthetic`（计划规定、与 demo 同口径，生成文本为占位）"的口径声明。
- 评审轮后复跑：`npm test` 74/74；`sh scripts/e2e-cloud.sh` 10/10 PASS（gap 新断言生效）；无残留 server 进程。

## 交付物

- `contracts/ingest-v1/`（README 语义权威 + OpenAPI 3.1 + 11 个确定性向量 + 生成器 `cmd/ingest-vectors`）
- `apps/cloud-ingest/`（Worker + D1 0001_init.sql + R2 + 74 测试）
- `internal/cloudsync/`（client/state/sync/asr + 4 个测试文件）+ `cmd/context-sync`（17 个 flags）+ migration v13
- `cmd/record-sim`（7 场景 + 4 故障变体）+ `scripts/e2e-cloud.sh` + `scripts/e2e-verify.py` + Makefile `e2e-cloud` + CI 3 个新 job
- `apps/android/`（Compose 三页 + FGS + 4 类 Worker + Room + 34 单测 + Gradle wrapper 9.8.1）
- 文档：`docs/android.md`、`docs/cloud-deploy.md`、`docs/end-to-end.md`、README 跨设备章节、`.env.example`
