# 检索质量与证据边界

## ADDED Requirements

### Requirement: 检索评测必须以冻结且无组泄漏的问题集报告可复核指标
质量评估 MUST 使用 160 条冻结问题，其中 120 条有正证据、40 条无证据；按 group 隔离划分 train/calibration/test 为 50/25/25。报告 MUST 列出各分区实际计数、precision、recall、延迟及 no-answer 指标和分母；不得以缺失标注、空分母或功能测试宣称质量提升。

#### Scenario: retrieval.topic-evidence-scope
- **GIVEN** 请求限定在某 Topic，嵌入笔记含有范围外原话证据
- **WHEN** 执行 Topic 范围检索
- **THEN** 结果不返回范围外原话证据

#### Scenario: retrieval.frozen-quality-set
- **GIVEN** 有经批准冻结的 160 条合成问题集及 group 标注
- **WHEN** 对候选版本运行评估
- **THEN** 数据分布为 120 正证据与 40 无证据，概念组不跨 50/25/25 分区

#### Scenario: retrieval.report-accounting
- **GIVEN** 冻结检索评估问题集通过输入校验
- **WHEN** 评估器完成每个实际检索请求
- **THEN** 报告按分区、模式、问题桶给出 precision、recall、延迟分位数和对应分母，并记录嵌入请求/索引开销

#### Scenario: retrieval.outage-distinct-from-empty
- **GIVEN** 语义服务不可用且词法召回为空
- **WHEN** 语义检索返回服务错误
- **THEN** 请求返回明确错误，而不是成功的 `no_matches` 结果；服务成功但无语义/词法命中时返回 `no_matches`

#### Scenario: retrieval.no-answer-latency-denominators
- **GIVEN** 冻结测试分区含有正证据和无证据问题
- **WHEN** 生成质量报告
- **THEN** 报告分别给出 no-answer 数量及延迟分位数；延迟只以实际完成的检索请求为分母
