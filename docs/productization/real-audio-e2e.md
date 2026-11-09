# 真实音频端到端验证（2026-11-09）

## 模式与披露

| 模式 | 转写来源 | 本轮状态 | 说明 |
|---|---|---|---|
| synthetic | `fixture` 转写器（确定性 fixture 文本） | PASS | 3 个生成 WAV：3 分钟、25 分钟、120 分钟 |
| real-audio-mock-asr | 真实音频 + `fixture` 转写 | 未执行（`NOT_RUN`） | 需要用户明确授权的真实音频；本机未使用 |
| real-asr | 真实音频 + doubao ASR | `NOT_RUN` | 需要 `--audio` 与 `VOLC_API_KEY`；未发起付费调用 |

**fixture 披露**：synthetic 模式的转写是 `fixture transcript ...` 确定性文本，不是真实 ASR 输出，不代表识别准确率。未读取 `data/asr`、`data/full-transcription`、`data/worldline-local` 或任何私人录音。

## 实际命令与退出码

```sh
python3 scripts/product-audio-e2e.py --mode synthetic --out artifacts/audio-e2e
# exit=0
```

`artifacts/audio-e2e/audio-e2e-result.json` 为原始结果。

## 指标

| 文件 | 时长 | 预期窗口 | 活跃附件 | turns | 缺口 | 索引 | 处理 | 单文件耗时 |
|---|---|---|---|---|---|---|---|---|
| synthetic-180s.wav | 3 分钟 | 3 | 3 | 3 | 0 | ready | completed | 0.55 s |
| synthetic-1500s.wav | 25 分钟 | 25 | 25 | 25 | 0 | ready | completed | 1.18 s |
| synthetic-7200s.wav | 120 分钟 | 120 | 120 | 120 | 0 | ready | completed | 8.73 s |

总计：148 音频分钟；148 turns / 148 活跃附件；全部 job `succeeded`；总 wall time 13.29 s；子进程 CPU 5.68 s；`ru_maxrss` 73.5 MB；CPU 成本 0.0384 秒/音频分钟。货币成本 `NOT_RUN`（未调用付费 API）。

覆盖区间：每个文件从 0 到完整时长连续覆盖（缺口列表为空），不是用 `MAX(global_end_ms)` 代表完整。

## 10 处音频抽查

JSON 中每个文件记录前 10 个 turn 的 `HEAD /api/turns/{id}/audio`：状态 200，`X-Audio-Start-MS`/`X-Audio-End-MS` 与窗口一致（例如 120 分钟文件最后抽查为 540000–600000 ms）。三份文件共 30 次抽查，全部 200。

## 检索一例

`synthetic-*` 上传后 `GET /api/search?q=fixture` 分别返回 3/5/5 条命中。

## 未执行项

- real-audio-mock-asr：未提供授权真实音频。复现命令：
  ```sh
  python3 scripts/product-audio-e2e.py --mode real-audio-mock-asr --audio /path/to/authorized.wav --out artifacts/audio-e2e-real
  ```
- real-asr：未提供音频与凭据。复现命令：
  ```sh
  VOLC_API_KEY=... python3 scripts/product-audio-e2e.py --mode real-asr --audio /path/to/authorized.wav --out artifacts/audio-e2e-real
  ```
- 真实 ASR 准确率：`NOT_RUN`（没有人工参考转写）。
