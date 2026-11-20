# 长音频可靠性（SMART-02）

目标：数小时录音在进程崩溃/重启后只补未完成窗口；覆盖时长按区间并集记账；失败给出可执行原因。数据库附件是检查点，`sync_recordings` 是可读投影。

SMART-02 的协议、场景矩阵和实测结果记录了当时的验证。新一轮合成闭环与授权录音验收使用 `--protocol context-engine-v04`，默认协议仍为 `legacy`。 [v0.4 协议](#v04-协议context-engine-v04) · [人工金标与离线评分](#人工金标与离线评分review-audio-acceptancepy)

## 协议

- 窗口计划：Go `cloudsync.PlanWindows(duration, windowMS, overlapMS)` 与 Python `plan_windows` 必须一致（`TestPlanWindowsMatchesRunnerDryRun`）。
- 分块：本地后端实现 `cloudsync.ChunkTranscriber`；`RunASRJob` 逐窗口写入 `source_attachments` 并回调更新 `completed_chunks/coverage_ms`。
- 续跑：启动时读取 `(recording_id, transcript_revision, active=1)` 的 `segment-%02d`，已存在窗口进入 `--skip`；全部存在时直接置 `done`，不加载模型。
- 覆盖：活跃区间并集，不用 `MAX(global_end_ms)`；`TestRunASRJobCoverageAccounting` 用 30 s/2 s 窗口确认覆盖为 31000 ms 而非 32000 ms。
- 失败：`RunnerError` 分类为 `asr_oom`/`asr_model_load_failed`/`asr_timeout`/`asr_runner_failed`；已提交窗口保留，`transcription_state='partial'`（无任何提交时为 `failed`），`failed_chunks` 计数一次。
- 非本地 provider（doubao/fixture）保持全量路径，并同样写 `total/completed/coverage='done'`。

## 场景矩阵

| ID | 场景 | 执行方式 | 状态 |
|---|---|---|---|
| T1 | 120 s 真实 Qwen3-ASR | `--mode real --t1 data/full-transcription/segments/segment-01.pcm` | 见下 |
| T1-ark | 120 s 真实 ARK-ASR | 同上，`--backend ark` | 见下 |
| T2 | 36.95 min 拼接真实 Qwen3-ASR | `--t2-run data/asr/ph261004155454/run.json` | 见下 |
| T3 | 2 h 合成 + 两次 SIGKILL | `--mode synthetic`（慢速故障注入 runner，非真实 ASR） | **PASS** |
| T4 | 窗口续跑 | `TestRunASRJobResumesCommittedWindows` | PASS |
| T5 | 覆盖记账 | `TestRunASRJobCoverageAccounting` | PASS |
| T6 | 窗口失败保留 partial | `TestRunASRJobKeepsPartialStateOnWindowFailure` | PASS |
| T7 | OOM 可执行动作 | `TestRunASRJobMapsCUDAOutOfMemory` | PASS |
| T8 | 索引重启后续跑 | `TestEmbedJobResumesAfterTranscriptCommit` | PASS |
| T9 | 重复字节导入 | `TestImportDuplicateBytesReuseJob` | PASS |
| T10 | 处理状态投影 | `TestProcessingStatusProjectsASRProgress` | PASS |

### T3（2 h 合成，故障注入）

```
python3 scripts/long-audio-reliability.py --mode synthetic
```

| 指标 | 值 |
|---|---|
| 音频 | 7200 s（合成静音） |
| 窗口 | 258 |
| 完成/期望 | 258/258 |
| 重复轮次 | 0（`duplicate_turns`） |
| 失败窗口 | 0 |
| 中断/重启 | 2 次（harness `kill_hard` 发送 SIGKILL） |
| 覆盖 | 7 200 000 ms = 时长 |
| 轮次 | 258 |
| 墙钟 | 26.3 s（慢速注入 runner，0.02 s/窗口） |
| RTF | 0.0037（仅机械验证，不代表真实 ASR 性能） |

输出标记 `label=synthetic`、`kind=synthetic-fault-injection`；**未**在 2 h 文件上运行真实本机 ASR（本轮限制，见 release report 的 `NOT_RUN`）。

### 真实音频

结果写入 `artifacts/long-audio-reliability/results.json` 的 `T1`/`T1-ark`/`T2` 条目；只记录聚合指标（时长、RTF、窗口数、覆盖、峰值显存），不复制音频或转写文本。

| ID | 音频 | 后端 | 窗口 | 覆盖 | RTF | 峰值显存 | 状态 |
|---|---|---|---|---|---|---|---|
| T1 | 120 s | Qwen3-ASR 1.7B float16 | 5 | 120 000 ms | 0.263 | 5398 MiB | PASS |
| T1-ark | 120 s | ARK-ASR 0.6B float16 | 5 | 120 000 ms | 0.179 | 3338 MiB | PASS |
| T2 | 36.95 min（拼接） | Qwen3-ASR 1.7B float16 | 80 | 2 216 891 ms | 0.033 | 5398 MiB | PASS |

T1 的墙钟包含模型加载与 ffmpeg 解码；RTF 是墙钟/音频时长。T2 的 80 个窗口全部完成，窗口覆盖 2 216 891 ms 等于拼接时长；按轮次区间统计只有 722 891 ms，因为 54 个窗口没有非空文本。这不是零填充：`results.json` 的 T2 条目记录实测 `audio_mean_db=-35.7`、`audio_max_db=-0.7`、`silence_spans=51`、`silence_seconds=590.06`（`ffmpeg volumedetect` / `silencedetect=noise=-40dB:d=5`）——该录音本身低语音密度，空窗口与静音/无语音一致。RTF 0.034 因此主要反映空窗口快速结束，不代表满语音吞吐；满语音参考是 T1 的 0.24。

### 五次连续导入（SMART-01 验收）

```
python3 scripts/long-audio-reliability.py --mode real --t1 /nonexistent --short-jobs 5
```

从授权 120 s 夹具切出 0/20/40/60/80 s 的 5 段 5 s 片段，顺序导入同一运行中的栈，断言 5 个不同会话、5 个成功的 ASR 任务、无重复抑制、`local_only=true`。使用故障注入 runner（协议验证，非真实 ASR）。

## v0.4 协议（context-engine-v04）

```sh
# 合成闭环：真实 web/worker 栈 + fixture ASR + 合成 WAV（无真实录音）
python3 scripts/long-audio-reliability.py --protocol context-engine-v04 --mode synthetic \
  --out "$(mktemp -d)"

# 先设置 ACCEPTANCE_MANIFEST 与本地 ASR 路径和身份。ASR_BACKEND 取 ark 或 qwen。
acceptance_out=$(mktemp -d)
python3 scripts/long-audio-reliability.py --protocol context-engine-v04 --mode real \
  --acceptance-manifest "$ACCEPTANCE_MANIFEST" \
  --asr-backend "$ASR_BACKEND" --asr-python "$ASR_PYTHON" \
  --asr-model "$ASR_MODEL" --asr-revision "$ASR_REVISION" --out "$acceptance_out"
```

可选参数为 `--asr-device`、`--generator-config` 和 `--audio-analysis-config`。

- **v04 synthetic**（条目 `v04.synthetic`）：独立 profile 栈（`context-profile create` + 真实 `context-web`/`context-worker`，fixture ASR，generator off），断言 `expected_chunks>0`、`completed==expected`、`failed==0`、覆盖 ≥ 音频时长、每个有时间的轮次 WAV 长度精确匹配；`--audio-analysis-config` 可选传入并记录在条目里。成功只是**机械口径**（`note: fixture ASR and synthetic WAV; protocol only, not an accuracy result`），不是准确率结果。该模式下 Go 回归测试 T4–T10 会被追加执行；`--mode real` 不隐含 Go 测试。
- **v04 real**：manifest 必须是 `context-engine-acceptance-v1`，四类 `quiet_two_person / far_field_meeting / overlapping_speech / media_interference`；样本含 `audio_path` / `audio_sha256` / `gold_path` / `gold_sha256` / `authorization{allowed:true,reference}`，路径必须落在 manifest 所属目录内，工具**绝不扫描**。ASR 身份必须显式钉住（backend/python/model/revision，`test-stub`/`fixture`/`synthetic` 直接拒绝；generator 文档只允许 `off|openai` 且 openai 必须是 loopback URL）。每个样本起一套隔离栈：上传 → 等全部 stage 终态 → 逐轮次 WAV 长度校验 → Web `/api/search/hybrid` 与 MCP `build_context` 双路采集 → 进程树 CPU/RSS 与设备 VRAM 采样 → 逐页读取全部轮次（`next_offset` 为 null 才停止；重复 turn id 或 offset 不前进直接失败，不把首页当全量）→ Web `/api/search/hybrid?include_raw_evidence=true` 与 MCP `build_context`（同一 profile 根目录与进程环境，显式要求原始证据）双路采集 → 从 `GET /api/conversations/{id}` 的 `recordings[].audio_analysis` 读取当前、活动、成功运行的 `acoustic_event`（`payload.label`）与 `speaker_segment`（`payload.speaker`），时间按 `run_start_ms`（缺省 run 的 `start_ms`）换算到录音时钟，未启用/失败状态与“成功但空”分开记录 → 通过 MCP `get_decision_trail` 逐会话 topic 采集 `decision_candidate`/`confirmed` 记录（按 candidate 去重，证据必须归属本样本；truncated 或工具失败按采集不完整处理并阻断质量通过）→ 产出 `context-engine-predictions-v1` bundle 到私密输出（目录 0700、`predictions.json` 0600）。同一 `--audio-analysis-config` 下 worker 的显式 `--stages` 会追加已启用的 `audio_vad/audio_tagging/audio_diarization/audio_speaker`，否则入队任务不会被领取。多 bundle 合并前校验样本/query/claim id 唯一、配置/二进制/模型身份一致，不一致就拒绝合并。缺一类授权样本只把该类标 NOT_RUN，不阻断其它类；无 manifest 时四类全部 NOT_RUN 且不读任何默认数据。

## 人工金标与离线评分（review-audio-acceptance.py）

```sh
# 1) 准备：显式路径 → 匿名 16 kHz 单声道 WAV + 离线标注页 + draft gold
review_root=$(mktemp -d)
python3 scripts/review-audio-acceptance.py prepare --audio "$AUTHORIZED_AUDIO" \
  --sample-id "$SAMPLE_ID" --out "$review_root/draft"
# 2) 打开 "$review_root/draft/annotate.html"，逐条标注并确认已人工复核。
#    将导出的 gold-reviewed.json 存到同一目录，与准备步骤生成的 audio.wav 放在一起。
# 3) 冻结：拒绝 draft；要求 category ∈ 四类、至少一个 scored_regions、
#    reviewed_at 带时区；产出 context-engine-acceptance-v1 manifest（钉住音频/金标哈希）
python3 scripts/review-audio-acceptance.py finalize --draft "$review_root/draft/gold-reviewed.json" \
  --authorization-reference "$AUDIO_AUTHORIZATION_REFERENCE" \
  --out "$review_root/frozen/manifest.json"
# 3b) 四类一次冻结（跨录音 speaker trials 与 required_spans 必须落在集合内）：
#     reviewed-set.json 使用 context-engine-reviewed-set-v1，列出每个 draft 与授权引用。
python3 scripts/review-audio-acceptance.py finalize-set --drafts "$REVIEWED_SET" \
  --out "$ACCEPT_ROOT/frozen/manifest.json"
# 4) 将 ACCEPTANCE_MANIFEST 设为冻结 manifest，执行上面的 real 命令。
# 4b) 生成人工裁决草稿（accepted 全为 null，annotator/reviewed_at 为空；未填写会被评分拒绝）
python3 scripts/review-audio-acceptance.py adjudicate-prepare \
  --predictions "$PREDICTIONS" --out "$ACCEPT_ROOT/adjudication"
# 5) 离线评分；--production-thresholds 触发产品门禁，非 PASS 退出非零（score.json 仍完整保存）
python3 scripts/review-audio-acceptance.py score --manifest "$ACCEPT_ROOT/frozen/manifest.json" \
  --predictions "$PREDICTIONS" --adjudications "$ADJUDICATIONS" \
  --audio-python "$AUDIO_PYTHON" --audio-analysis-config "$AUDIO_CONFIG" \
  --production-thresholds "$ACCEPT_ROOT/production-thresholds.json" \
  --out "$ACCEPT_ROOT/score"
```

`finalize-set` 在创建任何目录前完成全部校验：人工 provenance、路径型/重复 sample id、符号链接源、既有目标、哈希、全局 query/claim/trial id 唯一、跨样本引用在集合内、四类齐全、speaker evaluation 覆盖可算 EER；I/O 中途失败只删除本次新建的 root。

`--production-thresholds` 读取预先批准的 `context-engine-production-thresholds-v1`（四类 + 九项数值），`production_gate` 保存逐类分子/分母、`thresholds_sha256` 及 manifest/predictions/adjudications 哈希；缺失、非有限、零分母、NOT_RUN/NOT_EVALUABLE、experimental EER、未裁决 claim/ref、被排除样本、缺类或 stage 不完整一律不能通过，超线为 FAIL，材料不足为 BLOCKED，FAIL 优先。

评分可传入 `--adjudications`、`--audio-python`（包含 `pyannote.metrics`）和 `--audio-analysis-config`（声纹模型配置）。

- **gold 契约**（`context-engine-gold-v1`，`provenance:"human"`）：匿名 annotator、带时区 `reviewed_at`、`audio_duration_ms`、不重叠的 `scored_regions`、transcript turns、acoustic events、speaker trials（calibration/evaluation，只引用 manifest 内样本）、queries（`expected_sufficiency`、`evidence_completeness`、`required_spans`）、claims（decision/commitment）。**省略 = 未标注，显式空数组 = 确认为空**，二者语义分离。
- **指标口径**（`scripts/audio_acceptance_metrics.py`，共享纯函数；ASR 脚本复用同一编辑距离）：CER/WER 汇总为 `transcript_pooled`；DER 的参考来自 gold transcript、假设来自预测 bundle 的 `diarization`（缺字段即 NOT_RUN，绝不用 ASR speaker 标签顶替；能力失败与“成功但空”区分），collar=0、skip_overlap=false 并做最优 speaker 映射（pyannote DER 仅当 `--audio-python` 提供 pyannote.metrics，否则 BLOCKED）；acoustic event 为同标签 IoU≥0.5 一对一最大匹配；`evidence_accuracy` 分母 = 全部返回 ref（非法/stale/ambient/遗忘/越界/错录音错 revision 都记错误），命中需落在 gold span ±250 ms 内且 current；speaker EER 只取 evaluation split 的人工 same/different trials（calibration 不参与；需 `--audio-analysis-config` 受信声纹模型，否则 BLOCKED；类内不足 30/30 标 experimental 且不能发布）。零分母一律 NOT_EVALUABLE，绝不填 0 或 1。
- **人为裁决**：`context-engine-adjudications-v1` 绑定 `prediction_digest`，含 annotator、带时区 `reviewed_at`、逐 claim 的 `accepted` 布尔与逐 (query, ref) 的 evidence 裁决。`decision_precision` 只有当某 kind 的全部预测 claim 被裁决才出分，整体 precision 只在全部裁决后出分；`semantic_accuracy` 同样要求每个返回 ref 都被裁决；未裁决项保持 NOT_RUN，绝不计对错。裁决引用的是冻结 gold + prediction bundle 的 digest，二次评分不修改 gold；语义判定以人工 gold/adjudication 为准，不用 ASR 或其它模型当裁判。

## 证据边界与当前状态

- **合成闭环已跑通并留档**：`docs/engineering/followup/v04-synthetic-results.json`（8 场景 PASS：`v04.synthetic` chunks 1/1、coverage 3000 ms、轮次可播放，加 T4–T10）与 `acceptance-score-synthetic.json`（CER 0/6、WER 0/6、DER OK、pyannote BLOCKED 未安装、Web 证据准确率 1.0、MCP 空返回 NOT_EVALUABLE、claims NOT_RUN 未裁决、speaker BLOCKED 未传 config）；标注页实测留档 `annotation-page-check.json`（外部资源 0、未签名导出 provenance=draft、签名导出=human；headless 无法 seek 时以可控 `currentTime` 替代观察，页面逻辑未改）。
- **真实音频质量仍为 `NOT_RUN`**。已留档的授权样本运行验证了 ASR 管线和窗口覆盖。可发布的质量指标仍需人工逐字和区间金标、预测裁决及相应模型配置。 [真实验收待补证据](../engineering/context-engine-v04-release-report.md#76-待操作者提供)
- **本轮工具补充（2026-11-19）**：真实采集改为全量分页、MCP 使用栈自身 profile 根与密码、采集音频事件/说话人分段/决策候选；新增 `finalize-set`、`adjudicate-prepare` 与 `--production-thresholds` 产品门禁。合成闭环（四类合成样本 → `finalize-set` → 合成预测 → `adjudicate-prepare` → 测试裁决 → `score`）已跑通，`production_gate` 因缺声纹模型停在 `BLOCKED`（EER 无分母），其余类内计数完整。产品门槛文件、真实模型身份与发布结果保存在操作者私有目录（仓库外），不在仓库内复制；合成材料明确标注为测试材料。
- **证据校验工具**：`python3 scripts/check_context_engine_release.py <release-results.json> --root <repo>` 校验证据结构/产物哈希（PASS 需命令+exit 0、READY 只认 current 证据）。release READY 还必须列出音频门禁、v3 calibration、v3 holdout，`production_threshold` 需钉住阈值产物，音频门禁产物里的 `production_gate` 会按存储的分子/分母重算并核对其 `thresholds_sha256`；它仍不能证明测试充分或运行真实发生。

## 已知限制

- 窗口边界即时间戳精度；不做字级对齐。
- 真实 2 h 本机 ASR 未在本轮执行（时间与显存预算），只做了 120 s/36.95 min 与 2 h 故障注入。
- 合成 T3 的 RTF 只证明机械行为，不能用于性能声明。
- OOM/超时动作在 UI 中显示；测试只断言错误码与动作文本。
- v0.4 侧：真实人工金标/裁决未返回，质量指标保持 NOT_RUN；pyannote DER 与声纹 EER 分别需要 `--audio-python` 与 `--audio-analysis-config`，缺省为 BLOCKED 而非 0 分；v04 synthetic/real 只验证协议，不产出准确率声明。
