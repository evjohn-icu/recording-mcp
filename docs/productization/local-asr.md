# 本地离线 ASR（ARK-ASR / Qwen3-ASR，float16）

两个 Apache-2.0 模型可完全离线运行，不需要云端 key：

| backend | 模型 | 特点 |
|---|---|---|
| `ark` | `Edge0/ARK-ASR-0.6B` | 0.6B 解码器 + Whisper 风格编码器；中文 CER 约 4.3%（模型卡） |
| `qwen` | `Qwen/Qwen3-ASR-1.7B` | 30 语言 + 22 中文方言（合计 52）；中文 CER 约 3.6%（模型卡） |

两者都只以 **float16** 运行。内置模型默认固定到 2026-11-09 验证过的快照：

| backend | revision |
|---|---|
| `ark` | `45776b56d58cdfb2e2eb632f7e110f38684633e0` |
| `qwen` | `7278e1e70fe206f11671096ffdd38061171dd6e5` |
RTX 2070（Turing）没有原生 bf16；`--asr-dtype` 只接受 `float16`。

## 安装运行时

模型权重不进仓库。推荐独立 venv：

```sh
uv venv --python 3.12 .venv-asr
uv pip install --python .venv-asr/bin/python -r requirements-local-asr.txt
# 模型在首次使用时自动下载；也可预先下载：
.venv-asr/bin/python - <<'PY'
from huggingface_hub import snapshot_download
print(snapshot_download("Edge0/ARK-ASR-0.6B"))
print(snapshot_download("Qwen/Qwen3-ASR-1.7B"))
PY
```

显存参考（float16）：ARK 约 3 GB，Qwen3-ASR-1.7B 约 5 GB；8 GB 卡可单模型运行。
首次加载模型约数秒到十几秒；每个转写任务启动一次 Python 进程，模型随之加载。

## 启动

Web 与 worker 必须使用同一个 provider：

```sh
# ARK
ASR_PROVIDER=ark bin/context-web ... --asr-provider ark
ASR_PROVIDER=ark bin/context-worker ... --asr-provider ark

# Qwen3-ASR
ASR_PROVIDER=qwen bin/context-worker ... --asr-provider qwen

# 等价写法
bin/context-worker ... --asr-provider local --asr-backend qwen
```

常用参数：

| 参数 | 默认 | 说明 |
|---|---|---|
| `--asr-python` | `./.venv-asr/bin/python`（存在时），否则 `python3` | 解释器；启动时探测 `torch`/`transformers`/`numpy`（Qwen 另需 `qwen_asr`） |
| `--asr-script` | `scripts/local-asr.py` | runner 路径；从仓库根启动或传绝对路径 |
| `--asr-model` | 按 backend 取模型卡 id | 本地目录或 Hugging Face id；远程自定义 id 必须同时给 revision |
| `--asr-revision` | 内置模型固定到已验证快照 | 自定义模型时显式固定 commit；ARK 使用 `trust_remote_code`，不固定会随 main 变化 |
| `--asr-dtype` | `float16` | 仅支持 float16 |
| `--asr-device` | `cuda:0` | `cpu` 也可，但速度显著下降 |
| `--asr-window-ms` | `30000` | 窗口长度；ARK 上限 30 s |
| `--asr-overlap-ms` | `2000` | 相邻窗口重叠 |
| `--asr-max-new-tokens` | `512` | 每窗口生成上限 |
| `--asr-language` | 空（自动） | 例如 `Chinese`；qwen 作为参数，ark 写入提示词 |

runner 也可单独运行，便于排查：

```sh
.venv-asr/bin/python scripts/local-asr.py \
  --backend ark --model Edge0/ARK-ASR-0.6B \
  --pcm /tmp/audio.pcm --dtype float16 --device cuda:0
```

输出一个 JSON 文档（窗口 index/start_ms/end_ms/text），进度写 stderr。

## 验证与限制

- 单元测试：`go test ./internal/cloudsync -run 'TestLocalTranscriber|TestSelectTranscriber'`
- 真实模型冒烟：见 [local-asr-smoke.md](local-asr-smoke.md)。
- 真实录音 CER（Qwen 5.6%、ARK 10.4%、magpie doubao-lite 3.8%，单条 120 s）：见 [real-audio-local-asr.md](real-audio-local-asr.md)。
- 时间戳精度是窗口边界，不是逐字对齐；需要字级时间请另接 forced aligner。
- 不伪造 `confidence`、录音起始时间或 `valid_from`。
- Docker 镜像不包含 Python/torch；本地 ASR 目前用于宿主机 worker（有 NVIDIA GPU 时）。
- 模型加载按任务计费；长录音建议先用 `--asr-window-ms` 控制单窗口长度。
- ARK 使用 `trust_remote_code=True`，模型代码以 worker 用户身份执行；runner 只继承 `PATH/HOME/CUDA/HF_*` 等白名单变量，不继承 API key 或 profile 密码。自定义模型必须显式固定 revision。
- `scripts/local-start.sh` 会透传 `ASR_BACKEND/ASR_MODEL/ASR_PYTHON/ASR_DEVICE/ASR_REVISION`；compose 不转发这些变量。
