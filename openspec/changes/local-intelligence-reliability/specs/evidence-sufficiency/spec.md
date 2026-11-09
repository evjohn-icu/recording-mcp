# evidence-sufficiency 规范增量

## ADDED Requirements

### Requirement: 三种充分性状态
上下文包 SHALL 在 v2 检索下给出 `sufficient`、`insufficient` 或 `unknown`，且被引用证据 MUST 在最终预算证据内有效。

#### Scenario: sufficiency.three-states
- **WHEN** 以 20 个具名场景运行 v2 构建
- **THEN** 每个场景 MUST 得到其期望状态，且不可回答场景数不少于 5

#### Scenario: sufficiency.valid-refs
- **WHEN** 评估器返回不存在或预算外的证据引用
- **THEN** 状态 MUST 降级为 `unknown`，不得把无效引用当作证据

#### Scenario: sufficiency.no-false-sufficient
- **WHEN** 证据存在但缺少回答所需关键信息
- **THEN** 状态 MUST NOT 为 `sufficient`

#### Scenario: sufficiency.stale-forgotten
- **WHEN** 证据来自已删除/已忘记来源、过期转写修订或环境声
- **THEN** 该证据 MUST NOT 被引用为充分性依据

#### Scenario: sufficiency.unknown-time
- **WHEN** 决策时间未知
- **THEN** `valid_from` MUST 保持为空，不得推断时间

#### Scenario: sufficiency.unconfirmed-proposal
- **WHEN** 决策仅有未确认提案
- **THEN** 当前决策 MUST 为空，不得返回 `confirmed`

### Requirement: 界面证据可核对
检索界面 SHALL 展示充分性状态、对应证据文本、时间与来源会话链接，并对缺少评估器或降级给出原因。

#### Scenario: sufficiency.ui-trace
- **WHEN** 混合检索返回 v2 包
- **THEN** 页面 MUST 显示三种状态标签、被引用证据文本与可跳转的证据链接，并对 v1 或缺失字段不渲染该面板
