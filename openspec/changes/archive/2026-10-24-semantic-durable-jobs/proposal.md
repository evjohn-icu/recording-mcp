# 语义任务持久化与可复现 Worldline

## Why

语义任务需要跨重启续跑，但结果受输入快照、模型权重与评分 rubric 影响。提案明确任务粒度、分批边界、缓存键和源变化的局部失效，并将大规模可靠性留作待验证工作。

## What Changes

- Worldline 候选以每批 128 条、每对实体最长 45 秒的规定窗口处理。
- 缓存键纳入输入/模型权重/rubric 版本；变化时不复用旧分数。
- 300 条任务恢复执行时，对 source-changed 的候选仅重算受影响项。
- 区分已经测试的决策日志幂等与尚待测试的配置键、取消租约保护。

## Capabilities

- `semantic-job`

## Impact

涉及 durable jobs、缓存、Worldline 批处理及恢复。`/tmp/recording-plan-tasks/relation-quality-report.md` 报告 120 条合成夹具校验，但推理 NOT_RUN；300 条可靠性测试未尝试。以上实验不能当成当前运行通过证据。
