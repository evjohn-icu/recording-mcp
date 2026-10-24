# Memory Highlights / Context Brief 验证记录

本轮仅使用临时 SQLite、合成转写、模拟 PCM 和临时 Profile。未读取真实录音或真实用户数据库。

## 基线

- checkout/main/远端 main：ba43cad4193ab0f012b0d66f9f206f355ccf1fdf；开始时工作树干净。
- 分支：feat/memory-highlights-context-brief；迁移 v18 → v19。
- P2 生成式 Adapter 未实施；确定性模式不调用外部模型。

## 测试先行记录

- 子代理先新增 `internal/contextbrief/brief_test.go`，执行 `go test ./internal/contextbrief`，因 Build/Request 等尚未实现而编译失败；测试提交 5633506。
- 子代理先新增 `internal/memory/highlights_test.go`，执行 `go test ./internal/memory -run 'TestHighlight' -count=1`，因 Highlight API 尚未实现而编译失败；测试提交 7473084。
- HTTP 契约测试提交 fb3ec79；首次执行被同期 memory 未完成符号阻塞，不能作为 HTTP 行为红灯证据。
- 后续安全回归及浏览器测试在实现迭代期间增加；不能声称所有测试都先于实现。

## 中间执行结果

- `go test ./internal/memory ./internal/contextbrief ./internal/app ./internal/web ./cmd/context-memory`：FAIL。memory 测试发现合成来源 ID 使用错误；MCP 精确工具清单需要增加两个只读工具。该次 app/web/contextbrief 通过，但不是最终全量结论。
- `python3 scripts/run-local-browser-tests.py --help`：PASS，确认支持 --skip-build、--artifacts、--browser。
- `openspec` 裸命令：环境未安装全局可执行文件；使用仓库可用 `npx openspec`。

最终命令结果将在验收完成后更新；未运行项目不得标记 PASS。

## 安全审查

独立 Luna 审查与主 Agent 集成审查均已进行；实际修复包括：

- 未知 Highlight 的错误映射由 500 修正为 404；跨 Profile 同 ID 不读取另一库。
- 读取标记时再次校验 tombstone；旧 attachment/revision 不展示原话，ambient 原话不进入结果。
- 请求删除/遗忘后清除 payload 摘要，保留最小重放拒绝标识；退休清理新表并保留退休屏障。
- 纯时间标记及带 turn 的 range 必须让全部重叠证据满足 Brief 硬范围，不能借一个范围内 turn 带入范围外备注。
- Brief 原来的全库待关联标记计数已移除，避免范围外标记改变范围内快照；note、link 和 highlight 查询按已选择来源收窄。
- 新增两独立 SQLite 连接的 CAS 回归，先观察到 SQLITE_BUSY 失败（测试提交 d985143），再通过事务开始时预取写锁修复；重复三轮专项测试通过。
- 新增 Profile 清理测试发现 annotation_history 外键清理顺序问题，现先清历史再清 conversations。
- 录音遗忘合成链路检查托管备份删除、SQLite/WAL 中备注明文不残留、账本摘要为空。
- API 错误不回显备注和原话；新代码不记录私密正文。没有新增依赖、事件队列、向量数据库或自动确认事实路径。

## 测试过程中的已知波动

一次全量普通测试与浏览器/构建并行时，既有 `TestWorldlineMutationVersionIsAtomicAcrossServiceInstances` 返回 SQLITE_BUSY。随后该原有测试连续 10 次独立复测通过；没有删除、放宽或跳过它的断言。此项波动与新 Highlights 的两连接 CAS 回归分别记录，不以专项通过替代全量结果。

## 范围与限制

- P2 生成式润色 Adapter：NOT_IMPLEMENTED，按计划不启动；正式基线完全确定性，无模型依赖。
- 真实私人录音、真实用户数据库、外部模型质量和云端部署：NOT_RUN，本次未接触。
- Brief 保留当前原话及已保存的矛盾线索，不自动判定真伪；没有新的真值引擎。
- 单次音频最多 120 秒；范围内来源读取有 5000 轮次保护上限，超出明确标记截断。字节预算可返回带标记的短摘录。
- 迁移只在合成临时数据库验证，不能宣称真实用户数据库已升级。旧二进制不理解新表的遗忘规则，不应用于 v19 数据库的隐私清理。

## 最终验证结果

