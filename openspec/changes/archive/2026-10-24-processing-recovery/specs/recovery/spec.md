# 处理进度与恢复

## ADDED Requirements

### Requirement: 失效执行者与中断发布不得污染当前处理结果
系统 MUST 将任务执行绑定到租约、尝试和当前输入修订。只有仍持有有效租约且输入修订未变化的执行者可以提交结果；发布提交后中断时，重放 MUST 能修复发布/后续任务状态，且不重复产生效果。

#### Scenario: recovery.stale-lease.preemption
- **GIVEN** Worker A 持有任务租约，租约到期后任务由 Worker B 重新领取
- **WHEN** Worker A 在 B 领取后提交旧结果
- **THEN** A 的提交被拒绝，B 的有效结果不被覆盖

#### Scenario: recovery.commit-and-outbox
- **GIVEN** 注释/发布已提交，但进程在后续任务确认前退出
- **WHEN** 恢复流程重放该发布
- **THEN** 发布与后续任务最终一致，且重放不重复创建可观察副作用

#### Scenario: recovery.cancelled-worker
- **GIVEN** 任务已取消，旧 Worker 尚在执行
- **WHEN** 旧 Worker 返回结果
- **THEN** 结果不发布，任务仍保持取消状态

#### Scenario: recovery.current-revision
- **GIVEN** 任务针对输入修订 R1 执行，期间输入更新为 R2
- **WHEN** R1 的 Worker 尝试提交
- **THEN** R1 结果不能成为当前修订的结果

### Requirement: 进度、重试和 Worker 状态必须可区分且限定范围
客户端 MUST 能区分任务阶段、尝试、当前修订、终态失败与运行中状态。重试 MUST 针对指定任务/范围和预期尝试；重复 request ID 返回同一重试结果，未知内部故障不得伪装为客户端参数错误。监督启动的任一必需进程意外退出时 MUST 报告失败并清理同组进程。关闭生成器 MUST 阻止新的生成请求，不抹除既有任务/候选状态。

#### Scenario: recovery.progress-scoped-retry
- **GIVEN** 会话中有两个失败任务，客户端提供其中一个任务的 ID、预期尝试和 request ID
- **WHEN** 客户端请求重试并再次提交相同 request ID
- **THEN** 只重试指定任务且第二次返回同一结果；其他任务尝试数不变

#### Scenario: recovery.retry-internal-error
- **GIVEN** retry 请求参数有效，但数据库执行发生内部错误
- **WHEN** 客户端提交重试请求
- **THEN** API 返回通用 500 内部错误，不把服务端故障误报为 400 或泄露底层错误文本

#### Scenario: recovery.worker-liveness
- **GIVEN** 本地启动器同时监督 Web 与 Worker
- **WHEN** Worker 在 Web 仍运行时意外退出
- **THEN** 启动器清理 Web 并以失败状态退出，不报告整体服务正常

#### Scenario: recovery.generator-off
- **GIVEN** 生成器被配置为关闭且已有任务/候选记录
- **WHEN** 新处理请求到达
- **THEN** 不生成新候选，既有记录仍可查询且状态不被删除
