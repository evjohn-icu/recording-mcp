# 当前处理能力基线

状态：仅记录当前代码、测试和运行文档有直接证据的行为，不构成未来功能承诺。`contracts/ingest-v1/` 是采集与同步协议的冻结权威。`docs/specs/` 中 Context Layer 0.1 是未来领域草案，不表示当前实现兼容该契约。

## Purpose

为后续变更提供可复查的实现现状基线；未在此列出的提案行为仍是待实现或待验证事项，不应因出现在提案中而视为当前能力。

## Requirements

### Requirement: 原始输入与派生记忆可追溯
系统 SHALL 持久保存接收的原话及来源附件；派生注释以版本历史记录，并可引用证据轮次。依据：`docs/context-layer-runtime.md`、`docs/processing.md` 及 `internal/memory`、`internal/pipeline` 相关测试。

#### Scenario: 查询记忆证据
- **GIVEN** 已保存来源材料并生成派生记忆
- **WHEN** 查询当前派生记忆的证据
- **THEN** 返回的记忆可追溯至来源材料和证据轮次

### Requirement: 处理任务和租约持久化
系统 SHALL 将处理任务、执行尝试、重试状态和分析覆盖写入 SQLite，并通过租约约束任务执行者。依据：`docs/processing.md` 与 `internal/process/process_test.go`。本基线不承诺所有 Worker 生命周期或取消边界已覆盖。

#### Scenario: 过期租约重新领取
- **GIVEN** Worker 持有的任务租约已经过期
- **WHEN** 另一 Worker 领取该任务
- **THEN** 任务可重新领取，失效租约持有者不能确认其旧结果

### Requirement: 检索降级有可观察区别
系统 SHALL 支持混合检索及词法回退；语义服务不可用或索引为空时，运行文档描述响应可标记降级原因。依据：`docs/context-layer-runtime.md`、`docs/hybrid-retrieval.md`。质量评测与故障/成功空结果完整区分仍见对应变更提案，不在此宣称已验证。

#### Scenario: 语义索引不可用
- **GIVEN** 语义索引不可用
- **WHEN** 发起检索
- **THEN** 系统可返回词法结果并标记降级原因
