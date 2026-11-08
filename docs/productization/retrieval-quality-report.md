# 检索质量报告（product-review-v1，2026-11-09）

执行环境：本机 `go1.27.3`，离线 `hash` embedding（无外部 API）。数据集：`fixtures/eval/product-review-v1/`，4 个虚构中文会话（12 个 segment）、24 个问题：16 个可回答（synonym/negation/time_change/cross_session 各 4），8 个不可回答。作者 `local-review`，见 `REVIEW.md` 与 `manifest.json`。

分母：可回答 16 题；不可回答 8 题。每个可回答题目金标为 1 个轮次。所有指标来自实际执行的 `context-eval` 退出码 0 的产物：

- `docs/evals/productization-retrieval-v1lex.json`（v1 lexical）
- `docs/evals/productization-retrieval-v1hash.json`（v1 hybrid, hash）
- `docs/evals/productization-retrieval-v2lex.json`（v2 lexical）
- `docs/evals/productization-retrieval-v2hash.json`（v2 hybrid, hash）

## 指标

| 配置 | Recall@5 | MRR@10 | Precision@5 | 不可回答误召回条目 | 不可回答完全干净率 | p50 (ms) | p95 (ms) |
|---|---|---|---|---|---|---|---|
| v1 lexical | 0.250 | 0.135 | 0.063 | 0 | 1.000 | 0.288 | 2.795 |
| v1 hybrid (hash) | 0.750 | 0.373 | 0.163 | 36 | 0.563 | 5.302 | 7.539 |
| v2 lexical | 1.000 | 0.453 | 0.224 | 0 | 1.000 | 4.416 | 7.749 |
| v2 hybrid (hash) | 0.750 | 0.373 | 0.163 | 36 | 0.563 | 7.439 | 10.720 |

模型调用：v1 lexical 0；v1/v2 hybrid 各 26 次 embedding 请求（24 次查询 + 2 次索引），0 次生成模型调用；assessor 未配置（`NOT_RUN`）。

## 泄漏与错误分类

| 类别 | v1 lexical | v1 hybrid | v2 lexical | v2 hybrid |
|---|---|---|---|---|
| stale 泄漏 | 0 | 0 | 0 | 0 |
| ambient 泄漏 | 0 | 0 | 0 | 0 |
| scope 越界 | 0 | 0 | 0 | 0 |
| invalid evidence 引用 | 0 | 0 | 0 | 0 |

四类错误分开记录：

- 返回无关候选：v1 hybrid 在 8 个不可回答问题上共返回 36 个候选（平均 4.5 个/题），v1 lexical 与 v2 lexical 为 0。
- 证据不足：本报告无人工 sufficiency assessor，`NOT_RUN`，不得写成 0%。
- 错误宣称充分：同上 `NOT_RUN`。
- 无证据事实：不可回答集的 clean rate 见上表；未做事实性判定。

## 门槛判定

| 门槛 | 结果 | 说明 |
|---|---|---|
| Recall@5 ≥ 0.80 | FAIL（v1 lexical 0.25；v1 hybrid 0.75；v2 lexical 1.00；v2 hybrid 0.75） | 组合指标未统一达标 |
| 不可回答完全干净率 ≥ 0.90 | FAIL（hybrid 0.563） | hybrid 分支对否定/无依据问题仍返回候选 |
| p95 < 50 ms | PASS | 最大 10.72 ms |
| stale/ambient/scope/invalid 泄漏 = 0 | PASS | 全部为 0 |

结论：**默认检索版本保持 v1**。v2 仅在显式 `retrieval_version=v2` 时启用；本轮不调整排序参数、不切换默认、未运行 r3 冻结集的 holdout 阶段。v2 lexical 在本人工集上 Recall@5 达到 1.00，但样本仅 16 题且为自建集，不足以替代冻结基准结论。

## 未执行项（NOT_RUN）

- 真实中文 embedding：未配置端点/密钥，命令：`context-eval ... --embed-provider openai --embed-endpoint <url> --embed-model <model> --embed-key-env EMBED_API_KEY`。
- reranker：未配置 `--rerank-decision-url`。
- sufficiency assessor：未配置 `--assessor-decision-url`，因此“证据不足/错误宣称充分”为 `NOT_RUN`。
- r3 冻结集 holdout：未运行（无候选通过校准门槛，且计划禁止消耗 holdout）。
- 人工逐题判定：`REVIEW.md` 由本机作者完成，未做第二人复核。
