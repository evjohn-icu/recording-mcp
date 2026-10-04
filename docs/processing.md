# 处理链路现状

本文描述当前已实现的处理链路：接收统一 ASR 结果、原子入队处理任务、Worker 领取并在租约保护下执行，以及决策判断的持久记录。对应[开发执行计划](development-plan.md)中的 A（存储与任务）与 B（决策服务与执行器）切片；候选记忆、发布与检索工具尚未实现，文末有对照表。事实以代码为准，主要入口是 `internal/memory/ingestion.go`、`internal/process`、`internal/worker`、`internal/pipeline`、`internal/decision` 与 `cmd/context-worker`。

当前决策处于 **shadow 模式**：Worker 对每个分析窗口调用 Decision 引擎并把问题和回答完整写入 `decision_runs` / `decision_answers`，但**不依据判断结果修改注释**，也不跳过任何基线提炼——注释发布仍走既有的 `apply_annotations` 显式提交路径。这是为了在启用路由前先积累判断与用量数据。

## 处理链路

```text
供应商响应 → Adapter → asr-input/0.1
  → IngestTranscript（单事务）：
      turns / attachment_turns / source_attachments 入库
      + process.EnqueueTx 创建 extract 任务
  → context-worker（process.Claim 领取，随机租约令牌）
  → pipeline.ExtractHandler：
      LoadConversationTurns → BuildWindows → RenderState
      → decision.Engine.Evaluate → RecordDecisionRun
  → decision_runs / decision_answers 落库（shadow，不发布）
```

- **接收与入队原子**：`IngestTranscript` 在同一数据库事务内写入原话、来源附件并调用 `process.EnqueueTx`，来源保存和任务创建不会半途分离。重复导入相同分片时，附件和任务都幂等（`INSERT OR IGNORE`），返回已存在的任务 ID，不会产生重复业务任务。
- **任务标识**：任务 ID 为 `"job:" + sha256(幂等键)`；extract 幂等键由附件 ID 与处理器版本派生，任务 payload 携带 `conversation_id`、`recording_id`、`segment_id`、`attachment_id`。
- **执行器**：`internal/worker` 不含业务逻辑，只负责领取、续租和收尾；`worker.Executor` 接口返回结果引用。执行器失败会作为可重试错误交回队列，直到尝试次数耗尽。

## 表结构摘要

迁移集中在 `internal/migrations`（当前版本 3），时间列为 unix 毫秒 INTEGER。

### processing_jobs（迁移 v2 processing-queue）

| 列 | 说明 |
| --- | --- |
| `id` | 主键，`job:<hash>` |
| `stage` | 阶段名，当前只有 `extract` |
| `input_snapshot_id` | 不可变输入快照（附件）ID |
| `idempotency_key` | 唯一；幂等键决定任务 ID |
| `status` | `queued / running / succeeded / retry_wait / failed / cancelled`（`cancelled` 仅保留在约束中，当前无取消路径） |
| `priority`、`payload_json` | 领取排序按 `priority DESC, available_at ASC, created_at ASC` |
| `max_attempts` / `attempt_count` | 默认 3 次 |
| `available_at` | 下次可领取时间（重试退避在此之后） |
| `lease_token` / `lease_expires_at` / `worker_id` | 当前租约；未持有时为 NULL |

### processing_attempts（迁移 v2）

每次执行一行：`id`（`attempt:<hash>`，`(job_id, attempt)` 唯一）、`worker_id`、`lease_token`、`processor_version`、`started_at`、`finished_at`、`outcome`（`running / succeeded / retryable_error / fatal_error / lease_lost / cancelled`）、`error_json`、`result_ref`、`usage_json`。领取消耗一次 `attempt_count`。

### decision_runs / decision_answers（迁移 v3 decision-log）

| 表 | 列 |
| --- | --- |
| `decision_runs` | `id`（主键）、`job_id`（可空）、`attempt`、`input_snapshot_id`、`input_hash`、`model`、`weights_version`、`endpoint`、`questions_json`、`usage_json`、`created_at` |
| `decision_answers` | `run_id`、`question_id`、`question_type`、`answer_json`、`distribution_json`，主键 `(run_id, question_id)` |

`RecordDecisionRun` 的 run ID 是确定性的 `"run:" + sha256(job_id|attempt|input_hash)[:32]`：run 行 `INSERT OR IGNORE`，回答行 `INSERT OR REPLACE`，因此同一 (job, attempt, input) 重放保留首次 run 并以最新回答覆盖，run 与回答在同一事务写入。

## 状态机与租约语义

```text
queued → running → succeeded
   ↑         │
   │         ├─ 可重试失败（attempt_count < max_attempts）→ retry_wait →（available_at 到期再领取）
   │         └─ 尝试耗尽或不可修复 → failed
   └─ retry_wait 到期后可再次进入 running
```

