# 产品化 Beta 基线审计

执行日期：2026-11-09。基线分支 `feat/productization-beta`，起点提交 `3f2865e9d2f41f4d03bc982d1ed8a2958e450433`（父分支 `feat/memory-highlights-context-brief`；`main` 为 `ba43cad4193ab0f012b0d66f9f206f355ccf1fdf`）。

## 环境

| 项 | 值 |
|---|---|
| Go | `go1.27.3 linux/amd64` |
| Node | `v26.13.0` |
| npm | `11.20.0` |
| ffmpeg / ffprobe | `9.0.2` |
| Docker CLI | `29.9.2` |
| Docker daemon | 首次检查未就绪；启动 Docker Desktop 后恢复（29.9.2），容器构建与 6 项部署检查随后实际执行并通过 |

## 基线命令

| 命令 | 结果 |
|---|---|
| `go vet ./...` | 退出 0 |
| `go test -count=1 ./...` | 退出 0（全部包 ok；`internal/app` 62s、`internal/pipeline` 65s、`internal/memory` 58s） |
| `sh scripts/demo.sh` | 退出 0，`demo: PASS — verbatim hit + ready index + fresh evidence`，使用临时目录 `/tmp/recording-mcp-demo.*` |
| `npx openspec validate --all` | 13 passed, 0 failed |
| `python3 scripts/check_spec_test_map.py` | 结构 OK；新增 55 个 pending |

`make build`、`make spec-validate`、`go test -race ./...`、Web 构建、浏览器套件在后续步骤执行并单独记录。

## 模块审计

| 模块 | 代码入口 | 状态 | 自动测试 | 本次运行 | 产品化缺口 | 本轮动作 | 复用方式 |
|---|---|---|---|---|---|---|---|
| Go Backend/SQLite | `internal/memory/store.go::Open` | PASS | `internal/memory/*_test.go` | 是 | 未启用 WAL，多进程并发写受限 | 启用 WAL + 校验 | 直接复用 |
| Profile/Auth | `internal/profile/manager.go`、`internal/web/profiles.go` | PARTIAL | `internal/profile/*_test.go`、`internal/web/profiles_test.go` | 是 | 无容器内 profile 初始化/校验命令 | 新增 `verify` 与 init 脚本 | 复用 manager |
| Web UI | `web/src/pages/*`、`internal/web/server.go` | PARTIAL | `internal/web/web_test.go`、Playwright 脚本 | 部分 | 无导入入口、阶段进度不可读 | ImportPanel + phases | 复用现有路由/会话 |
| Worker/Job Queue | `internal/process/process.go`、`cmd/context-worker` | PASS | `internal/process/process_test.go` | 是 | 无 ingest/asr stage、无健康端点、心跳无能力 | 增 stage、健康、能力列 | 复用租约队列 |
| Audio Import/ASR Adapter | `cmd/asr-ingest`、`internal/cloudsync/asr.go` | PARTIAL | `internal/cloudsync/asr_test.go` | 是 | 仅 CLI；Web 不可导入 | 新增 `internal/ingest` 与上传 API | 复用 `cloudsync.Transcriber` |
| Android/Cloudflare Sync | `internal/cloudsync/sync.go`、`apps/android` | OUT_OF_SCOPE | `internal/cloudsync/sync_test.go` | 否 | Beta 不涉及移动端 | 不改 | 保持 |
| Extract/Publish | `internal/pipeline/*` | PASS | `internal/pipeline/*_test.go` | 是 | 无 | 保持 | 复用 |
| Transcript Revision | `internal/memory/ingestion.go::IngestTranscript` | PASS | `internal/memory/ingestion_test.go` | 是 | 无 `source_bytes` 元数据 | 迁移 v21 + 字段 | 复用 supersede 语义 |
| Hybrid Retrieval v1/v2 | `internal/contextbuild`、`internal/semantic` | PARTIAL | `internal/web/web_test.go::TestHybridRetrievalVersionSelection` | 是 | 缺产品化消融报告；v2 未达门槛 | 新增人工审查集与报告，默认仍 v1 | 复用 v1/v2 |
| Context Projection | `internal/projection` | PASS | `internal/projection/*_test.go` | 是 | 无 | 保持 | 复用 |
| Event Plane | `internal/plane/plane.go` | PASS | `internal/plane/plane_test.go` | 是 | 未启用 WAL | 启用 WAL | 复用 append-only |
| Worldline | `internal/app/worldline.go` | PASS | `internal/app/worldline*_test.go` | 是 | 无整体快照方法 | 新增 `Snapshot` | 复用 `project` |
| JEV Decision | `internal/decision` | PASS | `internal/decision/*_test.go` | 是 | 与决策轨迹无直接耦合 | 不改 | 保持 |
| MCP | `cmd/context-memory/main.go`、`internal/mcpserver` | PARTIAL | `main_stdio_test.go`、`main_test.go` | 是 | profile 模式事件库指向记忆库；无决策工具 | 修复 plane 路径 + 新增只读工具 | 复用 server 构造 |
| OpenSpec | `openspec/` | PASS | `scripts/check_spec_test_map.py` | 是 | 本变更尚未映射 | 新增 pending 映射 | 复用校验 |
| CI/E2E | `.github/workflows/ci.yml`、`scripts/run-local-browser-tests.py` | PARTIAL | 现有 CI | 部分 | 无容器 job、无产品浏览器 job | 新增两个 job | 复用编排约定 |

## 基线失败

无。`go vet` 与 `go test ./...` 均退出 0。

## Docker 阻塞

首次轮询时 daemon 未就绪；启动 Docker Desktop 后恢复。镜像构建成功，`RUN_DOCKER_DEPLOYMENT_TEST=1` 的 6 项检查全部通过（58 秒），记录见 `deployment-verification.md`。
