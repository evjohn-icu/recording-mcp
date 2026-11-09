# Sprint 3 发布报告：Local Intelligence Reliability

日期：2026-11-09。分支 `feat/local-intelligence-reliability`（起点 `8090255b034365b8aa91eccac3b04568910becca`）。未推送、未合并。基线见 `sprint-3-baseline.md`，TDD 过程证据见 `tdd-evidence.md`。

## A. SMART 记分卡

| SMART | 目标 | 验收标准 | 实测 | 结论 | 证据 |
|---|---|---|---|---|---|
| SMART-01 | 单命令离线 GPU 部署 | `--check` 真实模型探针；`--cycles 5` 五次干净关闭；五次连续本机 ASR 作业 | 探针 PASS（加载 10.4 s，峰值 2210 MiB）；五轮 PASS（冷启动 1.59–1.87 s，关闭 ≤254 ms）；short-jobs 5/5 | PASS | `artifacts/local-gpu/`、`gpu-deployment.md`、`artifacts/long-audio-reliability/results.json` |
| SMART-02 | 长录音可靠转写 | 2 h 故障注入中断后只补未完成窗口；覆盖=时长；无重复活跃片段；OOM/超时动作 | T3 2 h：258/258 窗口、覆盖 7 200 000 ms、2 次 SIGKILL、无重复；T4–T10 PASS | PASS（真实 2 h 本机 ASR 未运行） | `long-audio-reliability.md` |
| SMART-03 | 真实中文 embedding 评估 | 角色化边界；本地服务；冻结门禁；未通过不换默认 | 见 C 节 | 真实 embedding 校准未通过门禁（误报率 0.95、p95 超 1.5×、v2-openai 召回低于 v1）；未创建 holdout lock，默认保持 v1 + hash | `real-embedding-evaluation.md`、`docs/evals/sprint3-real-embedding/` |
| SMART-04 | 证据充分性可见 | 20 场景（≥5 不可回答）；三种状态；有效引用；UI 可核对 | 21 场景 21/21（11 不可回答）；浏览器 11/11 | PASS | `sufficiency-scenarios.md`、`artifacts/sufficiency/v1/results.json` |
| SMART-05 | 发布验证 | 12 步演示；命令清单；真实回归 | 12/12 步通过（含重启持久化与 MCP 读取）；清单可机读；`go vet`、`go test`、`go test -race`、`make build`、`make spec-validate`、Web typecheck/build、Python 114 项、浏览器 13/13 与 11/11 均通过；Docker 部署测试 NOT_RUN（仓库代理不可达） | PASS（Docker 部署 NOT_RUN 已记录原因） | `release-commands.json`、`artifacts/sprint3-demo/result.json` |

## B. 工程交付

### 变更文件

配置与契约：`openspec/changes/local-intelligence-reliability/`（proposal/design/tasks/5 个 spec delta）、`openspec/test-map.json`、`.github/workflows/ci.yml`、`scripts/model-revisions.json`、`requirements-local-embed.txt`、`fixtures/sufficiency/v1/scenarios.json`。

采集与转写：`scripts/local-asr.py`（probe/stream/skip/timeouts）、`internal/cloudsync/local_asr.go`（`PlanWindows`/`ChunkTranscriber`/`RunnerError`/流式校验）、`gpu_lock_unix.go`/`gpu_lock_other.go`、`probe.go`、`select.go`、`internal/ingest/service.go`（数据库检查点窗口循环）、`internal/migrations/asr_progress.go`（v22）、`internal/memory/ingestion.go`（`asr_*` 投影）。

运行与诊断：`internal/process/runtime.go`（GPU/显存/设备/错误心跳）、`internal/app/capabilities.go`（原因与 GPU 诊断）、`cmd/context-worker/main.go`（超时/锁 flag、降级启动）、`cmd/context-web/main.go`（`ASRInitError` 降级）。

检索与充分性：`internal/semantic/provider.go`（`RoleProvider`/`EmbedTexts`）、`index.go`、`search.go`、`sidecar_retriever.go`、`internal/eval/provider_accounting.go`、`web/src/components/SufficiencyPanel.tsx`、`SearchPage.tsx`、`DetailPage.tsx`、`ImportPanel.tsx`、`api/types.ts`、`styles.css`。

