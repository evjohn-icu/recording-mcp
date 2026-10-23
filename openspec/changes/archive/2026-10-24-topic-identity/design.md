# 设计：Topic 身份与历史

## Context

现有场景覆盖旧成员不应带出当前内容、修订重建移除被替代成员。`topic.same-name-not-merge` 仍 pending。Topic 标题是显示信息，不应据此推导稳定身份；同一主题以不同标题呈现也不应自动拆分。

## Decisions

- 使用稳定 Topic ID 表示身份，display title 及历史标题作为可变元数据。
- 同一标题不自动合并不同 Topic；不同标题只有显式关联/合并决定后才共享身份。
- 撤销某成员关系只撤销该关系，不删除同主题其他人物、事实或原始证据。
- 当前检索只显示当前有效成员的当前内容；历史标题可用于审计和解析旧引用，但不能令失效成员重新进入当前结果。
- 不把 ingest-v1 的 conversation/recording identity 改为 Topic identity。

## Risks

历史标题解析、显式不同标题归组和局部撤销保留语义尚无完整映射测试；不得以当前 stale-membership 测试替代这些验证。OpenSpec 映射不证明测试充分或先写测试。
