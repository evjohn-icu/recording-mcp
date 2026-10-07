# 事件与工具约定

## 事件

`ingest_events` 接受 1–500 条事件组成的批次。字段名使用 snake_case，时间使用 RFC 3339；`priority` 为 `high`、`normal` 或 `low`，`confidence` 范围为 0 到 1。

```json
{
  "id": "event-unique-id",
  "type": "commitment.created",
  "timestamp": "2026-10-03T10:42:00+08:00",
  "source": "meeting-notes",
  "actors": ["demo_user", "person_a"],
  "entities": ["示例项目网络 BOM"],
  "content": "示例用户 承诺周五把示例项目网络 BOM 发给示例联系人甲。",
  "structured_data": {
    "dimension": "commitment",
    "commitment_id": "demo-bom-delivery",
    "status": "pending",
    "deadline": "2026-10-09T23:59:00+08:00"
  },
  "confidence": 0.99,
  "priority": "high",
  "observed_at": "2026-10-03T10:42:00+08:00",
  "valid_from": "2026-10-03T10:42:00+08:00",
  "tags": ["commitment", "work"]
}
```

`id`、`type`、`timestamp`、`source`、`content`、`confidence`、`observed_at` 为必需字段。`actors`、`entities`、`structured_data`、`valid_from`、`valid_until`、`ttl_seconds`、`tags`、`conversation_id` 可按需提供。建议可投影状态的事件提供 `structured_data.dimension`，使用 `state_id`、`commitment_id`、`task_id`、`person_id` 或 `topic_id` 标识被描述的对象，并保留 `source`、时间、内容和置信度以便追溯。

批次在单个 SQLite 事务中写入。规范化后完全相同的事件 ID 和载荷可重复提交；同 ID 不同载荷会使批次失败。规范化会将时间转为 UTC、去重并排序人物/实体/标签。事件是追加的历史记录，状态和承诺是按事件时间、观察时间、有效区间与反馈投影出的读取结果。低置信度事实会保留在历史与当前状态中，返回置信度供调用方判断。

## 反馈

`record_feedback` 接收形状为 `{"event": <Event>}` 的参数，只接受以下类型：

| 类型 | 作用 |
| --- | --- |
| `commitment.completed` | 将承诺标记为完成 |
| `commitment.cancelled` | 将承诺标记为取消 |
| `deadline.changed` | 更新承诺期限，需提供 RFC 3339 的 `structured_data.deadline` |
| `agent.action.task_created` | 记录代理已在外部任务系统创建任务 |

生命周期反馈必须在 `structured_data.target_id` 中引用承诺的 ID 值本身，例如 `demo-bom-delivery`。反馈只记录事件，不会调用外部任务系统或执行其描述的动作。反馈示例见 [`../examples/feedback.json`](../examples/feedback.json)。

## 查询输入

工具名称及参数如下；未列出的分页参数 `limit`、`offset` 只在工具 schema 支持的地方使用。

| 工具 | 输入 |
| --- | --- |
| `search_events` | `query`（必填，可为空）、`person`、`start_time`、`end_time`、`min_confidence`、`limit`、`offset` |
| `get_timeline` | `start_time`、`end_time`（必填）、`entity`、`limit`、`offset` |
| `get_commitments` | `status`、`person`、`deadline_before`、`time` |
| `get_conversation_events` | `conversation_id`（必填）、`detail_level` (`summary` 或 `full`) |
| `get_person_context` | `person_id`（必填）、`start_time`、`end_time`、`topics`、`limit`、`offset` |
| `get_current_state` | `dimensions`、`time` |
| `ingest_events` | `events`（1–500 个 Event） |
| `record_feedback` | `event`（一个受支持的反馈 Event） |

`search_events` 与时间线查询返回历史记录，包含已过期的事件；`get_current_state` 会排除尚未发生、尚未观察、尚未生效或已过期的证据。有效区间为左闭右开，TTL 从事件发生时间起计算。省略 `time` 时，查询按服务时钟计算；启动服务时通过 `--at` 可固定演示时间。状态支持 `commitment`、`person`、`task`、`topic` 四个维度；同一维度和对象 ID 的新事件替换旧状态，`target_id` 反馈则合并已有字段并累积 provenance。最新状态过期后不会恢复先前的旧状态。

历史搜索的文本匹配是大小写不敏感的字面子串匹配。人物可按 `actors` 或 `entities` 中的精确值匹配，topics 为标签或实体的任一匹配。时间范围含起止边界，`deadline_before` 严格早于给定时间。分页默认 100，最大 500，返回 `events`、`total` 与可选的 `next_offset`。
