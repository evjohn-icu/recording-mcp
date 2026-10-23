# Tasks

- [x] 核对 `TestRecordDecisionRunIdempotent` 锚点，并为 durable semantic run 记录精确场景映射。
- [x] 为配置变化、失效租约、128 个 pair 批次、45 秒评估上下文及 source-only 恢复添加行为测试。
- [x] 验证缓存键包含 source note/version、真实 model/weights 与 rubric identity；rubric/model 变更均有独立缓存失效测试。
- [x] 运行 `go test ./internal/app -run 'TestDurableWorldline|TestWorldlineEmptyFrozenSnapshot|TestCancelledWorldlineLease|TestWorldlineAnalysis' -count=1` 与 `go test ./internal/decision ./cmd/context-decision -count=1`；全量 Go/race/vet 也通过。
- [x] 本地 JEV 合成关系基准校验通过但准确率未通过（calibration/test 均 37/60）；不把协议通过或 NOT_RUN 外部 provider 说成模型质量通过，详见 `docs/worldline-relation-quality.md`。
