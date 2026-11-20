# 本机 GPU 部署（SMART-01）

主部署形态是 **Mode B**：WSL2 原生 web + worker + `.venv-asr` 本机 ASR。Docker Compose 仍是非 GPU 部署；本轮不发布 GPU Compose profile。v0.4 新增可选音频分析（VAD/tagging/speaker 走 CPU ONNX、diarization 走 GPU，详见下文小节）；本轮只验证部署机制与锁，真实模型推理未执行。

## 前置条件

| 项 | 要求 |
|---|---|
| OS | Windows 11 + WSL2（本机验证）或 Linux |
| GPU | NVIDIA，空闲显存 ≥ 6000 MiB（qwen）/ 4500 MiB（ark） |
| 驱动 | 已验证 `610.88`，`nvidia-smi` 可用 |
| Python | `.venv-asr/bin/python`，含 `torch 2.8.0+cu126`、`transformers 4.57.6` |
| 模型 | `scripts/fetch-local-models.sh` 下载钉住快照（`scripts/model-revisions.json`） |
| 二进制 | `make build`（16 个二进制）与 `make web`（`web/dist/index.html`） |
| 媒体工具 | `ffmpeg` / `ffprobe` |

## 命令

```sh
# 仅前置检查 + 模型探针（加载一次模型，输出 device/dtype/VRAM）
MEMORY_PROFILE_PASSWORD=... scripts/local-gpu-up.sh \
  --profile sprint3 --port 8788 --profiles-root data/profiles --backend ark --check

# 单命令启动（前台；Ctrl-C/TERM 受控关闭并写 artifacts/local-gpu/startup.json）
MEMORY_PROFILE_PASSWORD=... scripts/local-gpu-up.sh \
  --profile sprint3 --port 8788 --profiles-root data/profiles --backend ark

# 五轮启停验收（全部成功才退出 0）
scripts/local-gpu-up.sh --profile sprint3 --backend ark --cycles 5 --no-build

# 可选本地 embedding（CPU 为支持默认；GPU 需 --gpu-lock）
scripts/local-gpu-up.sh --profile sprint3 --backend ark --with-embedding
```

`--env-file` 默认读取仓库根 `.env`；文件必须属于当前用户且不可被 group/other 读写（`chmod 600`），否则拒绝。它是被 `source` 的 shell 文件，视为可信输入。profile 不存在且设置了 `MEMORY_PROFILE_PASSWORD` 时会自动创建，否则报错退出。`--check` 在探针后停止，不启动 web/worker。`LOCAL_GPU_UP_*` 测试钩子仅在 `LOCAL_GPU_UP_TEST_MODE=1` 时生效。

## 探针与显存

`scripts/local-asr.py --probe` 只加载模型并输出一行 JSON：

```json
{"backend":"ark","model":"Edge0/ARK-ASR-0.6B","revision":"45776b5…","device":"cuda:0","dtype":"float16","model_load_ms":0,"vram_total_mb":8192,"vram_free_mb":0,"vram_peak_mb":0}
```

启动器把它写入 `artifacts/local-gpu/probe.json`，并打印 `load=…ms vram_total=…MiB vram_free=…MiB vram_peak=…MiB`。实测值见下方"实测"。

## 并发与锁

ASR、embedding 与音频分析可以通过同一锁文件串行使用 GPU。Go 在未配置锁路径时使用用户级默认值 `${XDG_RUNTIME_DIR:-$HOME/.local/state}/recording-mcp/recording-mcp-gpu.lock`，使用 `flock(LOCK_EX|LOCK_NB)` 每 200 ms 重试。多个 profile 使用该默认路径时会相互等待。

