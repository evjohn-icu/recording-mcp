# long-audio-reliability 规范增量

## ADDED Requirements

### Requirement: 窗口级检查点与续跑
ASR 任务 SHALL 按窗口计划分块执行，并把已提交窗口表示为持久化附件；重试 MUST 只处理未提交窗口，不得重复创建活跃附件或重复计费窗口。

#### Scenario: long-audio.window-resume
- **WHEN** 第一次尝试提交了前两个窗口后中断，随后重试
- **THEN** 重试 MUST 跳过已提交窗口、完成剩余窗口，且活跃附件不出现重复

#### Scenario: long-audio.coverage
- **WHEN** 窗口带 2 秒重叠
- **THEN** 覆盖时长 MUST 等于音频时长（活跃区间并集），不得等于窗口时长之和

#### Scenario: long-audio.partial-failure
- **WHEN** 某个窗口以类型化 runner 错误失败且上下文未取消
- **THEN** 转写状态 SHALL 为 `partial`，已完成与失败窗口计数 MUST 保留，且后续用正常 runner 重试时从失败窗口继续

#### Scenario: long-audio.duplicate-import
- **WHEN** 同一字节内容再次导入
- **THEN** 导入 SHALL 返回 `duplicate=true` 与既有会话/任务，且 MUST NOT 新建录音或附件

#### Scenario: long-audio.oom-actionable
- **WHEN** runner stderr 含 CUDA 显存不足特征
- **THEN** 错误码 MUST 为 `asr_oom`，动作文本 MUST 提示显存与替代方案

#### Scenario: long-audio.indexing-restart
- **WHEN** worker 在转写提交后、embedding 完成前被终止并重启
- **THEN** embed 任务 SHALL 在重启后完成，`indexing_status` 最终为 `ready`

### Requirement: 进度投影
`GetProcessingStatus` SHALL 返回期望/完成/失败窗口数、覆盖时长与最近错误；MUST NOT 用 `MAX(global_end_ms)` 推断完整性。

#### Scenario: long-audio.progress-projection
- **WHEN** ASR 处于部分完成状态
- **THEN** 状态响应 MUST 含 `asr_state`、`asr_expected_chunks`、`asr_completed_chunks`、`asr_failed_chunks`、`asr_coverage_ms`、`asr_duration_ms`，并与数据库投影一致
