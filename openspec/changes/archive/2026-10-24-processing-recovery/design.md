# 设计：处理进度与恢复

## Context

处理任务、租约、重试和发布恢复已有实现证据；`/tmp/recording-plan-tasks/review-processing-report.md` 是只读审查，不是测试结果。审查指出 local-start 仅等待 Web、retry handler 将未知错误作为 400 的风险，以及 one-shot 心跳短暂陈旧；这些是待改动/验证事项，不可在基线中写成已修复。

## Decisions

- 进度与重试必须有明确查询范围及任务/修订标识；重试以预期尝试和 request ID 防止误重试及重复提交。
- 处理结果只能属于当前输入修订；旧租约返回不得覆盖新运行。
- Worker liveness 必须表达实际进程存活而非仅存在未过期心跳；启动监督需覆盖 worker 与 web 两个进程。
- 生成器关闭只停止新增生成，不得隐式删除已存候选或阻断已入队任务的状态可见性；其具体停机语义需测试确认。
- 发布事务后重放须以确定性输出修复派生状态，避免重复创建或静默丢失后续任务。
- ingest-v1 冻结契约不变；Context Layer 0.1 仍是未来草案。

## Risks

- 仅有 `TestLeasePreemption` 和 `TestPublishHandlerRecoversAfterAnnotationCommit` 映射为 covered；取消返回、worker 监督、生成器关闭、修订隔离及精确 API 错误映射仍需场景测试。
- test-map 只检查映射锚点存在，不能证明充分覆盖、运行通过或遵循 TDD。不得据此声称验证完成。
