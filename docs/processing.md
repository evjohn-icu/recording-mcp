# 处理链路现状

本文描述当前已实现的处理链路：接收统一 ASR 结果、原子入队处理任务、Worker 领取并在租约保护下执行、决策判断的持久记录、候选记忆的生成校验与会话级发布，以及分析覆盖记录。对应[开发执行计划](development-plan.md)中的 A（存储与任务）、B（决策服务与执行器）与 C（候选记忆与会话级发布）切片；D 切片的基础检索工具（`list_conversations` / `get_evidence_context` / `search_memory`）已落地，文末有对照表。事实以代码为准，主要入口是 `internal/memory/ingestion.go`、`internal/memory/retrieval.go`、`internal/process`、`internal/worker`、`internal/pipeline`、`internal/generate`、`internal/decision` 与 `cmd/context-worker`。

决策判断仍处于 **shadow 模式**：Worker 对每个分析窗口调用 Decision 引擎并把问题和回答完整写入 `decision_runs` / `decision_answers`，但不依据判断结果修改注释，也不跳过任何基线提炼。C 切片在此基础上新增了一条独立于判断结果的自动链路：每个窗口在决策记录之后由生成器产出候选记忆，经证据校验后写入 `memory_candidates`，再由独立的 publish 任务合并进会话注释（标注为未经人工复核）。既有的 `apply_annotations` 显式提交路径不受影响。

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
      → generator.Generate（每个窗口一次）→ 候选校验
      → InsertCandidates → process.Enqueue 创建 publish 任务
      → RecordCoverage（窗口成功完成后）
  → decision_runs / decision_answers 落库（shadow，不据此发布）
  → memory_candidates 落库（validated / rejected）
  → context-worker 领取 publish 任务 → pipeline.PublishHandler：
      ListCandidates(validated) → CurrentAnnotations → 合并
      → ApplyAnnotations → publication_runs（committed / conflict）
      → process.Enqueue 创建 curate 任务（仅 committed 版本）
  → context-worker 领取 curate 任务 → pipeline.CurateHandler：
      运行 --curator-command（agent 经 MCP 只读调查，把提议写入 $CONTEXT_PROPOSALS）
      → DecodeProposalFile → ValidateTopicLinks → InsertLinks（status=proposed）
```

- **接收与入队原子**：`IngestTranscript` 在同一数据库事务内写入原话、来源附件并调用 `process.EnqueueTx`，来源保存和任务创建不会半途分离。重复导入相同分片时，附件和任务都幂等（`INSERT OR IGNORE`），返回已存在的任务 ID，不会产生重复业务任务。
- **任务标识**：任务 ID 为 `"job:" + sha256(幂等键)`；extract 幂等键由附件 ID 与处理器版本派生；附件 ID 作为任务的 `input_snapshot_id` 单独保存，payload 携带 `conversation_id`、`recording_id`、`segment_id` 与 `turn_ids`。
- **分析范围与覆盖记录**：payload 的 `turn_ids` 是本次分片实际入库的原话 ID（空白文本轮次不入库也不入列表）。`pipeline.ExtractHandler` 优先按 `turn_ids` 加载轮次并按确定性顺序切块，形成 per-segment 分析窗口；payload 未携带 `turn_ids` 的旧任务则回退为整会话分析。每个窗口在**成功完成**决策记录、候选生成校验入库与发布入队之后（未配置生成器时则在决策记录之后）调用 `pipeline.RecordCoverage` 把窗口内每个轮次写入 `analysis_coverage`：一行记录 (会话, 轮次, 处理器版本) 已被分析，`run_id` 记录对应的决策 run；`INSERT OR IGNORE` 使重复记录幂等，空列表为 no-op。生成或入库失败时不记录覆盖，任务按可重试错误交回队列，重试时这些轮次仍是未覆盖状态。`pipeline.UncoveredTurnIDs` 可按处理器版本列出尚未覆盖的轮次（顺序与 `LoadConversationTurns` 一致），`pipeline.ListCoverage` 按会话读取覆盖记录。
- **执行器**：`internal/worker` 不含业务逻辑，只负责领取、续租和收尾；`worker.Executor` 接口返回结果引用。执行器失败会作为可重试错误交回队列，直到尝试次数耗尽。
- **候选生成**：每个窗口在决策记录后调用 `generate.Generator.Generate(ctx, state, windowTurnIDs)`，返回候选草稿与模型名。候选与证据校验结果在同一窗口内入库，extract 任务的 resultRef 形如 `decision-run:<runID>+candidates:<validatedCount>`；未配置生成器时跳过生成，resultRef 保持 `decision-run:<runID>`。

## 候选记忆与会话级发布（C 切片）

### 生成器（internal/generate）

生成器实现 `Generator` 接口：输入渲染后的窗口状态与证据轮次 ID 列表，输出 `Draft`（`Kind`、`Summary`、`TopicTitle`、`EvidenceTurnIDs`、`InterpretationNote`）列表与模型名。两个实现：

- **Synthetic**：确定性合成生成器，由状态哈希派生 0–2 条候选草稿，不依赖外部服务。
- **OpenAICompat**：调用 OpenAI 兼容的 chat 端点，`POST {model, messages:[system,user], temperature:0}`；提示词要求返回严格 JSON `{"candidates":[{"kind","summary","topic","evidence_turn_ids","interpretation_note"}]}`，解析容忍 ```json 代码围栏；非 2xx 响应或坏 JSON 都返回错误。

