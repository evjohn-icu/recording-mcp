# 实现与交付验证记录

本记录汇总本轮规范驱动、处理恢复、Worldline、检索与本地浏览器交付证据。工作从 clean commit `4d01dea1f6ad788907e1cf58c7a0e6929561cfe6` 开始；改动尚未 commit/push。自动校验、自动化测试、质量门槛和手工审阅分别报告。使用的资料均为合成夹具；未读取私人录音、profile 数据或认证信息，未调用外部模型 provider。

六个 spec-driven changes 均已由 OpenSpec CLI 归档：`2026-10-24-openspec-workflow`、`2026-10-24-processing-recovery`、`2026-10-24-topic-identity`、`2026-10-24-semantic-review`、`2026-10-24-semantic-durable-jobs`、`2026-10-24-retrieval-quality`。主规范已同步至 `openspec/specs/<capability>/spec.md`；`make spec-validate` 最终通过 7 项主规范、35 个场景映射，唯一 pending 是人工核对的 TDD 流程原则，不是产品实现项。归档校验 `npx openspec validate --archived` 为 6/6。

## 规范与自动化测试

| 检查 | 结果 | 证据 |
|---|---|---|
| `make spec-validate` | PASS；7 个 OpenSpec 项目有效，场景 ID/锚点映射结构有效 | 命令输出；`openspec/test-map.json`；精确映射检查见 `scripts/check_spec_test_map.py` |
| 场景映射脚本单测 | PASS，7 tests | `python3 scripts/test_check_spec_test_map.py -v` |
| 检索夹具/门槛单测 | PASS，4 tests | `python3 scripts/test_retrieval_quality.py -v` |
| outage 与成功空结果区分 | PASS | `go test ./internal/contextbuild -run '^TestSemanticOutageIsDistinctFromSuccessfulEmptyResult$' -count=1` |
| generator-off 保留已有候选 | PASS | `go test ./internal/pipeline -run '^TestExtractHandlerNilGeneratorPreservesExistingCandidates$' -count=1` |
| retry 内部错误响应 | PASS | `go test ./internal/web -run '^TestProcessingRetryInternalFailureUsesGenericServerError$' -count=1` |
| Context Build 回归 | PASS | `go test ./internal/contextbuild -count=1` |
| 检索指标/语义索引/CLI | PASS | `go test ./internal/eval ./internal/semantic ./cmd/context-eval -count=1` |
| 全量 Go 单测 | PASS | `go test ./...`，运行日志 `/tmp/recording-plan-final-go.log` |
| 全量 Go race 检查 | PASS | `go test -race ./...`，运行日志 `/tmp/recording-plan-final-race.log` |
| Go 静态检查 | PASS | `go vet ./...`，运行日志 `/tmp/recording-plan-final-vet.log` |
| Python 回归 | PASS，54 tests、1 skip | `/tmp/recording-plan-final-python.log`；skip 是可选 `JEVD3_RUN_MODEL_SMOKE=1` 缓存模型 smoke test |
| Go 与 Web 构建 | PASS | `make build web`；最终合成浏览器 runner 日志 `/tmp/recording-plan-final-browser2.log` |

映射脚本只确认场景 ID、状态和精确测试函数锚点，不执行测试，也不判断断言质量、测试运行结果或测试是否先于实现。人工核对确认本记录没有把结构校验或测试映射表述为 TDD 证据。`workflow.no-tdd-inference` 是无法由脚本证明的流程原则，故保留为自动化 pending；这不是待实现的产品行为。最终场景表仅此一项 pending，详见 `openspec/test-map.json`。

## 合成浏览器验证

最终 runner 使用隔离的合成 profile，结果文件已保存：

- [本地 v2 浏览器结果](evals/browser-synthetic-final/local-v2-browser-results.json)：20/20 cases PASS。包括 profile 隔离、详情页与实际音频播放时间推进、错误草稿保留、并发转写/注释冲突、说话人审阅、事实历史、遗忘与 profile retirement。processing 状态端点返回 200，并报告无 worker；页面将候选生成器状态显示为未知，不能据此断言生成器配置状态。
- [Worldline 浏览器结果](evals/browser-synthetic-final/worldline-synthetic-browser.json)：22/22 checks PASS。使用 `synthetic/mock-engine`，覆盖关系复核与 reopen、409 后保留选择并对照新来源、主题重命名/分组/撤销、生成错误重试沿用 request ID、分页和 390px 响应式布局。

