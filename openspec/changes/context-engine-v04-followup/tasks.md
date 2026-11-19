# Tasks

> 未勾选项代表尚未完成或依赖于不可得输入（人工复核、受信本地模型）。不得把等待状态写成 PASS。

## 1. 人工金标与真实评分闭环

- [x] 1.1 收紧 gold schema：带时区 `reviewed_at`、`audio_duration_ms`、区间不重叠、ID 唯一、trial 只引用授权样本、省略/空数组语义
- [x] 1.2 `scripts/review-audio-acceptance.py prepare`：匿名 WAV、离线标注页、未签名草稿、私密权限、拒绝覆盖
- [x] 1.3 `finalize`：只接受 human 复核稿，先校验后建目录，冻结 gold + manifest
- [x] 1.4 `context-engine-predictions-v1` / `context-engine-adjudications-v1` 与 `prediction_digest`
- [x] 1.5 `score`：区间内 CER/WER、事件 PR、DER、EER、证据准确率、决策精确率；零模型/服务调用
- [x] 1.6 修正 `event_pr`（最大基数匹配）、`evidence_accuracy`（分母=全部返回）、`decision_precision`（全裁决才出分）、新增 `aggregate_error_rates`
- [x] 1.7 `speaker_span_embedding` 抽出，适配器与 EER 共用同一条推理路径
- [x] 1.8 `v04_real_run` 真实执行：隔离 Stack、终态等待、turn WAV 长度、Web/MCP 采集、资源采样、受信 ASR 配置校验
- [x] 1.9 生成器配置映射（仅 off/openai、loopback、key 变量名）与默认 claims NOT_RUN
- [ ] 1.10 操作者提供人工复核 gold（current 人工材料已请求，尚未返回）
- [ ] 1.11 在授权 120 s 样本上执行 real 运行并完成人工裁决与 `score`

## 2. 发布判定

- [x] 2.1 release results 升为 v2：`decision.merge_readiness/release_readiness/scope/blockers/evidence_snapshot`
- [x] 2.2 `scripts/check_context_engine_release.py` + `scripts/test_context_engine_release.py`
- [x] 2.3 每条证据区分 `current`/`historical`，PASS 必须 command + exit 0 + 产物 hash
- [x] 2.4 用本轮实际执行结果填充 current 证据并运行校验（`check_context_engine_release.py` PASS，READY/BLOCKED）

## 3. 有界 v3 时延实验

- [x] 3.1 `internal/eval.EngineTiming` 包装器 + 每 query 重置/快照
- [x] 3.2 `--profile-admission` 在 `cmd/context-eval` 接入 Engine 计时与请求计数
- [x] 3.3 语义不变优化：每候选只渲染/编码一次，批次检查与提交复用同一字节
- [x] 3.4 Go 测试：整包 `json.Marshal` 等价、64 KiB 边界、超大项后小项、取消与映射准确
- [x] 3.5 runner `--phase profile`：仅 train、threshold=0、不写 lock、不碰 test
- [ ] 3.6 受信本地 embedding/assessor 下运行 profile 对照（当前本地服务不存在 → BLOCKED）

## 4. 完整校准与一次性 holdout

- [x] 4.1 lock schema `retrieval-v3-selection-lock-v2`：完整 configuration（含 `embedding_input_type`）、identity、budget、warmup
- [x] 4.2 holdout 从 lock 恢复参数；显式冲突先失败；拒绝概率缓存；旧 schema 拒绝
- [x] 4.3 `O_EXCL` claim + fsync；崩溃/失败不可删标记重开
- [x] 4.4 v3 各 phase 只允许新目录或空目录
- [x] 4.5 完整 0..100 网格 + 最终无缓存复跑；失败不写 lock
- [x] 4.6 flush URL 必须指向实际 embedding 服务
- [ ] 4.7 受信模型下执行正式 calibration（当前 BLOCKED）
- [ ] 4.8 新 lock 后执行一次 holdout（依赖 4.7）

## 5. 验证

- [x] 5.1 `python3 -m unittest scripts.test_audio_acceptance_metrics scripts.test_review_audio_acceptance scripts.test_long_audio_reliability scripts.test_retrieval_quality_v2 scripts.test_context_engine_release`
- [x] 5.2 `go test -count=1 ./internal/contextbuild ./internal/eval`
- [x] 5.3 `make ci`（exit 0，184s）与 `make spec-validate`（17/17、271 场景）
- [x] 5.4 MCP 定向测试 `TestExecutableStdioRoundTrip|TestBuildContextToolSmoke`（2 项通过）
- [x] 5.5 标注页浏览器实测（合成页；产物 `docs/engineering/followup/annotation-page-check.json`）
- [x] 5.6 更新 `docs/engineering/context-engine-v04-release-report.md` 与 results 的 current 证据
