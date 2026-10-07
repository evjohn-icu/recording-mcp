# Context Layer 0.1 规范与原型

可运行链路已接通：**ASR 结果 → 供应商适配器 → 原话和来源入库 → 带证据的记忆注释 → MCP 查询**。从 [Context Layer 运行说明](docs/context-layer-runtime.md) 开始使用，包含构建、导入和 18 个 MCP 工具（13 个只读、5 个写入，其中 `build_context` 为实验性）。记忆层不调用 ASR；自动提炼的候选生成与发布已实现（内容未经人工复核），检索三件套 `list_conversations` / `get_evidence_context` / `search_memory` 已实现，关键词检索目前是字面子串匹配；Decision 判断在 extract 内仅作 shadow 记录，不参与发布；转写修订可通过 `supersedes_revision` 替换，旧修订保留为审计历史，其派生记忆标记 stale 并自动重算，不再出现在当前检索中。说话人身份不自动实名：经人工确认的 speaker binding 按 transcript 修订投影解析（`speaker_person_id`），未确认身份仍为未知。完整层级视图尚未实现。

下一阶段的处理流程、执行器对接、检索扩展、团队分工和验收条件见 [开发执行计划](docs/development-plan.md)。其中持久化任务队列与 Worker 租约执行（A 切片）、决策引擎契约与 shadow 判断记录（B 切片）、候选记忆的生成校验与会话级发布（C 切片）已实现，详见 [处理链路说明](docs/processing.md)；层级检索三件套 `list_conversations` / `get_evidence_context` / `search_memory` 已实现，详见 [Context Layer 运行说明](docs/context-layer-runtime.md)。本地 Decision 2.0 判断服务的 Adapter 已就绪：本机可直接运行（实测 `127.0.0.1:8765/v1/systemone` 应答 `jev-style-2b-v2` 判断），仓库另附同契约的本地参考实现 `bin/context-decision`（确定性沙盒判断，离线/CI 用；`weights_version` 如实标注），`--decision-url` 指向任一实现即可跑通 `--executor pipeline` 全链路；自动发布的注释内容未经人工复核；话题关联建议已支持（仅建议层）：agent 只读记忆并产出提议，经 `context-associate` 校验后以 `proposed` 状态写入 `memory_links`，不自动合并，详见 [关联建议说明](docs/associations.md)；人工复核通过 MCP 工具 `list_memory_links` / `review_memory_link` 执行（确认不等于合并话题，拒绝是黏性的），publish 提交后由 worker 自动入队 curate 任务；实验性的 `build_context` 已实现（预算受限、带来源与覆盖缺口；未配置决策引擎时不重排并如实标记 degraded）；Jevbox 的完整层级检索视图仍未实现。

0.1 的现实世界信息全部来自随身音频设备，经过录音分析中心处理后进入Context Layer。当前工作目标是制定两份 Context Layer 契约：**内部规范 0.1 规定存什么，MCP 规范 0.1 规定怎么传**。请从 [规范目录](docs/specs/README.md)、[内部规范](docs/specs/internal-context-0.1.md) 和 [MCP 规范](docs/specs/context-mcp-0.1.md) 开始阅读。

规范同时覆盖过往沟通：本地数据库持久保存已导入的逐轮转写、会话摘要、参与者与音频证据，支持以后查“和谁聊过什么”并追溯原话。当前新增的[会话记忆实验实现](docs/context-memory.md)提供 SQLite 导入与 MCP 查询，保存原话、话题和带证据的讨论记录；人物身份确认、状态推导及完整 0.1 契约尚待实现。

本项目计划公开规范与代码，目前为 0.1 草案。面向公开发布的范围、版本约定和原型状态见 [公开发布约定](docs/publication.md)。示例统一使用虚构身份与通用项目；个人录音、转写和运行数据库由使用者自行保存。

[豆包 ASR 适配器](docs/doubao-adapter.md)将已解析的供应商响应转为统一转写对象，保留原始证据。它独立于记忆层，不调用 ASR，也不执行人物或话题分析。