- `local-gpu-up.sh` 将用户级锁路径传给 worker 的 `--asr-lock` 和 CUDA embedding 服务的 `--gpu-lock`。`ASR_LOCK` 可覆盖这两处路径。ASR 默认等锁 10 分钟，CPU 或 fixture 不领取 GPU 锁。
- Python embedding 服务在 CUDA 设备上以 `--gpu-lock` 在模型加载与每批推理外加锁，超时返回 HTTP 503 `GPU busy`。
- CUDA 音频分析在整个适配器子进程生命周期内持锁。部署显式配置 `ASR_LOCK` 时，音频分析配置的 `gpu_lock` 需指向同一绝对路径。持锁后发现空闲显存小于 1024 MiB 时返回可重试的 `audio_gpu_busy`。
- `--with-embedding` 同时向 worker/web 传 `--embed-input-type` / `--semantic-input-type`（本地 Qwen 服务的 query 指令开关）；该字段默认关闭，避免严格 OpenAI 兼容端点拒绝未知字段。
- 启动器默认关闭音频分析。手动配置时，worker 和读取状态的 Web / MCP 进程需使用同一份配置。 [启用与显式入队](../processing.md#可选音频分析启用与显式入队)

锁的串行性由 `internal/gpulock` 测试（跨进程争用、同一锁路径跨 profile、取消）与 `internal/cloudsync/gpu_lock_test.go::TestGPUFileLockSerializesLocalASR` 验证（trace 必须是 start/end/start/end）。

## 降级

- 本机 ASR 选择失败（例如 `.venv-asr` 缺模块）或 CUDA 探针失败时，worker 与 web **不退出**：`transcriber=nil`，其他阶段继续；ASR 任务返回 `asr_unavailable` 与动作。CUDA 探针使用 transcriber 实际解析出的解释器（空 `--asr-python` 时是 `.venv-asr`），探针失败同样置空 transcriber。
- 探针与可选 embedding 服务以 `env -i` 白名单环境启动（PATH/HOME/CUDA/HF_*），不继承 API key 或 profile 密码。
- `GET /api/capabilities` 返回 `asr.available=false` 与原因（`asr_not_configured`/`worker_offline`/`worker_asr_mismatch`/`gpu_unavailable`/`model_probe_failed`），并带 `gpu_name`、`vram_total_mb`、`vram_free_mb`、`device`、`dtype`、`error`。
- Web 的导入面板与详情页显示原因、GPU 名称、显存与错误文本；其余功能保持可用。
- `doubao` 缺 key 仍是启动错误，不降级。

## 关闭行为

`SIGTERM`/`SIGINT` 发送给启动器后：向 `local-start.sh` 子进程发 `TERM`，最多等待 30 秒，超时 `KILL`；停止 embedding 服务；写 `artifacts/local-gpu/startup.json`（每轮冷启动毫秒、模型加载毫秒、峰值显存、关闭毫秒）。

## 可选音频分析（v0.4）

音频分析按能力逐个声明设备：**vad** = Silero VAD（ONNX，CPU）、**tagging** = Zipformer（sherpa-onnx，CPU）、**diarization** = pyannote Community-1（GPU）、**speaker** = CAM++（ONNX，CPU）。每能力在 `audio-analysis-config-v1` 里钉住 `enabled` / `model_id` / `model_revision` / `files[]`（本地路径 + 64 hex SHA-256）/ `device` / `frontend_id`；模型文件必须本地存在且哈希匹配，缺失只把该能力降级为 unavailable，不影响其它能力与 ASR。

```sh
# worker 执行 + web/context-memory 读投影指向同一份配置；模型路径相对配置文件目录解析
./bin/context-worker --profile sprint3 --profiles-root data/profiles \
  --audio-analysis-config ./audio-analysis.json
./bin/context-web --profile sprint3 --profiles-root data/profiles \
  --audio-analysis-config ./audio-analysis.json
```

- **适配器环境**：`requirements-audio-analysis.txt`（numpy / onnxruntime(CPU) / sherpa-onnx / kaldi-native-fbank / pyannote.metrics）。必须是独立 Python 环境：不与 `.venv-asr` 混装，CPU 与 GPU onnxruntime 不可共存；diarization 需要 GPU 组（torch cu126 + onnxruntime-gpu + pyannote.audio）。
- **gated 权重**：Community-1 权重为 CC-BY-4.0 且 gated——由操作者账号一次性取到本地目录并在 config 里钉哈希，运行期 `HF_HUB_OFFLINE=1`，适配器绝不下载或接受条款。
- Worker 启动时会终结未配置或不可用能力的可领取任务，使依赖这些任务的处理继续推进。
- **证据边界**：本轮只验证配置、锁、存储与适配器协议（`go test ./internal/audioanalysis`、`go test ./internal/gpulock`、`scripts/test_audio_analysis.py`）；真实模型推理与 gated 权重部署为 NOT_RUN/BLOCKED（见 [v0.4 发布报告](../engineering/context-engine-v04-release-report.md) Sprint C）。本节描述的是部署机制，不是已验证的模型性能。

## Docker GPU 探针（事实记录）

`docker run --rm --gpus all nvidia/cuda:12.6.3-base-ubuntu22.04 nvidia-smi` 在本机 **FAIL**：

```
docker: Error response from daemon: failed to resolve reference "docker.io/nvidia/cuda:12.6.3-base-ubuntu22.04":
... connecting via app settings HTTPS proxy http://172.22.23.142:7890:
dial tcp 172.22.23.142:7890: connectex: No connection could be made because the target machine actively refused it.
```

`docker info` 的 runtimes 只有 `runc` / `io.containerd.runc.v2`，没有 `nvidia` runtime。**本轮不声称任何 GPU 容器支持**；镜像本身也不含 Python/torch。若将来需要 Mode A，应作为独立部署变更：新增 GPU 推理镜像 + 模型缓存卷 + runtime 配置，并重新做真实 GPU 容器作业验证。

## 实测（2026-11-09，RTX 2070 8192 MiB）

探针（`--check`，ARK float16）：

| 项 | 值 |
|---|---|
| 模型加载 | 10402 ms（首轮）/ 9207 ms（五轮前重跑） |
| 探针总耗时 | 11255 ms |
| 加载后空闲显存 | 4804 MiB / 8191 MiB |
| 峰值显存 | 2210 MiB |

五轮启停（`--cycles 5 --no-build`，退出 0）：

| 轮次 | 冷启动 | 模型加载 | 峰值显存 | 关闭 |
|---|---|---|---|---|
| 1 | 1590 ms | 9207 ms | 2210 MiB | 253 ms |
| 2 | 1597 ms | 9207 ms | 2210 MiB | 253 ms |
| 3 | 1591 ms | 9207 ms | 2210 MiB | 253 ms |
| 4 | 1593 ms | 9207 ms | 2210 MiB | 254 ms |
| 5 | 1590 ms | 9207 ms | 2210 MiB | 253 ms |

`artifacts/local-gpu/startup.json` 保存同样数据（2026-11-09 修正 shell 后重跑）。冷启动不含模型加载（探针先于启动完成），加载耗时单独记录；关闭均在 260 ms 内完成。单周期前台模式在 TERM 时实测关闭耗时并写入 `shutdown_ms`。

模型快照下载（`scripts/fetch-local-models.sh`）：ARK 2.44 GiB、Qwen3-ASR 4.38 GiB、Qwen3-Embedding 1.12 GiB（SHA-256 校验通过）、Jev Decision 1.42 GiB（SHA-256 校验通过）。
