# Spec Delta

## ADDED Requirements

### Requirement: 只读决策轨迹工具
MCP MUST 只新增一个只读工具 `get_decision_trail`，响应包含 `schema_version: "decision-trail-v1"`，且不得修改既有工具的字段。

#### Scenario: mcp-context.current
- **WHEN** 查询有当前决策的 topic
- **THEN** 返回当前确认与证据，并由 `TestMCPDecisionTrailCurrent` 验证

#### Scenario: mcp-context.history
- **WHEN** 请求历史
- **THEN** 返回按声明时间排序的记录，并由 `TestMCPDecisionTrailHistory` 验证

#### Scenario: mcp-context.no-evidence
- **WHEN** 候选没有可解析证据
- **THEN** 状态为待解决且原因含 `source_unknown`，并由 `TestMCPDecisionTrailNoEvidence` 验证

#### Scenario: mcp-context.conflict
- **WHEN** 同一 topic 存在多个候选
- **THEN** 当前为空且原因含 `multiple_candidates`，并由 `TestMCPDecisionTrailConflict` 验证

#### Scenario: mcp-context.stale-source
- **WHEN** 证据 revision 失效
- **THEN** 记录被标记 `source_invalidated`，并由 `TestMCPDecisionTrailStaleSource` 验证

#### Scenario: mcp-context.unconfirmed
- **WHEN** 只有候选没有复核
- **THEN** 状态为 `decision_candidate` 而非 `confirmed`，并由 `TestMCPDecisionTrailUnconfirmedCandidate` 验证

#### Scenario: mcp-context.confirmed
- **WHEN** 候选已被人工确认
- **THEN** 状态为 `confirmed` 且带 reviewer，并由 `TestMCPDecisionTrailConfirmed` 验证

#### Scenario: mcp-context.multi-session
- **WHEN** 证据跨多个会话
- **THEN** 每个证据保留 conversation/recording，并由 `TestMCPDecisionTrailMultiSession` 验证

#### Scenario: mcp-context.known-at
- **WHEN** 指定 `known_at`
- **THEN** 只返回记录时间不晚于该时刻的复核，并由 `TestMCPDecisionTrailKnownAt` 验证

#### Scenario: mcp-context.profile-isolation
- **WHEN** 两个 profile 各自写入复核
- **THEN** 工具响应互不可见，并由 `TestMCPDecisionTrailProfileIsolation` 验证

#### Scenario: mcp-context.byte-budget
- **WHEN** 设置较小的 `max_bytes`
- **THEN** 序列化结果不超预算、`truncated=true` 且当前决策仍在，并由 `TestMCPDecisionTrailByteBudget` 验证

#### Scenario: mcp-context.deterministic-repeat
- **WHEN** 相同输入调用两次
- **THEN** 结构化 JSON 完全相等，并由 `TestMCPDecisionTrailDeterministicRepeat` 验证

#### Scenario: mcp-context.schema-version
- **WHEN** 调用工具
- **THEN** `schema_version` 为 `decision-trail-v1`，并由 `TestMCPDecisionTrailSchemaVersion` 验证

#### Scenario: mcp-context.read-only-registration
- **WHEN** 以 `--read-only` 启动
- **THEN** 工具仍注册且不出现写工具，并由 `TestMCPDecisionTrailReadOnlyRegistration` 验证

#### Scenario: mcp-context.profile-event-plane
- **WHEN** 以 profile 模式启动
- **THEN** 事件平面读取 profile 的 `plane/events.db` 而非记忆库，并由 `TestMCPProfileUsesProfileEventPlane` 验证
