# Design

## Context

当前 main ba43cad，迁移最高 v18。复用当前 memory、projection、Evidence Context、Profile 与受管理音频安全边界。

## Goals / Non-Goals

完成 P0/P1 纵向功能；不实现可选 AI 润色、独立事实表、自动身份合并或检索调优。

## Decisions

Highlights 使用 memory_highlights 与无正文请求账本，事务内校验来源和版本。v19 为新表创建退休屏障，永久遗忘包含区间重叠、预览摘要、账本清理和托管备份。时间锚点为完整录音毫秒；旧 revision 仅保留标记，需人工重新关联。

API 使用应用层 0.1 契约并复用 session、CSRF、Origin 与错误封装。UI 并发冲突保留草稿。

## Risks / Trade-offs

遗忘遗漏、旧证据泄漏和 Profile 越权必须通过合成回归验证；无法确定来源边界时保守排除，不猜测未知时间。

## Migration Plan

Highlights 注册 v19 幂等迁移，Brief 无独立持久表。仅在临时数据库验证升级；不访问真实用户库。
