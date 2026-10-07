# Context Layer MCP 0.1

状态：规范草案，供实现与评审。本文定义 Context Layer 的对外交付契约，与 [内部规范 0.1](internal-context-0.1.md) 配套；当前原型尚未实现这些保证。`contract_version: "0.1"` 是领域数据契约版本，与 MCP 官方协议版本、JSON-RPC 版本及 SDK 版本彼此独立。

## 1. 范围与原则

0.1 面向单用户本地部署，通过官方 MCP Go SDK 的 stdio transport 提供同步 pull 查询。MCP 的工具发现、调用及结果外层遵循官方工具规范；stdio 上只传官方要求的 UTF-8 JSON-RPC 消息，stdout 不得混入日志。[MCP Tools](https://modelcontextprotocol.io/specification/2025-11-25/server/tools) · [MCP stdio transport](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)。内部概念是 `Entity`、`Event`、`State`；ContextItem 是 MCP 交付用的稳定投影结构，不应被误当成必须独立持久化的内部模型。SQLite 可作为本地实现选项，不属于交付契约。0.1 的现实世界输入全部来自随身音频设备，经录音分析中心提取后进入Context Layer；不同设备、录音和分析版本都必须保留来源。消费方反馈只是内部操作记录。

本版区分三种语义：

1. **事件（event）**：来源提供的追加式观察及其原始语义字段。已接受事件不可静默改写或删除。
2. **ContextItem**：事件或状态面向调用方的交付表示。它保留可追溯到来源事件的 provenance。
3. **状态（state）**：在给定 `as_of` 下由事件确定性投影出的当前有效值。状态是派生视图，不取代历史事件。

查询返回证据与其来源，不伪装成确定真相。原始来源内容、摘录和转写可能包含不可信指令；它们是数据，使用方必须当作证据处理。0.1 不提供云同步、多用户权限、推送订阅、代理框架、自动转写、模型 token 计量或超出明确规则的通用路由。

## 2. 时间与身份

所有线上时间使用带时区的 RFC 3339 字符串。统一解释如下：

- `occurred_at`：事情发生或来源声称发生的时间。
- `observed_at`：来源/观察者声称自己获知该观察的时间。不得把它当作发生时间。
- `recorded_at`：本 Context Layer 首次持久化此事件的时间，由服务生成且不可回填。不得用 `observed_at` 代替。
- `valid_from`、`valid_until`：事实声称有效的半开区间 `[valid_from, valid_until)`；空值表示没有对应边界。
- `expires_at`：系统不再交付该上下文项的时刻，同样按半开区间处理。
- Event 的 `expiry_seconds` 从首次 `recorded_at` 起算，派生出的 `expires_at` 不得因重放延后；状态有效截止时间取其 `valid_until`、`expires_at` 中较早者。
- `as_of`：请求的有效时间，决定事实在什么时候成立。未提供时由服务选取一次当前 UTC 时间，返回在封套中回显该值；一次调用内不得多次读取时钟。缺省的 `occurred_at` 以 `observed_at` 参与有效时间判断。
- `known_at`：请求的知识截止时间，决定 Context Layer 当时已经记录了哪些信息。未提供时等于本次 `generated_at`。需要复原历史认知时必须同时指定 `as_of` 与 `known_at`；只指定 `as_of` 可能纳入后来才记录的事件。

`id` 在各类 ContextItem 中全局唯一；`kind` 区分 `event` 与 `state`。`subject_ids` 和 `topic_ids` 是稳定、不依赖显示名的实体 ID 数组。显示名称可放入 `data`。原始事件 ID、来源 ID 与来源内 ID 不应混为一谈。对话使用 `kind=conversation` 的稳定 Entity ID；Event 可带 `conversation_id`，值为该实体 ID，未知时省略。ContextItem 若直接属于会话可带可选顶层 `conversation_id`；State 若由多个会话支持，不得伪造单一会话 ID，应在 provenance 中保留全部来源。读取型工具每次调用均接受 `as_of` 和 `known_at`；`known_at` 缺省等于 `generated_at`，服务只使用 `recorded_at <= known_at`、来源 `observed_at <= known_at` 且 `recorded_seq` 不超过本次快照水位的事件。

## 3. 内部事件与上下文项

以下 BOM、人名和录音引用均为合成示例；假设来源与引用实体已经注册。接收事件的结构：

```json
{
  "id": "rec-01J9BOM",
  "type": "commitment.created",
  "schema_version": 1,
  "source_id": "recorder:local",
  "source_event_id": "recording-204:segment-18",
  "source_ref": "recording://recording-204#segment-18",
  "occurred_at": "2026-10-03T10:42:00+08:00",
  "observed_at": "2026-10-03T10:42:00+08:00",
  "subject_ids": [
    "person:owner",
    "person:contact-a"
  ],
  "topic_ids": [
    "topic:network-bom"
  ],
  "content": "示例用户 承诺周五把示例项目网络 BOM 发给示例联系人甲。",
  "data": {
    "description": "发送示例项目网络 BOM 给示例联系人甲",
    "owner_ids": [
      "person:owner"
    ],
    "commitment_id": "commitment:demo-bom-delivery",
    "deadline": "2026-10-09T23:59:00+08:00"
  },
  "confidence": 0.98,
  "priority": "high",
  "valid_from": "2026-10-03T10:42:00+08:00",
  "valid_until": null,
  "expiry_seconds": null,
  "classification": "private",
  "origin": "audio",
  "claim_mode": "explicit_statement",
  "confidence_basis": "合成示例：语义提取评分，不表示事实已验证",
  "audio_evidence": [
    {
      "device_id": "wearable_demo_01",
      "recording_id": "recording-204",
      "segment_id": "segment-18",
      "start_ms": 60000,
      "end_ms": 68000,
      "speaker_id": "speaker_0",
      "transcript_version": "asr-demo-1",
      "analysis_version": "extract-demo-1",
      "asr_confidence": null,
      "extraction_confidence": null,
      "speaker_confidence": null
    }
  ]
}
```

Event 字段遵循内部规范，音频观察还必须包含origin=audio、claim_mode与audio_evidence；confidence非null时需confidence_basis说明评分方法。id、type、schema_version、source_id、source_event_id、subject_ids、topic_ids、content、data、confidence、occurred_at、observed_at 必须出现；其中 occurred_at、confidence 可为 null。priority 缺省 normal；source_ref、confidence_basis、raw_excerpt、conversation_id 可选。recorded_at 和 recorded_seq 由服务生成，调用方不得填写。有效期字段省略等同 null；expiry_seconds 为非负整数或 null，锚定首次 recorded_at，重放不得重置期限。classification 缺省 private。禁止未知顶层字段；业务负载扩展使用注册类型的 schema_version，顶层契约变更须更新内部规范版本。

### 沟通内容事件

Entity `kind=conversation` 表示一次会话。Event 的可选 `conversation_id` 引用该 Entity。输入事件仍遵循统一 Event schema；以下字段是相应类型的 `data` payload，每条 conversation 事件必须为 `origin=audio` 并带有非空 `audio_evidence`，因此保留录音与分析版本的来源链。

| Event type | 必需 `data` | 语义 |
| --- | --- | --- |
| `conversation.session_recorded` | `participant_ids`（可空数组）、`started_at`（RFC 3339 或 null）、`ended_at`（RFC 3339 或 null）、`coverage`（非空数组：`recording_id`、`start_ms`、`end_ms`）、`completeness`（`partial \| complete`） | 会话与录音覆盖范围。`complete` 仅表示声明 coverage 内待导入分析记录已全部保存，不证明设备捕获所有现实沟通，也不证明转写正确。 |
| `conversation.turn_recorded` | `turn_id`、`turn_index`（非负整数）、`text`、`speaker_entity_id`（Entity ID 或 null） | 一轮已保存的识别文本。未知说话人必须为 null；音频证据中的局部 speaker ID 不可冒充 person Entity。 |
| `conversation.summary_recorded` | `summary`、`covered_event_ids`（非空、同一会话的 `conversation.turn_recorded` IDs）、`key_points`（数组）；可选 `mentioned_entity_ids` | 对所列已存轮次的摘要，不能声称超出其覆盖范围。其 `claim_mode` 为 `inference`，原文仍只在逐轮事件中。 |

逐轮文本确实持久化在 Event 的 `data.text`；交付 ContextItem 的 `data.payload.text`。完整可见性受下述 `include_turns` 控制。`audio_evidence` 必须保留实际录音片段定位，Context Layer 不下载音频，也不声称持有未导入的全量转写。会话可以是不完整的；`completeness=partial`、空参与者或 null speaker 信息必须原样保留，不得补猜缺失说话人或伪称覆盖整段沟通。稳定 `turn_id` 在会话内唯一；会话未知时按 source 与 turn_id 限定。重新分析时由更正关系确定唯一展示版本；被更正的旧 turn 不重复出现在会话时间线，但仍可按 Event ID 审计。若 summary 所覆盖任一轮被更正或撤回，该 summary 默认不交付；可按 Event ID 审计其原始记录。

对外 ContextItem 在有会话身份时保留可选顶层 `conversation_id`。State 若汇总了多个会话来源，不设置虚构的单一 `conversation_id`；按任一实际贡献会话筛选时，仍返回该状态的全部 provenance。

`append_events` 需验证整个批次后再原子写入。相同 `id`、相同调用方载荷是幂等重放；相同 ID、不同载荷是冲突，整批不写入。`recorded_at` 与 `recorded_seq` 是服务生成字段，不可由调用方填写，也不参与 replay payload 比较。`observed_at` 为来源声明的获知时间，不得晚于服务记录时间；未来时间拒绝。来源文本可保留为证据，但除非调用方明确提供，不得把未经核实推断写成高置信度事实。校验错误按字段返回，事务错误不得报告部分成功。`append_events` 可携带可选 `entities` 声明以同一事务原子注册新实体；重复完全一致的声明幂等，ID 冲突但内容不同则整批失败。所有 subject/topic 引用须在写入后能解析。`type` 必须来自版本化事件类型注册表；初始类型至少包括 `commitment.created`、`commitment.completed`、`commitment.cancelled`、`commitment.deadline_changed`、`fact.observed`、`fact.retracted`、`event.corrected`、`event.retracted`、`feedback.recorded`、`conversation.session_recorded`、`conversation.turn_recorded`、`conversation.summary_recorded`。未知类型或不支持的 `schema_version` 拒绝。

面向调用方的统一 ContextItem 结构：

```json
{
  "id": "state:commitment:demo-bom-delivery",
  "kind": "state",
  "subject_ids": [
    "person:owner",
    "person:contact-a"
  ],
  "topic_ids": [
    "topic:network-bom"
  ],
  "content": "示例项目网络 BOM 待发送给示例联系人甲。",
  "data": {
    "state_kind": "commitment",
    "status": "active",
    "deadline": "2026-10-09T23:59:00+08:00"
  },
  "confidence": 0.98,
  "priority": "high",
  "occurred_at": "2026-10-03T10:42:00+08:00",
  "observed_at": "2026-10-03T10:42:00+08:00",
  "recorded_at": "2026-10-03T10:43:12+08:00",
  "recorded_seq": 104,
  "valid_from": "2026-10-03T10:42:00+08:00",
  "valid_until": null,
  "expires_at": null,
  "provenance": [
    {
      "event_id": "rec-01J9BOM",
      "source_id": "recorder:local",
      "source_event_id": "recording-204:segment-18",
      "source_ref": "recording://recording-204#segment-18",
      "origin": "audio",
      "claim_mode": "explicit_statement",
      "audio_refs": [
        {
          "device_id": "wearable_demo_01",
          "recording_id": "recording-204",
          "segment_id": "segment-18",
          "start_ms": 60000,
          "end_ms": 68000,
          "speaker_id": "speaker_0",
          "transcript_version": "asr-demo-1",
          "analysis_version": "extract-demo-1",
          "asr_confidence": null,
          "extraction_confidence": null,
          "speaker_confidence": null
        }
      ]
    }
  ],
  "origin": "audio"
}
```

`provenance` 项字段为 `event_id`、`source_id`、`source_event_id` 与可选 `source_ref`；`source_event_id` 必填。ContextItem必须含origin；音频provenance必须含origin=audio、claim_mode和非空audio_refs，其中保留片段定位、处理版本及各阶段可空置信度。ContextItem可附recorded_at与recorded_seq。Event ContextItem 的 `data` wrapper 含 `event_type`、`schema_version` 与 `payload`（原始事件data）；State ContextItem 的 `data` wrapper 至少含 `state_kind`。输入 Event 的 `data` 只包含 registry 为该 type/version 声明的负载字段，不重复包装 type/version。`confidence: null` 表示未知，不等于零，也不应被当成高置信度。合并多个证据的派生项必须列出全部支撑来源，并使用有定义且可复现的置信度规则；0.1 实现若没有此规则，应返回独立项，不能虚构聚合分数。

## 4. MCP 传输与通用响应

服务器使用 MCP 官方 stdio transport 和 SDK 承载工具发现、调用、错误及 JSON-RPC 生命周期。每个工具必须公布 inputSchema 与成功结果的 outputSchema，成功载荷置于 structuredContent，同时在一个 TextContent 中返回相同对象序列化后的 JSON，兼容只读取文本的客户端。[MCP 结构化工具结果](https://modelcontextprotocol.io/specification/2025-11-25/server/tools)。

所有读取型工具返回同一封套：

```json
{
  "contract_version": "0.1",
  "request_id": "req-7f19",
  "as_of": "2026-10-03T12:00:00Z",
  "known_at": "2026-10-03T12:00:00Z",
  "generated_at": "2026-10-03T12:00:00.014Z",
  "items": [
    {
      "id": "state:commitment:demo-bom-delivery",
      "kind": "state",
      "subject_ids": [
        "person:owner",
        "person:contact-a"
      ],
      "topic_ids": [
        "topic:network-bom"
      ],
      "content": "示例项目网络 BOM 待发送给示例联系人甲。",
      "data": {
        "state_kind": "commitment",
        "status": "active",
        "deadline": "2026-10-09T23:59:00+08:00"
      },
      "confidence": 0.98,
      "priority": "high",
      "occurred_at": "2026-10-03T10:42:00+08:00",
      "observed_at": "2026-10-03T10:42:00+08:00",
      "recorded_at": "2026-10-03T10:43:12+08:00",
      "recorded_seq": 104,
      "valid_from": "2026-10-03T10:42:00+08:00",
      "valid_until": null,
      "expires_at": null,
      "provenance": [
        {
          "event_id": "rec-01J9BOM",
          "source_id": "recorder:local",
          "source_event_id": "recording-204:segment-18",
          "source_ref": "recording://recording-204#segment-18",
          "origin": "audio",
          "claim_mode": "explicit_statement",
          "audio_refs": [
            {
              "device_id": "wearable_demo_01",
              "recording_id": "recording-204",
              "segment_id": "segment-18",
              "start_ms": 60000,
              "end_ms": 68000,
              "speaker_id": "speaker_0",
              "transcript_version": "asr-demo-1",
              "analysis_version": "extract-demo-1",
              "asr_confidence": null,
              "extraction_confidence": null,
              "speaker_confidence": null
            }
          ]
        }
      ],
      "origin": "audio"
    }
  ],
  "provenance": {
    "complete": true
  },
  "page": {
    "limit": 20,
    "next_cursor": null,
    "snapshot_id": "snap-abc",
    "snapshot_watermark": 104
  },
  "budget": {
    "max_bytes": 12000,
    "used_bytes": 1532,
    "unit": "utf8_json_bytes"
  },
  "truncation": {
    "truncated": false,
    "reason": null,
    "omitted_items": 0
  }
}
```

封套字段要求：

- `contract_version` 必须为该响应遵循的领域契约版本。
- `request_id` 回显工具参数中的领域请求 ID；若调用方未传，实现生成唯一值。不得将 JSON-RPC/MCP request ID 当作领域 `request_id`。
- `as_of` 是本次读取使用的有效时间；`known_at` 是知识截止时间；`generated_at` 是响应生成时刻。
- `items` 按请求定义的稳定顺序排列。
- `provenance.complete` 表示返回项所需的来源说明是否完整；若来源已丢失或被裁剪则为 `false`，并提供 `missing` 或 `truncated` 说明。
- page.limit 是本页最大项数，实际返回数为 items.length。page 还包含游标、快照标识与水位；无下一页时 next_cursor 为 null。
- `budget` 只计算 UTF-8 编码后领域封套 JSON 的字节数，不是 token 数。它不含 MCP 外层 framing、`content` 文本副本、JSON-RPC/MCP 元数据或传输开销。`used_bytes` 不得超过 `max_bytes`。
- `truncation` 必须说明是否因预算或结果上限省略内容。`omitted_items` 是满足过滤条件但未返回的整项数；无法计算时为 `null` 并声明原因。

max_bytes 默认 12000，范围 1024–65536。按工具定义的排序装入完整项，不得截断 JSON 或省略必需字段。封套本身放不下时返回 BUDGET_TOO_SMALL。选择只返回连续前缀，不能跳过大项后返回后面的项；若已返回至少一项，next_cursor 指向第一项未交付内容，若首项单独即放不下，则返回 ITEM_TOO_LARGE。调用方改变 detail/预算后从新查询开始。

计量使用无空白的 UTF-8 JSON，Unicode 不转义为 ASCII，禁止非有限数值。used_bytes 包含自己在最终 JSON 中占用的字节，须迭代至数值与实际长度一致。omitted_items 是快照内尚未交付的合格项数；reason 为 page_limit、byte_budget 或 both，完整结果为 null。

服务为首次查询选择单调递增的 `snapshot_watermark`（最大 `recorded_seq`），封套的 `page.snapshot_id` 标识该水位。游标必须绑定 filters/selector、`conversation_id`、`include_turns`、`as_of`、`known_at`、kind、detail/representation、顺序、Entity directory revision、schema registry revision、projection_version 与完整 `snapshot_watermark`，同一快照翻页不得重复或遗漏。游标有效期至少 10 分钟；过期或不匹配时明确报错。历史认知查询同时应用 `recorded_at <= known_at` 与 `recorded_seq <= snapshot_watermark`。

detail 仅为 compact 或 full，默认 full。两者都保留 id、kind、subject_ids、topic_ids、conversation_id（若有）、confidence（可 null）、priority、occurred_at、observed_at、valid_from、valid_until、expires_at 和完整 provenance。recorded_at、recorded_seq 为可选辅助字段。compact 可省略 content 与 data，用于发现索引和来源；full 返回两者，用于实际消费。不得裁剪 provenance 或只截断正文来伪装 compact。

五个读取工具共用可选 request_id、as_of、known_at、detail、max_bytes；集合读取还接受 limit（默认20，范围1–100）和 cursor。后续页省略时间时沿用游标值，不重新读当前时间。改变筛选、预算、页大小或 detail 必须新查询，否则 CURSOR_MISMATCH。

过滤数组必须非空且去重。min_confidence 省略时保留未评分项；提供阈值时排除 confidence=null，阈值范围[0,1]。occurred_after 含起点、occurred_before 不含终点；未知发生时间按 observed_at 过滤和排序，仍返回 occurred_at=null。event_types 对事件按type匹配，对状态按实际贡献事件的任一type匹配。kinds 缺省[event,state]。空集合为成功封套，指定不存在ID为NOT_FOUND。

音频筛选支持device_ids、recording_ids和speaker_ids；同轴OR、不同轴AND。speaker_ids必须使用“录音ID/处理版本/局部说话人ID”的限定标识，不能当作跨录音人物身份。对state筛选时按实际贡献事件的音频引用匹配，返回全部贡献依据，不裁掉其他相关片段来制造单一来源印象。origin=audio的每项provenance必须保留audio_refs，即使compact也保留；默认不回传音频字节或全文转写。录音片段、处理版本不存在或偏移越界时返回INVALID_AUDIO_EVIDENCE。

## 5. 工具集合

以下七个工具构成 0.1 的建议最小语义面。音频来源查询的默认origins为[audio]，查询内部反馈必须显式指定origins=[feedback]。过滤条件和输出规则属于领域契约；工具命名可在评审时统一，但实现不得悄悄改变语义。

### 5.1 `query_context`

用途：按明确条件读取历史事件或上下文项。

```json
{
  "filters": {
    "subject_ids": [
      "person:contact-a"
    ],
    "topic_ids": [
      "topic:network-bom"
    ],
    "kinds": [
      "event",
      "state"
    ],
    "event_types": [
      "commitment.created"
    ],
    "conversation_ids": ["conversation:meeting-204"],
    "query": "付款",
    "occurred_after": "2026-10-01T00:00:00+08:00",
    "occurred_before": "2026-10-10T00:00:00+08:00",
    "min_confidence": 0.5
  },
  "as_of": "2026-10-03T20:00:00+08:00",
  "known_at": "2026-10-03T20:00:00+08:00",
  "include_turns": false,
  "limit": 20,
  "cursor": null,
  "max_bytes": 12000
}
```

`filters` 至少要有 subject、topic、event type、conversation、设备、录音、speaker 或非空 `query` 之一；仅指定 kinds 或时间不足以表达上下文范围。不同选择轴之间按 AND、同一轴内多个值按 OR 组合。`conversation_ids` 匹配 item 顶层 `conversation_id`；State 按实际贡献事件的任一会话匹配，但匹配后仍返回全部 provenance。时间过滤作用于 `occurred_at`；若其为 null，则用 `observed_at`。`min_confidence` 与 null confidence 同时出现时，null 项不匹配。`as_of` 影响项有效性，不改写历史；默认不返回过期 state，但历史 event 仍可查。排序按发生时间升序（未知时使用observed_at）、再按id字典序。

`query` 是普通 Unicode casefold 后的子串匹配：先 trim，空 query 拒绝；仅匹配已存 ContextItem `content` 和注册文本字段。在录音会话事件中，允许的字段是 Event 存储中的 `data.text`、`data.summary`，对应交付结构的 `data.payload.text`（turn）和 `data.payload.summary`（summary）；不递归搜索任意 JSON 或做向量/语义搜索。过滤先在完整已存字段上执行，再生成 compact/full 表示。不同 selector 轴之间按 AND 匹配。query 只在当前 `include_turns` 可检索集合中做直接匹配；要在逐轮原话中查词必须显式设 `include_turns=true`。默认 `include_turns=false` 会完全排除 `conversation.turn_recorded` 项，不会把逐轮文本送入调用方。`include_turns=true` 时 query 可用已存 turn 文本命中并返回 turn 项；detail=compact 时只返回不含文本的 turn 摘要项，detail=full 才交付逐轮文本。匹配某轮文本不会自动扩展为同一会话的 summary/session 或其他事件。

`get_context_item` 对指定 turn ID 且 `kind=event, detail=full` 可读取已存逐轮文本 `data.payload.text`。`include_raw_excerpt=true` 是单独的最小证据摘录开关；它不控制逐轮文本，也不访问 `source_ref` 指向的音频。

### 5.2 `get_context_item`

用途：读取一个已知 ID 的上下文项及其来源说明。

```json
{
  "id": "state:commitment:demo-bom-delivery",
  "kind": "state",
  "as_of": "2026-10-03T20:00:00+08:00",
  "detail": "full",
  "known_at": "2026-10-03T20:00:00+08:00",
  "max_bytes": 12000
}
```

kind 为 event 或 state，detail 默认 full。可选 include_raw_excerpt 缺省false，只有kind=event且detail=full时允许true；此时把已保存摘录放入data.evidence.raw_excerpt，也计入预算，不访问外部source_ref。原始摘录不会因full自动返回。查不到、类别不匹配或state在as_of下失效分别报告NOT_FOUND、KIND_MISMATCH或ITEM_NOT_EFFECTIVE；历史event不因有效期结束而失去按ID读取能力。

### 5.3 `get_current_state`

用途：按 subject/topic 与维度读取在某时刻生效的状态，是 `query_context` 的语义快捷入口。

参数包括subject_ids、topic_ids、device_ids、recording_ids、speaker_ids、dimensions、共同读取参数、limit与cursor；至少指定一个主体、主题或音频范围轴。dimensions 为 commitment 或 fact，省略表示两者。只返回 state，按 data.state_kind、状态 ID 升序，不根据自然语言暗中选择最相关的一条。

### 5.4 `build_context`

用途：为一次下游消费请求组装有上限、可追溯的上下文包。它只按必需的显式 selector 选择，再确定性排序，不是超出规则的 router。

```json
{
  "consumer": "planning_agent",
  "purpose": "tomorrow_plan",
  "selector": {
    "subject_ids": [
      "person:owner"
    ],
    "topic_ids": [
      "topic:network-bom"
    ],
    "conversation_ids": ["conversation:meeting-204"],
    "query": "BOM"
  },
  "as_of": "2026-10-03T20:00:00+08:00",
  "known_at": "2026-10-03T20:00:00+08:00",
  "include_turns": false,
  "detail": "full",
  "max_bytes": 12000
}
```

consumer 与 purpose 必须为非空字符串，仅为审计与用途标签，不是身份凭据，也不触发推断路由。selector必须含非空subject_ids、topic_ids、event_types、conversation_ids、device_ids、recording_ids、speaker_ids或非空query；不同轴AND，同轴多个值OR。`include_turns` 是顶层布尔参数，默认 false，与 query_context 一致。selector 使用query_context的filters字段，但kinds缺省[state]，减少原始事件与派生状态的重复交付。显式请求event可以返回原始证据。返回封套附 selection={consumer,purpose,selector,order:"priority_desc,time_desc,id_asc"}，也计入预算；按priority高到低、发生时间新到旧、id字典序排序，可用limit/cursor按连续前缀分页。

`max_bytes` 定义完整领域封套 JSON 的 UTF-8 字节上限，是 0.1 唯一规范性预算单位。`token_budget` 不作为硬保证；未来如增加，只能是另行定义 tokenizer 的建议值，不能替代 `max_bytes`。

### 5.5 `get_conversation_context`

用途：按稳定 `conversation_id` 读取会话边界、有效摘要和可选逐轮记录；结果使用通用 ContextEnvelope。

```json
{
  "conversation_id": "conversation:meeting-204",
  "include_turns": false,
  "request_id": "req-conv-204",
  "as_of": "2026-10-03T20:00:00+08:00",
  "known_at": "2026-10-03T20:00:00+08:00",
  "detail": "full",
  "limit": 20,
  "cursor": null,
  "max_bytes": 12000
}
```

`include_turns` 默认为 `false`；此时完全排除 turn 项。设为 `true` 且 `detail="compact"` 时可返回不含转写文本的 turn 索引元数据；设为 `true` 且 `detail="full"` 时交付本库已保存的 `conversation.turn_recorded` 文本，放在 Event ContextItem 的 `data.payload.text`。两种表示都受相同字节预算与游标规则限制。`limit`、`cursor` 与其他集合读取工具相同。若会话 Entity 不存在，返回 `NOT_FOUND`；会话存在但没有符合时间/知识条件的记录时返回空成功封套。

固定顺序为 session 项在前、有效 summary 项其次、turn 项最后；session 与 summary 各按 occurred_at（null 时 observed_at）升序、id 升序，turn 按 `turn_index`、id 升序。summary 若覆盖的 turn 已更正或撤回，默认不交付；更正/撤回旧 turn 不重复展示，但原事件仍可通过 `get_context_item` 按 ID 审计。会话查询是历史读取：即使 turn 已过期也可以在显式 `include_turns=true` 时查询，expires_at 不删除历史。

该工具只返回指定会话的 session/summary/turn 记录，不自动混入相关承诺、事实或其它项目状态；如需它们，使用带相同 `conversation_ids` 的 `query_context`。`completeness=partial` 必须保留，不能将局部记录描述成完整对话。说话人未知时保留 `speaker_entity_id=null`；不得按录音归属推断其身份。MCP 服务只交付已导入记录，不下载或播放来源音频。

### 5.6 `append_events`

用途：追加来源给出的结构化事件。参数为 events（1–500条）、可选 entities（最多500个新实体声明）、request_id 和 idempotency_key。设备、录音、来源目录与类型注册表由录音分析中心接入配置先建立，实体可同批创建。append_events只接收有音频片段依据的origin=audio事件。实体只允许创建或完全相同的重放；修订由内部版本化注册接口处理。

原子写入成功返回 WriteReceipt：contract_version、request_id、accepted（本次新插入事件数）、replayed（已有相同事件数）、event_ids（按输入顺序）。缺少音频依据或声称来自其他外部来源时整批失败。同ID或同来源键不同载荷整批失败；来源键存在但ID不同为SOURCE_EVENT_CONFLICT并给出已有ID。

可选idempotency_key作用于该工具及整批规范化载荷；同键同载荷返回首次提交结果并标明request_replayed=true，不重做写入；同键不同载荷为IDEMPOTENCY_CONFLICT。不传键时事件ID与来源键仍保证不重复。写入结果也以structuredContent及相同TextContent返回，但不使用读取封套或读取字节预算。服务不访问source_ref或执行转写。

### 5.7 `report_feedback`

用途：追加内部操作反馈，参数为target_id、event、可选request_id与idempotency_key。event.type只能为feedback.recorded，origin只能为feedback。target_id必须列入data.target_ids并可解析，成功返回WriteReceipt。该工具不能写入音频观察或直接改变承诺状态。

```json
{
  "target_id": "commitment:demo-bom-delivery",
  "event": {
    "id": "feedback-01J9DONE",
    "type": "feedback.recorded",
    "schema_version": 1,
    "origin": "feedback",
    "source_id": "agent:project-tracker",
    "source_event_id": "feedback-system:task-882:done",
    "subject_ids": [
      "person:owner",
      "person:contact-a"
    ],
    "topic_ids": [
      "topic:network-bom"
    ],
    "content": "消费方报告网络 BOM 已发送，待音频观察确认。",
    "data": {
      "action": "reported_delivery",
      "target_ids": [
        "commitment:demo-bom-delivery"
      ],
      "result": {
        "basis": "agent_report",
        "reported_status": "completed",
        "origin_event_ids": [
          "rec-01J9BOM"
        ]
      }
    },
    "confidence": null,
    "priority": "normal",
    "occurred_at": "2026-10-09T16:00:00+08:00",
    "observed_at": "2026-10-09T16:00:01+08:00"
  }
}
```

音频中明确说出完成、取消或修改期限时，由录音分析中心通过append_events提交对应生命周期事件并带audio_evidence。消费方报告或推断只记为feedback，不冒充新的音频证据。

## 6. 错误语义

未知工具、无法解析的 MCP 调用或不满足 `tools/call` 结构的问题属于协议错误。领域参数、日期/范围/Event schema 校验、游标过期、ID 冲突、预算和存储失败属于工具执行错误：按 MCP 规范返回 `isError: true`，将稳定领域错误 JSON 序列化放在 `TextContent`；由于成功 `outputSchema` 约束，不返回错误形状的 `structuredContent`。MCP 区分 JSON-RPC 协议错误和含 `isError` 的工具执行错误。[MCP Tools：错误处理](https://modelcontextprotocol.io/specification/2025-11-25/server/tools)。错误文本例如：

```json
{
  "contract_version": "0.1",
  "error": {
    "code": "EVENT_ID_CONFLICT",
    "message": "event id exists with different payload",
    "retryable": false,
    "details": {
      "id": "rec-01J9BOM"
    }
  }
}
```

错误至少包括 `code`、适合排障的 `message`、`retryable` 和可选 `details`。不得把内部数据库路径、SQL、密钥或完整敏感数据放入错误正文。领域错误代码需稳定且文档化。成功结果的 `structuredContent` 与向后兼容的 TextContent JSON 必须是同一个完整封套；byte budget 仅计封套本身，不重复计算文本副本和 MCP framing。

| code | 含义 | retryable |
| --- | --- | --- |
| INVALID_ARGUMENT | 日期、范围、类型或字段不合法 | false |
| INVALID_AUDIO_EVIDENCE | 缺少音频依据、引用错位或片段偏移非法 | false |
| UNKNOWN_REFERENCE | 实体、来源或目标未注册 | false |
| EVENT_ID_CONFLICT / SOURCE_EVENT_CONFLICT / IDEMPOTENCY_CONFLICT | 标识对应不同载荷或事件ID | false |
| NOT_FOUND / KIND_MISMATCH / ITEM_NOT_EFFECTIVE | 指定项不存在、类别错误或状态失效 | false |
| CURSOR_EXPIRED / CURSOR_MISMATCH | 游标失效或与参数不符，需新查询 | false |
| BUDGET_TOO_SMALL / ITEM_TOO_LARGE | 改变预算或表示后需新查询 | false |
| STORAGE_UNAVAILABLE | 暂时存储故障；写入重试保持ID与来源键 | true |

## 7. 单用户本地边界

0.1 假定单一 OS 用户拥有本地进程和存储。stdio 服务不开放监听端口；访问控制由该本地边界提供，不声称支持多用户权限隔离。

规范与代码可以公开分发；部署中保存的音频上下文不因此成为公开数据。classification=public 仅为分类元信息，不构成发布授权或共享操作。

调用方应只提交完成查询目的所需的内容；原始摘录按敏感数据处理。`full` 仅表示返回已存储的 ContextItem content/data，不授权访问原始媒体。保留策略由本地使用方设定。

## 8. 与现有原型的关系

仓库当前的八工具录音原型（`search_events`、`get_timeline`、`get_commitments`、`get_conversation_events`、`get_person_context`、`get_current_state`、`ingest_events`、`record_feedback`）是早期实现接口；其 `get_conversation_events`（原名 `get_conversation_context`，统一入口时改名以消除与记忆层的同名异约）不代表已经实现本文的会话数据模型、`include_turns` 行为或通用 ContextEnvelope。它不是本规范七个工具的实现证明。尤其现有原型没有完整的通用 ContextEnvelope、UTF-8 字节预算、稳定快照游标，也不实现本文定义的 `query_context`、`get_context_item` 或 `build_context`。兼容映射应另行记录，不能把文档写成已有功能。

## 9. 0.1 验收场景

规范实现至少通过以下场景；这是契约验收清单，不要求本草案仓库已经实现：

1. **事件重放**：同一 `id` 和相同规范化内容重复追加，第二次报告 replay；相同 ID 改了正文，整批原子失败且不写入其他新事件。
2. **事件与状态分离**：写入有音频依据的 deadline 变更事件后，旧事件仍能通过历史查询找到；`as_of` 在变更前后分别得到相应期限状态；`commitment.deadline_changed` 是追加事件，不改写原始 commitment.created。
3. **观察与发生时间分离**：晚到事件的 `observed_at` 晚于 `occurred_at`；按发生时间范围查询可找到它，响应仍保留两个原始时刻。
4. **可追溯性**：每个派生 state 都包含支撑它的 provenance；删除/缺失一条来源时，响应明确标记 provenance 不完整，不能静默伪造完整性。
5. **有效性边界**：`as_of` 达到 `expires_at` 时 state 不再返回；同一历史 event 仍可查询。未来 `valid_from` 的 state 在此前不返回。
6. **预算与翻页**：最小合法封套预算以下返回 `BUDGET_TOO_SMALL`；若单个下一项无法放入预算则返回 `ITEM_TOO_LARGE`，不得卡在不前进的游标。相同快照连续翻页没有重复或遗漏，游标与过滤条件不匹配时明确失败。

7. **音频追溯**：每个audio观察与派生状态都可追溯到设备、录音、片段和处理版本；片段越界或设备归属不符须拒绝。compact也不能丢失这些引用。
8. **说话人与结论区分**：未知speaker_id不得被自动映射为设备主人；转述和推断保留claim_mode及分阶段置信度。内部feedback不能伪装音频证据或直接完成承诺。
9. **会话检索与可见性**：query 按 Unicode casefold 子串命中已存摘要/转写字段；默认不返回 turn，include_turns=true 且 full 才交付文本，compact 只显示索引元数据。空 query 拒绝，结果不做自动会话扩展。
10. **部分会话与修订**：session 的 partial、未知参与者/说话人原样保留；turn 更正后会话视图不重复显示旧版本，覆盖了被更正或撤回 turn 的旧摘要不再默认交付，旧事件仍能按 ID 审计。
