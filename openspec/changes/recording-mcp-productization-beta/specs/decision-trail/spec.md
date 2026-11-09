# Spec Delta

## ADDED Requirements

### Requirement: 决策六态
决策轨迹 MUST 把观察、提议、候选决定、已确认、已取代、待解决区分为六态；讨论与候选不得被当作确认；一个 macro topic 只保留一个当前决策槽。

#### Scenario: decision-trail.scenario-a
- **WHEN** 单一会话从观察到确认
- **THEN** 8 个具名子断言覆盖状态、历史、当前、证据与时间顺序，并由 `TestDecisionTrailScenarioA` 验证

#### Scenario: decision-trail.scenario-b
- **WHEN** 多个候选冲突并经历拒绝与重新打开
- **THEN** 当前为空、历史保留双方，并由 `TestDecisionTrailScenarioB` 验证

#### Scenario: decision-trail.scenario-c
- **WHEN** 来源 revision 失效或内容被遗忘
- **THEN** 确认被降级且不可复活，并由 `TestDecisionTrailScenarioC` 验证

### Requirement: 人工复核
确认 MUST 只来自人工 review 事件；review MUST 记录 actor、note、request_id 幂等与 expected_version 冲突。

#### Scenario: decision-trail.review-idempotent
- **WHEN** 相同 request_id 与相同 payload 重放
- **THEN** 只追加一次事件并返回相同结果，并由 `TestDecisionReviewIsIdempotent` 验证

#### Scenario: decision-trail.review-traceable
- **WHEN** 确认或拒绝
- **THEN** 记录 reviewer、note 与时间，并由 `TestDecisionReviewRecordsActorAndNote` 验证

#### Scenario: decision-trail.version-conflict
- **WHEN** 提交过期 expected_version
- **THEN** 返回冲突且不追加事件，并由 `TestDecisionReviewRejectsStaleVersion` 验证

### Requirement: 时间语义
系统 MUST 不推断未知生效时间：`valid_from` 未提供时保持 null，历史视图使用 `known_at` 过滤记录时间。

#### Scenario: decision-trail.unknown-valid-time
- **WHEN** 候选没有显式生效时间
- **THEN** `valid_from` 为空且不填入 `recorded_at`，并由 `TestDecisionTrailDoesNotInferValidFrom` 验证

#### Scenario: decision-trail.history-order
- **WHEN** 按声明时间基线排序历史
- **THEN** `valid_from`、`mentioned_at`、`recorded_at` 依次回退，并由 `TestDecisionTrailOrdersByDeclaredTimeBasis` 验证

### Requirement: 来源失效与遗忘降级
系统 MUST 在来源 revision 失效或内容被遗忘时把决策标为待解决，不得继续作为当前决策或复活已遗忘内容。

#### Scenario: decision-trail.source-invalidated
- **WHEN** 证据 revision 被取代
- **THEN** 最新确认降级且不自动回退旧确认，并由 `TestDecisionTrailInvalidatesStaleSource` 验证

#### Scenario: decision-trail.forgotten-not-resurrected
- **WHEN** 决策事件正文被清理
- **THEN** 该事件被跳过且不出现在轨迹，并由 `TestDecisionTrailSkipsPurgedEvents` 验证

### Requirement: Profile 隔离
决策轨迹 MUST 按 profile 隔离，A profile 的复核不得出现在 B profile 的响应中。

#### Scenario: decision-trail.profile-isolation
- **WHEN** 两个 profile 各自写入复核
- **THEN** 响应互不可见，并由 `TestDecisionTrailProfileIsolation` 验证

### Requirement: 决策 Web 体验
Web MUST 展示六态与证据、支持确认/拒绝/重新打开、刷新后保留状态，且不得把 confidence 渲染为真实性。

#### Scenario: decision-trail.web-workflow
- **WHEN** 执行 8 项决策界面操作
- **THEN** 全部通过，并由 `scripts/decision-trail-browser.py` 验证

#### Scenario: decision-trail.web-no-confidence
- **WHEN** 响应携带 `confidence:0.99`
- **THEN** 页面不显示概率或置信度文案，并由对应脚本验证

#### Scenario: decision-trail.web-unresolved
- **WHEN** 存在多个候选
- **THEN** 显示"待解决"且不显示"已确认"，并由对应脚本验证

#### Scenario: decision-trail.web-review-refresh
- **WHEN** 确认后刷新页面
- **THEN** 仍显示已确认，并由对应脚本验证
