# 本机 GPU 部署（SMART-01）

主部署形态是 **Mode B**：WSL2 原生 web + worker + `.venv-asr` 本机 ASR。Docker Compose 仍是非 GPU 部署；本轮不发布 GPU Compose profile。

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

本机 ASR 与可选 embedding 共用 8 GB 显存：

- Go 侧 `--asr-lock` 指向 `data/profiles/<profile>/gpu.lock`，以 `flock(LOCK_EX|LOCK_NB)` 轮询（200 ms），超时（默认 10 分钟）报错。
- Python embedding 服务在 CUDA 设备上以 `--gpu-lock` 在模型加载与每批推理外加锁，超时返回 HTTP 503 `GPU busy`。
- 启动器总是把同一锁路径传给 worker；`--with-embedding` 在 GPU 设备上自动追加 `--gpu-lock`。

锁的串行性由 `internal/cloudsync/gpu_lock_test.go::TestGPUFileLockSerializesLocalASR` 验证（trace 必须是 start/end/start/end）。

## 降级

- 本机 ASR 选择失败（例如 `.venv-asr` 缺模块）或 CUDA 探针失败时，worker 与 web **不退出**：`transcriber=nil`，其他阶段继续；ASR 任务返回 `asr_unavailable` 与动作。CUDA 探针使用 transcriber 实际解析出的解释器（空 `--asr-python` 时是 `.venv-asr`），探针失败同样置空 transcriber。
- 探针与可选 embedding 服务以 `env -i` 白名单环境启动（PATH/HOME/CUDA/HF_*），不继承 API key 或 profile 密码。
- `GET /api/capabilities` 返回 `asr.available=false` 与原因（`asr_not_configured`/`worker_offline`/`worker_asr_mismatch`/`gpu_unavailable`/`model_probe_failed`），并带 `gpu_name`、`vram_total_mb`、`vram_free_mb`、`device`、`dtype`、`error`。
- Web 的导入面板与详情页显示原因、GPU 名称、显存与错误文本；其余功能保持可用。
- `doubao` 缺 key 仍是启动错误，不降级。

## 关闭行为

`SIGTERM`/`SIGINT` 发送给启动器后：向 `local-start.sh` 子进程发 `TERM`，最多等待 30 秒，超时 `KILL`；停止 embedding 服务；写 `artifacts/local-gpu/startup.json`（每轮冷启动毫秒、模型加载毫秒、峰值显存、关闭毫秒）。

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
