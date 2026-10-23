# Tasks

- [x] 核对租约抢占与发布恢复锚点，并将每个处理恢复场景映射到精确自动化测试。
- [x] 为失效执行者、当前修订、幂等重试、Worker liveness、generator off 和 retry 内部错误处理添加/确认行为测试。
- [x] 运行恢复相关 Go 测试；全量 `go test ./...`、`go test -race ./...` 和 `go vet ./...` 均通过；synthetic browser liveness/processing 状态案例通过。
- [x] 验证 retry 内部错误通过通用 500 响应，且不泄露底层错误：`TestProcessingRetryInternalFailureUsesGenericServerError` 通过。
- [x] 更新 Profile 启动及验证边界文档；实施状态和迁移 18 回滚限制记录在 `docs/implementation-validation.md`。
