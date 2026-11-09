# 本地 ASR 冒烟记录（float16）

执行日期：2026-11-09。硬件：WSL2，NVIDIA RTX 2070 8 GB（驱动 610.88），12 vCPU，23 GB RAM。
运行时：`.venv-asr`（Python 3.12.13，torch 2.8.0+cu126，transformers 4.57.6）。

样例：Qwen3-ASR 模型卡公开样例 `asr_zh.wav`（4.203 s），转成 16 kHz mono S16LE PCM。
不包含用户私人录音。

## 1. 直接运行 runner

```sh
.venv-asr/bin/python scripts/local-asr.py \
  --backend ark --model Edge0/ARK-ASR-0.6B \
  --pcm .cache/asr-samples/asr_zh.pcm --dtype float16 --device cuda:0
# 退出码 0，wall 13.1 s
# {"backend":"ark",...,"text":"甚至出现交易几乎停滞的情况。"}

.venv-asr/bin/python scripts/local-asr.py \
  --backend qwen --model Qwen/Qwen3-ASR-1.7B \
  --pcm .cache/asr-samples/asr_zh.pcm --dtype float16 --device cuda:0
# 退出码 0，wall 14.0 s
# {"backend":"qwen","language":"Chinese",...,"text":"甚至出现交易几乎停滞的情况。"}
```

两个模型输出一致，与模型卡样例文本相同。

## 2. 端到端（ffmpeg → ingest → asr → SQLite）

```sh
LOCAL_ASR_SMOKE=1 LOCAL_ASR_SAMPLE="$PWD/.cache/asr-samples/asr_zh.wav" \
LOCAL_ASR_BACKEND=ark LOCAL_ASR_EXPECT="交易几乎停滞" \
go test ./internal/ingest -run TestLocalASREndToEndSmoke -count=1 -v
# PASS 12.29s

LOCAL_ASR_SMOKE=1 LOCAL_ASR_SAMPLE="$PWD/.cache/asr-samples/asr_zh.wav" \
LOCAL_ASR_BACKEND=qwen LOCAL_ASR_EXPECT="交易几乎停滞" \
go test ./internal/ingest -run TestLocalASREndToEndSmoke -count=1 -v
# PASS 12.97s
```

断言：真实 WAV 经 `ffprobe`/`ffmpeg` 导入，`ingest`/`asr` 两个 job 成功，turn 文本非空且包含预期中文，
`source_attachments.result_json` 记录 `provider` 为 `local-ark` / `local-qwen`。

## 3. 说明

- 每次任务启动一次 Python 进程，模型加载占上述耗时的大部分；短音频时推理本身约 1–2 s。
- 未做长音频（小时级）与并发压力测试；窗口默认 30 s / 重叠 2 s。
- 未评估 CER；上述只证明链路与 fp16 推理可用，不证明识别准确率。
