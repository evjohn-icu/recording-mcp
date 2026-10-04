# Context Layer 实验运行链路

本实现接收供应商无关的 ASR 分片结果，先持久保存原话与来源，再接收有原话依据的记忆注释，通过 MCP 提供查询与处理覆盖情况。ASR 调用、声学识别、自动人物匹配和自动语义提取不属于本轮实现。

## 从供应商结果到记忆

```text
豆包已解析响应 → 豆包 Adapter ─┐
其他 ASR → 对应 Adapter ─────┼→ asr-input/0.1 → SQLite → MCP
                            └→ 原始来源附件
```

其他适配器只需生成 `internal/asrinput` 定义的统一对象。公共记忆代码不导入豆包包，也不读取豆包特有字段。话题、摘要和真实人物身份都不是原话入库的前置条件。

```sh
make build
./bin/adapter-doubao --input ./examples/asr/doubao-final.jsonl \
  --metadata ./examples/asr/doubao-metadata.json --output ./data/normalized.json
./bin/import-transcript --db ./data/context-memory.db --input ./data/normalized.json
./bin/context-memory --db ./data/context-memory.db
```

上述输入是可公开的合成样例；替换为自己的已解析响应与来源信息即可接入。

上游为每个分片提供稳定的设备、录音、分片、请求和版本标识，以及显式 `conversation_id`。当前一条录音只能归属一个会话；不同设备的同名会话不会自动合并。入库返回的会话与录音 ID 是内部稳定 ID，后续查询使用返回值。分片是接收与处理单位，不等于现实中的一次谈话。来源附件保存供应商原始结果，音频哈希与响应内容哈希用途不同，不相互替代。

## MCP 工具

精确输入和输出结构以 `tools/list` 为准。

| 工具 | 作用 |
| --- | --- |
| `ingest_transcript` | 接收一个统一格式的最终分片；相同内容可重放，冲突拒绝 |
| `get_processing_status` | 查看已接收分片、原话数量、话题关联覆盖及未分类情况 |
| `apply_annotations` | 显式提交一版话题和讨论记忆，要求引用本会话原话 |
| `get_annotation_history` | 获取当前与历史注释版本 |
| `query_context` | 按关键词、会话、话题或限定范围的说话人查询 |
| `get_conversation_context` | 查看沟通内容和处理状态，按需分页展开原话 |
| `get_context_item` | 展开一条记录及引用的原话 |
| `list_topics` | 发现已保存的话题 |
| `list_conversations` | 列出本地记忆库中的会话：可按主题、分析状态与录音开始时间过滤，并按录音开始时间倒序游标分页返回摘要 |
| `get_evidence_context` | 读取某个转录轮次及其同一录音内相邻轮次的上下文，用于核对原始证据。`turn_id` 必填；`radius`（默认 3，负数按 0 处理）取目标前后各若干条；`max_bytes`（默认 16000）是逐条累加的文本字节预算，超出时停止并置 `truncated` |
| `search_memory` | 在原始转录与派生笔记中执行大小写不敏感的子串检索。`text` 必填（trim 后非空）；scope 可选 `raw`（仅转录）、`derived`（仅笔记）或 `both`（默认全部），并可用 `topic_id`、`started_after`、`started_before` 过滤 |
| `build_context` | 实验性：为任务组装带预算与来源的上下文包，返回 `items` / `evidence` / `coverage` / `gaps` / `truncated` / `degraded` / `degrade_reason` / `trace`。`query`、`conversation_id`、`topic_ids` 至少一项必填；`max_bytes` 默认 16000、`max_items` 默认 12；`include_raw_evidence` 纳入原始轮次证据。默认排除 stale 派生记忆；仅在服务端配置 `--decision-url` 时做决策重排，否则 `degraded=true` 但确定性结果仍完整返回 |
| `list_memory_links` | 列出话题之间的记忆关联（`proposed` / `confirmed` / `rejected`）。`status` 与 `topic_id` 可选；`limit` 缺省 50、上限 200，按 `created_at, id` 返回端点标题、理由、证据轮次与置信度 |
| `review_memory_link` | 复核一条关联建议。`link_id` 与 `action` 必填：`confirm` 把 `proposed` 变为 `confirmed`；`reject` 把 `proposed` 或 `confirmed` 变为 `rejected`；`reopen` 把 `confirmed` 或 `rejected` 退回 `proposed`。重复同一动作幂等，记录不删除 |