脚本与文档：`scripts/local-gpu-up.sh`、`scripts/local-start.sh`、`scripts/local-embed.py`、`scripts/fetch-local-models.sh`、`scripts/long-audio-reliability.py`、`scripts/testdata/slow-asr.py`、`scripts/sprint3-demo.py`、`scripts/sufficiency-browser.py`、6 个新测试脚本，以及 `docs/productization/{gpu-deployment,long-audio-reliability,sufficiency-scenarios,real-embedding-evaluation,release-commands,sprint-3-release-report}.md/json`。

### 架构决策

1. **Mode B 为交付形态**：WSL 原生 web + worker + `.venv-asr`；Docker 保持非 GPU。GPU 容器探针失败且无 nvidia runtime，不声称容器 GPU 支持；GPU 镜像作为独立未来变更。
2. **数据库附件即检查点**：`source_attachments` 是唯一真相，`sync_recordings` 只是投影。这样进程崩溃、租约过期、重启都走同一条恢复路径，不新增队列或状态机。
3. **窗口计划单一来源**：Python/Go 计划由 dry-run 对照测试锁死，`--skip` 才可信。
4. **角色化 embedding 最小边界**：只在请求加 `input_type`，不新增 provider 实现；hash/远端 provider 行为不变。
5. **充分性契约与评估器质量分离**：场景测试用确定性评估器验证状态机与引用有效性；真实评估器只影响指标。
6. **v1 + hash 仍是默认**：真实 embedding 只在门禁通过时记录为可选路径；离线默认不依赖第二个模型服务。

### 复用与新增依赖

复用：持久化队列/租约、`cloudsync.Transcriber`、`memory.IngestTranscript` 与 supersede 语义、`contextbuild.Build` v2、`semantic.Provider`、既有 Search UI 与浏览器测试框架、`context-eval` 冻结评测协议。

新增 Python 依赖：`requirements-local-embed.txt`（torch 2.8.0+cu126、transformers 4.57.6、numpy 2.5.4、huggingface_hub 0.36.2）。Jev 评估器额外需要支持 `qwen3_5` 的 transformers；本轮用独立 `PYTHONPATH` 目录（5.22.2 + 兼容 hub/tokenizers）运行，不改动 `.venv-asr`，见 `real-embedding-evaluation.md`。无新增 Go 模块依赖。

### 兼容与迁移

- 迁移 v22 只加列（`sync_recordings` 3 列、`worker_heartbeats` 6 列），默认值安全；旧库升级测试通过。
- `LocalTranscriber.Transcribe` 仍返回单个 JSON 文档，旧 stub/fixture 不变；新增 `TranscribeChunks` 仅本地后端使用。
- 能力接口只加字段；旧客户端忽略未知字段。
- 默认检索版本、默认 embedding provider、`contracts/ingest-v1/`、Event Plane/Worldline、队列模型均未改变。

## C. 性能报告

### ASR

| 配置 | 音频 | 硬件 | 窗口 | 覆盖 | 峰值显存 | 墙钟/RTF | 状态 |
|---|---|---|---|---|---|---|---|
| ARK float16 探针 | — | RTX 2070 8 GB | — | — | 2210 MiB | 加载 10.4 s | PASS |
| 合成故障注入 | 2 h | CPU 协议桩 | 258 | 7 200 000 ms | — | 25.5 s / 0.0035 | PASS（非真实 ASR） |
| Qwen3-ASR 真实 | 120 s | RTX 2070 | 5 | 120 000 ms | 5398 MiB | 0.241 | PASS |
| ARK 真实 | 120 s | RTX 2070 | 5 | 120 000 ms | 3338 MiB | 0.177 | PASS |
| Qwen3-ASR 真实 | 36.95 min | RTX 2070 | 80 | 2 216 891 ms | 5398 MiB | 0.034 | PASS |

### 检索（冻结评测，`docs/evals/sprint3-real-embedding/manifest.json`）

| 配置（calibration） | Recall@5 | MRR@10 | p95 (ms) | 无证据误报 | 假充分 | 模型调用 | 全门禁 |
|---|---|---|---|---|---|---|---|
| v1 lexical | 0.0000 | 0.0000 | 2.6 | 2/20 | 0/0 | 0 | None |
| v2 lexical | 0.8750 | 0.3721 | 338.0 | 19/20 | 1/24 | 59 | False |
| v1 hash | 0.1500 | 0.1052 | 21.3 | 19/20 | 0/0 | 0 | None |
| v2 hash | 0.1625 | 0.0784 | 339.4 | 19/20 | 3/51 | 59 | False |
| v1 真实 embedding | 0.2250 | 0.0986 | 259.7 | 19/20 | 0/0 | 0 | None |
| v2 真实 embedding + 充分性 | 0.1375 | 0.0569 | 568.3 | 19/20 | 0/52 | 59 | False |

