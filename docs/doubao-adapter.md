# 豆包 ASR 输入适配器

豆包适配器将上游已解析的 WebSocket 响应转换为供应商无关的转写对象。它是 Context Layer 的输入边界：后续记忆处理读取统一对象，不必理解豆包的字段和协议标志。

此命令不连接豆包、不读取音频，也不需要 API Key。它处理此前保存的 JSONL 响应帧；协议二进制解码仍由上游接入程序完成。HTTP webhook 和其他 ASR 产品的返回格式不在此适配器范围内。

## 使用

```sh
make build
./bin/adapter-doubao \
  --input ./data/provider-responses.jsonl \
  --metadata ./data/source-metadata.json \
  --output ./data/normalized-transcript.json
```

来源封套示例（全部为合成标识）：

```json
{
  "provider": "doubao",
  "provider_request_id": "request-demo-01",
  "device_id": "device:demo",
  "recording_id": "recording:demo",
  "conversation_id": "conversation:demo",
  "segment_id": "segment-01",
  "segment_offset_ms": 0,
  "segment_duration_ms": 300000,
  "transcript_revision": "asr-v1",
  "completion_status": "final"
}
```

录音的绝对起始时间、时区、音频哈希、媒体引用和供应商模型信息可选。设备 ID 由上游登记，不能把匿名导入标识当作已经核实的硬件序列号。音频哈希缺失时保持缺失，响应内容哈希不能代替音频哈希。

## 适配边界

- 输入是单个识别请求的帧序列。最终帧确认完成，`definite` 只描述分句稳定性；累计中间结果不会逐帧追加为新原话。
- 分句文字、时间位置和匿名说话人转换为统一字段；时间位置加上分片偏移。缺失时间、人物身份和未知置信度保持为空。
- 原始最终 payload 保留词级信息、`additions` 和未知字段，供追溯。负数词时间留在原始证据中，不拿来补造分句时间。
- 分句 ID 与说话人作用域包含来源、录音、请求及版本，避免不同供应商或请求的同名标签被误合并。
- 不凭相同文字跨分片去重，不提取话题、摘要或承诺，也不匹配真实人物。
- 不完整、错误或矛盾的结果必须拒绝；失败不应覆盖已有输出文件。例外：final 结果既无 text 也无 utterances（如静音段）属于正常空结果，按“已接收、零轮次”转换并记 warning，原始 payload 保留在 raw_source。

输出是记忆层的输入产物。本命令本身不写 SQLite，不调用 MCP，也不声称完成记忆分析。可用 `import-transcript --db ./data/context-memory.db --input ./data/normalized-transcript.json` 或 MCP `ingest_transcript` 写入公共记忆层。完整链路见 [运行说明](context-layer-runtime.md)。旧 `import-conversation` 保留另一种实验输入结构。

更完整的接收与版本设计见 [ASR 到记忆层的输入契约](asr-memory-input.md)。真实响应、来源封套和适配结果都应放在本地 `data/` 中，不随代码公开分发。
