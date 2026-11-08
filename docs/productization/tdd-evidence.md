# TDD 证据（Beta 产品化）

记录顺序即实际执行顺序；每项先出现可观察失败，再实现并跑窄测试。命令均在仓库根目录执行。

| 场景 ID | Red 命令与首次失败摘要 | Green 命令与结果 |
|---|---|---|
| productization.deploy-* | `go test ./internal/memory -run TestWALAllowsConcurrentReaderAndWriter`：`journal_mode="delete", want wal` | 启用 WAL + 并发写锁重试后通过；`TestOpenWaitsForConcurrentWriterLock` 通过 |
| productization.import-* | 先写 `internal/ingest/import_test.go`，`go test ./internal/ingest`：包不存在（编译失败） | 实现 `internal/ingest` 后 9 个测试通过（含 ffmpeg 真实格式） |
| productization.import-progress-projection | 先写 `TestProcessingSnapshotProjectsPhases`：`phase 1 = pending, want failed`（初始实现未读 attempt 错误） | 阶段映射与 `process.LastError` 后通过 |
| productization.web-workflow | `python3 scripts/run-product-browser.py`：`empty-onboarding` 超时（登录落在 `/worldline`） | 修正路由后仍失败：`.turn-row` 超时（管线完成后页面不刷新） |
| productization.web-workflow（续） | 同上，第二次失败 `waiting for locator(".turn-row")` | 完成后刷新 + 按 turn 数重挂载后 13/13 |
| productization.import-large-file | `scripts/product-audio-e2e.py --audio 180s.wav`：`BrokenPipeError`（profile 路由把 `/api/imports` 限制为 1 MiB） | 路由按路径选择 2 GiB 上限后 180s 上传通过 |
| decision-trail.scenario-* | 先写 24 个具名子测试：`topics=0 want 1`、`version conflict`、`reopen kept the decision confirmed` | 增加 orphan topic、把 `decision.review` 纳入版本、最新复核治理后 24/24 |
| decision-trail.review-* | `TestDecisionReviewRejectsStaleVersion` 初次返回 `nil`（Review 未校验 Snapshot 版本） | 版本/请求/证据校验后 7/7 |
| mcp-context.* | `TestMCPDecisionTrailByteBudget`：`serialized trail=1229 bytes > 512`（截断作用于副本） | 原地截断 + 改用 1 KiB 预算后 15/15 |
| mcp-context.profile-event-plane | `TestMCPProfileUsesProfileEventPlane` 先确认旧实现打开记忆库；修复后断言 `paths.Plane` | 通过 |
| productization.retrieval-* | 先写 `test_productization_retrieval.py`：报告缺指标/结果文件 | 生成 4 份 JSON 与报告后 3/3 |
| productization.demo-ten-steps | `beta-demo.py` 首跑：step 6 `state=unresolved` 误判、step 8 `X-Audio-Start-MS` 大小写 | 修正断言后 10/10 |

说明：`scripts/check_spec_test_map.py` 只验证锚点存在，不证明测试先写或覆盖充分；本表是唯一的过程证据。
