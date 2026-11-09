# Sprint 3 基线审计

执行日期：2026-11-09。分支 `feat/local-intelligence-reliability`，起点 `8090255b034365b8aa91eccac3b04568910becca`（父分支 `feat/productization-beta`）。工作树干净（`git status --porcelain` 为 0 行）。未推送、未合并。

## 环境

| 项 | 值 |
|---|---|
| Go | `go1.27.3 linux/amd64` |
| Node | `v26.13.0` |
| npm | `11.20.0` |
| ffmpeg / ffprobe | `9.0.2` |
| GPU | `NVIDIA GeForce RTX 2070`, 8192 MiB 总显存, 7329 MiB 空闲, driver `610.88` |
| `.venv-asr` | `torch 2.8.0+cu126`, CUDA 12.6, `torch.cuda.is_available()=True`, 设备 `NVIDIA GeForce RTX 2070` |
| Docker CLI | `29.9.2` (build `a82e528`) |
| Docker runtimes | 仅 `runc` / `io.containerd.runc.v2`；无 `nvidia` runtime |
| Docker GPU 探针 | **FAIL**：`docker run --rm --gpus all nvidia/cuda:12.6.3-base-ubuntu22.04 nvidia-smi` 无法拉取镜像，daemon 报 `registry-1.docker.io` 经 `http://172.22.23.142:7890` 代理连接被拒绝（`actively refused`）。未做镜像构建，未尝试 GPU 容器作业。 |

### 模型缓存与钉住版本

| 用途 | repo | revision | 本地快照 |
|---|---|---|---|
| ARK ASR | `Edge0/ARK-ASR-0.6B` | `45776b56d58cdfb2e2eb632f7e110f38684633e0` | `~/.cache/huggingface/hub/models--Edge0--ARK-ASR-0.6B/snapshots/45776b5…`（2.5G） |
| Qwen3 ASR | `Qwen/Qwen3-ASR-1.7B` | `7278e1e70fe206f11671096ffdd38061171dd6e5` | `.../models--Qwen--Qwen3-ASR-1.7B/snapshots/7278e1e…`（4.4G） |
| Qwen3 Embedding | `Qwen/Qwen3-Embedding-0.6B` | `97b0c614be4d77ee51c0cef4e5f07c00f9eb65b3` | 基线时缺失；步骤 3 由 `scripts/fetch-local-models.sh` 下载，1.12 GiB，`model.safetensors` SHA-256 与钉住值一致 |
| Jev Decision | `chaoliangUNSW/Jev-Style-0.8B-Decision-v3` | `b023d1f9c7858fbf01504577a3bfc349ea5c7385` | 基线时快照仅有 200K `model.safetensors`；步骤 3 重新下载完整 1.42 GiB 快照并校验 SHA-256 |

`HF_HOME` 未设置，缓存使用默认 `~/.cache/huggingface/hub`。权威钉住表写入 `scripts/model-revisions.json`，由 `scripts/test_model_pins.py` 与 Go/Python 常量对照。

## 基线命令（实际执行）

| 命令 | 结果 |
|---|---|
| `go vet ./...` | 退出 0 |
| `go test -count=1 ./...` | 退出 0 |
| `go test -race -count=1 ./...` | 退出 0 |
| `make build` | 退出 0（16 个二进制） |
| `make spec-validate` | 退出 0；`openspec validate --all` 15 passed / 0 failed；场景映射结构 OK |
| `npm run typecheck --prefix web` | 退出 0 |
| `npm run build --prefix web` | 退出 0 |
| `python3 -m unittest discover -s scripts -p 'test_*.py' -v` | 退出 0；93 tests, OK (skipped=8) |
| `RUN_DOCKER_DEPLOYMENT_TEST=1 python3 -m unittest scripts.test_productization_deploy -v` | 基线时 NOT_RUN：镜像仓库代理不可达。步骤 5 用本地已有镜像重跑，6/6 通过（72.9 s） |
| `python3 scripts/run-product-browser.py` | NOT_RUN：基线轮未执行浏览器套件（Sprint 3 在步骤 4.3/5.2 执行） |

