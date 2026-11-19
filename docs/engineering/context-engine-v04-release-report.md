# Context Engine v0.4 发布证据

状态：**不建议 Production Ready**。核心实现、回归、Docker 恢复栈与 Android 构建已完成；v3 真实
calibration 因 p95 门禁 FAIL 且未生成 lock，真实音频质量因缺人工金标 NOT_RUN。下文只记录实际执行过的
命令和结果。

## 1. 分支与基线

| 项 | 值 |
|---|---|
| 分支 | `engineering/context-engine-v04` |
| 基线 HEAD | `6fc48d1cb42883c4c90466823c0d54bc88217649` |
| 基线记录 | `docs/engineering/context-engine-v04-baseline.md` |
| 临时隔离根 | `CE04_TMP=/tmp/recording-ce04.*`，所有 profile/DB/音频/日志只写这里 |

## 2. 交付内容与命令证据

### Sprint A：可靠性

| 要求 | 证据 | 状态 |
|---|---|---|
| A1 故障分类 + `Ready(ctx)` + 降级 | `go test ./internal/semantic ./internal/contextbuild`；`TestProviderUnavailableClassification`、`TestReadyPropagatesDatabaseErrors`、`TestSemanticGracefulDegradation` | PASS |
| A2 预算跳过/证据窗口/UTF-8 | `TestContextBudgetSkipsOversizedCandidates`、`TestContextBudgetEvidenceWindows`、`go test ./internal/memory` | PASS |
| A3 停写备份/恢复/屏障 | `go test ./internal/backup`（12 项）；`TestBackupRestoreIntegrityAndAudio`、`TestBackupRejectsPreForgetArchive`、`TestRestoreDetectsCorruptDatabase`、`TestRestoreRejectsUnfinishedSourceForget` | PASS |
| A3 Docker 恢复实测 | `RUN_DOCKER_DEPLOYMENT_TEST=1 python3 -m unittest scripts.test_productization_deploy -v` | PASS：7/7，含停写备份、完整性/音频引用、恢复后检索与 turn WAV、早于遗忘的归档拒绝 |

### Sprint B：证据准入 v3

| 要求 | 证据 | 状态 |
|---|---|---|
| 准入类型/策略/决策/64KiB 分批 | `TestBuildV3AdmissionAndSufficiency`（16 子用例） | PASS |
| v3 UI/入口 | `npm run typecheck/build --prefix web`；`node --test src/api/retrieval-version.test.mjs` | PASS |
| v3 冻结夹具（780 问题/330 组） | `scripts/generate-quality-v3-fixture.py --self-check`；`test_v3_fixture_is_frozen_and_valid` | PASS |
| 阈值网格/锁/一次性 holdout | `python3 -m unittest scripts.test_retrieval_quality_v2`（25 项） | PASS |
| 无模型协议运行 | `run-retrieval-quality-v2.py --protocol v3 ... --phase calibrate`（exit 0；`v3-grid NOT_RUN`，无 lock） | PASS（协议）/NOT_RUN（质量） |
| 真实 embedding/assessor calibration | 本地 `Qwen3-Embedding-0.6B@97b0c614`（CPU）+ magpie `typesafe/jev-latest`→`jev-1.14.0`（batch=32）。阈值 0：Recall@5=0.875、MRR@10=0.8786、FP=0、FalseSufficient=0、泄漏/失败=0；p95=2444.7ms vs v1 308.5ms（1.5× 上限 462.7ms） | **FAIL（p95）** |
| holdout | 无 lock | NOT_RUN |
| 默认值 | `default_changed=false`，默认仍 v1+hash | 未改变 |

### Sprint C：音频分析

| 要求 | 证据 | 状态 |
|---|---|---|
| 契约/存储/迁移 v23 | `docs/engineering/audio-analysis-v1.md`；`TestAudioAnalysisUpgradePreservesSources`；`go test ./internal/audioanalysis` | PASS |
| 生命周期/遗忘/revision/模型替换/身份 | `TestAudioAnalysisLifecycle`、`TestAudioAnalysisForgetDuringCommit`、`TestAudioAnalysisRevisionInvalidation`、`TestAudioAnalysisModelReplacement`、`TestAudioAnalysisIdentityNeedsReview`、`TestAudioAnalysisRejectsInvalidResult`、`TestAudioFailUnclaimedReleasesDependents` | PASS |
| 队列依赖/finalized | `TestJobDependenciesWaitWithoutAttempts`；`TestAudioAnalysisEndToEnd`（不重复终结） | PASS |
| revision 整链 | `TestEndToEndSupersedeReprocess` 扩展：映射 run invalidated、head 清空、旧 note 不再 current | PASS |
| Python 适配器 | `python3 -m unittest scripts.test_audio_analysis`（68 项）；`requirements-audio-analysis.txt` | PASS（协议/纯函数） |
| 真实模型推理（Silero/Zipformer/Community-1/CAM++） | 需要受信本地模型文件 | NOT_RUN/BLOCKED |
| GPU 锁 | `go test ./internal/gpulock`（跨进程争用、profile 共享、取消） | PASS |
| event 音频片段 | `TestEventAudioClipReadsExactRange`；`GET /api/audio-analysis/events/{id}/audio` | PASS |

