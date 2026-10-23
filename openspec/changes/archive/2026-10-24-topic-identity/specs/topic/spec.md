# Topic 稳定身份

## ADDED Requirements

### Requirement: Topic 身份独立于显示标题，成员修订不得泄漏过期内容
每个 Topic MUST 有独立稳定身份；标题及历史标题是可变显示信息。相同标题本身不能证明身份相同，不同标题本身也不能证明身份不同。当前查询只暴露当前有效成员及其当前内容；撤销一个成员不得删除同一 Topic 的其他人物、事实或证据。

#### Scenario: topic.stale-membership-filter
- **GIVEN** 旧修订的 Topic 成员关联已失效，但关联内容仍留在历史记录
- **WHEN** 查询该 Topic 的当前内容
- **THEN** 结果不包含由失效关联带出的内容

#### Scenario: topic.revision-removes-stale-membership
- **GIVEN** 自动成员关系由修订 R1 重建为 R2，R1 中有一条被替代的关联
- **WHEN** R2 的成员关系生效
- **THEN** 只移除被替代关联，R2 有效关联继续可见

#### Scenario: topic.same-name-not-merge
- **GIVEN** 两个不同稳定 ID 的来源 Topic 当前显示标题相同
- **WHEN** 系统生成面向用户的 thread 列表
- **THEN** 列表可按完全相同标题形成只读展示组，但不得合并来源 Topic 身份、改写成员关系或将其内容标记为已确认事实

#### Scenario: topic.different-title-explicit-group
- **GIVEN** 两个不同标题的来源 Topic 被用户选中
- **WHEN** 用户显式建立可撤销的 thread 分组
- **THEN** 展示层按该分组组织条目，来源 Topic 和各自记忆保持独立，旧 ID 仍可解析

#### Scenario: topic.revoke-preserves-peers
- **GIVEN** 一个 Topic 包含人物 A、人物 B 与多个事实，且只撤销 A 的成员关系
- **WHEN** 读取当前 Topic 及历史标题
- **THEN** 分组撤销后两个来源 thread 与其独立条目仍可读取，旧 ID 仍可解析，撤销不合并或删除 claim
