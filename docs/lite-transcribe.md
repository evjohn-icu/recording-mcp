# lite-transcribe(doubao-seed-2.1-lite 逐字转写流水线)

`scripts/lite-transcribe.py`:不依赖 ASR plan 配额的备用转写管线,走 Ark coding gateway 的
`doubao-seed-2.1-lite` 多模态入口,输出带 `[mm:ss]` 定位标记的逐字稿 `<stem>.lite.txt`。

## 何时用

- ASR plan 配额耗尽/服务不可用(`asr-ingest` 报 `init ack timeout`)时的兜底
- 需要"人声全召回"的场合(实测对远场/小声对话召回优于 ASR plan 流式管线)
- 不入库:产物是纯文本稿,无 recordings/turns 落库;要入库仍走 `asr-ingest`

## 用法

```sh
python3 scripts/lite-transcribe.py \
  --audio /path/to/XXX.mp3 [--audio more.mp3 ...] \
  --out /path/to/transcripts \
  [--chunk-seconds 240] [--concurrency 3] [--retries 3] \
  [--model doubao-seed-2.1-lite] [--key ...]
```

Key 解析顺序:`--key` > `$ARK_CODING_API_KEY` > `~/.omp/agent/models.yml` 的
`volces-coding-02.apiKey`。依赖:`ffmpeg`/`ffprobe`(linuxbrew 路径已在 PATH)、Python 3 标准库。

## 流程

1. **分块**:`ffmpeg silencedetect` 取静音中点,对齐到 `--chunk-seconds`(默认 240s,±30s 窗);
   计划缓存于 `<work>/<stem>/plan.json`
2. **VAD 跳过**:块内发声占比 < 2% 直接记"(无有效语音)",不发请求
3. **转写**:16kHz 单声道 24kbps mp3,`input_audio` part;`thinking: disabled`(实测质量无差、
   token 减半);提示词强制"只输出转写文本、小声对话也要转写、无时间戳"
4. **逐块质检**:时间戳模式(出现 ≥2 次)/空输出/超长/`(听不清)` 密度任一命中即用纠偏提示词
   重问一次;远场块以 `(听不清)` 为主属如实转写,接受而非判败;请求级失败退避重试 `--retries` 次
5. **块首补转**:块内有声但产出 < 0.3 字/秒时,对块首 ≤90s 窄窗二次转写,句级去重后拼接——
   修复"块首非语音污染注意力导致开头对话丢失"(实测 241s 块 40 字 → 补回 67 字)
6. **拼稿落盘**:块首 `[mm:ss]` 标记;退出码非 0 表示有块重试后仍失败(占位符标注)

断点续跑:块 mp3/txt 与 plan.json 均缓存,重跑只补缺失块(二跑 0 请求)。

## 已知边界(2026-10 实测)

- 无说话人分离、无轮次结构;标点为模型生成
- 长独白/多人重叠段可能出现篇幅缩水(强提示词后中位块相似度 0.68→尾部 0.9+;
  最差块 0.38→0.82,见 A/B 记录)
- 240s 块实测通过;更长单块未验证,加大会放大缩水风险
- 成本:约 6.3 audio_tokens/秒 + 少量文本 token;1.5h 音频 ≈ 3.4 万 audio_tokens
- 该模型明确不支持音频的兄弟款:`doubao-seed-2.1-pro` 返回
  `audio input is not supported by this model`,勿换模型名