**已知限制**：speaker 能力当前按固定/非重叠 span 提取 CAM++ 向量并聚类，未消费 diarization 的 cluster
片段；`speaker_identity_proposal` 仅由适配器/评审流程产生，不自动写确认。该限制使 speaker 身份质量验收
保持 EXPERIMENTAL，报告不据此声称身份准确率。

### Sprint D：真实验收

| 要求 | 证据 | 状态 |
|---|---|---|
| 共享指标模块 | `python3 -m unittest scripts.test_audio_acceptance_metrics`（53 项）；`scripts/eval-asr-cer.py` 复用 | PASS |
| manifest/gold 校验与隔离 | `python3 -m unittest scripts.test_long_audio_reliability`（8 项） | PASS |
| v04 synthetic 闭环 | `scripts/long-audio-reliability.py --protocol context-engine-v04 --mode synthetic` | 见 §4 |
| v04 real（无 manifest） | 四类指标 NOT_RUN，不访问默认数据 | PASS（诚实口径）/NOT_RUN（质量） |
| 真实人工金标/模型 | 历史已授权录音与机器转写已定位；缺人工逐字/区间金标与人工 adjudication | 质量 NOT_RUN；管线 smoke PASS |

## 3. Web/MCP 兼容性

- MCP 工具名与必填参数未改变；`build_context`、`get_context_item` 仅增加可选字段。
- `get_context_item` 新增可选 `audio_analysis`；Web `RecordingView` 新增可选 `audio_analysis`；
  `SufficiencyPanel` 仅在 v3 且存在 trace 时显示准入诊断。
- Web 与 MCP 共用 `app.Service.BuildContext` / `app.Service.AudioAnalysis`，因此来源与版本一致是结构性
  保证；未单独编写跨传输一致性自动化测试（本报告不声称已测）。

## 4. 最终回归

完整逐项记录见 `docs/engineering/context-engine-v04-release-results.json`。

| 命令 | 退出码 | 结果 |
|---|---|---|
| `make ci` | 0 | OpenSpec 16/16；spec map 236 场景；254 Python 测试 OK（10 skip）；`go vet`；`go test -race ./...`；全部二进制构建 |
| `npm run typecheck --prefix web` / `npm run build --prefix web` | 0 / 0 | v3 UI |
| `node --experimental-strip-types --test web/src/api/retrieval-version.test.mjs` | 0 | 6 项 |
| `npm run typecheck --prefix apps/cloud-ingest` / `npm test --prefix apps/cloud-ingest` | 0 / 0 | 向量与同步回归 |
| `RECORDING_E2E_ROOT=$CE04_TMP/cloud-e2e sh scripts/e2e-cloud.sh` | 0 | 10 个 cloud 场景在临时 root 通过 |
| `scripts/run-local-browser-tests.py --skip-build` | 0 | 本地浏览器套件 |
| `scripts/run-product-browser.py --skip-build` | 0 | 14/14 产品流程 |
| `long-audio-reliability.py --protocol context-engine-v04 --mode synthetic` | 0 | v04 synthetic PASS（chunks 1/1，coverage 3000ms，turn 可播放） |
| `long-audio-reliability.py --protocol context-engine-v04 --mode real` | 0 | 四类真实指标 NOT_RUN，未访问默认数据 |
| `run-retrieval-quality-v2.py --protocol v3 ... --phase calibrate`（无模型） | 0 | v1 基线 PASS；v3 grid NOT_RUN；无 lock |
| 同上，真实 Qwen3-Embedding + magpie Jev | 143（阈值 0 后停止） | Recall 0.875、MRR 0.8786、FP/FalseSufficient/泄漏均 0；p95 2444.7ms 超过 1.5× 上限；无 lock；未进入 holdout |
| `RUN_DOCKER_DEPLOYMENT_TEST=1 python3 -m unittest scripts.test_productization_deploy -v` | 0 | 7/7 通过（93.7s） |
| `./gradlew :app:testDebugUnitTest :app:assembleDebug --no-daemon`（JDK17 + SDK 37.0/build-tools 36.0.0） | 0 | BUILD SUCCESSFUL（39s），APK 已生成，单测通过 |
| `LOCAL_ASR_SMOKE=1 ... /tmp/real-120s.wav`（ark/qwen） | 0 / 0 | 真实 120s 授权样本经 Go ingest→ASR 通过（32.2s / 34.5s）；质量指标仍 NOT_RUN |

