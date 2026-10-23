# 设计：语义任务与 Worldline 恢复

## Context

决策运行日志有 `TestRecordDecisionRunIdempotent` 证据；配置版本键、取消租约拒绝仍 pending。关系质量审查报告记录：120 条合成关系 fixture 仅 validate-only；本地推理协议返回 405，推理 NOT_RUN，300 条候选可靠性测试未尝试。不得将这些报告写作推理通过。

## Decisions

- 一个批次最多 128 条候选；每对实体的评估窗口上限 45 秒，边界值按协议显式测试。
- 结果缓存键包含 source snapshot、模型权重标识和 rubric 版本；任一改变均产生新键。
- 300 条恢复场景中只有源变化的候选重新计算，未变化候选可复用版本匹配的结果。
- durable job 的取消/租约与提交必须原子校验；未验证前保留 pending。
- 不把合成 fixture 的结构校验等同于模型质量评估，不调用外部 provider。

## Risks

缓存命中正确性和 300 条选择性恢复均为未验证设计行为；test-map 不证明性能、推理质量或 TDD。
