# 语义任务持久化与 Worldline

## ADDED Requirements

### Requirement: durable semantic run 必须以版本化输入幂等恢复
语义任务 MUST 绑定 source snapshot、模型权重版本和 rubric 版本；同一版本的重复请求幂等，配置改变必须产生不同缓存键。取消或租约失效的执行者不得提交结果。

#### Scenario: semantic-job.decision-run-idempotency
- **GIVEN** 相同决策输入及配置已记录一次运行
- **WHEN** 相同运行请求再次提交
- **THEN** 返回既有运行结果，不重复记录决策

#### Scenario: semantic-job.config-version-key
- **GIVEN** 输入快照相同，但模型权重或 rubric 版本改变
- **WHEN** 创建语义任务
- **THEN** 使用不同缓存/任务键，不复用旧版本结果

#### Scenario: semantic-job.cancelled-lease-rejected
- **GIVEN** 任务已取消或其租约已被新执行者取代
- **WHEN** 旧执行者返回输出
- **THEN** 输出不能覆盖有效运行结果

### Requirement: Worldline 分批、时限与恢复范围必须确定
Worldline MUST 每批至多处理 128 个候选，每一对实体的评估窗口不得超过 45 秒。恢复 300 个候选时，只有 source version 改变的候选重新计算；缓存结果仅在 source snapshot、模型权重及 rubric 全部一致时可用。

#### Scenario: semantic-job.worldline-batch-window
- **GIVEN** 有 129 个候选待处理，且每对实体输入均在评估窗口内
- **WHEN** 建立 Worldline 批次
- **THEN** 首批不超过 128 条，剩余候选进入后续批次，单对窗口不超过 45 秒

#### Scenario: semantic-job.cache-model-rubric
- **GIVEN** source snapshot 相同且存在缓存结果
- **WHEN** 模型权重或 rubric 版本与缓存版本不同
- **THEN** 缓存不命中并执行新版本评估

#### Scenario: semantic-job.resume-source-changed-only
- **GIVEN** 300 条候选任务中只有若干候选的 source version 改变
- **WHEN** 恢复任务
- **THEN** 只重算 source changed 项，其余仅复用版本完全匹配的缓存结果
