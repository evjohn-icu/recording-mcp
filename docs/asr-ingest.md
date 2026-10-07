# 豆包流式语音接入（asr-ingest）

`bin/asr-ingest` 是语音进入记忆库的接入程序（ingress adapter）：它直接连火山引擎 **Agent Plan** 的豆包流式语音识别服务，把录音切成窗口、并发识别、解析二进制协议帧，再用 `internal/adapters/doubao` 归一化、`memory.Store.IngestTranscript` 入库。它取代了 `scripts/asr_full.py` + `scripts/asr_to_context.py` 这两段上游脚本，不再需要外部 agent（pi）或 Python 运行时；两个脚本保留为可选实验工具，不再是主链路。

## 端点与资源（Agent Plan）

| 项 | 值 |
| --- | --- |
| 双向流式（全双工，默认） | `wss://openspeech.bytedance.com/api/v3/plan/sauc/bigmodel_async` |
| 流式输出 | `wss://openspeech.bytedance.com/api/v3/plan/sauc/bigmodel_nostream` |
| Resource-Id（ASR，豆包流式语音识别2.0） | `volc.seedasr.sauc.duration` |
| Resource-Id（TTS，豆包语音合成2.0） | `seed-tts-2.0` |
| 鉴权 | `X-Api-Key`（Agent Plan key）+ `X-Api-Resource-Id` |

**不要**使用 `https://ark.cn-beijing.volces.com/api/v3`（按量计费入口）。Agent Plan 的 key 与路由绑定：同一把 key 打非 plan 路由（`/api/v3/sauc/bigmodel`）握手即被拒，因此本项目的协议层固定走 plan 路由，端点可用 `--endpoint` 覆盖。语音模型不支持 Auto 或控制台切换，Resource-Id 必须显式指定（默认已内置）。超额后付费需在控制台开启。

`GizClaw/doubao-speech-go` 等开源 SDK 已评估：其 WS 路径写死 `/api/v3/sauc/bigmodel` 且没有端点覆盖项，plan key 在该路由不可用，因此未采用；WebSocket 层使用 `github.com/gorilla/websocket`，协议编解码（约 150 行）在本仓库内实现并带测试。

## 用法

```sh
make build
export VOLC_API_KEY=...            # Agent Plan API key
./bin/asr-ingest --audio recording.m4a --db data/context-memory.db \
  --conversation-id conversation:demo --recording-id recording:demo
```

| 参数 | 说明 |
| --- | --- |
| `--format auto\|pcm` | auto 用 ffmpeg 解码任意音频；pcm 直接吃 16 kHz 单声道 s16le |
| `--segment-ms 300000` `--overlap-ms 2000` | 窗口长度与相邻窗口重叠 |
| `--concurrency 3` | 并发窗口数 |
| `--packet-ms 200` | 音频帧发送节奏。默认 `200` 按实时节奏（1×，300s 长窗唯一可行档）；`0` = 无节奏整段推送，只对短窗安全且快得多 |
| `--out <dir>` | 运行目录（默认 `data/asr/<文件名>-<时间戳>`） |
| `--db` / `--conversation-id` / `--recording-id` | 给定时把完成的窗口入库；两个 id 缺省时按音频文件名派生（`recording:<name>` / `conversation:<name>`） |
| `--resource-id` / `--endpoint` / `--model-name` | 默认 `volc.seedasr.sauc.duration` / plan 全双工端点 / `bigmodel` |
| `--key-env` / `--key` | key 来源，默认环境变量 `VOLC_API_KEY` |

不带 `--db` 时只产出审计产物，不写记忆库。

### 批量导入档位（实测）

| 档位 | 300s 真实音频 | 20s 片段 | 说明 |
| --- | --- | --- | --- |
| `--segment-ms 300000 --packet-ms 200`（默认） | ~300s（1× 实时） | 22s | 与参考实现一致；分句粒度较粗（300s 单窗 31 轮） |
| `--segment-ms 60000 --packet-ms 0`（快档） | **9.7s** | 2.4s | 8 分钟录音 47s 跑完（8 窗 56 轮）；窗口变小会改变分句粒度 |
| `--segment-ms 300000 --packet-ms 0` | **失败**（服务端 8s 等包超时后杀会话） | — | 长段无节奏不可用 |

结论：文件批量导入用快档（`--segment-ms 60000 --packet-ms 0`）；需要与旧链路完全一致的分句粒度时用默认档。窗口大小只影响分句粒度，不丢音频覆盖。

## 产物（默认目录 0700、文件 0600）

```text
<out>/run.json                              # 清单：窗口、尝试、覆盖、入库摘要
<out>/segments/segment-01.pcm               # 该窗口的 16 kHz 单声道 PCM
<out>/segments/segment-01-attempt-1.jsonl   # 解析后的服务端帧（逐行）
<out>/segments/segment-01-server-only.jsonl # 成功响应帧（adapter 的输入）
<out>/segments/segment-01-envelope.json     # 来源信封（asrinput.Metadata）
<out>/segments/segment-01-normalized.json   # 归一化转写（asr-input/0.1）
```

离线链路仍可复现同一结果：

```sh
./bin/adapter-doubao --input <out>/segments/segment-01-server-only.jsonl \
  --metadata <out>/segments/segment-01-envelope.json --output /tmp/normalized.json
./bin/import-transcript --db data/context-memory.db --input /tmp/normalized.json
```

`asr-ingest --db` 的入库结果与这条链路逐字节一致（回归测试与真机冒烟均验证）。

## 失败与重试

- 每个窗口最多 3 次尝试，退避 5s、10s；服务端错误帧（例如 `45000081` 等包超时）记录在 `run.json` 的 `attempts[].error`。
- 任一窗口最终失败 → 整轮非零退出，且**不入库**（不会导入半份录音）。
- 音频帧必须携带结束包（`flags=2` 且 sequence 取负），否则服务端会一直等下一个包直到 8 秒超时——这是最容易踩的协议点，`internal/asrstream` 的测试钉住了它。

## 测试

```sh
go test ./internal/asrstream/                      # 协议编解码、帧序列、桩服务端全流程、分段器
ASR_API_KEY=... go test ./internal/asrstream/ -run TestLiveTranscribe -v
```

真机测试打真实 plan 端点，默认取 `data/sample-validation/intro-90s-16k-mono.pcm` 的前 3 秒；可用 `ASR_ENDPOINT`、`ASR_PCM` 覆盖。