下文描述之前完成的 Recording MCP 原型，用于参考已验证的数据库与协议连接能力。它尚未实现上述 Context Layer 0.1 草案，当前接口仍以 [原型契约](docs/contract.md) 为准。

Recording MCP 是一个基于 Go、SQLite 和 MCP stdio 的本地事件记录服务。它把结构化事件写入 SQLite，并提供按时间、人物、会话和状态查询的 MCP 工具。示例数据全部是为演示编写的合成内容，不是实际录音、聊天记录或真实人物资料。

## 构建与启动

需要 Go 1.27.1 或更新版本。在项目根目录执行：

```sh
go build -o bin/recording-mcp ./cmd/server
```

直接启动 MCP stdio 服务：

```sh
./bin/recording-mcp --db ./data/recordings.db
```

首次演示时可用 `--seed` 将内嵌样例写入数据库；重复启动时，已存在且内容相同的事件会保持不变。若事件 ID 冲突但内容不同，导入会报错。传入 `--at 2026-10-03T20:00:00+08:00` 可让“当前状态”和有效期查询固定在样例锚点，便于复现；不传时使用当前时间。

```sh
./bin/recording-mcp --db ./data/recordings.db --seed --at 2026-10-03T20:00:00+08:00
```

数据库目录会在需要时创建。此进程使用标准输入和标准输出承载 MCP JSON-RPC；日志写到标准错误。不要在 stdio 通道中输出其他内容。

## MCP 工具

下文描述的既有 Recording MCP 原型（与上面的 Context Layer 实验实现相互独立）提供八个工具：

| 工具 | 用途与主要参数 |
| --- | --- |
| `search_events` | 搜索事件。`query` 必填（可为空）；可选 `person`、`start_time`、`end_time`、`min_confidence`、`limit`、`offset`。 |
| `get_timeline` | 取时间范围内的事件。`start_time`、`end_time` 必填；可选 `entity`、`limit`、`offset`。 |
| `get_commitments` | 查询承诺。可选 `status`、`person`、`deadline_before`、`time`。 |
| `get_conversation_events` | 获取会话记录（事件视图）。`conversation_id` 必填；`detail_level` 可为 `summary` 或 `full`。 |
| `get_person_context` | 获取人物相关事件。`person_id` 必填；可选 `start_time`、`end_time`、`topics`、`limit`、`offset`。 |
| `get_current_state` | 获取指定维度的当前有效状态。可选 `dimensions`、`time`。 |
| `ingest_events` | 导入 1–500 条结构化事件；批次为原子操作，重复导入相同 ID 和内容可安全重试，冲突 ID 会失败。 |
| `record_feedback` | 记录明确的反馈事件。当前支持 `commitment.completed`、`commitment.cancelled`、`deadline.changed`、`agent.action.task_created`，目标 ID 使用原始 ID 值，例如 `demo-bom-delivery`。 |

时间字段使用 RFC 3339。事件保留来源、观察时间、置信度、标签和原始内容，查询结果可用于回溯依据。事件是追加的观察记录；当前状态和承诺列表是查询时根据事件及反馈计算的视图。导入不会进行音频转写、说话人识别或自动推断，调用方应提供结构化事件及其来源信息。

当前版本将事件 JSON 存入 SQLite 的 `events` 表，使用触发器阻止更新和删除。查询时扫描事件并推导状态，适合小规模本地原型；状态仍保留低置信度证据及其分数，调用方应自行判断，历史搜索可使用 `min_confidence` 过滤。`make test` 执行数据库与真实 stdio MCP 的自动化测试。

详细字段示例见 [`docs/contract.md`](docs/contract.md)。可导入的合成事件位于 [`fixtures/recordings.json`](fixtures/recordings.json)，反馈示例见 [`examples/feedback.json`](examples/feedback.json)。

## MCP 客户端配置

可将 [`examples/mcp.json`](examples/mcp.json) 中的 server 项合并到 MCP 客户端配置。示例使用 `/absolute/path/to/recording-mcp` 占位路径，必须替换为实际二进制和数据库的绝对路径后使用。配置只供参考，本项目不会自动修改客户端的 MCP 配置。