`context-worker` 通过 `--generator synthetic|openai`（默认 `synthetic`）选择；`openai` 模式需配合 `--generator-url`（端点地址），API key 从 `--generator-key-env` 指定的环境变量读取（默认 `GENERATOR_API_KEY`），`--generator-model` 指定模型名（默认 `glm-5.3-flash`）。

本机统一走 **Agent Plan** 地址 `https://ark.cn-beijing.volces.com/api/plan/v3/chat/completions`（key 放 `VOLC_API_KEY`；`glm-5.3-flash`、`deepseek-v4.1-flash` 实测可用，`doubao-seed-2.0-mini` 亦可用但偶发超时）。不要用 `https://ark.cn-beijing.volces.com/api/v3`——那是按量计费入口；plan/coding 是订阅制路由。

### 候选校验与入库（internal/pipeline）

每个窗口生成后立即校验，规则：

- `kind` 必须在允许枚举内；
- `summary` 非空且不超过 1000 个 rune；
- `evidence_turn_ids` 非空，且全部属于当前窗口的轮次。

违规候选不会被丢弃，而是以 `status=rejected` 连同 `rejection_reason` 入库，便于后续审计生成质量。允许的 kind 枚举为 `observation / suggestion / request / constraint / intention`。`InsertCandidates` 的候选 ID 是内容确定性的 `"cand:" + hex(sha256(conversationID|kind|summary|evidenceJSON))[:32]`，不含 run/attempt，配合 `INSERT OR IGNORE`：同一会话内相同内容的候选无论来自哪次 run 或重试都映射到同一行，重放不会产生重复行；`run_id` / `job_id` 仅作为来源列，重复候选保留首个生产者的值。`ListCandidates(conversationID, status)` 按会话（可选按状态）读取候选，`status` 为空表示全部。校验完成后 extract 任务调用 `process.Enqueue`（事务包装 `EnqueueTx`）创建 publish 任务：`Spec{Stage: "publish", InputSnapshotID: conversationID, IdempotencyKey: "publish|<conversationID>|<setHash>", Payload: {"conversation_id": …}, MaxAttempts: 5}`，其中 `setHash` 是 `hex(sha256(该会话当前全部 validated 候选 ID 排序后以 | 连接))[:16]`，无 validated 候选时为 `none`；幂等键由候选集合派生，重试或重复窗口在候选集合不变时不会产生新的 publish 任务。

### 发布器（pipeline.PublishHandler）

`PublishHandler{DB, Store, ProcessorVersion}` 实现 `worker.Executor`，`context-worker` 的 pipeline 执行器按任务 stage 分发：extract → `ExtractHandler`，publish → `PublishHandler`，curate → `CurateHandler`。发布任务从 payload 读取 `conversation_id` 后执行：

