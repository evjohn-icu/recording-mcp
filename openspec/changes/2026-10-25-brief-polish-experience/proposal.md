# Why
P0/P1 已提供确定性简报和 Highlights 闭环；补充用户授权的可选润色和导出体验。

# What Changes
- 增加默认关闭、逐次明确同意的候选润色适配器与 Web 入口。
- 增加精简/详细 Markdown 导出、下载及安全转义。
- 改进 Highlights 标签筛选、时间展示与版本冲突比较。

# Capabilities
## New Capabilities
- `brief-polish`: 有限资源、来源约束、候选状态和失效检查。
## Modified Capabilities
无冻结契约变更。

# Impact
影响 contextbrief、app、context-web 和 React。无数据库迁移，无正文缓存，无新依赖。性能调优不作为独立工作；仅复用已有限额，不重构检索。

补充：显式复用 Pi provider 的模型与鉴权配置，打通 local-start 启动参数及真实模型合成浏览器验证。
