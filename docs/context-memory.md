# 会话记忆实验实现

这个实现将分析后的录音转写、话题和带证据的讨论记录保存到 SQLite，再通过 MCP 查询。它用于验证从真实沟通到可追溯记忆的完整流程。接口带有 `experimental: true`，尚未实现 Context Layer 0.1 草案的全部契约。

## 保存内容

每场沟通保存录音标识、来源哈希、逐句文字、录音内的时间位置、局部说话人标签、主题和讨论记录。讨论记录区分观察、建议、请求、约束和意向；这几种记录不会自动生成承诺或已验证事实。

说话人的真实身份默认为未知。分片内相同的编号不能证明跨片或跨录音身份相同。缺少的时间保持为空，录音绝对时间不从文件名推断。上游 ASR 的分片末句可能略超分片结束位置；实验实现允许最多 5 毫秒的分片结束容差并保留原值，整段录音边界仍需合法。话题和摘要是对转写的解释，使用前应查看所引用的原话。

下述旧 `import-conversation` 导入排除空白文字和已标记的重叠重复句，但不改写来源文件。记录以稳定 ID 保存；同一批来源重放不重复插入，同 ID 内容变化会报冲突。现在可通过 `apply_annotations` 修订派生记忆并保留历史；转写分片可通过 `supersedes_revision` 替换（旧修订保留为审计历史，其派生记忆标记 stale 并自动重算），详见 [运行说明](context-layer-runtime.md)。新的供应商无关输入与完整工具列表见 [运行说明](context-layer-runtime.md)。

## 构建与导入

在项目根目录运行：

```sh
make build
./bin/import-conversation \
  --db ./data/context-memory.db \
  --transcript ./data/full-transcription/transcript.json \
  --annotations ./data/full-transcription/annotations.json \
  --run ./data/full-transcription/run.json
```

这里的输入路径表示使用者自己的本地分析产物，不随代码公开分发。`transcript.json` 来自录音分析步骤；`run.json` 保存来源和处理信息；`annotations.json` 包含会话标题、摘要、主题、讨论记录及其原话引用。导入命令本身不会调用语音服务或语言模型。

## MCP 查询

启动独立服务：

```sh
./bin/context-memory --db ./data/context-memory.db
```

客户端配置模板见 [context-memory.json](../examples/context-memory.json)，使用前替换绝对路径。

| 工具 | 用途 |
| --- | --- |
| `list_topics` | 发现已导入的话题 |
| `query_context` | 按关键词、话题、会话或局部说话人筛选；明确指定 `include_turns` 才检索原话 |
| `get_conversation_context` | 查看一场沟通的记录，可显式包含原话并分页 |
| `get_context_item` | 展开一条记录及其引用的原话证据 |

各工具的精确输入由 MCP `tools/list` 公布。本实验使用 `limit` 和 `offset` 分页，不支持草案要求的快照游标、统一字节预算、双时间查询或状态推导。关键词检索是字面匹配；`build_context` 另有可选的语义召回分支（离线哈希嵌入器，默认需 `bin/context-embed` 先建索引）与读取时上下文投影（`get_current_context`）；话题合并和自动人物身份确认尚未实现（身份只来自人工确认的 speaker binding）。

## 与既有代码的关系

`context-memory` 是独立的会话实验入口，`recording-mcp` 仍是早期事件原型。两个服务有部分同名工具，但输入输出契约不同，应在客户端中使用不同的服务器名称。既有原型数据库不会自动迁移。

SQLite 数据库、真实转写、录音以及真实注释文件均放在被忽略的 `data/` 目录。公开分发规则见 [公开发布约定](publication.md)。
