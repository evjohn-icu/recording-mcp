# Context Engine v0.4 基线（执行前）

记录时间：2026-11-13（本地时区）。执行根目录 `/home/ev/code/recording-mcp`。
本文件只记录**实际执行**的只读/构建/测试状态；未执行项明确写 NOT_RUN。计划阶段的测试/构建均未运行，不能作为证据。

## 1. 仓库与分支

| 项 | 值 |
|---|---|
| 分支 | `engineering/context-engine-v04` |
| HEAD | `6fc48d1cb42883c4c90466823c0d54bc88217649` |
| main | `6fc48d1cb42883c4c90466823c0d54bc88217649` |
| 工作区 | 干净（仅本轮新建的 `openspec/changes/context-engine-v04/` 未跟踪） |

main 与 HEAD 一致，未出现用户修改，未使用 stash，未新建 worktree。

## 2. 工具链

| 工具 | 观察值 | 仓库/CI 期望 |
|---|---|---|
| Go | `go1.27.3 linux/amd64` | `go.mod` 要求 ≥ 1.27.1 |
| Node | `v26.13.0` | CI 使用 Node 22（差异已记录，未替换） |
| Python | `3.15.2` | CI 使用 Python 3.12（差异已记录，未替换） |
| ffmpeg/ffprobe | `/home/linuxbrew/.linuxbrew/bin/ffmpeg`, `.../ffprobe` | 存在 |
| Docker | client 29.9.2（`docker info` 输出被截断，daemon 状态在执行时复核） | opt-in |
| Java | 存在（未验证 17/build-tools 36） | Android 可选 |
| openspec | `node_modules/.bin/openspec 1.14.3`（全局不存在） | 仓库内 CLI |

`node_modules/.bin/openspec context --json` 确认本仓库为 root，schema `spec-driven`。

## 3. 基线命令与退出码

| 命令 | 退出码 | 结果 |
|---|---|---|
| `go test -race -count=1 -json ./...` | 0 | 32 个包 `ok`，14 个 `no test files`，无失败 |
| `make python-test` | 0 | 102 个测试 `ok`，9 个 skip（见 §4） |
| `npx openspec validate --all` | 0 | 16 items passed, 0 failed |
| `python3 scripts/check_spec_test_map.py` | 0 | 236 scenarios；新增 CE04-* 为显式 pending |
| `npm run typecheck --prefix web` | 0 | 无输出错误 |
| `npm test --prefix apps/cloud-ingest` | 0 | 11 tests passed |

未执行：`make ci`（最终回归时一次执行）、cloud E2E（`scripts/e2e-cloud.sh` 硬编码 `data/e2e`，基线记为 NOT_RUN，实现 `E2E_ROOT` 后在临时 root 执行）、Docker 部署测试（opt-in，基线跳过）、真实模型/真实音频（NOT_RUN）。

## 4. 基线 skip 明细

| 测试 | 原因 |
|---|---|
| `test_beta_demo.BetaDemoTest.test_ten_step_demo` | `RUN_BETA_DEMO=1` 未设置 |
| `test_decision_local_server.RealModelSyntheticSmoke.test_bundled_decision_v3_inference_smoke` | `JEVD3_RUN_MODEL_SMOKE=1` 未设置（真实缓存模型） |
| `test_productization_deploy.DeploymentTest.*`（6 个） | `RUN_DOCKER_DEPLOYMENT_TEST=1` 未设置 |
| `RUN_SPRINT3_DEMO` 相关（1） | 需要 GPU 与固定 ASR 模型 |

这些 skip 不计为 PASS；最终报告按项记录 BLOCKED/NOT_RUN。

## 5. 既有结构与接口（审计基线）

- 语义：`internal/semantic`（`Provider`、`EmbedRole`、`OpenAICompatProvider`、`SQLiteRetriever.Ready() bool`、`HNSWRetriever.Ready() bool`、`minSemanticScore=0.05`）。
- 组装：`internal/contextbuild/build.go`（`Deps{Store,DB,Reranker,Semantic,Assessor}`、`BudgetTrace`、`maxEvidenceItems=3`、`evidenceRadius=1`、`defaultRerankPool=64`、`maxRerankPool=128`、`maxRerankInputBytes=64KiB`、`defaultMaxBytes=16000`）。
- 证据：`internal/memory/retrieval.go::evidenceContext` 在读取阶段按 `maxBytes` 截断，`GetCurrentEvidenceContext` 已有 current-only 版本。
- 队列：`internal/process`（`EnqueueTx`/`Claim`/租约/重试）、`processing_jobs`。
- 迁移：当前最新 schema v22（`internal/migrations`）。
- 音频：仅 `experiments/` PoC（Silero/Zipformer/CAM++ CPU、Community-1 GPU），与主应用隔离；无正式契约/存储/消费者。
- 备份：`docs/deployment.md` 直接打包运行卷；Docker 测试只比较 hash。
- 模型/许可证：Silero MIT、Zipformer Apache-2.0、pyannote runtime MIT / Community-1 权重 CC-BY-4.0 gated、CAM++ Apache-2.0（PoC ONNX 为第三方导出）。除 Zipformer `b0d6db68…a36a` 外 pin 不完整。
- 真实音频：仅有 ASR 产物参考，无人工金标；本轮无授权样本。

## 6. 安全边界

- `CE04_TMP` 临时目录用于本轮所有 profile/DB/音频/日志/评估输出；不访问 `data/`、模型缓存或用户录音。
- 测试进程不继承真实 API key 与 live-test opt-in。
- 未授权 push/merge/发布/付费调用。