1. `ListCandidates` 读取该会话全部 `validated` 候选；没有候选时直接返回 `"publish:noop"`。
2. 通过 `memory.Store.CurrentAnnotations` 读取当前注释（`conversations.annotations_json` + `annotation_version`；为空时得到零值注释和空版本）。
3. 合并候选：保留已有 Topics、Notes、TurnTopics；新 topic ID 为 `"topic:auto:" + hex(sha256(title))[:16]`，新 note ID 为 `"note:auto:" + hex(sha256(candidateID))[:16]`，note 的 kind、topic、summary、证据轮次来自候选；已存在的 ID 跳过——因此重复合并是幂等的。
4. 合并结果的 `AnnotationVersion` 设为 `"auto-" + hex(sha256(candidateIDs 以 | 连接))[:12]`，`AnnotationBasis` 设为 `自动分析；未经人工复核`；合并结果没有标题时取 `conversations.title`。
5. 以当前版本为期望版本调用 `ApplyAnnotations`：成功则写一条 `publication_runs`（`committed`）并返回 `"publish:<version>"`；版本冲突（期间注释被显式提交路径修改，错误含 `annotation version conflict`）则先写一条 `conflict` 记录，再把错误交回队列按可重试处理；其他应用失败同样先写一条 `failed` 记录再返回错误。

失败与冲突都会记入 `publication_runs`：冲突行保留当时读取的期望版本，下次尝试会重新读取最新注释再合并；由于候选 ID 与 note ID 都确定性派生，重试不会注入重复内容。发布任务的默认 `max_attempts=5`，退避语义与 extract 任务一致。提交成功后还会入队 curate 任务（见下节）；重放同一次发布不会重复入队。

### 策展触发器（pipeline.CurateHandler）

每次 publish 提交后自动入队一个 curate 任务，幂等键为 `curate|<conversation_id>|<annotation_version>`，payload 携带 `conversation_id` 与 `annotation_version`，`max_attempts=3`。`publish:noop`（没有 validated 候选）与冲突/失败路径不入队，因此 curator 只在会话注释真正变化后运行。

`CurateHandler{DB, DBPath, Command, Timeout, Model, ProcessorVersion}` 执行一次策展：

1. payload 缺少 `conversation_id` 或 `annotation_version`、或版本带前后空白时任务直接报错——publish 入队的 payload 总是携带 `auto-<hash>` 版本，这类值只能来自手工构造的坏任务。
2. `Command` 为空时任务以 `curate:<version>+skipped:no-command` 成功结束，不写任何记录、不读数据库——未配置策展的部署不会陷入重试风暴，任务记录本身说明原因。
3. 新鲜度检查（在 no-command 之后、运行 curator 之前）：读取 `conversations.annotation_version` 与 payload 版本逐字节比对。版本不一致说明该任务对应的注释已被后续提交取代（新的 publish 或手工 `apply_annotations` 都会改版本），任务以 `curate:<version>+skipped:superseded` 成功结束——不运行 curator、不写任何链接；会话不存在则报错。curator 运行结束后、写入前会再查一次版本，运行期间被取代同样以 `+skipped:superseded` 收尾、不写入。陈旧任务按成功收尾（而非报错）是为了不被 `max_attempts=3` 反复重试后标成 `failed`。
4. 否则在临时目录里以 `sh -c` 运行命令，并注入环境变量：`CONTEXT_DB`（数据库绝对路径）、`CONTEXT_PROPOSALS`（命令必须写入的提议文件路径）、`CONTEXT_CURATE_CONVERSATION`、`CONTEXT_CURATE_VERSION`。curator 是外部 agent：它只通过 MCP 读记忆库，`CONTEXT_DB` 仅供其 MCP 客户端指向同一个库，校验与写入仍在本进程完成，agent 无法绕过证据规则。命令输出（合计上限 4 KiB）只用于错误信息。
5. 命令失败、超时（`--curator-timeout`，默认 15 分钟）、没有写出提议文件或 JSON 非法，都作为可重试错误交回队列。
6. 读到提议文件后依次调用 `DecodeProposalFile`（必须是常规文件且为合法 UTF-8；拒绝未知字段与多余内容；文件上限 4 MiB，读取时同步限量、不信任 stat 快照；`proposals` 上限 500 条；每条证据至多 100 轮——文件级超限整份失败，单条证据超限只拒该条）、`ValidateTopicLinks`、`InsertLinks`。结果引用形如 `curate:<version>+proposals:<通过数>+rejected:<被拒数>+written:<实际写入行数>`；`written` 只统计新建或刷新的行，重放同一提议为 0，被拒绝过的行保持原判不动。

