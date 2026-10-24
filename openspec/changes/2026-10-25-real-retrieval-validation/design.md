# Context
现有 v2 在完整读取和投影所有当前候选后才按 ConversationID 过滤；无关会话异常也可能阻断范围内查询。v1 明确保留历史 ConversationID 处理行为。

# Goals / Non-Goals
目标：v2 保持相同合格集合和排序，并提前过滤范围；Web 显式选择版本。非目标：改默认版本、自动修复真实数据、建立新事实/检索框架或上传全部语料。

# Decisions
将 v2 的会话过滤推到 SQL 来源读取阶段，仍完整评分范围内集合，不提前 top-k。shared helpers 若接收范围，v1 清空 ConversationID 以保留既有行为。stale/ambient/provenance 与确认人物规则不变。搜索页版本写入 URL，刷新保持，缺省为 v1。

# Risks / Trade-offs
真实诊断仅衡量自动拟题所指定来源的命中率，不等价于全部相关来源召回率或精确率。v2 并非对所有问题更好，因此不替用户切换默认值。临时副本含私人文本，存入受限目录并在完成后清理；公开记录仅保留汇总。

# Migration Plan
无 schema 迁移；不修改任何真实库，不自动启用新索引或默认版本。

# Open Questions
真实用户问题的人工相关性标注仍需后续积累，不阻断本次范围优化。