| 实际命令 | 结果 |
| --- | --- |
| `make spec-validate` | PASS：10 项 OpenSpec，57 场景映射；保留仓库原有 workflow.no-tdd-inference pending |
| `go test ./...` | PASS：最终完整重跑通过；过程中一次既有并发测试失败见上文 |
| `go test -race ./...` | PASS：最终完整重跑通过 |
| `go vet ./...` | PASS |
| `make build web` | PASS |
| `cd web && npm run typecheck` | PASS |
| `python3.14 scripts/run-local-browser-tests.py --skip-build --browser /usr/bin/google-chrome` | PASS：15 项新功能检查，以及既有 local-v2 / worldline / processing 套件 |
| `python3 -m unittest discover -s scripts -p 'test_browser_runner.py' -v` | PASS：3 项；既有测试提示未关闭 socket 的 ResourceWarning，不影响退出状态 |
| `go test ./internal/app -run TestWorldlineMutationVersionIsAtomicAcrossServiceInstances -count=10` | PASS：10 次独立复测 |
| `go test ./internal/memory -run TestHighlightCASAcrossIndependentStoreConnections -count=3` | PASS：跨连接写锁修复后重复验证 |
| `git diff --check` | PASS |
| `git diff --name-only ba43cad -- contracts/ingest-v1/` | PASS：无输出，冻结契约无修改 |

浏览器报告（全部为合成数据）：`/tmp/recording-browser-k66799kt/highlights-brief-browser-results.json`，包含 15 项 PASS 与空 page_errors。最终重复构建的浏览器目录为 `/tmp/recording-browser-9ahvyh7o`；15 项新功能报告保存在仓库 `docs/evals/highlights-brief-browser.json`。

两个 OpenSpec 变更各 5/5 个实施任务完成；未归档，便于审阅。运行方式、API/MCP 与升级说明见 `docs/memory-highlights-context-brief.md`。所有实现与测试均在 feature branch 提交，未合并 main、未推送远端。

## 文件清单

A 表示新增，M 表示修改；不包含生成的 bin/web/dist 或临时浏览器 Profile。

```text
M	README.md
M	cmd/context-memory/main.go
M	cmd/context-memory/main_stdio_test.go
M	cmd/context-memory/main_test.go
A	docs/evals/highlights-brief-browser.json
A	docs/memory-highlights-context-brief-validation.md
A	docs/memory-highlights-context-brief.md
A	internal/app/highlight_audio.go
A	internal/app/highlight_audio_test.go
A	internal/app/highlights.go
A	internal/app/highlights_brief_integration_test.go
M	internal/app/worldline.go
A	internal/contextbrief/brief.go
A	internal/contextbrief/brief_scope_test.go
A	internal/contextbrief/brief_test.go
M	internal/contextbuild/scope.go
M	internal/memory/forget.go
M	internal/memory/forget_profile.go
A	internal/memory/highlights.go
A	internal/memory/highlights_test.go
M	internal/memory/transcript_correction.go
M	internal/memory/turnscope.go
A	internal/migrations/highlights.go
A	internal/migrations/highlights_test.go
M	internal/migrations/migrations.go
M	internal/migrations/migrations_test.go
M	internal/migrations/processing_runtime_test.go
M	internal/migrations/profile_retirement.go
A	internal/projection/note_input.go
M	internal/web/audio.go
A	internal/web/highlights.go
A	internal/web/highlights_brief_test.go
M	internal/web/server.go
A	openspec/changes/2026-10-25-context-brief/.openspec.yaml
A	openspec/changes/2026-10-25-context-brief/design.md
A	openspec/changes/2026-10-25-context-brief/proposal.md
A	openspec/changes/2026-10-25-context-brief/specs/context-brief/spec.md
A	openspec/changes/2026-10-25-context-brief/tasks.md
A	openspec/changes/2026-10-25-memory-highlights/.openspec.yaml
A	openspec/changes/2026-10-25-memory-highlights/design.md
A	openspec/changes/2026-10-25-memory-highlights/proposal.md
A	openspec/changes/2026-10-25-memory-highlights/specs/memory-highlights/spec.md
A	openspec/changes/2026-10-25-memory-highlights/tasks.md
M	openspec/test-map.json
A	scripts/highlights-brief-browser.py
M	scripts/run-local-browser-tests.py
M	web/src/App.tsx
A	web/src/api/brief.ts
A	web/src/api/highlights.ts
M	web/src/api/types.ts
M	web/src/components/AudioBar.tsx
A	web/src/components/HighlightPanel.tsx
A	web/src/pages/BriefPage.tsx
M	web/src/pages/DetailPage.tsx
```
