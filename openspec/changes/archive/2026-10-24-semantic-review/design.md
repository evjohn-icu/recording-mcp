# 设计：语义关系复核

## Context

候选和来源证据可持久化，但语义正确性需要人工确认。现有测试 `TestInsertAndListCandidatesFilters` 只支撑候选列表/过滤相关行为，不足以证明完整复核生命周期。

## Decisions

- 决策枚举严格区分 accept、reject（理由必填）和 insufficient evidence；系统/网络错误保持错误状态，不可转写为 abstain。
- 每个决策绑定候选版本、来源快照与 request ID；相同 request ID 重放不产生第二个决策。
- 对已接受候选再次请求复核需显式 reopen，产生新版本/决策上下文，旧记录保留审计。
- 来源修订变化时旧结论标记 stale，不可作为当前确认；被 forget 的来源不可由重放候选重新发布。
- ingest-v1 不变，Context Layer 0.1 仍为未来草案。

## Risks

当前 `review.semantic-confirmation-distinct`、`review.revision-invalidates-review` 仍 pending。test-map 仅验证锚点存在，不证明 TDD、覆盖质量或命令通过。