真实 embedding/assessor calibration 已实际执行到阈值 0：正确性、泄漏、误报与假充分门禁全部通过，但 p95 2444.7ms 超过同 provider v1 的 1.5 倍（462.7ms），因此整体 FAIL；准入成本与阈值无关，未继续其余阈值，无 lock；holdout 未生成、未执行。Docker 恢复栈 7/7 通过，Android 构建与单测通过；真实 120s 授权样本的本地 ASR 管线 smoke 通过，但真实音频质量仍因缺人工金标 **NOT_RUN**；`default_changed=false`。

## 5. 发布决定与后续

- **v3 维持关闭**：真实校准在正确性/泄漏/误报/假充分项全部通过，但 p95 2444.7ms 远超同 provider v1 的
  1.5 倍上限（462.7ms），未产生 selection-lock。`capabilities.v3_available` 需要已校验的 selected policy
  与 assessor，当前部署没有，因此 Web/MCP 不会提供 v3。
- **v0.4 以 v1+hash 交付**：A（故障降级、预算/证据、备份恢复）和 C（音频分析）能力保留；v3 仅作为显式
  实验路径存在，不改变默认。Docker 恢复栈与 Android 构建已在本环境通过；真实音频质量仍需人工金标
  （历史授权录音只用于管线 smoke，不作为质量证据）。
- **本地运行时 spike 已做，结论为不成立**：本地 2B/0.8B GGUF 单题约 25–36ms，但 admission pool 保持
  40 时串行/批处理都超出 150ms 预算；把 pool 降到 4 后 train Recall@5 只有 0.386（要求 0.85），质量
  崩塌。0.8B GGUF 的 letter-logprob 读数还出现均匀分布，不能直接作为 admission 模型。详见
  `retrieval-v3-report.md` 的 spike 小节。因此不再继续时延工程，按 (a) 收口。
- **1.5× 时延预算不在实现侧悄悄放宽**；若要改，属于产品决策，需要同步更新冻结协议、报告和默认门禁。

## 5. 隐私与限制

- 声纹是敏感生物特征：向量只存 `audio_speaker_embeddings`，不进入 `result_json`/Web/MCP/日志。
- 临时 PCM 与请求文件在子进程结束后删除；stderr 只保留安全错误码。
- profile 隔离：声纹比较仅同 profile、同模型/前端、current 且人工 confirmed 参考；跨 profile 不匹配。
- 遗忘：turn 级遗忘保守地使该 recording 全部声学分析失效并清除向量；恢复拒绝早于最新屏障的归档。
- 离线旧归档不会被工具“撤回”；没有存活可信 root 的灾难恢复 BLOCKED。

## 6. 回退

1. 关闭 `--audio-analysis-config`（能力显示 disabled）；
2. 显式 `retrieval_version=v1`，保留 hash provider；
3. 排空/停止 audio stage（worker 启动会以 `audio_disabled` 终结遗留任务）；
4. 回旧二进制需停写并恢复 A3 验证、且不早于最新 forget 的备份到新 root。

---

# 7. 后续跟进（第二轮，2026-11-13）

本节只记录本轮实际执行的命令与结果。判定以机器校验的
`context-engine-v04-release-results.json`（schema v2）为准，验证命令：

```sh
python3 scripts/check_context_engine_release.py docs/engineering/context-engine-v04-release-results.json
```

校验结论：`merge_readiness=READY`、`release_readiness=BLOCKED`、
`production_ready_recommendation=false`。合并就绪只覆盖 v1+hash 候选的工程证据；
生产发布仍被人工金标、v3 受信模型与事先质量阈值三项阻断。

## 7.1 人工金标与真实评分闭环

