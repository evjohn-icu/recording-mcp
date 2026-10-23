# 语义关系候选复核

## ADDED Requirements

### Requirement: 复核决策必须区分关系接受与无效旧结论
系统 MUST 将 accept、reject、insufficient evidence 分别持久化。复核绑定候选与来源版本及 request ID；重放相同 ID 幂等。accept 只确认两条记忆之间的关系建议，不确认其来源事实、人物身份或现实状态。来源变化使旧决定失效；reopen 创建新的复核版本；已忘记来源不得由旧候选/重放恢复。

#### Scenario: review.relation-reject-recorded
- **GIVEN** 一条语义关系建议被复核者拒绝
- **WHEN** 保存 reject 决定
- **THEN** 关系建议记录为拒绝，拒绝仅作用于该建议，不改变事实或来源记忆；此场景不要求拒绝理由字段

#### Scenario: review.semantic-confirmation-distinct
- **GIVEN** 候选证据结构有效但尚无人工语义确认
- **WHEN** 查询关系复核状态
- **THEN** 它仍需复核；accept 仅将这条关系建议标记为已接受，不将被引用事实或人物身份标记为已确认

#### Scenario: review.revision-invalidates-review
- **GIVEN** 候选已基于来源版本 S1 被接受
- **WHEN** 来源变更为 S2
- **THEN** S1 的决定标记为过期，不得作为 S2 的有效确认

#### Scenario: review.reject-insufficient-distinct
- **GIVEN** 复核者无法从现有证据判断关系
- **WHEN** 记录 insufficient evidence
- **THEN** 该状态不同于 reject，且请求/服务错误不能被记为证据不足

#### Scenario: review.reopen-versioned
- **GIVEN** 候选已有已接受决定
- **WHEN** 授权复核者显式 reopen 并提交新 request ID
- **THEN** 建立新的复核版本，历史决定仍可审计

#### Scenario: review.request-id-replay
- **GIVEN** 一个复核请求已处理
- **WHEN** 相同 request ID 和内容再次提交
- **THEN** 返回既有结果且不新增复核事件

#### Scenario: review.forget-suppresses-source
- **GIVEN** 复核建议引用的来源已被 forget
- **WHEN** 旧候选或复核请求再次提交
- **THEN** 已忘记来源不得因此恢复或重新发布
