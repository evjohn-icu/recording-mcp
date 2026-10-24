# Why
经用户授权的真实来源只读诊断发现：主库缺少 embedding、v2 范围内查询仍先处理范围外元数据，Web 未暴露已有 v2 开关。仅做有证据的范围过滤优化与显式版本选择。

# What Changes
- v2 在元数据投影前应用会话硬范围，保留排名、预算和当前来源规则。
- Web 混合搜索可显式选择 v1/v2，默认 v1 不变。
- 使用原有 build_context 与临时副本进行真实来源诊断，不构建另一套检索引擎。

# Capabilities
## New Capabilities
- `scoped-retrieval`: 提前应用已有会话范围及可选版本入口。
## Modified Capabilities
无冻结协议变更。

# Impact
影响 memory 检索投影与 SearchPage。没有数据库迁移、HNSW/JEV 调优或事实确认；原库只读，临时副本中的 hash 索引仅用于对照。自动拟题结果不冒充人工标注召回率。