人工复核仍由 MCP 工具 `list_memory_links` / `review_memory_link` 执行（见 [associations.md](associations.md)），自动触发只负责产出 `status=proposed` 的建议。

## 表结构摘要

迁移集中在 `internal/migrations`（当前版本 9：v1–v5 为处理链路，v6 `memory_links`，v7 `annotation_history`，v8 `derived_artifacts` / `derived_artifact_sources`，v9 `speaker_bindings` / `speaker_binding_events`），时间列为 unix 毫秒 INTEGER。

### processing_jobs（迁移 v2 processing-queue）

| 列 | 说明 |
| --- | --- |
| `id` | 主键，`job:<hash>` |
| `stage` | 阶段名，当前为 `extract`、`publish` 或 `curate` |
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

### memory_candidates / publication_runs（迁移 v4 memory-candidates）

`memory_candidates` 一行一个候选草稿，校验失败的候选同样入库：

| 列 | 说明 |
| --- | --- |
| `id` | 主键，`cand:<hash>`，由会话、kind、summary 与证据列表确定性派生（不含 run/attempt）；相同内容跨 run 映射到同一行，重复候选 `INSERT OR IGNORE` |
| `conversation_id` | 所属会话 |
| `run_id` / `job_id` | 产出候选的 decision run 与 extract 任务，可为空；重复候选保留首个生产者的值 |
| `kind` | 候选类型，允许枚举之外走 rejected |
| `summary` / `topic_title` | 候选摘要与主题标题 |
| `evidence_turn_ids_json` | 证据轮次 ID 列表 |
| `interpretation_note` | 生成器的解读说明 |
| `status` | `validated / rejected` |
| `rejection_reason` | 校验失败原因，通过校验为空 |
| `generator_model` | 产出候选的生成器模型名 |
| `created_at` | unix 毫秒 |

索引 `memory_candidates_conv` 覆盖 `(conversation_id, status)`，对应 `ListCandidates` 的读取模式。

`publication_runs` 一行一次发布尝试的终态记录：

| 列 | 说明 |
| --- | --- |
| `id` | 主键，`pub:<hash>`，由会话、期望版本、候选集合与状态确定性派生；同一 run 重试会替换旧行（`INSERT OR REPLACE`），更新其终态 |
| `conversation_id` | 发布目标会话 |
| `expected_version` | 提交时读到的注释版本 |
| `committed_version` | 实际写入的新版本；`conflict` / `failed` 时为空 |
| `candidate_ids_json` | 本次合并的候选 ID 列表 |
| `status` | `committed / conflict / failed`：`conflict` 为注释版本冲突，`failed` 为其他应用失败 |
| `error` | 冲突或失败详情 |
| `created_at` | unix 毫秒 |

### analysis_coverage（迁移 v5 analysis-coverage）

分析覆盖账本：一行记录某个处理器版本已经分析过某条原话，供查询覆盖情况并避免未来增量重分析时重复处理。

| 列 | 说明 |
| --- | --- |
| `conversation_id` / `turn_id` | 所属会话与被覆盖的原话 |
| `processor_version` | 完成分析的处理器版本；覆盖按版本隔离，不同版本互不掩盖 |
| `run_id` | 对应的 decision run ID，可为空字符串 |
| `covered_at` | unix 毫秒 |

主键 `(conversation_id, turn_id, processor_version)` 配合 `INSERT OR IGNORE`：同一会话的同一轮次在同一处理器版本下重复记录时保留首行，重放不会产生重复行。`pipeline.RecordCoverage` 每个窗口一次写入，`pipeline.UncoveredTurnIDs`（LEFT JOIN 反查）与 `pipeline.ListCoverage` 提供读取。

### annotation_history / derived_artifacts / derived_artifact_sources（迁移 v7–v8）

`annotation_history` 记录被替换的注释快照（主键 `(conversation_id, annotation_version)`），由 `ApplyAnnotations` 在每次完整替换前归档；v7 之前该表由旧版本在运行时惰性创建，迁移用 `CREATE TABLE IF NOT EXISTS` 接管，两者兼容。

失效传播账本：

