# Spec Delta

## ADDED Requirements

### Requirement: 硬范围
系统 MUST 对条目和全部嵌入证据应用硬范围，不因检索或关联扩大来源。

#### Scenario: brief.scope-is-hard
- **WHEN** 生成范围内简报
- **THEN** 满足本需求的来源、安全及状态约束，并由对应合成测试验证

### Requirement: 人物关联
系统 MUST 仅使用当前 revision 有效的 confirmed speaker binding，不根据人名推断身份。

#### Scenario: brief.person-requires-binding
- **WHEN** 按 Person 生成
- **THEN** 满足本需求的来源、安全及状态约束，并由对应合成测试验证

### Requirement: 陈述状态
系统 MUST 保持 proposed，来源 current 不得视为事实确认。

#### Scenario: brief.proposed-is-not-fact
- **WHEN** 使用 request、intention、constraint 或用户跟进标记
- **THEN** 满足本需求的来源、安全及状态约束，并由对应合成测试验证

### Requirement: 证据必需
系统 MUST 提供可核对的当前来源引用，缺少证据时报告缺口而非编造。

#### Scenario: brief.evidence-required
- **WHEN** 输出主要内容
- **THEN** 满足本需求的来源、安全及状态约束，并由对应合成测试验证

### Requirement: 冲突保留
系统 MUST 分别保留原话和证据，不自动裁决真伪。

#### Scenario: brief.conflict-preserved
- **WHEN** 来源存在相互矛盾陈述
- **THEN** 满足本需求的来源、安全及状态约束，并由对应合成测试验证

### Requirement: 修订有效性
系统 MUST 重建简报时排除旧转写及其无效派生内容。

#### Scenario: brief.source-revision-invalidates
- **WHEN** 来源被新 revision 取代
- **THEN** 满足本需求的来源、安全及状态约束，并由对应合成测试验证

### Requirement: 未知时间
系统 MUST 保持时间未知；有时间筛选时排除并报告缺口。

#### Scenario: brief.unknown-time-separated
- **WHEN** 来源缺少录音时间
- **THEN** 满足本需求的来源、安全及状态约束，并由对应合成测试验证

### Requirement: 遗忘过滤
系统 MUST 重建时排除来源、标记与备注，不缓存隐私正文。

#### Scenario: brief.forgotten-source-excluded
- **WHEN** 来源被永久遗忘
- **THEN** 满足本需求的来源、安全及状态约束，并由对应合成测试验证

### Requirement: 资源预算
系统 MUST 确定性截断并报告 truncated 和 gaps，禁止悬空引用。

#### Scenario: brief.budget-truncation
- **WHEN** 条目证据或字节超预算
- **THEN** 满足本需求的来源、安全及状态约束，并由对应合成测试验证

### Requirement: 确定性快照
系统 MUST 返回相同规范化内容及 snapshot_id，模型不可用不影响确定性模式。

#### Scenario: brief.deterministic-snapshot
- **WHEN** 相同来源版本范围模板与预算生成
- **THEN** 满足本需求的来源、安全及状态约束，并由对应合成测试验证
