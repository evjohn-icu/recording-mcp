# Spec Delta

## ADDED Requirements

### Requirement: 默认关闭和显式同意
系统 MUST 只有启动配置启用且逐次 consent=true 时调用模型；确定性生成不调用模型。

#### Scenario: polish.explicit-consent
- **WHEN** 用户请求可选润色
- **THEN** 满足上述约束并由合成测试核验

### Requirement: 封闭证据和候选状态
系统 MUST 拒绝额外引用、重复条目和状态覆盖；候选始终 proposed，原简报不变。

#### Scenario: polish.closed-evidence
- **WHEN** 模型返回候选
- **THEN** 满足上述约束并由合成测试核验

### Requirement: 来源变化
系统 MUST 再次构建快照并拒绝返回过期候选。

#### Scenario: polish.source-changed
- **WHEN** 调用期间修订或永久遗忘来源
- **THEN** 满足上述约束并由合成测试核验

### Requirement: 失败隔离
系统 MUST 返回无正文泄漏的降级结果，保留确定性功能。

#### Scenario: polish.failure-fallback
- **WHEN** 模型超时、响应超预算或格式无效
- **THEN** 满足上述约束并由合成测试核验

### Requirement: 安全导出
系统 MUST 转义不可信文本且保留快照、来源引用、状态和缺口，不承诺撤回外部副本。

#### Scenario: polish.markdown-provenance
- **WHEN** 用户选择精简或详细导出
- **THEN** 满足上述约束并由合成测试核验

### Requirement: 可恢复编辑
系统 MUST 保留草稿，显示服务器内容，用户明确选择后才基于新版本重试。

#### Scenario: polish.highlight-conflict
- **WHEN** 并发保存发生版本冲突
- **THEN** 满足上述约束并由合成测试核验
