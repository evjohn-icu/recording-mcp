# Design
## Context
基线为 main `6fc48d1`。ase 已有：revision/stale、候选提炼发布、人工 speaker binding、ambient 隔离、混合检索、profile 与遗忘屏障、process 队列（EnqueueTx/Claim/租约/重试）、`source_attachments` 作为 ASR/revision 权威、migration v22。
不变式：
- 预算口径为文本字节 `Σlen(Summary)+len(Content)+Σlen(evidence.Text)`；evidence 仅按 TurnID 去重。
- 遗忘屏障（tombstone/fence/profile_control/retired catalog）是恢复的唯一准入事实来源。
- 默认检索版本 v1 + hash provider；v3 与音频能力均显式 opt-in。
- 合成夹具只能证明协议/过滤，不能证明语义质量或真实准确率。

## Goals
- A：故障分类可判别、降级不伪造成功、预算与证据窗口可解释、备份恢复可验证且不绕过遗忘。
- B：只有能为问题提供实质支持/否定的候选进入答案；v3 默认关闭，通过冻结门禁也只建议显式启用。
- C：音频结果版本化、可失效、可遗忘、可观察，不进入 turn/记忆候选，不跨 profile 匹配。
- D：真实质量指标可测量且诚实标注 NOT_RUN/BLOCKED/EXPERIMENTAL。

## Non-Goals
- 不引入外部删除账本；无受信 live root 的灾难恢复记为 BLOCKED。
- 不做云端众包评测、不自动下载模型、不接受 gated 许可。
- 不重写 `/api/turns/{id}/audio`，不改 ingest-v1 契约。
- 不把 v3 设为默认，不因门禁通过自动切换。

## Decisions
### A1 故障分类
`ProviderUnavailableError{Reason,Cause}`，Reason ∈ `provider_timeout|connection_refused|model_unavailable|provider_unavailable`。仅以下归为可降级：独立 HTTP timeout、ECONNREFUSED、502/503/504、结构化 `code=model_not_found`。其它 HTTP/协议/向量形状错误不降级，不做字符串匹配。请求 ctx 已取消优先返回 `ctx.Err()`；provider 自身 timeout 用 `Timeout` 派生子 ctx 实现，不改共享 `http.Client`。
`SemanticRetriever.Ready(ctx) (bool,error)` 替代无参版本，SQL/索引错误返回 error；`false,nil` 仅表示真正空索引。迁移所有实现与调用（`search.go`、`sidecar_retriever.go`、`build.go`、测试），不留兼容 shim。

### A2 预算与证据窗口
`GetCurrentEvidenceWindow(ctx,turnID,radius)` 复用 `evidenceContext` 的 current window 取数，返回完整窗口不预截字节；旧 API 行为不变。Build：items 先按 kind/score/id 稳定排序装箱；每个被接受 item 按 center→previous→next 顺序尝试窗口，至多 3 个成功窗口；超预算/缺失中心不占名额；超大 item 跳过并记 `skipped_item_ids`；达 `max_items` 才停止。BudgetTrace 新增可选 `skipped_item_ids`/`skipped_evidence_turn_ids` 与 gap `item_budget_exceeded|evidence_budget_exceeded|evidence_missing`，保留 `budget_exhausted`。UTF-8 在硬过滤后、预算前校验，非法字节返回 `invalid_utf8_content`。

### A3 备份与恢复
新增 `cmd/context-backup` + `internal/backup`：`backup`/`restore`/`verify`。仅停写模式；原生需 `--confirm-writers-stopped`，前后比较 DB/WAL/SHM/音频清单与 hash，变化返回 `writers_active` 且不发布。恢复要求 `--barrier-source` 指向停写的受信 live root；屏障集合为排序 tombstone.id、fence(tombstone_id,provider_recording_id,device_id)、profile_control.tombstone_id/retired、retired-profiles catalog 条目，紧凑 JSON 固定字段顺序 SHA256。archive 缺屏障 → `backup_predates_forget`；多屏障/不匹配/未完成 → `forget_barrier_unverifiable`。恢复后跑 `integrity_check`（唯一 ok）与 `foreign_key_check`，检查所有 available 音频引用。归档 schema `backup-archive-manifest-v1`。Docker 入口 `scripts/backup-profile-volume.py` 只停本 compose writer，发现其它 running writer 则拒绝。

### B1 v3 准入
`AdmissionCandidate`/`AdmissionDecision`/`CandidateAssessor`/`AdmissionPolicy`。准入位于融合与硬过滤之后、dedupe 与预算之前。pool 限 `RerankCandidateLimit`（默认 64/最大 128），超出记 `pool_limited_ids` + `admission_pool_limited`。决策关系 `supports|refutes|related|unknown`；概率 = P(supports)+P(refutes)。仅达到校准 threshold 且证据完整（当前 revision、scope/ambient 合法）才准入。缺 policy/评估失败 → v3 空 Items/Evidence、unknown、degraded 与 `admission_unavailable|admission_failed`，不回退 v1。policy `phase:"calibration"|"selected"`，服务只接受 selected。准入概率不用于排序；准入项保持原序，若配置 reranker 按 v2 合法 permutation 重排。SourceStatus：`raw_current`(turn)/`derived_proposed`(note)/`navigation_only`(conversation)。空 query 的显式导航仍可返回导航上下文，unknown + `assessment_query_missing`。

