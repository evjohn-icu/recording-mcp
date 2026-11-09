# release-validation 规范增量

## ADDED Requirements

### Requirement: 12 步产品演示
发布验证 SHALL 通过自动化脚本完成 12 步端到端演示，包含启动、能力检查、导入、阶段轮询、转写与证据核对、检索、跨会话记忆、充分性、决策轨迹、重启、MCP 查询与状态完整性确认。

#### Scenario: release.demo-twelve
- **WHEN** 设置 `RUN_SPRINT3_DEMO=1` 运行演示测试
- **THEN** 脚本 MUST 逐步记录 PASS/FAIL 与命令，12 步全部通过才退出 0；默认单元测试运行 MUST 记为 skipped 而不是 passed

### Requirement: 发布命令可机读
发布报告的命令记账 SHALL 以 `docs/productization/release-commands.json` 为准，每个必需命令 MUST 有状态，`NOT_RUN` MUST 附原因。

#### Scenario: release.regression-commands
- **WHEN** 校验命令清单
- **THEN** 每个必需命令 MUST 存在且状态只能是 `PASS`、`FAIL` 或 `NOT_RUN`

#### Scenario: release.not-run-accounting
- **WHEN** 某命令状态为 `NOT_RUN`
- **THEN** 其 `reason_if_not_run` MUST 非空
