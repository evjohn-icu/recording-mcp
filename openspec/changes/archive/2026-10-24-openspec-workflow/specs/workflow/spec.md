# 可验证的规范驱动工作流

## ADDED Requirements

### Requirement: 场景与验证证据必须分别记录
行为场景 MUST 使用稳定 ID 和可观察 GIVEN/WHEN/THEN。每个场景在 test-map 中标记 covered 或 pending；covered 仅表示精确测试文件/锚点存在。结构校验、映射检查、测试执行结果及测试先于实现的过程证据 MUST 分别陈述，不得互相推导。

#### Scenario: workflow.scenario-map.anchor-exists
- **GIVEN** 场景 ID 映射到存在的测试文件和精确函数锚点
- **WHEN** 运行映射检查
- **THEN** 检查确认锚点存在，但不声称测试已运行或行为充分覆盖

#### Scenario: workflow.missing-scenario-fails
- **GIVEN** 规范中新增场景但映射文件没有该 ID
- **WHEN** 运行映射检查
- **THEN** 检查失败并指出缺失的场景 ID

#### Scenario: workflow.pending-is-visible
- **GIVEN** 新行为尚无对应自动化测试
- **WHEN** 场景映射标记为 pending 并运行校验
- **THEN** 校验显示该 pending 场景，不借用相邻测试宣称覆盖

#### Scenario: workflow.no-tdd-inference
- **GIVEN** 测试映射锚点存在但没有测试先于实现的记录
- **WHEN** 汇总验证结果
- **THEN** 只报告锚点存在，不声称测试优先开发已证明
