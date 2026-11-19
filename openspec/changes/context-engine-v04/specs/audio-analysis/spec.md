# Spec Delta
## ADDED Requirements
### Requirement: 版本化音频分析契约
系统 MUST 提供内部契约 `audio-analysis-v1`（Go 类型、Python 适配器、存储共享同一字段集），ASR MUST 继续只写 `source_attachments`；非转写音频结果 MUST NOT 伪装 turn 或直接写 memory candidate。`Request` MUST 含 schema_version、run_id、recording_id、source_audio_sha256、input_sha256、transcript_revision(nullable)、model_id、model_revision、model_files_sha256、capability、start_ms/end_ms、config_sha256，以及 `source_attachment_ids` 与 `source_snapshot_sha256`。
#### Scenario: CE04-C1-01
- **WHEN** 调用方提交跨能力运行
- **THEN** 请求与结果字段一致，非法 hash/范围/维度被拒绝
#### Scenario: CE04-C1-02
- **WHEN** 多段录音的 attachment 有不同 revision 的映射运行
- **THEN** run 级 transcript_revision 为 null，每个映射 event 列出精确 attachment_id/turn_id/transcript_revision

### Requirement: 结果状态与时间有效性
Result MUST 使用 `succeeded|failed|invalidated`，排队/运行状态 MUST 投影自 `processing_jobs`；禁用能力显示 disabled、缺模型显示 unavailable，MUST NOT 伪造成功空输出。MUST 允许 succeeded + 空 events（真实无检出）。事件时间 MUST 满足 `0<=start<end<=PCM duration`；置信度 MUST 有限且在 [0,1] 或 null。
#### Scenario: CE04-C1-03
- **WHEN** 真实无检出或模型不可用
- **THEN** 前者 succeeded 且 events 为空，后者 unavailable，二者可区分
#### Scenario: CE04-C1-04
- **WHEN** 事件时间越界或置信度为 NaN/1.5
- **THEN** 结果校验失败，不落库为 succeeded

### Requirement: 存储、原子提交与幂等
migration MUST 新增 `audio_analysis_runs`、`audio_analysis_events`、`audio_speaker_embeddings`、`processing_job_dependencies`、`audio_analysis_heads`（v23，若占用则顺延，绝不修改旧 migration）。run ID MUST 由录音、两个 hash、capability、模型 revision、config hash、source_snapshot hash、generation 的 SHA256 决定；队列 key MUST 为录音+capability+generation。同 key 但规范 payload 或依赖集不同 MUST 返回 `audio_request_conflict`；相同 MUST 幂等返回且不倒退 head。
#### Scenario: CE04-C1-05
- **WHEN** 同一 request-id 重复 enqueue
- **THEN** 只有一个 active run，返回幂等结果
#### Scenario: CE04-C1-06
- **WHEN** 提交时 lease 过期、generation 过期或 snapshot 变化
- **THEN** 拒提交，不写 events/active/head，其它能力不受影响

### Requirement: 执行结果原子终结
worker 执行结果 MUST 含明确 finalized 状态；提交事务 MUST 检查 desired_job、running lease token 与未过期时间、profile/forget/source/current 附件，原子写 events/active/head 并调用 `process.CompleteTx` 终结 job 与 attempt；已原子终结时 MUST 跳过外层 Complete，MUST NOT 依赖吞 `ErrLeaseLost`。
#### Scenario: CE04-C1-07
- **WHEN** 提交前发生 forget 或 source supersede
- **THEN** 迟到的 in-flight 输出被拒，不留 active 结果
#### Scenario: CE04-C1-08
- **WHEN** 进程在提交后崩溃并重启
- **THEN** 任务不重复终结，结果保持一致

### Requirement: revision 失效与模型升级
source supersede MUST 以被替换 attachment ID 查询 `json_each(source_attachment_ids_json)` 失效所有成员匹配的映射 run 并清 `heads.current_run_id`；纯声学 run MUST 保持有效。MUST NOT 自动重跑声学模型，重跑 MUST 走显式 `context-audio enqueue` 新 generation；MUST NOT 复制旧确认。
#### Scenario: CE04-C1-09
- **WHEN** 双 segment 录音只替换一个 attachment revision
- **THEN** 整个映射 run 失效且音频 event 不可读，pure acoustic run 仍有效
#### Scenario: CE04-C1-10
- **WHEN** 模型升级或显式重跑
- **THEN** 新 generation 产生新 run，旧 run 不 current

### Requirement: 遗忘与隐私清理
redaction、forget_profile 与 purge MUST 覆盖新表（含 embedding BLOB、run result_json、job payload 引用），先删依赖再删行；turn 级遗忘 MUST 保守失效该 recording 全部声学分析并清除向量。queued/running analysis MUST 在 claim 与 commit 检查 barrier；临时 PCM MUST 在子进程结束后删除。
#### Scenario: CE04-C1-11
- **WHEN** turn 级遗忘后仍有队列任务或迟到输出
- **THEN** 队列任务被拒，迟到输出不能激活，向量被清除
#### Scenario: CE04-C1-12
- **WHEN** profile 被退休或备份 verify 检查
- **THEN** 新表被同步覆盖/报告

