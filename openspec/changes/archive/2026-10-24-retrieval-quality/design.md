# 设计：检索评测与无答案行为

## Context

`TestTopicScopeAlsoFiltersEmbeddedNoteEvidence` 支持 Topic evidence scope；检索质量冻结集和 outage-vs-empty 仍 pending。检索审查报告说明 160 条批准问题集缺失，旧 fixture 不满足分组/配比，因此没有 baseline/candidate 质量报告，也没有真实 provider 调用。

## Decisions

- Fixture 固定 160 问题，其中 120 有标注正证据、40 明确无证据；按 group 隔离切为 50% train、25% calibration、25% test。实际整数计数必须写入报告，不用比例文字替代分母。
- 报告 precision、recall、查询延迟（至少 p50/p95）、no-answer 正确性，并列出超时、故障和各 bucket 实际分母。质量提升只能在同一冻结集合对比后声明。
- 语义服务不可用时标记降级/故障；服务成功但无匹配时标记正常空结果，两者不能混淆。
- 禁止通过复制旧 fixture 伪造 160 条标注；未准备批准语料时质量实验保持 NOT_RUN。

## Risks

质量 fixture 与报告指标待建设。test-map 只检查锚点存在，不证明质量、性能、调用 provider 或 TDD。
