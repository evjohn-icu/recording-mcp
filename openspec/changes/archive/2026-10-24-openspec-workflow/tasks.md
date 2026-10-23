# Tasks

- [x] 建立 OpenSpec 与场景映射的初始校验记录；校验结果与自动化测试结果分开记录。
- [x] 运行 `make spec-validate` 验证当前 7 项 OpenSpec 工件及 35 个场景映射；未映射场景检查通过，唯一 pending 为不可自动证明的 TDD 流程原则。
- [x] 对本轮场景映射、检索故障区分及检索指标行为执行对应测试；映射检查不替代测试。
- [x] 人工检查验证说明：映射/CLI 仅证明结构与锚点，不证明 TDD。没有提交充分的逐测试时间线，因此 `workflow.no-tdd-inference` 继续标为自动化 pending。
- [x] 检查冻结 ingest-v1 契约文件未改动；`openspec/specs/processing-baseline/spec.md` 与 `docs/specs/README.md` 仍将 Context Layer 0.1 明确为未来草案。
