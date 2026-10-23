# Tasks

- [x] 核对证据范围回归锚点 `TestTopicScopeAlsoFiltersEmbeddedNoteEvidence`，并运行映射检查确认精确锚点存在。
- [x] 创建并冻结 160 条合成问题集：120 有证据、40 无证据，group 隔离的 50/25/25 划分；记录在首次评分后曾尝试改写且已恢复，当前 test split 不作为独立盲测结论。
- [x] 为冻结集分组、outage 与成功空结果区分、no-answer 分母添加精确自动化测试；部分超时统计不属于当前报告场景，单独保留为后续验证边界。
- [x] 实现并验证报告中的实际计数、precision、recall、延迟分位数及 no-answer 指标。
- [x] 运行离线 baseline 与 calibration candidate 对比；无候选通过质量门槛，默认阈值维持 0.05；真实 embedding provider 为 `NOT_RUN`，未发起外部调用。