这两组结果只证明合成环境里的交互和协议路径。它们不验证真实录音、外部生成/嵌入服务或真实用户事实。

Profile 处理状态 polling 的独立回归也已通过：模拟延迟终止时会停止 polling，retry 重用相同 request ID；运行 10.5 秒后检查无额外请求。该检查包含在最终 `scripts/run-local-browser-tests.py` runner 中，父级最终日志为 `/tmp/recording-plan-final-browser2.log`。

## 检索质量与规模结果

检索基准为 60 条合成来源事实与 160 条中文查询（120 有证据、40 无证据），按概念组隔离为 50/25/25。查询和来源 hash、首次运行分区文件及报告保存在 [`evals/retrieval-quality-offline-v1`](evals/retrieval-quality-offline-v1/manifest.json)。在首次评分后曾尝试改写问题并检查结果，随后恢复到首次 hash；该测试分区已暴露，不作为独立盲测，也不用于声称质量提高。外部 embedding provider 为 `NOT_RUN`。

首次冻结评测没有候选阈值通过 recall 与无证据误报门槛，`SQLiteRetriever` 默认阈值保持 `0.05`。无证据误报率高，报告是失败质量结果，不是功能通过证明。完整口径见 [检索质量验证说明](retrieval-quality-validation.md)。

Worldline 关系判断另对本机已运行的 JEV 模型执行了真实推理；校准集和 test 集各 60 条，均为 `37/60` 正确、23 条误判，协议/响应校验错误为 0，质量门槛 FAIL。响应报告模型 `Jev-Style-0.8B-Decision-v3` 与权重 `b023d1f9c7858fbf01504577a3bfc349ea5c7385`。校准集 mean/p50/p95 为 100.8/102.3/136.1 ms，test 为 88.9/87.4/108.6 ms。校准与 test 的详细混淆矩阵、延迟和逐例结果见 [校准报告](evals/worldline-relations-calibration.json)、[test 报告](evals/worldline-relations-test.json) 及 [关系质量说明](worldline-relation-quality.md)。这是本机 loopback 的真实模型推理，不是外部 provider 调用；外部 embedding/generation provider 仍为 `NOT_RUN`。这些结果是合成样本上的模型质量 FAIL，不能把通过协议校验误报成准确率通过。

规模回归报告 [`evals/storage-bench.json`](evals/storage-bench.json) 收录了 10k 的 12 个样本（3 repeats × 4 场景），无 fallback、无安全泄漏、fixture recall@10/@40 为 1.0；但 HNSW 的 p50/p95 在所有四个场景都慢于 exact SQLite，优化 gate 为 FAIL，因此 exact SQLite 继续作为默认。100k 尺寸在 600 秒预算内未完成，标记 BLOCKED，不报告该规模性能。2,050 候选的 SQLite 表达式树深度问题已通过分块候选查询修复并有回归测试；具体实现及限制见 [存储检索验证](storage-retrieval-validation.md)。所有数据均为合成向量，不代表真实语义质量。

## 启动与数据库迁移

Profile Web/Worker 的启动流程见 [README 本地 Profile 启动](../README.md#本地-v2独立资料库纠错与遗忘)。先构建，再用 `MEMORY_PROFILE_PASSWORD` 和 `bin/context-profile create --profiles-root ./data/profiles --name home` 创建 profile，然后运行 `scripts/local-start.sh home`。默认绑定 loopback；生成器默认关闭，新 AI 摘要/候选要等显式配置后才会产生。运行说明不要求或自动提供外部模型服务。

代码的最高 schema migration 是 v18 `processing-runtime`，会新增 `worker_heartbeats` 与 `processing_retry_requests`（后者引用 `processing_jobs`）。应用迁移的启动代码通过 `migrations.Apply` 只向前执行未应用迁移，并在单个迁移事务中记录版本；仓库没有自动 downgrade 路径。本轮没有迁移任何私人/用户数据库。部署前先备份数据库副本并核实实际 `schema_migrations`；不要把“代码中已登记 migration 18”当成某个用户数据库已升级。回滚应用二进制不会自动撤销数据库 v18；若需回到旧库，必须人工选择并验证已知兼容的备份副本，恢复会丢失备份之后的写入。不要直接删除迁移表记录或手工删表来模拟降级。

冻结的 `contracts/ingest-v1/` 本轮未修改，仍是采集协议权威；Context Layer 0.1 文档仍为未来草案，当前原型不宣称兼容该草案。