## 模块分类

| 模块 | 入口 | 状态 | 缺口 | 本轮动作 |
|---|---|---|---|---|
| 本机 ASR runner | `scripts/local-asr.py:1` | PARTIAL | 无 `--probe`/`--stream`/`--skip`/加载与窗口超时；默认模型常量未固定 | 新增四个行为，常量与钉住表一致 |
| Go 本机转写器 | `internal/cloudsync/local_asr.go:21`、`:60` | PARTIAL | 一次性整段转写；无分块回调、无类型化 runner 错误、无 GPU 锁 | 新增 `PlanWindows`/`ChunkTranscriber`/`RunnerError`/文件锁 |
| ASR 选择器 | `internal/cloudsync/select.go:43` | PASS | 无超时/锁配置透传 | 扩展 `TranscriberConfig` |
| ASR 任务 | `internal/ingest/service.go:325` | PARTIAL | 全量转写后一次写库；无窗口检查点与覆盖记账；失败不分类 | 改为数据库检查点窗口循环 |
| 迁移 | `internal/migrations/migrations.go:190`（注册表） | PARTIAL | 无 ASR 进度与 GPU 心跳列 | 新增 v22 |
| 心跳 | `internal/process/runtime.go:17` | PARTIAL | 无 GPU 名称/显存/设备/错误 | 扩展结构 + 读写 |
| 能力接口 | `internal/app/capabilities.go:60` | PARTIAL | 只有 provider/worker 布尔；无 GPU/错误原因 | 扩展原因与字段 |
| Worker 启动 | `cmd/context-worker/main.go:177` | PARTIAL | ASR 选择失败即退出 | 降级启动 + `--asr-*` 超时/锁 flag |
| Web 启动 | `cmd/context-web/main.go:242` | PARTIAL | 本机 ASR 失败即退出 | 降级 + `ASRInitError` |
| 启动器 | `scripts/local-start.sh:1` | PARTIAL | 固定 `hash` provider；无超时/锁透传；无单命令 GPU 启动 | 新增 `local-gpu-up.sh`，扩展 `local-start.sh` |
| 处理状态投影 | `internal/memory/ingestion.go:41`、`:363` | PARTIAL | 无 ASR 窗口/覆盖字段 | 新增字段 + `deriveASRProgress` |
| Embedding provider | `internal/semantic/provider.go:24`、`:177` | PARTIAL | 无 document/query 角色 | `RoleProvider` + `EmbedTexts` |
| 检索器 | `internal/semantic/search.go`、`index.go` | PARTIAL | 查询与文档同一调用形态 | 查询用 query 角色 |
| 本地 embedding 服务 | 不存在 | BROKEN（缺失） | 无 OpenAI 兼容本地服务 | 新增 `scripts/local-embed.py` |
| 充分性构建 | `internal/contextbuild/build.go:150`、`:269`、`:1107` | PARTIAL | 实现已有，缺 20 个具名场景与 UI 展示 | 新增场景夹具 + UI 面板 |
| 检索 UI | `web/src/pages/SearchPage.tsx` | PARTIAL | 不展示充分性与证据 | 新增 `SufficiencyPanel` |
| MCP | `internal/mcpserver` | PASS | 无 | 复用（`get_processing_status`/`build_context` 自动带新字段） |
| 发布验证 | `docs/productization/` | PARTIAL | 无 Sprint 3 12 步演示与命令清单 | 新增脚本、清单与报告 |
| Docker GPU | `Dockerfile`、`docker-compose.yml` | OUT_OF_SCOPE | 镜像无 Python/torch；无 GPU profile | 记录探针失败，不声称容器 GPU 支持 |

## 数据使用

只读取授权夹具的元数据/时长：`data/full-transcription/segments/segment-01.pcm`（120 s）与 `data/asr/ph261004155454/run.json`（拼接 36.95 min）。未读取私有录音正文，未复制音频或转写文本进仓库。
