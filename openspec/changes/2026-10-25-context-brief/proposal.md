# Proposal

## Why

用户需要从有来源的录音记忆中保留重点并恢复沟通背景，同时遵守修订、遗忘和事实状态边界。

## What Changes

- 实现 context-brief 的应用契约、核心逻辑、Web 和只读 MCP 入口。
- 增加合成夹具、安全回归和真实 API 浏览器验证。

## Capabilities

### New Capabilities

- `context-brief`：来源可核对、受范围及遗忘约束的用户功能。

### Modified Capabilities

无。

## Impact

影响 memory、app、web、MCP 与 React；Highlights 新增 SQLite v19。不修改 ingest-v1，不新增外部服务。
