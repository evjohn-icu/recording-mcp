# Design

## Context

当前 main ba43cad，迁移最高 v18。复用当前 memory、projection、Evidence Context、Profile 与受管理音频安全边界。

## Goals / Non-Goals

完成 P0/P1 纵向功能；不实现可选 AI 润色、独立事实表、自动身份合并或检索调优。

## Decisions

Brief 读取当前来源实时生成，不持久化正文。Person 依赖当前 confirmed binding；Topic 用稳定 ID；Project 为显式 Topic/Conversation 并集，时间条件取交集。六个固定栏目、逐项证据、独立来源与陈述状态。默认预算 40/80/65536，最大 200/400/262144；稳定排序、去重和 SHA-256 快照，不调用外部模型。

API 使用应用层 0.1 契约并复用 session、CSRF、Origin 与错误封装。UI 并发冲突保留草稿。

## Risks / Trade-offs

遗忘遗漏、旧证据泄漏和 Profile 越权必须通过合成回归验证；无法确定来源边界时保守排除，不猜测未知时间。

## Migration Plan

Highlights 注册 v19 幂等迁移，Brief 无独立持久表。仅在临时数据库验证升级；不访问真实用户库。
