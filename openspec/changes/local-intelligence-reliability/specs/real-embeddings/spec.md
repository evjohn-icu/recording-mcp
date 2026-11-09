# real-embeddings 规范增量

## ADDED Requirements

### Requirement: 角色化 embedding 调用
系统 SHALL 区分文档与查询 embedding；实现角色接口的 provider MUST 在请求中携带 `input_type`，不实现该接口的 provider MUST 回退到普通 `Embed`。

#### Scenario: embedding.role-query
- **WHEN** SQLite 检索器为查询生成向量
- **THEN** 角色 MUST 为 `query`；索引写入时角色 MUST 为 `document`

#### Scenario: embedding.local-provider
- **WHEN** 调用本地 OpenAI 兼容服务的 `/v1/embeddings`
- **THEN** 服务 MUST 接受字符串或数组输入、按输入顺序返回 1024 维向量、对 `input_type=query` 追加 Qwen3 查询指令，并拒绝超过 128 条输入、非回环绑定、浏览器 Origin 与未知模型

#### Scenario: embedding.failure-degrade
- **WHEN** embedding 服务不可用或返回错误
- **THEN** 检索 SHALL 降级为字面匹配并把该情况与"成功但空结果"区分记录，不得伪造成功

### Requirement: 冻结评测门禁
真实 embedding 候选 MUST 通过冻结校准门禁（Recall@5、MRR@10、无证据误报、假充分、p95 等）才可创建 selection lock；未通过时 MUST 保持 v1 + hash 为默认，且不得修改测试或夹具。

#### Scenario: embedding.frozen-gates
- **WHEN** 校准结果任一门禁不达标
- **THEN** 评测 MUST NOT 创建 holdout lock、MUST 记录失败检查并保持 `default_changed=false`

#### Scenario: embedding.no-promotion-without-gates
- **WHEN** 未配置真实评估器或评估器失败
- **THEN** 报告 MUST 把假充分率记为 `NOT_RUN` 而不是 0%，且真实 embedding 只能作为可选路径记录