### B2 冻结评测
`--protocol v2|v3`（默认 v2）。v3 语料 `fixtures/eval/quality-v3-r1/`：11 类 × 每 split 10 来源组，每组 1 complete + 1 none（110 组/220 问题），4 类各加 1 partial（每 split +40，三 split 共 780）。split=train/calibration/test（test 即 holdout）。threshold 网格 0.00…1.00（整数 `threshold_hundredths`），仅 calibration 选择；选择顺序 Recall@5↓、MRR@10↓、p95↑、threshold_hundredths↓（更严格）。calibration 缓存仅用于筛选，最终候选同硬件、禁缓存、warmup 一致复跑一次用于门禁与 lock；lock 原子写，holdout 用 `O_CREAT|O_EXCL` 一次性 consume。门禁：Recall@5≥.85、none-FP≤.05、FalseSufficient≤.05、泄漏全 0、无失败请求、Recall 严格优于同 provider v1、MRR 不退化、p95≤v1×1.5。不通过则 holdout NOT_RUN、default_changed=false。

### C1 音频契约与存储
`audio-analysis-v1` 内部契约（新文件 `docs/engineering/audio-analysis-v1.md`）。Request 字段固定；Result 状态 `succeeded|failed|invalidated`，排队/运行状态投影自 `processing_jobs`。migration v23（若被占用顺延）新增 `audio_analysis_runs`、`audio_analysis_events`、`audio_speaker_embeddings`、`processing_job_dependencies`、`audio_analysis_heads`。run id = SHA256(recording+audio hashes+capability+model_revision+config_hash+source_snapshot_hash+generation)；队列 key = recording+capability+generation。提交事务校验 desired_job、lease token/时间、current 附件，原子写 events/active/head 并 `process.CompleteTx`；执行结果增加明确 finalized 分支。supersede 以 `json_each(source_attachment_ids_json)` 失效映射 run；纯声学 run 不失效。forget/redaction/purge 覆盖新表，turn 级遗忘保守使该 recording 全部声学分析失效。

### C2 Python 适配与 GPU 调度
`scripts/audio-analysis.py`（stdin JSON → stdout JSON）+ `scripts/audio_analysis_models.py`；`requirements-audio-analysis.txt` 固定 PoC 已验证版本。能力：Silero VAD、Zipformer tagging、Community-1 diarization、CAM++ speaker。配置 `audio-analysis-config-v1`，默认 off，模型文件必须本地存在且 64hex SHA 匹配，`HF_HUB_OFFLINE=1`。GPU 锁统一 `internal/gpulock`：`${XDG_RUNTIME_DIR:-$HOME/.local/state/recording-mcp}/recording-mcp-gpu.lock`，launcher 显式传递，Go 父进程持锁覆盖子进程生命周期；`gpu_embedding_conflict`/`gpu_busy` 明确错误。

### C3 队列与消费者
`process.Spec.Dependencies []string`；Claim 仅选无未终态依赖的 job，父 succeeded/failed 都算终止，等待不消耗 attempt；环/自依赖/不存在父/依赖集冲突拒绝且事务无部分写入。`RunIngestJob` 增 audio_vad/audio_tagging，ASR 依赖 VAD 终态（失败仍完整转写并记 gap），diarization 依赖 ASR 终态但可在 ASR 失败后独立分析；speaker 依赖 diarization 与 tagging 终态。worker 启动恢复本 profile 待处理 audio stage，被关闭/模型缺失则以 `audio_disabled|model_unavailable` 终结为 failed 并释放依赖。`cmd/context-audio` 只入队。`app.AudioAnalysisView` 统一读投影；新增 `GET /api/audio-analysis/events/{id}/audio` 复用 `app.AudioClip` 路径校验。身份提案仅同 profile、同模型/前端、current 且人工 confirmed 参考 cluster 间比较，输出 `proposed`，绝不写确认事件。

### D 真实验收
`scripts/long-audio-reliability.py --protocol context-engine-v04`，manifest `context-engine-acceptance-v1`，gold `context-engine-gold-v1`（`provenance:"human"`）。无 manifest 的 real 模式四类指标全部 NOT_RUN 且不访问默认 data。共享指标 `scripts/audio_acceptance_metrics.py`（ASR 脚本也调用，不复制编辑距离）。CER/WER/DER/EER/event P-R/evidence accuracy/decision precision/RTF/资源采样定义在 spec 中固定；零分母 NOT_EVALUABLE。

## Risks / Trade-offs
- 屏障集合相等无法从两个静态副本数学证明 source 最新；旧 archive + 同样旧 source 副本不可区分，属明确不支持场景 → 无受信 live root 的灾难恢复 BLOCKED。
- v3 需要本地模型；缺依赖时门禁 BLOCKED，不能以合成测试替代。
- 音频模型 pin/许可证不全时该能力 BLOCKED，适配器与关闭路径仍完成。
- rerank 子集重排必须严格保持未提交项原 slot，否则会静默改变排序。

## Migration Plan
新增表与可选字段，v22→v23 保留 source/turn/队列数据；测试真实旧 schema 升级。回退顺序：关闭 audio config → 显式 `retrieval_version=v1` → 保留 hash provider → 排空新 stage。回旧二进制需停写并恢复 A3 验证、且不早于最新 forget 的备份到新 root。

## Open Questions
无阻塞项。真实模型与真实音频样本由操作者提供；缺失时按 BLOCKED/NOT_RUN 记录。
