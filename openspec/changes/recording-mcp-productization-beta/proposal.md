# Why

当前仓库已具备本地 SQLite 事件平面、Worldline 投影、ASR 管线与 Web 只读界面，但缺口集中在"可交付"上：没有容器化部署、录音只能通过 CLI 导入、处理进度暴露原始 job 行、决策历史没有六态投影、MCP 没有可信决策上下文工具。Beta 需要让用户在本地用一条命令启动、在浏览器里导入录音、看到可理解的阶段进度，并能通过 MCP 读取带证据和复核状态的决策轨迹。

本变更不重写 Event Plane、Worldline、ASR Pipeline、检索主体或 ingest-v1 契约；不修改 `contracts/ingest-v1/`；不切换默认检索版本（保持 v1）。

# What Changes

- 部署：新增多阶段 `Dockerfile`、`docker-compose.yml`、`.dockerignore`、`scripts/docker-profile-init.sh`，`context-profile` 增加 `verify` 子命令，`context-web` 增加 `--container-bind`。
- 运行时：SQLite 与事件平面启用 WAL；`/healthz` 反映数据库可用性；worker 心跳携带 ASR/embedding 能力；新增 `GET /api/capabilities`。
- 导入：新增 `internal/ingest` 包和 `POST /api/imports`，multipart 流式落盘、`ffprobe` 真实探测、SHA-256 稳定 ID；worker 新增 `ingest`/`asr` stage；处理投影新增 `phases/current_stage/asr_available`。
- Web：导入面板、空列表 onboarding、阶段化进度 UI、移动端布局。
- 决策轨迹：`internal/app` 新增确定性 `DecisionTrail` 六态投影与人工复核；`GET /api/decisions`、`POST /api/decisions/review`；Worldline 页面加入决策面板。
- MCP：新增只读工具 `get_decision_trail`（`schema_version: decision-trail-v1`）；修复 profile 模式使用 `plane/events.db` 而不是记忆库。
- 质量：新增产品化人工审查检索集与指标报告（不改变默认 v1）；新增 synthetic 音频 E2E、产品浏览器工作流、Beta demo 与发布材料。

# Capabilities

- `productization`：部署、导入、进度投影、浏览器工作流、检索报告、Beta 演示。
- `decision-trail`：决策六态、人工确认/拒绝/重新打开、来源失效与遗忘降级。
- `mcp-context`：MCP 只读决策轨迹工具与 profile 事件平面隔离。

# Impact

- 代码：`internal/ingest`（新增）、`internal/app`、`internal/web`、`internal/process`、`internal/memory`、`internal/worker`、`cmd/context-*`、`web/src`。
- 数据：迁移 v20（worker 能力列）与 v21（`recordings.source_bytes`），仅新增列/表，不重写既有行。
- 契约：ingest-v1 不变；MCP 只新增工具，不修改既有工具字段。
- 行为：默认检索版本仍为 v1；未配置 ASR 时上传返回 `asr_unavailable` 而不是静默排队。
