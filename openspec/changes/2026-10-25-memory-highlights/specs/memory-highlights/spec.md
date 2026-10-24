# Spec Delta

## ADDED Requirements

### Requirement: 来源校验
系统 MUST 在事务内校验录音、turn、attachment 与当前 revision 一致，并存储稳定录音时间和证据双锚点。

#### Scenario: highlights.create-with-source
- **WHEN** 创建标记
- **THEN** 满足本需求的来源、安全及状态约束，并由对应合成测试验证

### Requirement: 请求幂等
系统 MUST 对相同 request_id 和相同 payload 只创建一次，不同 payload 返回冲突，删除或遗忘后不得重放复活。

#### Scenario: highlights.idempotent-replay
- **WHEN** 重复创建或不同内容重放
- **THEN** 满足本需求的来源、安全及状态约束，并由对应合成测试验证

### Requirement: 并发编辑
系统 MUST 使用 optimistic concurrency 拒绝覆盖，并让界面保留用户草稿。

#### Scenario: highlights.version-conflict
- **WHEN** 提交旧版本
- **THEN** 满足本需求的来源、安全及状态约束，并由对应合成测试验证

### Requirement: 修订失效
系统 MUST 保留标记并显示 needs_relink，禁止显示旧原话为当前证据；重新关联必须由用户显式执行。

#### Scenario: highlights.revision-needs-relink
- **WHEN** ASR revision 被取代
- **THEN** 满足本需求的来源、安全及状态约束，并由对应合成测试验证

### Requirement: 关注不是事实
系统 MUST 将标签视为关注度，不得确认来源陈述为真。

#### Scenario: highlights.manual-not-fact
- **WHEN** 用户创建或重新关联标记
- **THEN** 满足本需求的来源、安全及状态约束，并由对应合成测试验证

### Requirement: 永久遗忘
系统 MUST 清理标记和备注，包括无 turn_id 的重叠时间锚点，并清理托管副本及拒绝重放；无法排除重叠时保守清理。

#### Scenario: highlights.forget-purges-content
- **WHEN** 遗忘 turn、recording 或 conversation
- **THEN** 满足本需求的来源、安全及状态约束，并由对应合成测试验证

### Requirement: 退休屏障
系统 MUST 拒绝写入，清理标记及请求账本正文，并保持 Profile 隔离。

#### Scenario: highlights.retired-profile-blocks-writes
- **WHEN** Profile 退休后新增或重放
- **THEN** 满足本需求的来源、安全及状态约束，并由对应合成测试验证

### Requirement: 受控音频
系统 MUST 通过受管理 PCM 校验绝对录音时间、时长、可用性和遗忘状态，禁止客户端文件路径。

#### Scenario: highlights.audio-respects-source
- **WHEN** 播放标记区间
- **THEN** 满足本需求的来源、安全及状态约束，并由对应合成测试验证