| 表 | 列 |
| --- | --- |
| `derived_artifacts` | `kind`（`note` / `candidate`）、`artifact_id`、`conversation_id`、`stale`、`stale_reason`、`stale_at`；主键 `(kind, artifact_id)` |
| `derived_artifact_sources` | `kind`、`artifact_id`、`attachment_id`；把派生记录连接到构建它的转录附件，主键三者联合 |

写入路径直接落账（候选入库与 `ApplyAnnotations` 都会重建自己的行），转录修订替换时 `markStaleForAttachment` 把旧附件的派生记录一次性标记为 stale；`Store.InvalidateDerivedArtifacts` 是对外的幂等失效入口，`WHERE stale=0` 使重复调用不产生额外变化。当前检索默认排除 stale 笔记与来自非活动附件的原话，`include_stale` 是显式审计开关；发布器在合并候选时同样跳过 stale 候选，stale 内容从不出现在新版本哈希中。v8 迁移的 backfill 为存量笔记与候选推导同样的初始状态。

### 修订失效后的 current projection 语义

transcript 被 supersede 后,系统对“历史”与“当前”做如下划分:

- **A. 历史 taxonomy / 审计**:`topics`、`topic_conversations`、`turn_topics` 行、`annotation_history` 快照、旧 `source_attachments`/`turns` 行都保留不删。`list_topics` 与 `get_annotation_history` 是审计视图,会如实显示这些历史映射。
- **B. current source-derived projection**:所有 current-state 检索与统计(`query_context` / `get_context_item` / `search_memory` / `list_conversations` / `get_processing_status` / `build_context`)只统计 active revision 的原话与未标记 stale 的派生记忆;`include_stale` 是显式审计开关。话题成员判定只认 current 轮次(`CurrentTurnPredicate`),因此一个只剩 superseded 成员的 stale 话题映射不会把 current 检索绕回历史内容。

### 环境音分离（migration v11：`turn_classes`）

转写轮次按内容分为真人对话与环境音（语音助手指令、交通/商超广播、歌词、媒体播放）。分类是内容寻址的确定性判定：`internal/turnclass` 的规则在 ingest 时对命中轮次立即落 `turn_classes`（method=rule）；规则无法判定的轮次（歌词、有声剧等）由 `bin/turnclass-refill` 批量交模型判定（method=model），缺失分类行始终表示"未判定"而非环境音。语义派生只看对话内容：extract 窗口排除环境音轮次（但为其记录 analysis 覆盖，状态视图不误报缺口）；读面分离覆盖 `search_memory`、`query_context`、`get_conversation_context` 与 `build_context`（后三者经 `Store.Query` 自动继承），默认不混入环境音、`include_ambient=true` 显式纳入并带 `ambient` 标记（笔记按其首要证据轮次归类），`get_processing_status` 把环境音计入独立 `ambient_turns` 桶而不算未归类缺口。`get_evidence_context` 是唯一的全量审计例外：按 turn_id 展开原话证据时保留环境音，供核对转写事实。环境音保留可查可审计，只是不进业务语义层。

剩余工作(TODO):话题 taxonomy 本身尚未做 revision-aware 重建——publish 替换注释时 `topic_conversations` 的成员关系会保留历史痕迹,`list_topics` 仍会列出只有 stale 成员的话题。这不影响 current 内容隔离(成员查询已按 current 轮次过滤),完整方案需要在注释替换时重建话题成员或为 topic 建立 derived 状态。

### speaker_bindings / speaker_binding_events（迁移 v9 speaker-binding）

局部分说话人标签到真实人物的绑定契约（本实现不含声学识别，只存人工/模型确认的绑定）：`speaker_bindings` 以 `(recording_id, speaker_scoped_id)` 为标签作用域、`transcript_revision` 为修订作用域——同一个 diarization 标签在不同转写修订间可能被复用，身份绝不跨修订继承。状态 `proposed / confirmed / rejected / revoked`，`version` 是行在标签组内的稳定序号（复核不改写）；`speaker_binding_events` 每次动作追加一行（`propose` / `confirm` / `reject` / `revoke`，actor 与备注）。`Store.SpeakerProjection(recordingID, transcriptRevision)` 只返回该修订下 `confirmed` 的行，撤销立即生效；未确认身份仍是未知。生命周期策略（文档化并有测试）：proposed 行是可刷新草稿；confirmed 同修订重放幂等；其他终态行（不同修订的 confirmed、rejected、revoked）是不可变历史，新提议创建下一个版本行，因此撤销后可基于新证据重新提议，被拒绝也不阻塞未来的新主张。同一标签在另一录音中不受影响。

