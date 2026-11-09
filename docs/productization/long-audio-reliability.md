# 长音频可靠性（SMART-02）

目标：数小时录音在进程崩溃/重启后只补未完成窗口；覆盖时长按区间并集记账；失败给出可执行原因。数据库附件是检查点，`sync_recordings` 是可读投影。

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

## 已知限制

- 窗口边界即时间戳精度；不做字级对齐。
- 真实 2 h 本机 ASR 未在本轮执行（时间与显存预算），只做了 120 s/36.95 min 与 2 h 故障注入。
- 合成 T3 的 RTF 只证明机械行为，不能用于性能声明。
- OOM/超时动作在 UI 中显示；测试只断言错误码与动作文本。