| 项 | 实现 | 证据 |
|---|---|---|
| 三步流程 | `scripts/review-audio-acceptance.py prepare/finalize/score` | `scripts/test_review_audio_acceptance.py`（21 项） |
| gold 契约 | 带时区 `reviewed_at`、`audio_duration_ms`、区间不重叠、ID 唯一、trial 只引用授权样本、省略/空数组分离 | 同上 |
| 预测/裁决 | `context-engine-predictions-v1`、`context-engine-adjudications-v1`、`prediction_digest` | 同上 |
| 真实执行 | `v04_real_run` 逐样本隔离 Stack、终态等待、turn WAV 长度、Web+MCP 采集、CPU/RSS/VRAM 采样 | `scripts/test_long_audio_reliability.py`（26 项） |
| 计量口径 | `event_pr` 最大基数匹配、`evidence_accuracy` 分母=全部返回、`decision_precision` 全裁决才出分、`aggregate_error_rates` | `scripts/test_audio_acceptance_metrics.py` |

合成闭环（无真实录音）已实际跑通：`prepare` → 浏览器标注页导出 → `finalize` → `score`。
结果：CER `0/6`、DER `OK`、pyannote `BLOCKED`（未安装）、Web 证据准确率 `1.0`、
MCP 空返回 `null`、claims `NOT_RUN`（未裁决）。产物见
`docs/engineering/followup/acceptance-score-synthetic.json`。

标注页用无头 Chromium 实测：外部资源 0 个、类别 5 项、区间标记加入成功、导出 JSON 正确；
未勾选“我已逐条复核”时导出仍是 `provenance:"draft"`，`finalize` 会拒绝——签名不可由页面代劳。
无头环境没有可 seekable 的音频区间，因此观察时把 `player.currentTime` 替换为可控值，
其余页面逻辑（标记、区间表、导出、签名）未改动；该替代已记录在
`docs/engineering/followup/annotation-page-check.json`。

**仍未完成**：真实 120 s 授权样本的人工逐字/区间 gold 与 claim/ref 裁决未返回，因此真实音频质量
本轮没有任何数字，保持 NOT_RUN。

## 7.2 发布判定

新增 `scripts/check_context_engine_release.py`（19 项测试）。规则：PASS 必须有命令与 exit 0
（交互式证据必须钉住产物 hash）、NOT_RUN/BLOCKED/EXPERIMENTAL 必须给原因、FAIL 不能支撑 READY、
EXPERIMENTAL/未完成证据不能支撑生产推荐、READY 只能引用 current 证据、snapshot 的源码/配置 hash
必须与工作树一致。

因此本轮把历史记录中两条“审阅式 PASS”降级为 EXPERIMENTAL（`privacy.review`），
并在文档里保留 `downgraded_from`；历史行统一标 `evidence_kind:"historical"`。

## 7.3 v3 时延与准入批次构造

- `internal/eval.EngineTiming` 逐 query 累计 `decision.Engine.Evaluate` 墙钟时间（准入+最终评估），
  `--profile-admission` 同时采集请求数/候选数/字节数；Go 测试覆盖转发、错误传播、累加与重置。
- `admission.go` 改为每候选只渲染 block 与 question 一次，`escapedStringLen` 累加得到批次字节数；
  Go 测试逐一比对整包 `json.Marshal` 字节数，并固定“恰好 64 KiB 通过 / 多 1 字节拆分 /
  超大候选被拒后小候选仍提交”。
- runner 新增 `--phase profile`：仅 train、threshold=0、不写 lock、不碰 test；协议路径实测结果为
  `runs.profile=NOT_RUN`（没有 assessor）、`selection_lock=null`、`test_touched=false`。
- **受信模型缺失**：本机没有受信的 embedding/assessor 端点，按计划不扫描缓存、不下载模型，
  因此 v3 profile 实测、完整校准与 holdout 全部 BLOCKED；本轮不声称任何时延或质量结论。

## 7.4 校准与一次性 holdout 协议

- lock schema 升为 `retrieval-v3-selection-lock-v2`：完整 configuration（含 `embedding_input_type`）、
  identity、budget、warmup 规则、fixture/binary/policy hash；旧 schema 直接拒绝。
- holdout 从 lock 恢复 provider/endpoint/model/revision/batch/rerank/budget；显式冲突先失败、
  不消费；拒绝概率缓存；flush URL 必须与 embed endpoint 同 host:port。
- `O_CREAT|O_EXCL` claim + fsync，先 claim 与原子 `holdout_started` 再发 test；崩溃后不可复用。
- 每个 v3 phase 只允许新目录或空目录；校准执行完整 0..100 网格，最终门禁只取一次无缓存复跑；
  无可验证 identity（如 `latest` 别名且无实际模型）拒绝写 lock。

