# 检索质量评估

## Why

功能回归不等价于检索质量。需要固定合成集、隔离分组切分、明确分母和质量/延迟/无答案指标，并将服务故障与成功无匹配分开。

## What Changes

- 使用 160 条固定问题：120 条有证据、40 条无证据；按 group 以 50/25/25 切分，组不得跨分区。
- 报告实际样本数、precision、recall、延迟及 no-answer 表现，并记录失败/超时与降级，不从空分母计算虚假得分。
- 区分语义服务故障降级与成功执行但没有匹配。

## Capabilities

- `retrieval`

## Impact

评测 runner、冻结 fixture、检索响应。已存在的证据范围回归测试不代表 160 问题集或质量门槛已通过。`/tmp/recording-plan-tasks/retrieval-report.md` 指明批准的 160 问题集尚不存在且未执行真实 provider；不伪造样本或报告。
