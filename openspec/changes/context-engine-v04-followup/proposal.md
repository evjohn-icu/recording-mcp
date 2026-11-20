# Proposal

## Why

v0.4 的四项后续工作仍未闭合，且现有代码在这些位置会给出比实际更强的结论：

- `scripts/audio_acceptance.py` 对已通过校验的人工 gold 仍返回 `NOT_RUN`，真实执行、采集与复评没有接通，
  “真实音频验收”因此只有占位结果。
- 共享指标口径与验收定义不符：`event_pr` 用贪心匹配会低估 TP；`evidence_accuracy` 把非法引用从分母中剔除；
  `decision_precision` 只用部分人工复核就算出分数。三者都会把更差的系统评得更好。
- 没有从人工草稿到冻结 manifest 的可复核流程，也没有“冻结预测 + 独立裁决”的离线复评入口。
- `context-engine-v04-release-results.json` 只有散落的命令记录，没有机器可校验的合并/发布判定，
  历史结果与当前结果也无法区分。
- v3 的时延结论缺少“只测 assessor 引擎自身耗时”的证据，因此无法判断继续优化客户端请求构造是否有意义；
  同时 `admission.go` 为每个候选重复渲染并整体 `json.Marshal` 一次，属于可消除的构造成本。
- v3 的 selection lock 只存 configuration hash，holdout 仍要求操作者重新传全部参数，崩溃后也无法诚实恢复；
  阈值网格的“提前停止”结论写在报告里而不是代码保证。

## What Changes

- A（人工金标与真实评分闭环）：新增 `scripts/review-audio-acceptance.py` 的 `prepare`/`finalize`/`score`；
  收紧 gold schema（带时区时间、`audio_duration_ms`、区间不重叠、ID 唯一、trial 只引用同 manifest 授权样本、
  省略数组与空数组语义分离）；新增 `context-engine-predictions-v1` 与 `context-engine-adjudications-v1`；
  `v04_real_run` 改为真实执行（隔离 Stack、上传、等待终态、校验 turn WAV、Web/MCP 双通道采集、
  CPU/RSS/VRAM 采样），并要求显式受信 ASR 配置。
- B（计量口径修正）：`event_pr` 改同 label 最大基数匹配；`evidence_accuracy` 分母改为全部返回引用并区分
  结构合法率与需要人工判定的语义准确率；`decision_precision` 只在该类全部预测裁决完成后给出正式分数；
  新增 `aggregate_error_rates` 做分子/分母汇总；spike 出 `speaker_span_embedding` 供 EER 与适配器共用。
- C（发布判定）：`context-engine-v04-release-results.json` 升为 v2，新增 `decision.merge_readiness`、
  `release_readiness`、`scope`、`blockers`、`evidence_snapshot`；新增
  `scripts/check_context_engine_release.py` 校验 PASS 的命令/退出码/产物 hash、历史与当前证据的区分、
  以及 READY 的门槛。
- D（有界 v3 时延实验）：新增 `--phase profile`（仅 train、threshold=0、不写 lock、不碰 test）；
  `internal/eval` 新增 `EngineTiming` 包装器与请求计数；`admission.go` 的批次构造改为“每候选只渲染/编码一次、
  批次检查与提交复用同一份字节”，并保证与整包 `json.Marshal` 逐字节等价。
- E（校准与一次性 holdout）：lock schema 升为 `retrieval-v3-selection-lock-v2`，保存完整 configuration、
  实际模型/运行时/prompt 身份、fixture/binary/policy hash、预算、warmup 规则；holdout 从 lock 恢复参数、
  显式冲突先失败、拒绝概率缓存；v3 各 phase 只允许新目录或空目录；校准执行完整 0..100 网格。

## Capabilities

### New Capabilities
- `context-release-evidence`：发布判定文档的证据完整性、历史/当前区分与 READY 门槛校验。

### Modified Capabilities
- `real-audio-acceptance`：人工 gold 流程、预测/裁决 bundle、真实执行与离线复评口径。
- `evidence-admission-v3`：v3 时延 profile、准入批次构造、lock v2 与一次性 holdout 恢复。

## Impact

`scripts/audio_acceptance.py`、`scripts/audio_acceptance_metrics.py`、`scripts/audio_analysis_models.py`、
`scripts/long-audio-reliability.py`、`scripts/review-audio-acceptance.py`、`scripts/check_context_engine_release.py`、
`scripts/run-retrieval-quality-v2.py`、`internal/contextbuild/admission.go`、`internal/eval/*`、
`cmd/context-eval/main.go`、`docs/engineering/*`、`openspec/test-map.json`。

默认检索仍为 v1 + hash；v3 保持关闭；音频能力默认关闭。不访问用户私有录音，不上传音频，不下载模型，
不 push/merge/release。
