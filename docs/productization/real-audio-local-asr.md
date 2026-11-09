# 真实录音上的本地 ASR 质量（CER）

执行日期：2026-11-09。硬件：RTX 2070 8 GB，float16，`.venv-asr`（torch 2.8.0+cu126）。
授权：用户明确允许使用本机 `data/` 下的录音做测试。**音频与转写正文不提交**，本文只记录聚合指标。

## 数据

- 录音：`data/full-transcription/segments/segment-01.pcm`，120 s，16 kHz mono（来自已有 `asr-excerpt-120s.mp3` 流水线）。
- 参考稿：`data/full-transcription/transcript.json`（18 turns，同一条录音的既有 doubao 转写；不是人工逐字校对稿）。
- 评分：字符级 CER，数字统一为阿拉伯数字，忽略标点与空白；`overlap_ms=0`，30 s 窗口，避免重叠文本重复计入。

## 结果

| backend | 模型/revision | 参考字符 | 识别字符 | 编辑距离 | CER |
|---|---|---|---:|---:|---:|
| `ark` | `Edge0/ARK-ASR-0.6B` @ `45776b56` | 500 | 496 | 52 | **10.4%** |
| `qwen` | `Qwen/Qwen3-ASR-1.7B` @ `7278e1e7` | 500 | 499 | 28 | **5.6%** |
| magpie chat | `ev-ma03/3106-agent/doubao-seed-2.1-lite` | 500 | 500 | 19 | **3.8%** |

同一录音经 Go 管线（ffmpeg → ingest → asr → SQLite）跑通：

```sh
LOCAL_ASR_SMOKE=1 LOCAL_ASR_SAMPLE=/tmp/real-120s.wav \
LOCAL_ASR_BACKEND=ark \
go test ./internal/ingest -run TestLocalASREndToEndSmoke -count=1 -v
# PASS 32.41s
# qwen 同命令：PASS 36.61s
```

复现 CER：

```sh
python3 scripts/eval-asr-cer.py \
  --pcm data/full-transcription/segments/segment-01.pcm \
  --reference data/full-transcription/transcript.json \
  --backend qwen --out artifacts/real-asr/qwen.json
```

输出只含聚合指标，不含转写正文。

## magpie 渠道实测

| 模型 | 协议 | 结果 |
|---|---|---|
| `ev-ma03/3106-agent/doubao-seed-2.1-lite` | `/v1/chat/completions` + `input_audio`（base64 wav） | **可用**；公开样例逐字正确，120 s 真实录音 CER 3.8%（11.7 s） |
| `ev-ma03/3106-agent/volc.seedasr.sauc.duration` | `/v1/audio/transcriptions` | 上游 Ark 404；聊天路由 `UnsupportedModel`（agent plan） |
| `ev-ma03/group/asr` | `/v1/audio/transcriptions` | 同上游 Ark 404 |
| `ev-ma03/agy-cli/gemini-3.8-flash` | chat / `generateContent` + inline audio | 上游地区封锁 `FAILED_PRECONDITION` |
| `doubao-seed-2.1-pro` | chat + `input_audio` | 明确不支持音频输入 |
| `Qwen3.8-*` / `MiniCPM*` / `nemotron-vl` | chat + `input_audio` | 返回空、拒绝音频或无音频端点 |

`seedasr.sauc` 的权威路径仍是火山 WSS `plan/sauc/bigmodel_async`（`data/full-transcription/run.json`），
magpie 没有把它包成 HTTP 转写接口。参考稿本身来自该 WSS 通道，因此 3.8% 是“同厂商模型间的一致度”，
不是独立人工金标。音频经用户授权后发往 magpie；转写正文未入库。

## 限制

- 单条 120 s 录音；参考稿来自既有 ASR，不是人工金标，CER 只能作相对比较。
- 未测长音频、并发与说话人分离；窗口级时间戳，非逐字对齐。
