# 话题关联建议（E 切片建议层）

## 背景

Context Layer 开发计划的 E 切片是“关联与失效”。当前先落地其中的**建议层**：由 agent（记忆策展人）以只读方式调查本地记忆库，发现话题之间的关联并产出提议；`context-associate` CLI 负责校验与写入。提议保存在 `memory_links` 表中，状态固定为 `proposed`（建议）。本层不会自动合并话题，也不修改任何已有记忆；后续的人工确认与自动触发见文末“后续”一节。

## 工作流

1. **只读调查**：agent 通过 MCP 工具 `list_conversations` / `list_topics` / `search_memory` / `get_evidence_context` 查看现有会话、话题、原话和证据上下文，不修改记忆库。
2. **产出提议 JSON**：只在有真实原话证据时写出一个提议文件（模式见下文“提议 JSON”）。可直接使用的策展人提示词见 [curator-prompt.md](curator-prompt.md)。
3. **校验与写入**：运行 `context-associate`，程序解析提议并执行 `ValidateTopicLinks` 校验；非 `--dry-run` 时通过校验的条目由 `InsertLinks` 写入 `memory_links`，`status=proposed`。
4. **不自动合并**：校验只是挡住证据不足或字段非法的条目，通过校验也仅代表“形式合格”，语义是否成立仍需人工确认。任何条目都不会触发话题合并。

## 数据表 memory_links

migration `v6 "memory-links"` 创建 `memory_links` 表及 `memory_links_from`（`from_kind, from_id`）、`memory_links_to`（`to_kind, to_id`）两个索引。

| 列 | 说明 |
| --- | --- |
| `id` | 内容寻址 ID：`"link:"` + `sha256(from_kind|from_id|to_kind|to_id|relation)` 的十六进制前 32 位。写入使用 `INSERT OR IGNORE`，同一关联重复提交不会产生重复行。 |
| `from_kind` / `from_id` | 关联的源端。当前实现只写 `topic`，`from_id` 即话题 ID。 |
| `to_kind` / `to_id` | 关联的目标端，同样只写 `topic` 与话题 ID。 |
| `relation` | 关系类型：`same_topic`、`follow_up`、`contradiction`、`update`。 |
| `reason` | 提议理由，去除首尾空白后非空且不超过 500 字符（按 rune 计）。 |
| `evidence_turn_ids_json` | 支撑该关联的 turn ID 数组（JSON 字符串），每条必须是 `turns` 表中真实存在的轮次。 |
| `confidence` | 提议方对关联成立的把握，取值范围 [0, 1]。 |
| `status` | `proposed` / `confirmed` / `rejected`。本实现只写入 `proposed`。 |
| `model` | 产出提议的模型标识，来自 CLI `--model` 参数，可为空。 |
| `created_at` | 写入时间，unix 毫秒。 |

## 提议 JSON

输入文件的顶层是一个对象，`proposals` 为提议数组：

```json
{
  "proposals": [
    {
      "kind": "topic_link",
      "from_topic_id": "topic:example",
      "to_topic_id": "topic:another",
      "relation": "same_topic",
      "reason": "两次讨论围绕同一交付物展开，均引用了对应的原话。",
      "evidence_turn_ids": ["turn:3f1a6c0e9d2b4f5a8b7c1d0e3f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d0e1f2a"],
      "confidence": 0.7
    }
  ]
}
```

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `kind` | string | 必须为 `topic_link`。 |
| `from_topic_id` | string | 源话题 ID，必须存在于 `topics` 表。 |
| `to_topic_id` | string | 目标话题 ID，必须存在于 `topics` 表，且不能与 `from_topic_id` 相同。 |
| `relation` | string | `same_topic` / `follow_up` / `contradiction` / `update` 之一。 |
| `reason` | string | 提议理由；去除首尾空白后必须非空，且不超过 500 字符（rune）。 |
| `evidence_turn_ids` | []string | 至少 1 条 turn ID，每条必须存在于 `turns` 表。turn ID 形如 `turn:` 加十六进制哈希（例如 `turn:3f1a...`），以记忆库返回的原值为准。 |
| `confidence` | number | 取值范围 [0, 1]。 |

## 校验规则

`ValidateTopicLinks` 对每条提议独立检查；任一规则不满足的条目在内存中被标记为 `rejected` 并记录拒绝原因，通过全部规则的条目标记为 `validated`。被拒绝的条目不会被写入数据库（`InsertLinks` 跳过它们），只出现在 CLI 输出中；通过校验的条目入库时 `status=proposed`。校验不通过不影响其他条目。

| 检查项 | 规则 |
| --- | --- |
| 类型 | `kind` 必须是 `topic_link`。 |
| 话题存在 | `from_topic_id` 和 `to_topic_id` 都必须存在于 `topics` 表。 |
| 非自关联 | `from_topic_id` 与 `to_topic_id` 不能相同。 |
| 关系类型 | `relation` 必须是四种枚举值之一。 |
| 理由 | 去除首尾空白后非空，且不超过 500 字符（rune）。 |
| 证据 | `evidence_turn_ids` 至少 1 条，且每条都必须存在于 `turns` 表。 |
| 置信度 | `confidence` 必须在 [0, 1] 范围内。 |

## CLI 用法

```sh
go run ./cmd/context-associate \
  --db ./data/context-memory.db \
  --input proposals.json \
  --model my-curator
```

| 参数 | 说明 |
| --- | --- |
| `--db` | 必填，SQLite 数据库路径。 |
| `--input` | 必填，提议 JSON 文件路径。 |
| `--model` | 记录到 `memory_links.model` 的模型标识，默认为空。 |
| `--dry-run` | 只解析并校验，不写库。 |

程序在标准输出逐条打印提议的处理结果，格式为 `<状态> <from> -> <to> [<relation>] <说明>`：状态是 `validated`（通过）或 `rejected`（被拒）；被拒条目的说明是拒绝原因（如 `from topic "topic:x" not found`），通过条目的说明是提议理由。最后汇总 `accepted=<n> rejected=<n>`，`--dry-run` 时在末尾追加 ` (dry-run)`。输出示例：

```text
validated topic:release -> topic:launch [same_topic] 两次讨论围绕同一交付物展开。
rejected topic:missing -> topic:launch [follow_up] from topic "topic:missing" not found
accepted=1 rejected=1
```

输入文件中出现未知字段、JSON 解析失败或文件缺失时以非零退出码结束，不写库；所有提议都被拒时仍正常退出（退出码 0），只报告计数。

## 后续（尚未实现）

- **人工确认**：把 `proposed` 改为 `confirmed` 或 `rejected` 的确认/撤销流程尚未实现；当前表中不会出现 `confirmed` 状态。
- **自动触发**：导入或发布后自动扫描生成提议尚未实现，提议始终由 agent 显式产出。
- **读取接口**：查询 `memory_links` 的 MCP 工具尚未提供，当前只能直接查库。
- **关联的下游使用**：确认后的话题合并、跨话题层级视图等尚未实现。
