# ASR 到记忆层的输入契约草案

本项目接收上游已经完成的语音识别结果，负责保存沟通、建立记忆与通过 MCP 提供上下文。本草案根据本地豆包 ASR 实测响应制定。最终分片接收、豆包适配器和独立注释修订已实现，见 [运行说明](context-layer-runtime.md)。本页同时描述后续设计；转写替换（`supersedes_revision`，每次替换只有一个活动修订）与派生记忆失效传播（stale 标记与自动重算）已实现：旧修订保留为审计历史，其派生笔记与候选被排除出当前检索。原话入库无需预先准备 annotations。

## 实际返回的结构

此次使用的是 WebSocket 响应，不是 HTTP webhook。服务端二进制帧由接入程序解析后，产生 `message_type`、`flags`、`sequence`、`is_last_package`、`code` 和 `payload` 等记录字段；这些解析字段不能宣称是豆包 JSON 自带的字段。

实测 JSON payload 包含 `audio_info.duration` 和 `result`。`result.text` 是当前累计全文，`result.utterances` 是分句数组；每句有 `text`、`definite`、可缺失的 `start_time` / `end_time`、`additions.speaker_id` 和 `words`。初始化响应可能只有 `result.additions`，不能当作空会话或转写成功。

`definite=true` 表示分句稳定，不等于整条流结束。最终帧标志由协议适配器解析；本次实测最终 flags 为 3。不能简单以全文非空或分句 definite 判断完成。累计全文与分句数组可能在多帧重复出现，不能逐帧追加成新的历史沟通。

## 上游提交边界

0.1 建议首先接收最终分片快照。上游添加以下来源封套，再提交供应商原始 payload：

| 字段 | 用途 |
| --- | --- |
| provider / provider_request_id | 供应商与一次识别请求的标识 |
| conversation_id | 显式会话归属；当前接收器要求提供，不从分片或文件名猜测 |
| device_id / recording_id | 设备与录音的稳定标识，不依赖文件名 |
| segment_id / segment_offset_ms / segment_duration_ms | 分片及其相对整段录音的位置 |
| transcript_revision | 一次转写版本，重新识别时变化 |
| completion_status | final、partial 或 failed；只有 final 进入最终转写视图 |
| recording_started_at / capture_timezone | 可为空；不能从文件名补猜绝对时间 |
| source_audio_sha256 / source_ref | 可选来源哈希与媒体引用，本层无需读取音频 |
| received_at | 本层生成接收时间 |
| payload | 原始 ASR JSON，保留供追溯与重新适配 |

此封套是本项目建议的接收格式，不是豆包接口要求。模型版本、请求选项、协议最终标志与服务日志标识也应作为来源元数据保留。

## 转为记忆的规则

1. 先保存原始快照和来源，再标准化逐句记录。不要求提前有主题、摘要、人物实名或讨论结论，原话可以独立入库。
2. 使用 provider、request、segment 和 transcript revision 限定识别结果。相同最终快照重放幂等；同键不同内容报冲突。最终快照内的分句位置可生成该版本内的稳定 ID，不能声称重跑后的相同位置还是同一句。
3. `result.text` 保留为供应商全文；逐句记忆以 `utterances` 为依据。两者不能分别导入成两套重复对话。
4. 分句时间加上 segment offset 得到整段录音位置。缺失分句边界保持 null。词级时间可另外生成有来源标记的派生边界，但不能冒充供应商返回的分句边界；负数词时间属于不可用定位，原始值仍保留。
5. speaker 标签限定在该识别请求和处理版本内，真实人物 ID 默认为 null。跨片合并和真实身份确认属于后续带依据的关联操作。
6. 供应商未返回的置信度保持 null，不能把 definite 或最终帧当成准确率。原始词级定位和供应商 additions 留在证据层，MCP 默认不输出全部原始响应。
7. 分片重叠不能仅凭同一句文字就去重。保留原始记录，结合时间与重叠范围确认重复；整段录音完成须检查所有分片状态和覆盖范围。
8. 新转写版本追加保存，更正当前转写视图；依赖旧分句的话题、摘要和其他记忆标为待复核，不能静默继续视为最新结论。

## 后续记忆处理与 MCP 输出

主题关联、人物关联、摘要和结构化记忆是独立的派生步骤，分别记录处理状态、版本、原话引用与覆盖范围。未分析不能解释为没有相关内容；已有转写不能宣称已经完成记忆提取。

MCP 应支持检索已存原话与派生记忆、展开原话依据，并返回处理覆盖情况。缺少派生记忆时仍能通过显式原话检索查到沟通。现有实验的关键词查询可显式包含原话，但尚未提供这些完整的处理状态和修订语义。

下一步实现应先补 ASR 结果适配与原话独立入库，再补可重试的记忆处理和版本化关联。ASR 调用、音频解码和声学识别不属于这一层。