MCP 面已暴露（`context-memory`）：写入 `propose_speaker_binding` / `review_speaker_binding`，只读 `get_speaker_projection` / `list_speaker_bindings`；current 检索返回的原话条目带 `speaker_person_id`（confirmed 投影解析结果，`turns.speaker_entity_id` 始终保持 NULL——身份永远是投影，不是行内实名）。

### 语义索引（migration v12：`memory_embeddings`）

混合检索的语义分支由离线索引支撑：`bin/context-embed --db ... --provider hash`（默认）或 `--provider openai` 接入 OpenAI 兼容嵌入端点，把当前非 stale 的笔记与轮次嵌入到 `memory_embeddings`（主键 `(kind, artifact_id, provider_id, model_id)`，`content_sha256` 使重复索引幂等）。当前环境音工件也会被索引——隔离不变量由查询路径执行（见 `internal/semantic` 的候选 SQL：轮次过 `CurrentTurnPredicate` + ambient 排除，笔记过 `NoteStalePredicate` + 主证据 ambient 排除），这样"语义检索绝不复活 stale/ambient"是可测试的性质。工件被删除后用 `--prune` 清理孤儿行。提供方边界在 `internal/semantic.Provider`，检索边界在 `SemanticRetriever`（空索引或未配置时优雅降级，绝不让检索失败）。

### 读取时上下文投影（internal/projection）

`get_current_context` 在读取时从当前证据推导 person / topic / commitment / preference / event 对象（状态 current / superseded / unresolved，`conflicting` 保留），带证据轮次、来源与冲突对；无投影表、无 LLM、确定性输出。规则、否定线索清单与"明确不投影的内容"见 [context-projection.md](context-projection.md)。评估用 `fixtures/eval/projection.json` 与 `internal/eval` 的投影指标（`evidence_valid`、`stale_not_current`、`superseded_not_current`、`conflicting_honest`、`deterministic`）守护。

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

# 判断服务（二选一）：真实 Decision 2.0 服务（本机默认 127.0.0.1:8765）已可运行；
# 离线/CI 可用同契约的确定性沙盒参考实现（默认 127.0.0.1:8799，避开真服务端口；
# weights_version 如实标注为 sandbox）
./bin/context-decision --addr 127.0.0.1:8799

# shadow 决策执行器：调用本地 Decision 服务并记录判断，
# 同时用默认合成生成器产出候选并入队 publish 任务
./bin/context-worker --db ./data/context-memory.db \
  --executor pipeline --decision-url http://127.0.0.1:8765/v1/systemone \
  --decision-model jev-style-2b-v2

# openai 兼容生成器：候选由 Agent Plan 的远程模型生成
./bin/context-worker --db ./data/context-memory.db \
  --executor pipeline --decision-url http://127.0.0.1:8765/v1/systemone \
  --generator openai --generator-url https://ark.cn-beijing.volces.com/api/plan/v3/chat/completions \
  --generator-model glm-5.3-flash --generator-key-env VOLC_API_KEY

# 自动策展：publish 提交后运行 curator 命令；命令必须把提议写入 $CONTEXT_PROPOSALS，
# 校验与入库由 worker 完成（scripts/run-curator.sh 会按 CONTEXT_DB 配置 MCP）
./bin/context-worker --db ./data/context-memory.db \
  --executor pipeline --stages extract,publish,curate \
  --curator-command 'scripts/run-curator.sh' \
  --curator-model volces-agent-01/glm-5.3-flash
