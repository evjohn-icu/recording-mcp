# Tasks

- [x] 核对候选列表过滤锚点；复核 lifecycle 场景均映射到针对性的自动化测试。
- [x] 测试 relation-only accept、来源修订失效、reject 与 insufficient 的区别、reopen、新 request ID 重放及 forget 来源清理。
- [x] 验证请求错误不会被计为 insufficient evidence；reject/insufficient 不改变建议关系或来源事实；forget 会清除旧建议与复核事件内容。
- [x] 执行 `go test ./internal/app -run 'TestWorldline(SemanticReview|ReviewAccept)|TestDurableWorldlineCache' -count=1`；全量 Go/race/vet 均通过，见 `docs/implementation-validation.md`。
- [x] 记录质量边界：accept 只确认关系建议，不确认来源事实、人物身份或现实状态；本机 JEV 质量结果为 FAIL，见 `docs/worldline-relation-quality.md`。
