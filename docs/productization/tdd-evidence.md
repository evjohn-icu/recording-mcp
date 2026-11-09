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
| productization.import-progress-projection（>100 jobs） | `--durations 7200` 的音频 E2E 卡在 `current_stage=ingest`（快照只取最新 100 个 job，ingest/asr 被挤出） | 按 stage 直查最新 job 后 `TestProcessingPhasesSurviveMoreThanListLimitJobs` 通过；E2E 完成 |
| productization.web-workflow（profiles 路由） | 3 分钟音频上传 `BrokenPipeError`（外层 profile 路由把 `/api/imports` 限制为 1 MiB） | 路由按路径选择 2 GiB 上限后 E2E 通过；新增 `TestProfilesAllowsLargeImportUpload` |
| mcp-context.profile-event-plane | `TestMCPProfileUsesProfileEventPlane` 先确认旧实现打开记忆库；修复后断言 `paths.Plane` | 通过 |
| productization.retrieval-* | 先写 `test_productization_retrieval.py`：报告缺指标/结果文件 | 生成 4 份 JSON 与报告后 3/3 |
| productization.demo-ten-steps | `beta-demo.py` 首跑：step 6 `state=unresolved` 误判、step 8 `X-Audio-Start-MS` 大小写 | 修正断言后 10/10 |

说明：`scripts/check_spec_test_map.py` 只验证锚点存在，不证明测试先写或覆盖充分；本表是唯一的过程证据。

## Sprint 3（local-intelligence-reliability）

只记录实际观察到的红/绿。未观察到红阶段的项不计为 TDD 证据，并明确标注。

| 场景 ID | Red 命令与首次失败摘要 | Green 命令与结果 |
|---|---|---|
| model-pins | `python3 -m unittest scripts.test_model_pins`：`AssertionError: scripts/local-asr.py has no DEFAULT_MODELS` | 写入 `DEFAULT_MODELS`/`DEFAULT_REVISIONS` 后 3/3 通过 |
| long-audio.* | 先加 `long_audio_test.go`，`go test ./internal/ingest -run TestProcessingStatusProjectsASRProgress`：`status.ASRState undefined` | 迁移 v22 + `deriveASRProgress` + 分块 `RunASRJob` 后 `go test ./internal/ingest -count=1` 通过 |
| embedding.role-query | 先加 `role_provider_test.go`，`go test ./internal/semantic`：`undefined: EmbedRole`、`provider.EmbedRole undefined` | `RoleProvider`/`EmbedTexts` 接入索引与检索后 `go test ./internal/semantic -count=1` 通过 |
| local-gpu.degraded | `go test ./internal/app -run TestCapabilitiesReportsGPUUnavailable`：`reason = "worker_asr_mismatch", want gpu_unavailable` | 调整能力原因优先级后通过 |
| sufficiency.*（夹具） | `go test ./internal/contextbuild -run TestSufficiencyScenarios`：5 个场景失败（跨会话 bigram 未命中、过期 note 与原始 turn 的语义混淆） | 修正夹具语义（不动期望以掩盖实现缺陷）后 21/21 通过 |
| release.regression-commands | `make spec-validate`：`release.regression-commands: test file not found: scripts/test_release_commands.py` | 新增清单测试后映射校验通过 |
| local-gpu.*（脚本） | 先加 `test_local_gpu_up.py`，首跑 `test_lock_and_timeouts_are_forwarded` 失败（未传 profiles-root）并因单周期前台模式超时 | 修正测试参数/前台模式后 7/7 通过 |
| local-asr 协议（`--probe/--stream/--skip/超时`） | 未观察到红：协议测试在 runner 实现之后编写；仅 `test_local_asr` 首跑因系统 Python 无 numpy 报错（测试环境问题，不是 TDD 红） | `python3 -m unittest scripts.test_local_asr -v` 10/10 通过 |
| gpu_lock | 未观察到红：锁实现与测试同批提交 | `go test ./internal/cloudsync -run TestGPUFileLock -count=1` 通过 |
| sufficiency.ui-trace | 未观察到红：浏览器脚本首跑 11/11 通过 | `scripts/sufficiency-browser.py` 11/11 |

## Sprint 3 review 修复（非严格 TDD，红阶段未逐项观察到）

代码评审发现并修复了以下缺陷；除注明外，修复前的失败由评审脚本/命令直接复现，回归测试在修复后加入，未观察到“先写测试再修复”的红阶段，因此不计为 TDD 证据。

| 缺陷 | 证据 | 回归 |
|---|---|---|
| embedding 左填充池化取到首个 token，批量文档向量错误 | 评审用 torch 复现 `mask.sum-1` 选中索引 2 而非 11；冻结评测重跑后 v1-openai Recall@5 从 0.225 变为 0.85 | 修正为 `hidden[:, -1]`；`SUFFICIENCY` 无关，评测 manifest 重跑覆盖 |
| `local-start.sh` 把 `EMBED_*` 转发成 `--embed_endpoint`（下划线），两个二进制退出 2 | 评审对已构建二进制复现 `flag provided but not defined` | 新增 `test_embedding_configuration_reaches_both_services`（先修正实现，未观察到红） |
| `local-gpu-up.sh --with-embedding` 从未成功启动 | 同上 | 新增 `test_with_embedding_exports_openai_compatible_config` |
| 窗口几何改变后续跑会跳过未转录音频并报 done | 代码分析 + 新测试 | `TestRunASRJobRejectsChangedWindowPlan` |
| v21 sync 行显示 0/1 覆盖 | 代码分析 | `TestProcessingStatusDerivesLegacyProjection` |
| worker CUDA 探针用系统 python3 而非 `.venv-asr`，误报 gpu_unavailable | 代码分析；本机系统 python3 无 torch | 修正为使用 transcriber 解析出的解释器；探针失败置空 transcriber |
| 单周期/中断的 cycles 退出码与 artifact 不真实 | 评审用 stub 复现 exit 0 与空 cycles | `test_interrupted_cycles_exit_nonzero` |
| health `curl` 无超时 | 评审复现挂起 | `--connect-timeout 2 --max-time 5` |
