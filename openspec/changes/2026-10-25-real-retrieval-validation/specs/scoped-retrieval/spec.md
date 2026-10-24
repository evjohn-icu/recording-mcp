# Spec Delta

## ADDED Requirements

### Requirement: 范围先于投影
系统 MUST 在读取及投影候选元数据前按会话过滤，范围外无效数据不得阻断范围内结果，并保持原有范围内评分。

#### Scenario: retrieval.scope-before-projection
- **WHEN** 限定到某个会话查询
- **THEN** 满足上述不变量并通过合成回归验证

### Requirement: 历史兼容
系统 MUST 保留 v1 历史行为，不因 v2 优化改变结果。

#### Scenario: retrieval.v1-compatible
- **WHEN** 请求缺省版本或显式 v1
- **THEN** 满足上述不变量并通过合成回归验证

### Requirement: 显式版本选择
系统 MUST 将选定版本发送到既有接口并保存在 URL；未选择时仍为 v1。

#### Scenario: retrieval.version-ui
- **WHEN** 用户在混合搜索选择 v2 后刷新页面
- **THEN** 满足上述不变量并通过合成回归验证