上表中的 `list_conversations`、`get_evidence_context` 与 `search_memory` 是与既有工具不同的检索契约：它们不走 `query_context` / `get_conversation_context` 的 offset 分页，而是返回游标（`next_cursor`），下一页请求把游标原样带回 `cursor` 字段。时间过滤字段（`started_after` / `started_before`）为 RFC 3339 字符串，且只对录音开始时间已知的会话生效；有界查询会排除开始时间未知的会话。`list_conversations` 的 `status` 只接受 `pending` / `partial`（大小写不敏感），语义与 `get_processing_status` 的 `analysis_status` 一致；`limit` 缺省 20、上限 100。`search_memory` 的 `limit` 同理，多读一条记录判断是否有下一页。

`list_memory_links` 与 `review_memory_link` 服务于关联建议层（见 [associations.md](associations.md)）：提议由 `context-associate` 写入并停留在 `proposed`，只有显式复核才改变状态；确认不代表合并话题，下游视图尚未实现。

`analysis_status` 为 `pending` 或 `partial`，不会自动宣称整场沟通已分析完成。空白分句留在原始附件，不计为可检索原话；不自动删除跨分片的重复文字。

`include_turns=true` 才将原话纳入 `query_context` / `get_conversation_context` 的结果。`search_memory` 不受该开关约束：命中直接区分 kind（派生笔记在前、原话在后，组内按 ID 升序），每条命中附带 `why` 说明命中原因；在没有提炼记录时检索仍可能只有原话命中，这表示尚未形成可检索的派生记忆，不能解释为没有发生沟通。使用处理状态了解覆盖情况，或显式检索原话。

## 记忆注释的修订

`apply_annotations` 提交的是该会话当前注释视图的完整替换，不是增量补丁。写入时带上 `expected_version` 和新的 `annotation_version`。首次尚无注释时预期版本为空字符串；后续版本必须匹配当前值，防止并发修改被覆盖。原话保持不变，旧注释存入历史表。

讨论记录区分观察、建议、请求、约束和意向，均保留解释性质与原话引用，不自动升级成承诺或已核实事实。证据使用查询返回的完整 turn ID，也兼容该会话中无歧义的来源 turn ID。

## 当前边界

已接收多少分片不等于整份录音已上传完整；系统不靠最大的时间位置宣称全部覆盖。话题关联覆盖不等于语义提取准确或人工复核通过。说话人标签只在来源范围内有效，未确认身份仍返回未知。

转写来源修订与记忆注释修订是不同操作。已接收分片可通过 `supersedes_revision` 替换为新的 `transcript_revision`：新修订必须显式指明被替换的活动修订，替换后旧附件置为非活动但保留原始结果与原话作为审计历史；由旧附件派生的笔记与候选记忆被标记为 stale，不再出现在常规检索中，同时自动为新修订入队重新提炼与发布。每个分片任意时刻只有一个活动修订，同键不同内容仍然拒绝，跨分片替换不允许。stale 内容仍可显式审计：`search_memory` 与 `get_context_item` 支持 `include_stale`，`get_evidence_context` 的结果带 `stale` 标记（标明目标轮次来自已被替换的分片），`get_annotation_history` 保留旧注释版本。

这是独立的实验接口，不宣称完整兼容领域 0.1 草案的快照分页、双时间与状态推导；`build_context` 提供实验性的字节预算与覆盖缺口，但其契约不是草案契约。客户端使用 [配置模板](../examples/context-memory.json) 并替换路径；项目不会自动修改用户的 MCP 配置。