## 7.5 本轮命令与结果

| 命令 | 退出码 | 结果 |
|---|---|---|
| `npx openspec validate --all && python3 scripts/check_spec_test_map.py` | 0 | 17/17；271 场景映射 |
| `python3 -m unittest scripts.test_audio_acceptance_metrics scripts.test_review_audio_acceptance scripts.test_long_audio_reliability scripts.test_retrieval_quality_v2 scripts.test_context_engine_release` | 0 | 166 项 |
| `go test -count=1 ./internal/contextbuild ./internal/eval ./cmd/context-eval` | 0 | 148 项 / 3 包 |
| `go test -count=1 ./cmd/context-memory -run 'TestExecutableStdioRoundTrip\|TestBuildContextToolSmoke'` | 0 | 2 项 |
| `python3 scripts/long-audio-reliability.py --protocol context-engine-v04 --mode synthetic` | 0 | 8 场景 PASS（v04.synthetic + T4–T10） |
| `python3 scripts/review-audio-acceptance.py prepare/finalize/score`（合成） | 0 / 0 / 0 | 见 §7.1 |
| `python3 scripts/run-retrieval-quality-v2.py --protocol v3 --phase profile ...` | 0 | profile NOT_RUN、无 lock、未碰 test |
| `npm run build --prefix web` | 0 | 3.25s，`dist` 产物更新 |
| `python3 scripts/run-product-browser.py --skip-build --artifacts $FOLLOWUP_TMP/product-browser` | 0 | 14/14 检查通过（产物留在仓库外） |
| `make ci` | 0 | OpenSpec 17/17、spec map 271、334 Python 测试（10 skip）、vet、race、全部二进制构建；184s |
| `python3 scripts/check_context_engine_release.py docs/engineering/context-engine-v04-release-results.json` | 0 | READY/BLOCKED，推荐 false |

## 7.6 待操作者提供

1. 授权录音的人工逐字/区间 gold（含 `scored_regions`、speaker trials、queries、claims）与签名；
2. 对预测 bundle 的 claim/ref 裁决；
3. 若要把 v3 变成可发布候选，需要受信本地 embedding/assessor 的实际身份与其本机端点，
   以及在运行前确定的 production 质量阈值。

## 7.7 本轮补充（2026-11-19）

- 上文表格是当时工作树的记录。上面的证据快照对源码/配置做 SHA-256 绑定，源码随后已变更，
  因此现在重跑同一命令会报告 `evidence_snapshot` hash 漂移（这是快照机制在发挥作用，不是新证据）。
  本轮证据另存于操作者私有目录（仓库外，0700/0600）：`release-results.json` 与产物日志；
  该文档的 `merge_readiness=READY`、`release_readiness=BLOCKED`、`production_ready_recommendation=false`。
- 工具改动：真实采集改为全量分页与栈自身 profile 环境、采集音频事件/说话人分段/决策候选；
  新增 `finalize-set`、`adjudicate-prepare`、`score --production-thresholds`（`production_gate`
  保存分子/分母与阈值/产物 hash）；`check_context_engine_release.py` 现要求 release READY 列出音频门禁、
  v3 calibration、v3 holdout，并从门禁产物重算比例、核对阈值 hash。
- 本轮实际执行：四个 Python 套件 189 项通过、Go 5 包通过、`make build`/`make web`/`make spec-validate`
  通过、v04 synthetic 8 场景通过、合成四类闭环跑通（`production_gate` 因缺声纹模型 `BLOCKED`）、
  标注页浏览器实测（未签名=draft / 签名=human、外部请求 0）。
- v3：pinned Qwen embedding 服务校验与真实调用通过；Jev assessor 运行时按文档装在仓库外隔离
  `PYTHONPATH`（transformers 5.22.2），smoke 通过（`weights_version=b023d1f9…`，真实分布非均匀）。
  profile 三档全部 FAIL：batch=1/8/32 分别在 300/1800/3600 s 总时限内停在 train split（越大越慢），
  v1 基线 p95 298.7–314.1 ms，上限 448.1–471.2 ms，而评估器单次调用 1.34 s 已超上限 ⇒ model-bound。
  未做 101 阈值校准、无 selection lock、未碰 test/holdout，v3 保持关闭；未通过缩池、提高上限、
  换 GGUF 或安装内核来“修复”。
- 发布阻碍仍是：四类授权样本与人工 gold/裁决、受信 ASR/声学/generator 身份、v3 的 model-bound 时延与
  尚未产生的校准/holdout。
