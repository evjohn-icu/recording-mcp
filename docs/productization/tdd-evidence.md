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
| demo 第 9/11 步对空决策轨迹也 PASS | 代码分析（恒真断言） | 断言改为 topics>0；demo 重跑 12/12 |
| MCP 子进程非 session leader、stderr 未排空、失败路径不关闭 | 代码分析 | `start_new_session`、`DEVNULL`、`finally close` |
| T3 PASS 不校验覆盖/重复 | 代码分析 | 增加 `status_coverage_ms` 与 `duplicate_turns` 门槛；2 h 重跑 258/258、覆盖=时长、重复 0 |
| probe/embedding 继承 API key；测试钩子未受 TEST_MODE 限制；`.env` 权限未校验 | 安全评审 | `env -i` 白名单、钩子仅 TEST_MODE、属主/600 校验 |
| e2e-cloud 本地卡在 `npx wrangler`（解析/网络） | 本地运行 15 分钟无输出；直接调用本地 wrangler 后 `dev` 10 秒就绪 | 脚本改用 `./node_modules/.bin/wrangler`；本地 10/10 场景通过 |

### 第二轮评审修复（2026-11-09，提交 `6d9e386`）

| 缺陷 | 证据 | 回归 |
|---|---|---|
| `input_type` 默认发给所有 OpenAI 兼容端点，严格端点可能 400 | 代码评审确认请求体始终携带 `input_type`；未在真实严格端点复现 | 改为 opt-in：`TestOpenAICompatProviderOmitsInputTypeByDefault`、`TestOpenAICompatProviderSendsInputType`；启动器/评测脚本/文档同步。红阶段未观察 |
| `.env` 权限校验调用尚未定义的 `die`，错误路径退出 127 | 用旧脚本（`50681f0:scripts/local-gpu-up.sh`）对 0644 `.env` 复现：`line 64: die: command not found`，rc=127 | `test_env_file_permissions_fail_with_clean_error`（修复后 rc=1 且输出 chmod 600 提示） |
| 非测试模式启用 embedding 时 `EMBED_CMD`/`EMBED_HEALTH_CMD` 未初始化，`set -u` 中止 | 代码评审确认赋值仅在 `TEST_MODE=1` 分支 | 默认初始化 + `${VAR:-}` 展开；非测试端到端未复现。红阶段未观察 |
| 启动器传 `ASR_SCRIPT`，`local-start.sh` 未消费，自定义 runner 被忽略 | 代码评审确认 `local-start.sh` 只转发 `ASR_PYTHON` | `test_local_start.py` 断言 worker/web 收到 `--asr-script`。红阶段未观察 |
