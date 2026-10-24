# Memory Highlights / Context Brief 验证记录

本轮仅使用临时 SQLite、合成转写、模拟 PCM 和临时 Profile。未读取真实录音或真实用户数据库。

## 基线

- checkout/main/远端 main：ba43cad4193ab0f012b0d66f9f206f355ccf1fdf；开始时工作树干净。
- 分支：feat/memory-highlights-context-brief；迁移 v18 → v19。
- 此节记录 P0/P1 交付（691d186）；当时 P2 未实施。后续 P2 结果见文末补充。

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

- P0/P1 验收时 P2 为 NOT_IMPLEMENTED；后续已补充，详见文末。确定性正式基线仍无模型依赖。
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

## P2 增量验证（2026-10-25）

起点为干净工作树 `691d186`，继续同一 feature branch。增量 OpenSpec 为 `openspec/changes/2026-10-25-brief-polish-experience/`。不追加迁移，schema 仍为 v19。

### 测试先行与中间结果

- 应用层 consent/default-off/snapshot 测试先执行 `go test ./internal/app -run TestBriefPolish -count=1`：FAIL（缺少新符号），测试提交 `3aae0c1`；实现后 PASS。
- Web 测试 `go test ./internal/web -run TestBriefPolishRoutesConsentAndCSRF -count=1`：FAIL（新 config 路由 404），测试提交 `5b8b938`；注册路由后 PASS。
- 配置测试 `go test ./cmd/context-web -run TestBriefPolisherConfigurationIsExplicit`：FAIL（缺少配置函数），测试提交 `30f0e95`；实现后 PASS。
- 适配器第一轮符号缺失红灯由子代理执行；证据成员校验补测确实失败（接受不在 brief 的 evidence），再修复通过。对无效引用的负例已改为有效 proposed 状态，避免被错误状态提前拦截而掩盖引用校验。
- 主 Agent 增加 `TestPolishRejectsSupersededOrUnavailableEvidence`，先观察到接受 superseded 证据的 FAIL，补充当前状态校验后 PASS；随后修正合成 fixture 补齐有效 current 状态，并执行普通/race 测试通过。
- 真实响应省略空范围数组导致导出失败的回归已补齐；格式和异步 UI 修改并非每项都有实现前失败测试。Highlights UI 最初直接修改后 typecheck，没有声称它完成了测试先行。
- 首轮浏览器新增 P2 检查通过，既有复制检查失败：脚本测试精简下载后未恢复详细模板，旧断言要求「来源说明」。修正脚本状态，不删除旧断言。
- 误用 `python3 -m unittest discover -s scripts -p 'test_local_browser_runner.py'`：NO TESTS RAN（退出 5），不计 PASS。改用实际文件 `test_browser_runner.py` 后 3 个测试 PASS（既有 unclosed socket ResourceWarning 仍存在）。

### 已执行命令

| 命令 | 结果 |
| --- | --- |
| `make spec-validate` | PASS：11 items，63 scenarios；仅既有 workflow.no-tdd-inference pending |
| `go test ./...` | PASS |
| `go test -race ./...` | PASS |
| `go vet ./...` | PASS |
| `make build web` | PASS |
| `go test -race ./internal/contextbrief` | PASS：最后的 current 状态补强后重新执行 |
| `go vet ./internal/contextbrief` | PASS：最后的补强后重新执行 |
| `cd web && npm run typecheck` | PASS |
| `cd web && node --experimental-strip-types --test src/api/brief-markdown.test.mjs` | PASS：4/4 |
| `python3.14 -m unittest discover -s scripts -p 'test_browser_runner.py'` | PASS：3/3，含既有 ResourceWarning |
| `git diff --check` | PASS |

全量 Go 命令在最后补强前已通过；补强后追加该包 ordinary/race/vet，并重建 context-web。全部数据都是合成输入，浏览器模型端点为 loopback 假服务。

### P2 专项安全审查

独立子代理与主 Agent 均审查；验证调用前后重建快照，修订/遗忘时不发布旧候选；另一 Profile 的私密标记不进入模型输入；确认 speaker binding 与硬范围仍由原 Builder 强制。候选只接受原 entry 的完整 current evidence 集合，状态仅 proposed；不写 Memory/Worldline、不保留隐私缓存。客户端不能传端点、正文或 Profile 切换参数。端点/密钥只由启动配置控制，默认关闭、逐次同意、禁重定向、超时和大小上限，错误不含模型响应正文。UI 草稿在 CAS 冲突时保留，显式查看新版后重试；保存中禁用本条编辑，避免清除新输入。Markdown 对不可信文本转义，候选与原简报分开。

未变更 ingest-v1，未新增依赖。结构校验不能证明模型文字真实，故候选需人工核对；外部模型质量/真实服务互通、真实用户数据与部署为 NOT_RUN。没有独立性能调优项目，取消过期请求和资源预算属于本次体验改进。复制或明确发送给外部服务的内容不能由本地遗忘撤回。


### P2 浏览器最终结果

`python3.14 scripts/run-local-browser-tests.py --skip-build --browser /usr/bin/google-chrome`：PASS，退出 0。Highlights/Brief 21/21，page_errors=[]；其余已有 local-v2、Worldline、processing-poll 浏览器流程均完成。最终目录 `/tmp/recording-browser-d_cp6yl_/`，机器可读结果已更新到 `docs/evals/highlights-brief-browser.json`。首轮 20/21 的脚本状态问题保留在 `/tmp/recording-browser-bf24rnqh/`。假模型第二次故意返回 503 用于验证降级，非外部模型调用。

新增检查包含：标签筛选空态与服务器冲突信息、未同意前模型调用数为零、同意后独立 proposed 候选可展开证据、模型失败不改变确定性内容、精简/详细文件真实下载。原有音频、转写修订、重新关联和永久遗忘断言保留且通过。

### P2 文件清单

- `cmd/context-web/brief_polish.go`
- `cmd/context-web/brief_polish_test.go`
- `cmd/context-web/main.go`
- `docs/evals/highlights-brief-browser.json`
- `docs/memory-highlights-context-brief-validation.md`
- `docs/memory-highlights-context-brief.md`
- `internal/app/brief_polish.go`
- `internal/app/brief_polish_safety_test.go`
- `internal/app/brief_polish_test.go`
- `internal/app/service.go`
- `internal/contextbrief/polish.go`
- `internal/contextbrief/polish_test.go`
- `internal/web/highlights.go`
- `internal/web/highlights_brief_test.go`
- `openspec/changes/2026-10-25-brief-polish-experience/.openspec.yaml`
- `openspec/changes/2026-10-25-brief-polish-experience/design.md`
- `openspec/changes/2026-10-25-brief-polish-experience/proposal.md`
- `openspec/changes/2026-10-25-brief-polish-experience/specs/brief-polish/spec.md`
- `openspec/changes/2026-10-25-brief-polish-experience/tasks.md`
- `openspec/test-map.json`
- `scripts/highlights-brief-browser.py`
- `scripts/run-local-browser-tests.py`
- `web/src/api/brief-markdown.test.mjs`
- `web/src/api/brief-markdown.ts`
- `web/src/api/brief.ts`
- `web/src/components/HighlightPanel.tsx`
- `web/src/pages/BriefPage.tsx`
- `web/src/styles.css`