```

`context-worker` 的完整参数：`--db`（必填）、`--worker-id`（默认 主机名+随机后缀）、`--stages`（默认 `extract,publish,curate`）、`--lease`（默认 30s）、`--poll`（默认 1s）、`--once`、`--executor`（`synthetic|pipeline`，默认 `synthetic`）、`--decision-url`（默认 `http://127.0.0.1:8765/v1/systemone`）、`--decision-model`（默认 `jev-style-2b-v2`）、`--generator`（`synthetic|openai`，默认 `synthetic`）、`--generator-url`、`--generator-model`（默认 `glm-5.3-flash`）、`--generator-key-env`（默认 `GENERATOR_API_KEY`）、`--curator-command`（默认空，空则 curate 任务以 skipped 结束）、`--curator-timeout`（默认 15m）、`--curator-model`（默认 `curator`）、`--synthetic-delay`。启动时会先执行 `migrations.Apply`。本地 Decision 服务未部署时，`--executor pipeline` 的任务会失败并按可重试错误留在队列中；`--generator openai` 未提供 `--generator-url` 时启动即报错。

## 与开发计划的对应

开发计划以工作包 A–F 划分（见[开发执行计划](development-plan.md)），当前进度：

| 工作包 | 已完成 | 未完成 |
| --- | --- | --- |
| A 存储与任务 | `internal/migrations` v1–v9（含 `analysis_coverage` 覆盖账本、`memory_links` 关联表、`derived_artifacts` 失效状态与 `speaker_bindings` 绑定契约）；`IngestTranscript` 与 `process.EnqueueTx` 原子提交；`Claim/Renew/Complete/Fail` 租约与幂等；重复导入幂等 | outbox 形式的跨命令任务、`processing_windows` 落表 |
| B 决策服务与执行器 | `internal/decision` Engine 契约（choice/noul/score，完整分布校验，非法结果整体拒绝）；Synthetic 与本地 `SystemOneClient` 两个 Adapter；`worker.Run/RunOnce`；`pipeline.ExtractHandler` shadow 记录 | 本地权重实际部署与加载、三个小模型对比、TypeSafe 托管 Adapter、路由启用 |
| C 提炼与发布 | `internal/generate`（Synthetic 与 OpenAI 兼容生成器）；候选校验（kind 枚举、summary 长度、证据⊆窗口）与 `memory_candidates` 入库；`pipeline.PublishHandler` 会话级发布、`publication_runs` 冲突/提交记录；`process.Enqueue` 包装 | 发布内容的人工复核与精化（合并/去重策略）、候选质量评测 |
| D 层级检索与 MCP | `list_conversations` / `get_evidence_context` / `search_memory`（`internal/memory` 检索接口 + `cmd/context-memory` 注册，游标分页）；实验性 `build_context`（`internal/contextbuild`：预算受限的任务上下文包，来源证据、覆盖缺口、确定性 trace，可选决策重排） | 完整层级视图、快照分页与双时间查询 |
| E/F | 话题关联建议层：`memory_links`（内容寻址、proposed/confirmed/rejected）、`context-associate` 校验入库、MCP `list_memory_links` / `review_memory_link` 复核、publish 提交后自动触发 `pipeline.CurateHandler`；转写修订替换（`supersedes_revision`）与派生记忆失效传播（migration v7–v8：`derived_artifacts` / `derived_artifact_sources`，stale 过滤与 `include_stale` 审计，自动重析）；说话人绑定契约（migration v9：`speaker_bindings` / `speaker_binding_events`） | 模型评测、确认后的话题合并视图 |

计划中的 Processing 层判断记录（`decision_runs` / `decision_answers`）已按拟议接口落地；窗口表尚未独立建表，窗口构建在 `internal/pipeline` 内存中完成（v1：默认单窗口，超过 200 轮按顺序切块；渲染超过 8000 字符截断并标注）。确定性保证：原话顺序为"有 `global_start_ms` 的按升序在前，再按 `segment_index`，再按 `original_turn_id`"，缺失时间仍有确定的来源序；输入哈希由 Decision 契约对 state 与问题集计算，缓存键与运行 ID 都可复现。候选与发布链路同样以确定性 ID 为基础：候选 ID、topic/note ID 与 publication run ID 都由内容哈希派生，重放不会产生重复行。

后续启用路由前需完成：shadow 数据评测、阈值校准（计划要求 ≥98% 关键窗口保留召回）、候选质量评测。注意自动发布不经人工复核，发布内容均带 `自动分析；未经人工复核` 标注；注释版本冲突会记入 `publication_runs` 并按可重试处理，与显式 `apply_annotations` 提交互不干扰。届时本文档应更新为描述实际启用状态。
