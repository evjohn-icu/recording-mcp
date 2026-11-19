# Audio analysis v1（内部契约）

本文件是 `audio-analysis-v1` 的冻结描述。它不是 ingest-v1 的一部分：ASR 仍只写
`source_attachments`，非转写音频结果不进入 turn，也不直接成为 memory candidate。

## 1. 传输

- Go 与 Python 适配器之间每次运行一个 JSON 文档：请求经临时受管 PCM 文件路径与 stdin 传入，
  结果经 stdout 返回；stderr 只允许出现安全错误码。
- 路径只作为本地子进程参数，不出现在 `Result` 的公共字段中。
- 适配器版本随结果返回；`capability` 固定为 `vad|tagging|diarization|speaker`。

## 2. Request

| 字段 | 类型 | 说明 |
|---|---|---|
| `schema_version` | string | 固定 `audio-analysis-v1` |
| `run_id` | string | 由下面的身份字段计算，重试复用 |
| `recording_id` | string | memory 内部 recording id |
| `source_audio_sha256` | string | 原始上传文件哈希 |
| `input_sha256` | string | 实际送入模型的 16k 单声道 PCM 哈希 |
| `transcript_revision` | string\|null | 仅当参与附件共享同一 revision 时填写 |
| `model_id` / `model_revision` | string | 受信本地 manifest |
| `model_files_sha256` | map[string]string | 文件名 → 64 hex SHA-256 |
| `capability` | string | `vad`/`tagging`/`diarization`/`speaker` |
| `start_ms` / `end_ms` | int | 分析窗口在原录音时钟中的位置；`0 <= start < end` |
| `config_sha256` | string | 能力配置哈希 |
| `source_attachment_ids` | []string | 排序后的参与附件；纯声学运行为 `[]` |
| `source_snapshot_sha256` | string | 上述附件的 `id/revision` 规范 JSON 哈希 |

`run_id = SHA256(recording_id, source_audio_sha256, input_sha256, capability, model_id, model_revision,
config_sha256, source_snapshot_sha256, generation)`。队列 key 为 `recording_id + capability + generation`；
CLI 的 `request-id` 即 generation，重跑或模型升级必须使用新 generation。

## 3. Result

`status` 固定为 `succeeded|failed|invalidated`。排队/运行状态来自 `processing_jobs`，不写入结果。

| 字段 | 说明 |
|---|---|
| 身份字段 | 与 Request 完全一致（含 `source_attachment_ids` / `source_snapshot_sha256`） |
| `events` | 允许空数组（真实无检出），与 failed 区分 |
| `provenance` | `adapter_version`、`frontend_id`、`device`、输入/模型/config 哈希、`parent_run_ids` |
| `error` | `code`、`retryable`、`action`；不含正文、向量或未清洗 stderr |

事件时间使用**窗口内 PCM 时钟**（`0..end_ms-start_ms`），读投影/播放时加上 `start_ms` 换算回录音时钟。

每个事件：`id`、`event_type`（`speech_activity|acoustic_event|speaker_segment|speaker_identity_proposal`）、
`start_ms/end_ms`、`confidence`（null 或 [0,1] 有限值）、`payload`。

- 时间必须落在 `0 <= start < end <= PCM 实际时长`；未知时间不允许用 0 事件填充。
- speaker cluster 只在 recording/run 内有意义；`source_kind` 为 `unknown` 或 `ambiguous`。
- 声纹向量只存 `audio_speaker_embeddings`，不进入 `result_json`、Web、MCP 或日志。

## 4. 提交语义

- 开始处理时在同一事务插入 `running` 行并持久化输入快照；`job_id` 唯一，重试复用。
- 提交事务检查 `audio_analysis_heads.desired_job_id`、running lease token 与未过期时间、profile/forget
  屏障、source 与 current 附件，然后原子写 events/active/head，并调用 `process.CompleteTx` 终结 job 与
  attempt。已终结的 worker 跳过外层 Complete。
- 旧 generation、旧 snapshot、source superseded、forget 之后的迟到输出一律拒绝。
- `generation` 相同但规范 payload 或依赖集不同 → `audio_request_conflict`；完全相同 → 幂等返回。

## 5. 失效与遗忘

- supersede 事务按 `json_each(source_attachment_ids_json)` 找到所有成员匹配的映射 run 并失效，
  清除 `audio_analysis_heads.current_run_id`；纯声学 run 保持有效。
- 修订不会自动重跑声学模型；重新关联必须显式 `context-audio enqueue` 新 generation，且不复制旧确认。
- redaction/forget/purge 先删依赖再删新表行（含 embedding BLOB 与 result_json）；turn 级遗忘保守地
  使该 recording 的全部声学分析失效并清除向量。

## 6. 能力语义

- **VAD（Silero ONNX CPU）**：16k float32，512 样本帧 + 64 context，递归 state 连续；末帧 pad 后裁回
  真实长度；输出概率不再做 sigmoid。只作为带分数的语音活动证据，不删除 ASR 输入窗口。
- **Tagging（Zipformer sherpa-onnx CPU）**：固定 5s 不重叠窗口，末窗 pad 后裁回；保留 top 标签与原始
  score；音乐/广播/重叠不能推出真实人物。
- **Diarization（Community-1 GPU）**：batch_size=1，一次一 recording；speaker 标签 run-local；不使用
  oracle 人数。
- **Speaker（CAM++ ONNX CPU）**：显式 fbank（16k、25ms/10ms、snip_edges、dither=0、80 mel、low20/high0、
  energy_floor=0、log power）与逐 span 全片段 CMN；每个非重叠 span 独立推理；192 维 L2 向量；
  空 fbank/非有限/零范数返回无有效声纹与结构化 error。

## 7. 状态投影

- 能力 disabled → 前端显示 `disabled`；缺少模型 → `unavailable`；不得伪造成功空输出。
- `succeeded` 且 events 为空是合法结果（真实无检出）。
- 读投影只暴露 run 状态/event 元数据，不暴露向量；`GET /api/audio-analysis/events/{id}/audio`
  按服务端 `start_ms/end_ms` 生成有界 WAV，未知时间/inactive/forgotten/跨 profile 不可读。