### Requirement: 队列依赖与 ingest 接线
`process.Spec` MUST 支持 `Dependencies`，Claim MUST 只选无未终态依赖的 queued/retry_wait job，父 succeeded/failed 均视为终止且等待不消耗 attempt；环/自依赖/不存在父/依赖集冲突 MUST 拒绝且事务无部分写入。ingest MUST 入队已配置可用的 audio_vad/audio_tagging，ASR 依赖 VAD 终态（失败仍完整转写并记录 gap），diarization 依赖 ASR 终态但可在 ASR 失败后独立分析，speaker 依赖 diarization 与 tagging 终态。
#### Scenario: CE04-C3-01
- **WHEN** 父 job 处于 retry_wait 或最终失败
- **THEN** 前者继续等待且不消耗 attempt，后者放行子 job
#### Scenario: CE04-C3-02
- **WHEN** 某能力模型缺失或未配置
- **THEN** 该能力不入队并显示 unavailable，不挂住 ASR
#### Scenario: CE04-C3-03
- **WHEN** worker 重启时存在本轮关闭的 audio stage 任务
- **THEN** 以 `audio_disabled|model_unavailable` 终结为 failed 并释放依赖，非 audio 任务不被终止

### Requirement: 身份提案需人工确认
声纹比较 MUST 仅在同 profile、相同模型/前端、current 且有人工 confirmed speaker binding 的参考 cluster 间进行，输出 `speaker_identity_proposal`（status=proposed，最多 3 个，含 cosine 分数与参考 refs）；MUST NOT 写入确认事件或声称 verified。候选 cluster 与 ASR turn MUST 仅在完整 turn 时段被单一非重叠 cluster 覆盖时关联。
#### Scenario: CE04-C3-04
- **WHEN** 同 profile 有可用参考 cluster
- **THEN** 输出 proposed 提案，人物投影不变
#### Scenario: CE04-C3-05
- **WHEN** 参考来自其它 profile、旧 revision、已遗忘或 ambiguous 片段
- **THEN** 不做匹配，返回 unknown

### Requirement: 读投影与音频片段
`app.AudioAnalysisView` MUST 作为 Web/MCP 共用读投影，按 current source/hash/revision/model 配置与 forget 过滤；MCP MUST NOT 删除工具、增加必填参数或修改原字段类型。`GET /api/audio-analysis/events/{id}/audio` MUST 复用 `app.AudioClip` 认证与容器路径校验，按服务端 start/end 生成有界 WAV；未知时间、inactive、forgotten、跨 profile MUST 不可读。
#### Scenario: CE04-C3-06
- **WHEN** 用户查看录音详情与来源引用
- **THEN** 显示 disabled/unavailable/queued/running/succeeded/failed/invalidated 与安全 error_action，event 片段返回准确区间 WAV
#### Scenario: CE04-C3-07
- **WHEN** Web 与 MCP 查询同一 item
- **THEN** 两者返回同一来源集合与版本

### Requirement: 适配器与能力语义
Python 适配器 MUST 由受管临时 PCM 文件驱动、stdin/stdout JSON、每个 run 一次、stderr 只含安全错误码；MUST NOT import experiments 目录。能力固定为 Silero VAD（16k float32、512+64 帧、递归 state、末帧 pad 后裁回）、Zipformer tagging（5s 不重叠窗口，裁回真实长度）、Community-1 diarization（batch_size=1，run-local 标签）、CAM++（192 维 L2，显式 fbank 与逐 span CMN）。VAD MUST 只作为带分数证据，不删除 ASR 窗口；MUST NOT 从声音断言真人现场来源。
#### Scenario: CE04-C2-01
- **WHEN** 合成 silence/tone/短尾帧输入
- **THEN** 事件时间在真实长度内，空检出为 succeeded+空 events
#### Scenario: CE04-C2-02
- **WHEN** 重叠语音或媒体干扰片段
- **THEN** 标 ambiguous，不作为跨录音 identity 候选
#### Scenario: CE04-C2-03
- **WHEN** fbank 为空或向量非有限/零范数
- **THEN** 返回无有效声纹与结构化 error，不猜 identity

### Requirement: 配置、模型 pin 与 GPU 锁
`audio-analysis-config-v1` MUST 默认 off，含 python、runner、gpu_lock、lock/load/run 超时与各能力 enabled/model_id/model_revision/files(path,sha256)/device/frontend_id；MUST 要求本地存在且 64hex SHA 匹配，`HF_HUB_OFFLINE=1`，MUST NOT 运行时下载或接受 gated 条款；缺模型仅该能力 unavailable。GPU 锁 MUST 统一为 `${XDG_RUNTIME_DIR:-$HOME/.local/state/recording-mcp}/recording-mcp-gpu.lock`，由 launcher 显式传递，Go 父进程持锁覆盖子进程生命周期，Python MUST NOT 重复获取；冲突返回 `gpu_embedding_conflict|gpu_busy`。
#### Scenario: CE04-C2-04
- **WHEN** 模型文件 hash 不匹配或文件缺失
- **THEN** 该能力 unavailable，ASR/Web/MCP 仍正常启动
#### Scenario: CE04-C2-05
- **WHEN** 两个 OS 进程竞争 GPU 锁或两个 profile 非重叠执行
- **THEN** 竞争失败方返回 gpu_busy，非重叠执行互不干扰
