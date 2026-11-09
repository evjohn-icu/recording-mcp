# Local-first Beta 发布清单

执行日期：2026-11-09。基线提交 `3f2865e9d2f41f4d03bc982d1ed8a2958e450433`（分支 `feat/productization-beta`）。

## 演示（10 步）

| # | 步骤 | 命令/证据 | 状态 |
|---|---|---|---|
| 1 | 生成 fixture 资料库 | `bin/context-eval --corpus fixtures/productization/beta-demo/corpus.json --projection .../projection.json` | PASS |
| 2 | 只保留 5 条人工投影笔记 | `scripts/beta-demo.py` 步骤 2 | PASS |
| 3 | 生成合成音频桥接 | `scripts/fixture-audio-bridge.py` | PASS |
| 4 | 创建隔离 profile | `bin/context-profile adopt --name demo` | PASS |
| 5 | 登录并读取 3 个会话 | `GET /api/conversations` | PASS |
| 6 | 决策轨迹包含 3 个候选 | `GET /api/decisions?topic_id=...` | PASS |
| 7 | 人工确认最终候选 | `POST /api/decisions/review` | PASS |
| 8 | 音频片段元数据 | `GET /api/turns/{id}/audio`（`X-Audio-Start-MS: 0`） | PASS |
| 9 | MCP 读取同一决策 | `bin/context-memory --profile demo --read-only` → `get_decision_trail` | PASS |
| 10 | 重启后决策仍在 | 重启 `context-web` 后再次读取 | PASS |

复现：`python3 scripts/beta-demo.py --out artifacts/beta-demo`（结果写入 `artifacts/beta-demo/result.json`）。

## SMART 对照

| SMART | 交付物 | 证据 |
|---|---|---|
| SMART-01 部署 | `Dockerfile`、`docker-compose.yml`、`scripts/docker-profile-init.sh`、`docs/productization/deployment*.md` | `docker compose config`、`RUN_DOCKER_DEPLOYMENT_TEST=1` 的 6 项检查 |
| SMART-02 导入/进度 | `internal/ingest`、`POST /api/imports`、`GET /api/capabilities`、`phases` 投影 | `internal/ingest/import_test.go` 9 项、`TestProcessingSnapshotProjectsPhases` |
| SMART-03 音频 E2E | `scripts/product-audio-e2e.py`、`docs/productization/real-audio-e2e.md` | synthetic 三段结果；real 模式 `NOT_RUN` |
| SMART-04 Web 工作流 | `ImportPanel`、阶段 UI、`run-product-browser.py` | `artifacts/product-browser/product-workflow-results.json` 13/13 |
| SMART-05 检索 | `fixtures/eval/product-review-v1/`、`retrieval-quality-report.md` | 4 份指标 JSON；默认仍 v1 |
| SMART-06/07 决策 | `internal/app/decision_trail*.go`、决策面板 | 24 个场景子测试 + 浏览器 14/14 |
| SMART-08 MCP | `get_decision_trail`、profile 事件平面修复 | 15 个 MCP 契约测试 |
| SMART-09 演示/发布 | 本清单与 `beta-release-report.md` | 10/10；Web 与 MCP 同源 |

## DoD

- [x] `POST /api/imports` 拒绝未配置 ASR、错误格式与损坏音频，并给出 action。
- [x] 导入幂等；重复上传与重试不增加 conversation/recording/active attachment。
- [x] 处理阶段 UI 区分等待/处理中/完成/失败/跳过，失败带错误与重试。
- [x] 决策六态；确认只来自人工复核；`valid_from` 未知不推断。
- [x] MCP 只新增只读 `get_decision_trail`，`schema_version=decision-trail-v1`。
- [x] 默认检索版本仍为 v1。
- [x] demo 脚本只使用临时目录；不读取 `data/` 私人录音。

## 隐私与安全声明

- 演示语料、转写、音频全部为合成内容；fixture transcript 明确标注，不是真实 ASR。
- 未上传、未读取任何私人录音；`data/asr`、`data/full-transcription`、`data/worldline-local` 未被访问。
- 未调用付费 ASR/embedding API；真实 ASR 与真实模型指标为 `NOT_RUN`。
- 容器以非 root 运行，宿主只发布回环端口。

## 截图与录屏

- `artifacts/product-browser/product-workflow-desktop.png`
- `artifacts/product-browser/product-workflow-mobile.png`
- 决策面板：`artifacts/decision-browser/decision-trail.png`（由 `scripts/decision-trail-browser.py` 生成）
- 录屏：未录制（`NOT_RUN`）。