- **领取（Claim）**：单个事务内先回收过期租约——过期 running 的 attempt 记为 `lease_lost`；`attempt_count >= max_attempts` 的任务转入 `failed`，其余转回 `retry_wait`；然后按优先级选出下一个 `queued/retry_wait` 且 `available_at` 到期的任务，条件更新独占并生成新的随机租约令牌，同时写入一条 `running` attempt。丢失条件的领取返回无任务。
- **续租（Renew）与收尾（Complete/Fail）**：全部校验当前租约令牌，失去令牌的操作返回 `ErrLeaseLost` 且不会改动任务。失去租约的旧 Worker 无权提交结果。
- **心跳**：`internal/worker` 以租约的 1/3 为间隔调用 `Renew`；若续租发现租约已被抢走，会取消执行上下文并且不提交任何结果，任务由接管方继续。
- **重试退避**：`RetryBackoff(attempt) = 5s × 2^(attempt-1)`，上限 60 秒，写入 `available_at`。
- **失败分类**：当前 Worker 把执行器返回的所有错误按可重试处理，错误详情记入 attempt 的 `error_json`；尝试耗尽后任务转为 `failed`。即使进程被关闭信号打断，收尾也会记录已完成或已失败的一次尝试，而不是等租约自然过期。

## 命令示例

```sh
make build

# 接收：适配器输出统一格式，导入原子入库并入队 extract 任务
./bin/adapter-doubao --input ./examples/asr/doubao-final.jsonl \
  --metadata ./examples/asr/doubao-metadata.json --output ./data/normalized.json
./bin/import-transcript --db ./data/context-memory.db --input ./data/normalized.json

# 合成执行器处理一次（默认 --executor synthetic）
./bin/context-worker --db ./data/context-memory.db --once

# shadow 决策执行器：调用本地 Decision 服务并记录判断
./bin/context-worker --db ./data/context-memory.db \
  --executor pipeline --decision-url http://127.0.0.1:8765/v1/systemone \
  --decision-model jev-style-2b-v2
```

`context-worker` 的完整参数：`--db`（必填）、`--worker-id`（默认 主机名+随机后缀）、`--stages`（默认 `extract`）、`--lease`（默认 30s）、`--poll`（默认 1s）、`--once`、`--executor`（`synthetic|pipeline`，默认 `synthetic`）、`--decision-url`（默认 `http://127.0.0.1:8765/v1/systemone`）、`--decision-model`（默认 `jev-style-2b-v2`）、`--synthetic-delay`。启动时会先执行 `migrations.Apply`。本地 Decision 服务未部署时，`--executor pipeline` 的任务会失败并按可重试错误留在队列中。

## 与开发计划的对应

开发计划以工作包 A–F 划分（见[开发执行计划](development-plan.md)），当前进度：

| 工作包 | 已完成 | 未完成 |
| --- | --- | --- |
| A 存储与任务 | `internal/migrations` v1–v3；`IngestTranscript` 与 `process.EnqueueTx` 原子提交；`Claim/Renew/Complete/Fail` 租约与幂等；重复导入幂等 | outbox 形式的跨命令任务、`analysis_coverage`、`processing_windows` 落表 |
| B 决策服务与执行器 | `internal/decision` Engine 契约（choice/noul/score，完整分布校验，非法结果整体拒绝）；Synthetic 与本地 `SystemOneClient` 两个 Adapter；`worker.Run/RunOnce`；`pipeline.ExtractHandler` shadow 记录 | 本地权重实际部署与加载、三个小模型对比、TypeSafe 托管 Adapter、路由启用 |
| C 提炼与发布 | —（shadow 判断已采集，作为前置） | 候选记忆（`memory_candidates`）、证据校验、会话级发布器 |
| D 层级检索与 MCP | — | `list_conversations` / `get_evidence_context` / `search_memory` / `build_context` 三个首批工具与层级视图 |
| E/F | — | 人物绑定、话题关联、修订失效、模型评测 |

计划中的 Processing 层判断记录（`decision_runs` / `decision_answers`）已按拟议接口落地；窗口表尚未独立建表，窗口构建在 `internal/pipeline` 内存中完成（v1：默认单窗口，超过 200 轮按顺序切块；渲染超过 8000 字符截断并标注）。确定性保证：原话顺序为"有 `global_start_ms` 的按升序在前，再按 `segment_index`，再按 `original_turn_id`"，缺失时间仍有确定的来源序；输入哈希由 Decision 契约对 state 与问题集计算，缓存键与运行 ID 都可复现。

后续启用路由前需完成：shadow 数据评测、阈值校准（计划要求 ≥98% 关键窗口保留召回）、候选与发布链路。届时本文档应更新为描述实际启用状态。
