# Proposal
## Why
现有实现的可观察行为不足以支撑 Evidence-first Personal Context Engine v0.4 声明：
- 语义检索故障被当作“索引为空”或一般错误，调用方无法区分可降级 outage 与数据库/过滤失败。
- 上下文预算在证据窗口取数时已截断，超大候选会挤占名额而不留 trace，证据无法证明“完整窗口”。
- 备份文档直接打包运行卷，无停写校验、无恢复后 SQLite/音频/遗忘屏障实测。
- v2 只有排序与充分性判定，没有 candidate admission，相似但无事实的候选仍可进入答案证据。
- PoC 音频分析与主应用隔离，非 ASR 结果没有契约、版本、生命周期与遗忘集成。
- 真实音频质量只有弱参考，缺少授权 manifest、人工金标 schema 与独立指标定义。

## What Changes
- A：新增 `ProviderUnavailableError` 分类与 `Ready(ctx)` 接口，Build 在可降级 outage 时保留字面/导航结果并标记 degraded/gap；预算改为“先装 items 再装证据窗口”，跳过大候选并记录 skip trace，新增 `GetCurrentEvidenceWindow` 与 UTF-8 校验；新增停写备份/恢复/校验工具，恢复时用受信屏障集合拒绝早于永久遗忘的归档。
- B：新增显式 `retrieval_version=v3` 候选准入（admission）与 `AdmissionPolicy`，仅 supports/refutes 且达到阈值的候选进入 Items/Evidence；新增冻结 v3 评测协议（780 问题、test 即 holdout、threshold 网格、calibration lock、一次消费），默认仍为 v1 + hash。
- C：新增 `audio-analysis-v1` 内部契约、`internal/audioanalysis`、migration v23 表、Python 适配器、GPU 锁统一、job 依赖、ingest 接线、`cmd/context-audio`、读投影与 Web/MCP 消费、遗忘与 revision 失效。
- D：扩展 long-audio harness 为 `--protocol context-engine-v04`，新增授权 manifest/gold schema、共享指标模块与真实质量指标 NOT_RUN/BLOCKED 诚实口径。

## Capabilities
### New Capabilities
- `context-reliability-v04`：可降级语义故障、预算与证据窗口完整性、停写备份与遗忘屏障恢复。
- `evidence-admission-v3`：候选准入、证据充分性、冻结 v3 评测与默认值决定。
- `audio-analysis`：版本化非 ASR 音频结果、生命周期、遗忘、调度与消费者。
- `real-audio-acceptance`：真实录音 Harness、授权 manifest、人工金标与指标定义。

### Modified Capabilities
无。`ingest-v1` 冻结契约不变；已有 `local-intelligence-reliability`（SMART-01…05）与本轮交付无关，未被计为本轮证据。

## Impact
`internal/semantic`、`internal/contextbuild`、`internal/memory`、`internal/backup`、`internal/audioanalysis`、`internal/process`、`internal/ingest`、`internal/app`、`internal/eval`、`internal/web`、`cmd/*`、`web/src`、`scripts/*`、`fixtures/eval/*`、`docs/engineering/*`、`docs/deployment.md`、`Makefile`。
默认检索仍为 v1 + hash provider；音频能力默认关闭；不修改 ingest-v1 OpenAPI/向量；不访问用户私有数据。
