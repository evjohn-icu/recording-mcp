# Local-first Beta 发布报告（2026-11-09）

## A. Engineering Summary

在 `feat/productization-beta` 分支上，以 `3f2865e9d2f41f4d03bc982d1ed8a2958e450433` 为基线，交付本地 Beta：

- 可复现容器部署：多阶段 `Dockerfile`、`docker-compose.yml`、profile 初始化与健康检查、非 root/只读根文件系统。
- 真实上传管线：`POST /api/imports` → `ingest`（ffmpeg → PCM）→ `asr`（复用 `cloudsync.Transcriber`）→ 现有 `IngestTranscript` → `extract/embed`。
- 可理解的进度：`ProcessingSnapshot.phases/current_stage/asr_available/worker_alive`，失败阶段带 `error_code/error_action`。
- 决策轨迹：Worldline + 事件平面确定性投影六态；人工确认/拒绝/重新打开；来源失效与遗忘降级。
- MCP：只新增只读 `get_decision_trail`（`decision-trail-v1`），并修复 profile 模式事件平面路径。
- 质量：产品浏览器 13/13、决策浏览器 14/14、MCP 15 项、导入 9 项、检索 4 份指标。

不重写 Event Plane、Worldline、ASR Pipeline、检索主体或 ingest-v1；默认检索版本仍为 v1。

## B. SMART Scorecard

| Task | Planned | Actual | Status | Evidence |
|---|---|---|---|---|
| SMART-01 部署 | 容器 + 健康 + 备份恢复 | 镜像构建成功；`docker compose config` 通过；部署测试 6 项 | PASS（见 C） | `Dockerfile`、`docker-compose.yml`、`docs/productization/deployment-verification.md` |
| SMART-02 导入/进度 | 8 场景 + 阶段投影 | 9 个 Go 测试（含真实 ffmpeg 格式） | PASS | `internal/ingest/import_test.go`、`internal/app/processing_test.go` |
| SMART-03 音频 E2E | synthetic 三段 + real NOT_RUN | 3/25/120 分钟三段跑通 | PASS（synthetic） | `docs/productization/real-audio-e2e.md` |
| SMART-04 Web 工作流 | 8 步 + 刷新/移动/无错误 | 13/13 检查 | PASS | `artifacts/product-browser/product-workflow-results.json` |
| SMART-05 检索 | 4 配置 + 报告 + 默认 v1 | 指标与泄漏已记录 | PASS（真实模型 NOT_RUN） | `docs/productization/retrieval-quality-report.md` |
| SMART-06 决策后端 | 24 个具名断言 | 24/24 | PASS | `internal/app/decision_trail_scenarios_test.go` |
| SMART-07 决策 Web | 8 项 + 4 附加 | 14/14 | PASS | `scripts/decision-trail-browser.py` |
| SMART-08 MCP | 12 核心 + 3 兼容 | 15/15 | PASS | `cmd/context-memory/decision_trail_test.go` |
| SMART-09 演示 | 10/10 + 同源 + 重启 | 10/10 | PASS | `artifacts/beta-demo/result.json` |

## C. Testing Evidence

实际运行的命令与结果（退出码 0，除非注明）：

- `go vet ./...` — 通过。
- `go test -count=1 ./...` — 通过（含新增包）。
- `go test -race -count=1 ./...` — 通过。
- `make build`、`make spec-validate` — 通过。
- `python3 scripts/run-product-browser.py` — 13/13。
- `python3 scripts/product-audio-e2e.py --mode synthetic` — PASS：148 音频分钟、148 turns、零覆盖缺口、13.29 秒 wall time。
- `python3 scripts/beta-demo.py` — 10/10。
- `RUN_DOCKER_DEPLOYMENT_TEST=1 python3 -m unittest ... test_productization_deploy.py` — 6/6，58.0 秒，见 `deployment-verification.md`。
- `python3 scripts/decision-trail-browser.py` — 14/14。
- `python3 -m unittest scripts.test_productization_retrieval` — 3/3。

未执行：

- 真实 ASR（doubao）与真实 embedding/reranker/assessor：`NOT_RUN`（无凭据、未授权付费调用），命令见检索报告与音频 E2E 文档。
- Android `./gradlew test`：`NOT_RUN`（本轮无 SDK 验证）。
- r3 冻结集 holdout：按计划不运行。

## D. Product Demo

1. `docker compose up -d --build` 后打开 `http://127.0.0.1:8788`，选择 profile 并登录。
2. “录音”页导入音频；处理阶段依次显示音频接收/语音转写/信息提炼/发布/索引。
3. 详情页查看转写、搜索并打开证据窗口播放片段。
4. 世界线页选择主题，查看决策轨迹；确认/拒绝/重新打开并刷新验证。
5. MCP 示例：

```json
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_decision_trail","arguments":{}}}
```

返回 `schema_version: "decision-trail-v1"`，当前决策与 Web 一致。

截图：`artifacts/product-browser/product-workflow-desktop.png`、`artifacts/product-browser/product-workflow-mobile.png`。

## E. Git Deliverables

- 分支：`feat/productization-beta`（基于 `3f2865e9d2f41f4d03bc982d1ed8a2958e450433`）。
- 本地提交：见文末 `git log --oneline` 输出；**未推送、未创建 PR（未授权）**。
- 主要文件：`Dockerfile`、`docker-compose.yml`、`internal/ingest/**`、`internal/app/decision_trail*.go`、`internal/web/import.go`、`internal/web/decision_trail.go`、`web/src/components/ImportPanel.tsx`、`web/src/components/DecisionTrailPanel.tsx`、`cmd/context-memory/decision_trail_test.go`、`scripts/product-*.py`、`scripts/beta-demo.py`、`docs/productization/**`。

## F. Next Sprint Backlog

| 项 | 用户价值 | 阻塞 | 投入 | 成功标准 |
|---|---|---|---|---|
| 1. 真实 doubao ASR 端到端 | 真实录音可直接导入 | 需要授权音频与付费凭据 | 2 天 | 真实音频三段无缺口，准确率人工评估 |
| 2. 真实中文 embedding/reranker | 提升 v2 召回与负例质量 | 需要模型服务 | 3 天 | 负例 clean rate ≥0.9，Recall@5 ≥0.8 |
| 3. 决策候选召回改进 | 减少漏掉的决策句 | 需要标注集 | 2 天 | 人工集候选召回 ≥0.9，保持零误确认 |
| 4. 容器 CI 稳定化 | 每次 PR 验证部署 | CI 网络/缓存 | 1 天 | docker job 稳定通过并上传日志 |
| 5. Android 回归接入 | 移动端同步不退化 | 需要 SDK 镜像 | 2 天 | `./gradlew test` 在 CI 通过 |

## 结论

Beta 的本地闭环（导入 → 转写 → 检索 → 决策 → MCP → 演示）已在合成数据上端到端验证。真实模型能力与移动端回归明确标记为 `NOT_RUN`，未用合成结果冒充。
