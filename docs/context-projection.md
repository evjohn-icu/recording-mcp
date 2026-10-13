# 上下文投影 v0（get_current_context）

上下文投影是一个**读取时**的确定性推导层：给定记忆库的当前状态，直接推导出消费方应该看到的上下文对象——person（人物）、topic（话题）、commitment（承诺）、preference（偏好）、event（事件），每个对象都带证据轮次、来源与状态。投影不落库、不新增表：每次调用都从当前行重建，因此天然可重复构建，永远不会与记忆库产生漂移。

实现：`internal/projection`（`Build`），由 MCP 工具 `get_current_context` 暴露。无 LLM 参与，无真值引擎。

## 对象与状态模型

| 字段 | 含义 |
|---|---|
| `id` | 对象的稳定标识：笔记对象即笔记行 id，话题对象即 topic id，人物对象即 person id |
| `kind` | 五类之一：person / topic / commitment / preference / event |
| `subject` | 归组键：承诺按主题组（`topic_ids_json[0]`，无主题则 `conversation:<id>`），话题与人物是其自身 id |
| `status` | current / proposed / superseded / unresolved（`conflicting` 保留给未来真值引擎，v0 **绝不**输出） |
| `confidence` | 固定的规则置信度，见下方映射表 |
| `evidence_turn_ids` | 支撑该对象的当前、非 ambient 轮次 |
| `derived_from` | 来源记录（`note:<id>` 或 `binding:<id>`） |
| `conflict_with` | 冲突对端的对象 id（仅有冲突时出现） |

状态优先级：`superseded`（冲突规则判定）> `unresolved`（证据缺失）> `proposed`（笔记派生的承诺/偏好）> `current`。

**语义安全（v0.1 起）**：`intention` / `request` / `constraint` 派生的对象一律为 `proposed` 且 `status_reason="unconfirmed"`。投影没有任何自动确认路径——说话人说"打算/请求/要求"不等于事实成立；消费方**不得**把 `proposed` 对象当作已确认承诺使用，必须先经显式人工复核。`observation` 派生的 event 仍为 `current`（它只断言"说过/发生过"，不断言未来）。

## 笔记类型映射

| notes.kind | 投影 | 置信度 | 状态 | 条件 |
|---|---|---|---|---|
| intention | commitment | 0.6 | `proposed` / `unconfirmed` | 无 |
| request | commitment | 0.5 | `proposed` / `unconfirmed` | 无 |
| constraint | preference | 0.6 | `proposed` / `unconfirmed` | 无 |
| observation | event | 0.5 | `current` | 仅当主证据轮次（ordinal 0）的 `global_start_ms` 非空 |
| suggestion | 不投影 | — | — | 记入 `unresolved`，原因 `kind_not_projected_in_v0` |
| observation（无时间锚） | 不投影 | — | — | 同上 |

笔记必须同时满足两条与检索层完全一致的过滤：非 stale（`NoteStalePredicate`）、主证据轮次非 ambient。

## 冲突规则（仅承诺，确定性）

冲突只报告，**从不删除或改写**任何笔记：被否定的对象保持其证据与来源，只是状态标记为 `superseded`。

否定线索（`supersedeCues`，逐字匹配，顺序即判定顺序）：

```
取消  暂时不  不再  放弃  改为  延期  不去了  不搬  不办  no longer  cancelled
```

全部为多字线索；单字"不"永远不会触发（"不喜欢"不是否定承诺）。规则：

1. 把承诺对象按 `subject` 分组；
2. 组内按主证据轮次的 `global_start_ms` 升序（并列时按笔记 id 升序）排列；
3. 顺序扫描 i<j 的组合，找到**第一对**满足"较晚摘要含线索、较早摘要不含线索"的组合；
4. 较早者 → `superseded` / `negated_by_later_note`，较晚者 → `current`，双方互写 `conflict_with`，并输出一条 `Conflict`（含命中的线索）；
5. 每组最多一条冲突。

## 话题对象

对每个"仍有当前成员"的话题行输出一个 topic 对象：成员指经 `json_each(topic_ids_json)` 挂在该话题下的当前笔记，或经 `turn_topics` 挂在该话题下的当前非 ambient 轮次。`conversation_ids` 来自 `topic_conversations`，`evidence_turn_ids` 取自 `turn_topics` 中最多 5 个按 id 排序的当前轮次，`link_ids` 是与该话题相关的非 stale 且 confirmed 的关联。置信度 0.7。

## 人物对象

只来自 `speaker_bindings` 中 `status='confirmed'` 且 `person_id` 非空的行——proposed / rejected / revoked 绝不投影。证据是该绑定自己的转录修订内、同录音同说话人标签的当前轮次（经 `attachment_turns`/`source_attachments` active=1 连接），按时间序最多 5 条。身份按（录音，修订）隔离，绝不跨修订沿用。同一 person 的多条绑定合并为一个对象；当所有证据轮次都已过时，对象状态为 `unresolved` / `evidence_superseded`，而不是被悄悄丢弃。置信度 1.0（复核裁决即来源事实）。

## 审计模式

`audit=true` 时额外加载 stale 笔记，输出为 `superseded` / `stale_evidence` 对象；默认模式下 stale 笔记完全不出现（这是 `stale_not_current` 评估指标的依据）。

## 范围、排序与限额

- `conversation_ids` / `topic_ids` / `person_ids` / `kinds` 都是 any-of 硬过滤；空表示不限。
- 时间边界 `started_after` / `started_before` 作用于对象的会话开始时间（任一会话命中即可）；开始时间未知的会话在有界请求中被排除。
- 排序：对象按（kind 升序，subject 升序），冲突按（subject 升序，earlier_id 升序），unresolved 按（kind, subject, reason）升序。
- `limit`（默认 20，最大 200）只截断 `objects`；`conflicts` 与 `unresolved` 永远完整返回，因为它们是诚实性输出，被限额截掉会让消费者误以为没有冲突。

## 确定性保证

- 输出中不出现任何 map 迭代顺序：所有集合在输出前排序；
- 融合、排序、限额全部确定；同一数据库快照 + 同一请求 = 逐字节相同的 JSON；
- 无时钟输入：没有字段依赖 wall-clock；
- `trace.rules` 是固定字面列表：`note_kind_mapping`、`commitment_conflict_cues`、`topic_from_membership`、`person_from_confirmed_bindings`。

## 明确不投影的内容

- `suggestion` 笔记（v0 无规则，进 `unresolved`）；
- 无时间锚的 `observation`（拒绝编造事件时间）；
- `project` 类型：保留，v0 不做任何虚构分类；
- 没有真值引擎：冲突只报告，不裁决；
- 投影路径上没有 LLM：一切规则都是代码里的字面规则；
- ambient 内容：主证据轮次为 ambient 的笔记从不进入投影。
