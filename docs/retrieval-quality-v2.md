# Retrieval Quality v2

v2 是显式启用的实验检索路径；未指定 `retrieval_version` 时仍使用 v1。质量报告通过与否均不会自动修改默认配置。冻结 ingest-v1 未改动，验证仅使用合成语料及临时数据库。

## 使用

MCP `build_context` 接受 `retrieval_version: "v2"`；`mode` 仍为 `lexical` 或 `hybrid`。Web `/api/search/hybrid` 接受同名查询参数，以及 `max_items`、`max_bytes`、`rerank_candidate_limit`。现有普通 `search_memory` 子串搜索及其游标保持原语义。

`context-memory` 和 `context-web` 的 `--decision-url` / `--decision-model` 配置重排。新增 `--sufficiency-url` / `--sufficiency-model` 独立配置证据充分性判断，端点使用现有 Decision `system_one` 协议，不是 chat-completions。只有显式 v2 请求会执行充分性判断。模型仅接收请求允许的候选或最终证据。v2 配置充分性模型时，即使调用方没有显式要求 raw evidence，也会在同一最终预算内附加并返回可审计的来源证据；无法容纳的来源不会发送给模型。

## 检索行为

v2 lexical 使用中文双字 gram、英文和数字 token 的 BM25（k1=1.2、b=.75），支持中文单字查询，并为精确短语命中加分。它在合格语料上评分后选取候选，不再要求整句出现在原文中。不含专为评测编写的同义词字典，因此不保证召回没有词面重合的同义表达。

来源版本、conversation/topic/time scope、ambient、stale 和证据过滤继续使用原有权威读取路径。融合与证据去重之后形成有界池，默认64项、最多128项，重排输入最多64KiB；重排结束才执行最终条目、字节及证据预算。模型必须返回原候选的完整排列，不能新增、修改或删除来源对象；调用失败或非法输出保留确定性顺序并报告故障。

## 充分性

v2 返回 `sufficiency.status`（`sufficient`、`insufficient`、`unknown`）和经过校验的证据引用。部分检索结果始终保留，不生成自然语言答案。

- 没有最终证据：不足；预算限制和正常无匹配在 gaps 中分别说明。
- 有证据但没有配置判断模型：unknown。
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