门禁结论：无候选通过 `false_positive_rate_at_most_0_05` 与 `p95_at_most_1_5x_same_provider_v1`；v2-openai 另失败召回与 MRR。`quality_gate_passed=false`，未创建 lock、未触碰 test。泄漏（stale/ambient/scope/跨 split/invalid refs）均为 0，失败调用 0。

延迟为冻结协议定义的检索构建 p50/p95；v1 与 v2 的测量阶段差异已在评测脚本中披露。模型调用/失败次数、索引构建、泄漏计数见 manifest 与 `real-embedding-evaluation.md`。

## D. 产品演示

- 启动：`MEMORY_PROFILE_PASSWORD=... scripts/local-gpu-up.sh --profile sprint3 --port 8788 --profiles-root data/profiles --backend ark`
- 模型：ARK-ASR `Edge0/ARK-ASR-0.6B@45776b56…`（float16, cuda:0）；可选 embedding `Qwen/Qwen3-Embedding-0.6B@97b0c614…`（CPU 默认）
- 演示夹具：授权 `data/full-transcription/segments/segment-01.pcm`（120 s）+ 合成多会话 `fixtures/productization/beta-demo/`
- UI：`http://127.0.0.1:8788`
- MCP：`bin/context-memory --profile sprint3 --profiles-root data/profiles --read-only`，调用 `build_context` 与 `get_decision_trail`
- 12 步结果：12/12 PASS。步骤：启动（10.7 s）→ 能力/GPU（RTX 2070，7326/8192 MiB）→ 导入 120 s → 阶段轮询（asr 5/5）→ 5 条转写与证据 → 字面检索命中 1 → 跨会话混合检索 8 项 → 充分性 unknown/refs 有效（未配置评估器）→ 决策轨迹 1 个主题 → 重启后会话仍在（10.9 s）→ MCP 读取 8 项/1 主题 → 重复导入 duplicate=true 且 5 个来源片段。报告 `/home/ev/code/recording-mcp/artifacts/sprint3-demo/result.json`，密码已脱敏。

## E. 发布决定

**READY_FOR_PRIVATE_BETA**

依据：

- SMART-01：五轮启停稳定，探针与显存数据完整；ASR 不可用时降级而非拒绝启动。
- SMART-02：窗口检查点、覆盖并集、OOM/超时动作均有测试与合成 2 h 证据；真实 120 s/36.95 min 结果见 C 节。
- SMART-03：真实 embedding 未通过冻结门禁且低于 v1-openai 质量；未创建 holdout lock，默认保持 v1 + hash，启用方式仅作为可选路径记录。
- SMART-04：21 个场景（11 不可回答）与三种状态 UI 均通过。
- SMART-05：12 步演示全部通过，重启与 MCP 读取可复现。
- 未发现数据完整性或 profile 隔离失败；默认仍是 v1 + hash。

未完成/限制（逐项 `NOT_RUN` 及原因）：

| 项 | 原因 |
|---|---|
| Docker GPU 探针与 GPU 容器支持 | 镜像仓库代理不可达（`172.22.23.142:7890` 拒绝连接），daemon 无 `nvidia` runtime；本轮不发布 GPU 镜像 |
| `RUN_DOCKER_DEPLOYMENT_TEST=1 python3 -m unittest scripts.test_productization_deploy -v` | 同上，无法拉取部署镜像 |
| 真实 2 h 本机 ASR | 时间/显存预算限制；T3 用故障注入 runner 验证 2 h 机制，未在 2 h 文件上运行真实模型 |
| `EMBED_DEVICE=cuda:0` 的 embedding 延迟复跑 | 真实 embedding 同时失败召回与 MRR 门禁，换设备不改变结论 |
| Embedding 索引构建时间/内存单列指标 | 冻结评测 manifest 只记录调用次数/失败数；未单独测量索引构建耗时与 RSS |
| Docker Compose GPU profile | 本轮明确不交付（需要多 GB CUDA/torch 镜像与模型缓存卷），不是失败项 |
