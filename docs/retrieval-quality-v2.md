# Retrieval Quality v2

`retrieval_version` 默认 `v1`，语义 provider 默认使用离线 `hash`。显式选择 `v2` 可使用中文召回、预算前重排和证据充分性评估。`v3` 增加了实验性的证据准入。 [检索 v3](#检索-v3证据准入实验)

## 使用

MCP `build_context` 接受 `retrieval_version`（`v1`/`v2`/`v3`，默认 `v1`，其他值报错）；`mode` 仍为 `lexical` 或 `hybrid`。Web `/api/search/hybrid` 接受同名查询参数，以及 `max_items`、`max_bytes`、`rerank_candidate_limit` 和 `include_raw_evidence`（默认 false；非法布尔返回 400）。现有普通 `search_memory` 子串搜索及其游标保持原语义。

`context-memory` 和 `context-web` 用 `--decision-url` / `--decision-model` 配置重排，用 `--sufficiency-url` / `--sufficiency-model` 配置证据判断。两类端点使用 Decision `system_one` 协议。显式 `v2` 请求在配置充分性评估后会附加并返回最终预算内的来源证据。`v3` 请求按同一预算附加证据，并在预算前用有界候选及其证据进行准入。最终充分性评估只使用包内证据。 [检索 v3 配置](#检索-v3证据准入实验)

## 检索行为

v2 lexical 使用中文双字 gram、英文和数字 token 的 BM25（k1=1.2、b=.75），支持中文单字查询，并为精确短语命中加分。它在合格语料上评分后选取候选，不再要求整句出现在原文中。不含专为评测编写的同义词字典，因此不保证召回没有词面重合的同义表达。

来源版本、conversation/topic/time scope、ambient、stale 和证据过滤继续使用原有权威读取路径。融合排序后形成有界池，默认64项、最多128项，重排输入最多64KiB；同源证据去重改到最终接受阶段执行（v0.4 起）：被预算跳过的超大候选不再为它的来源占位，小候选仍可进入结果。条目先装入预算再附加证据窗口，超出预算的候选整条跳过并记入 `trace.budget.skipped_item_ids`，被跳过的证据轮次记入 `skipped_evidence_turn_ids`；重排输入的 64KiB 上限只在配置了重排模型时生效，未配置时保留确定性候选顺序并返回 degraded 与 `rerank_unavailable`。模型必须返回原候选的完整排列，不能新增、修改或删除来源对象；调用失败或非法输出保留确定性顺序并报告故障。

hybrid 模式下语义分支按故障类别降级（v0.4 起）：provider 超时、连接拒绝、502/503/504 与结构化 `model_not_found` 属于可降级 outage——保留已收集的字面、导航与确认链接结果，返回 `degraded=true` 与 `semantic_unavailable` gap；其余 HTTP/协议/向量形状错误继续让请求失败。就绪检查的数据库错误记 `readiness_db_error` 而不是当作空索引，真正的空索引记 `semantic_index_empty`（`internal/contextbuild/build_degradation_test.go`、`internal/semantic`）。

## 充分性

v2 返回 `sufficiency.status`（`sufficient`、`insufficient`、`unknown`）和经过校验的证据引用。部分检索结果始终保留，不生成自然语言答案。

- 没有最终证据：不足；预算限制和正常无匹配在 gaps 中分别说明。
- 有证据但没有配置判断模型：unknown（`evidence_assessment_unavailable` gap、包 degraded；返回条目完全不带证据引用时为 insufficient）。
- 成功判断证据不能完整回答：insufficient。
- 模型调用、响应验证或引用校验失败：unknown，并有明确故障 gap。

检索指标计算原始返回结果；不会通过隐藏“不足”结果降低负例误召回率。充分性单独衡量，不能将模型故障算作正确拒答。

## 评测协议

`fixtures/eval/quality-v2-r3/` 独立保存240条合成中文问题：同义表达、否定、时间变化、跨会话各40条正例，另有80条无证据问题。概念及相关来源组按50/25/25隔离，冻结文件以SHA-256校验。旧v1集合不作为新的独立holdout。

训练集用于开发，校准集用于选择；锁定配置后才运行test，不根据test重新调参。正式对比固定 `max_items=10`、`max_bytes=65536`，报告 Recall@5、MRR@10、负例查询误召回率、p95、各桶分母、调用数量及故障。p95包含检索、组装及启用的模型阶段，索引开销单独列出。

对比v1/v2 lexical、hash hybrid，以及显式配置时的真实embedding和对应重排。真实模型未配置必须记NOT_RUN，mock只能证明功能协议。

门槛为同配置v1基线上的Recall@5严格提升、MRR@10不退步、负例误召回率≤5%、零隔离/引用泄漏、p95≤1.5倍；缺指标或请求失败不能通过。充分性误判充分率另需≤5%。实际执行命令和结果见本变更验证记录；功能测试通过不等于质量门槛通过。

## 运行命令

```sh
go build -o /tmp/context-eval-quality-v2-final ./cmd/context-eval
python3 scripts/run-retrieval-quality-v2.py \
  --binary /tmp/context-eval-quality-v2-final \
  --out /tmp/retrieval-quality-v2-r3-final
```

默认只跑train/calibration，关闭classic pass以免间接执行holdout概念问题。只有校准门槛通过时才生成配置/二进制/语料哈希锁；`--phase holdout --selection-lock <锁文件>` 仅消费该锁一次。无合格候选时不运行test，不创建锁。真实模型参数为 `--embed-endpoint/--embed-model/--embed-dims`、可选 `--embed-key-env`，以及 `--rerank-url/--rerank-model`、`--assessor-url/--assessor-model`。

首批 `quality-v2` 因语义质量与classic隔离问题被否决，保留冻结文件与[诊断记录](evals/retrieval-quality-v2-rejected/REJECTED.md)，不可作为发布holdout。R2的语义审查记录随夹具保存；R3仅修复正整数turn索引并通过真实导入预检；哈希和结构校验不能替代人工核对金标。最终执行记录与限制见 [验证报告](retrieval-quality-v2-validation.md)。

启用充分性模型时，v2端到端延迟包含新增判断阶段，v1迁移基线没有该阶段；1.5倍门槛因此是保守的总成本约束，并非相同模型调用次数的纯检索微基准。报告同时列出实际调用次数。

## 检索 v3（证据准入，实验）

v3 在融合排序之后、同源去重与预算之前加一道显式准入门：只有 assessor 判定为 `supports`/`refutes` 且概率达到策略阈值的候选进入 Items/Evidence，准入概率不参与排序。冻结协议（policy 字段、阈值网格、selection-lock 与一次性 holdout）见 `openspec/changes/context-engine-v04` 的 `evidence-admission-v3` spec，此处不重复定义，只记录运行入口与状态。

### 服务端配置

- 服务端用 `context-memory` / `context-web` 的 `--admission-policy <evidence-admission-policy-v1 JSON>` 启用；该 flag 要求同时配置 `--sufficiency-url` 且 `--sufficiency-model` 等于策略的 `assessor_model`，否则启动失败。`phase:"calibration"` 的策略被服务端拒绝，只有离线 `context-eval --calibration-mode` 接受；准入还要求语义后端已配置且 provider/model 与策略一致（`cmd/context-memory/main.go`、`cmd/context-web/main.go`、`internal/contextbuild/admission.go`）。
- MCP `build_context` 与 Web `/api/search/hybrid` 均接受 `retrieval_version: "v3"`。Web 默认选择 `v1`，仅在 `capabilities.retrieval.v3_available` 为真时提供 `v3`。该字段要求已验证的策略、assessor 与语义后端。具体请求的 embedding 身份匹配由准入阶段检查。 [Web 版本选择](../web/src/api/retrieval-version.ts) · [服务能力声明](../internal/app/capabilities.go)
- 缺策略、身份不匹配、缺 assessor、评估失败或输出非法 → v3 返回空 Items/Evidence、`sufficiency=unknown`、`degraded=true` 与 `admission_unavailable` / `admission_failed` gap，不回退 v1/v2；单候选完整证据超过 64KiB 时拒绝该候选而不截断正文；空 query 的显式导航返回导航上下文并标 `assessment_query_missing`（`internal/contextbuild/admission_inputs.go`、`admission_v3_test.go`）。

### 实验与发布记录

- 冻结夹具 `fixtures/eval/quality-v3-r1/`（780 问、test 即 holdout、`lifecycle.json` 真实遗忘后检索）；实验命令 `scripts/run-retrieval-quality-v2.py --protocol v3 --phase calibrate|holdout|profile`，其中 `--phase profile` 只跑 train、threshold=0、不写 lock、不碰 test。
- 已记录的真实模型校准使用本地 Qwen3-Embedding 与 magpie Jev，端到端 p95 为 2444.7 ms，超过同 provider v1 的 1.5 倍上限（462.7 ms）。校准未生成 selection lock，holdout 尚未执行。`v3` 在当前发布候选中保持关闭，默认仍为 `v1` + `hash`。
- 本轮（2026-11-19）固定 `Qwen/Qwen3-Embedding-0.6B`（CPU float32，`--verify-model` 校验 SHA-256=0437e45c…，1024 维，`/healthz` 报 `model_sha256_ok:true`）与 `chaoliangUNSW/Jev-Style-0.8B-Decision-v3`（CUDA float16，SHA-256=0f8c8616…）。按仓库文档把 `transformers 5.22.2`（+hub 2.4.0/numpy 2.5.5）装到仓库外隔离目录，用 `PYTHONPATH` 只影响评估器进程，`.venv-asr` 未改动；typed readout smoke 通过（`weights_version=b023d1f9…`），单题真实分布为 true=0.988/false=0.0116，非均匀伪概率。评估器未装 `causal_conv1d`/`flash-linear-attention`，走参考 PyTorch 实现。
- v3 profile 实测（三档全部 FAIL）：v1 基线 PASS（p95 308.5 / 314.1 / 298.7 ms，同配置上限 462.8 / 471.2 / 448.1 ms）；v3 batch=1 在默认 300 s 总时限停在 `df-train-08-q2`，batch=8 在 1800 s 停在 `tc-train-09-q1`，batch=32 在 3600 s 停在 `df-train-05-q1`（越大越慢）。评估器单次请求实测 1.34 s，已超过 v1×1.5 上限；`DecisionCandidateAssessor` 按候选顺序对完整 state 打分，批次只放大 prompt 成本。结论：model-bound，v3 保持 BLOCKED，不缩池、不提高时延上限、不换 GGUF、不装内核来“修复”。
- 发布判定以机器校验为准：`python3 scripts/check_context_engine_release.py docs/engineering/context-engine-v04-release-results.json`（记录为 merge_readiness=READY、release_readiness=BLOCKED、production_ready_recommendation=false）。逐条命令与门禁状态见 [检索 v3 报告](engineering/retrieval-v3-report.md) 与 [v0.4 发布证据](engineering/context-engine-v04-release-report.md)。
