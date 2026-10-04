# Context Layer 内部规范 0.1

状态：**规范草案，供评审；当前二进制尚未实现本规范。**
版本：`internal-context/0.1`。本文件定义存储语义，不绑定 Go、SQLite、录音平台或 MCP 协议版本。

本文的“必须”“不得”“应”分别表示要求、禁止和推荐。实现声明兼容 0.1 时必须满足所有“必须”条款。

## 1. 范围

本层面向单用户、本地的随身音频上下文。0.1 的现实世界观察全部来自随身音频设备，经录音分析中心转写和语义提取后入库；多台设备、多次录音属于同一种输入来源。
目标是保留可追溯的信息，并从事件推导可查询的当前状态；本层不定义代理框架、消费者路由引擎或模型推理流程。
存储只回答“有什么信息、来自哪里、何时可用、状态如何变化”。用途筛选、重要性排序及预算裁剪由交付层定义。

0.1 只定义三个核心对象：`Entity`、不可变 `Event`、可重建 `State`。
设备与录音目录、来源目录、类型注册表、索引属于辅助元数据，不增加核心业务对象。消费方反馈是内部操作记录，必须与音频观察区分，不能作为新的录音证据。

## 2. Entity：稳定身份

实体必须使用稳定、不含展示名的 ID，例如 `ent_person_001`、`ent_project_bom`。
展示名、别名和来源中的原始名称不得作为主键；名称相同不得自动合并身份。
引用必须指向已存在的实体，或在同一原子写入中创建该实体。
追加操作可以附带 `entities` 数组，与事件在同一事务中注册，支持空库初始化。
无法识别人名时应创建明确标记为 `unresolved` 的实体，不得猜测真实身份。
实体合并与名称修订必须保留原 ID 的解析关系和修订历史，不得静默改写历史事件引用。

| 字段 | 类型与约束 |
| --- | --- |
| `id` | 全库唯一稳定字符串 |
| `kind` | `person`、`organization`、`project`、`topic`、`object`、`commitment` 或 `conversation` |
| `display_name` | 可更改的展示字符串，不参与事件身份 |
| `aliases` | 字符串数组，只帮助查找，不证明身份 |
| `resolution` | `resolved` 或 `unresolved` |
| `classification` | 默认 `private`；可显式设为 `public` |

主题也是 `kind=topic` 的实体。`subject_ids` 表示信息涉及的主体，`topic_ids` 表示主题；两者都使用实体 ID。
承诺可作为 `kind=commitment` 的实体，使完成、取消和期限变更始终引用同一对象。

## 3. Event：不可变记录

事件表示“某来源报告了某信息或某动作”，不等于已经验证的现实。
事件提交后必须禁止覆盖或删除；修正、撤回和反馈均追加新事件。
原始音频和上游全文转写留在录音分析中心；本层必须持久保存已导入会话的逐轮转写文本，不能只保存音频 URL。
会话摘要与抽取结论可以另存为事件，但不得代替逐轮记录。

| 字段 | 类型与约束 |
| --- | --- |
| `id` | 全库唯一事件 ID |
| `type` | 注册的事件类型 |
| `schema_version` | 对应类型的数据契约版本，例如 `1` |
| `source_id` | 来源目录中的稳定来源 ID |
| `source_event_id` | 来源内稳定记录 ID，必须提供 |
| `source_ref` | 可选音频定位符，引用已登记录音的片段，不自动访问媒体 |
| `origin` | `audio` 或 `feedback`；现实世界观察必须为 audio |
| `claim_mode` | audio 观察为 `explicit_statement`、`reported_statement` 或 `inference` |
| `audio_evidence` | audio 观察必需的非空片段证据数组；feedback 不得伪造此字段 |
| `subject_ids`、`topic_ids` | 去重的实体 ID 数组 |
| `conversation_id` | 可选会话实体 ID；未知关联时省略，不能用录音 ID 代替 |
| `content` | 来源信息的简短可读表述 |
| `data` | 按类型注册表校验的结构化对象；不使用任意字段猜测业务语义 |
| `confidence` | `0..1` 或 `null`；不确定程度，非真实性标签 |
| `confidence_basis` | 可选字符串，说明来源评分或提取方法 |
| `priority` | `low`、`normal`、`high`；默认 `normal` |
| `occurred_at` | RFC3339 时间或 `null`；信息涉及的发生时刻 |
| `observed_at` | RFC3339 时间；来源声明的首次观察时刻 |
| `recorded_at` | 服务端生成的 RFC3339 首次接收时刻，不接受调用方覆盖 |
| `recorded_seq` | 服务端原子分配的全库单调递增整数，不接受调用方覆盖 |
| `valid_from`、`valid_until` | RFC3339 时间或 `null`；声明的有效区间 |
| `expiry_seconds` | 非负整数或 `null`；保存后多少秒停止作为当前信息 |
| `classification` | 默认 `private`，可显式设为 `public` |
| `raw_excerpt` | 可选最小原文片段，仅作证据；默认交付不包含 |

`occurred_at` 即 happened-at 语义：录音中的事情发生时间，可能早于收到转写的时间。
`observed_at` 是来源的观察时间，属于来源声明；`recorded_at` 才表示本层何时收到记录。
`recorded_seq` 建立接收顺序和分页快照水位；不得以可回填的来源时间代替接收序号。
入库必须校验 `observed_at <= recorded_at`；未来来源观察时间必须拒绝，不得静默改写。
所有时间必须转换为 UTC 保存，保留 RFC3339 的瞬时时间含义；展示可以转换时区。
未知发生时间使用null，不得用接收时间伪装为已知发生时间。相对日期无法解析时保留raw_excerpt中的原始表达，deadline省略；不能提交缺少明确deadline的期限变更。

有效区间使用 `[valid_from, valid_until)`；非空上界必须晚于非空下界。
`valid_from=null` 表示未声明开始限制，不代表事件在本层首次接收前已被系统知晓。
`valid_until=null` 表示未声明结束限制，不表示永久正确。
`expiry_seconds=null` 表示未声明过期；`0` 表示收到后立即不进入当前有效上下文。
`expires_at = recorded_at + expiry_seconds`；计算必须检查溢出，不得回绕。
有效结束取 `valid_until` 与 `expires_at` 的较早值；达到边界即失效。
过期只影响当前可用性，历史事件与审计查询仍须保留。

`confidence=null` 表示未评分，不得解释为 0 或 1。
转写、提取、来源自身说法的分数必须说明含义；分数不得跨来源假定完全可比。
高优先级只表示显著性意图，不得提高置信度、延长有效期或证明事实。

## 3.1 随身音频来源与片段证据

录音分析中心可以暂时用模拟数据实现，但输出必须遵守同一契约。Context Layer 不负责转写或说话人识别算法；它必须保存足够信息，让消费方知道提取结论来自哪段录音。

设备目录登记稳定 device_id；录音目录登记 recording_id、device_id、recording_started_at（未知可为null）、capture_timezone（未知可为null）、duration_ms（未知可为null）和可选媒体引用。原始音频与全文转写留在录音分析中心，Context Layer 保存音频引用、已导入会话的逐轮文本及按需的证据摘录。

每个 audio_evidence 条目必须含以下字段：

| 字段 | 含义与约束 |
| --- | --- |
| device_id / recording_id | 已登记设备与录音ID，录音所属设备必须一致 |
| segment_id | 该录音中的稳定片段ID |
| start_ms / end_ms | 相对录音起点的毫秒偏移；0≤start_ms<end_ms，已知时不得超过录音长度 |
| speaker_id | 该录音和说话人分析版本内的说话人标识，未知为null；不能直接当成真实人物ID |
| transcript_version / analysis_version | 本次转写与语义分析的版本，必须可追溯 |
| asr_confidence / extraction_confidence | 分别表示转写与提取的不确定性，0..1或null |
| speaker_confidence | 说话人归属的置信度，0..1或null |

片段ID、说话人标签与人物实体ID不同。说话人到人物的映射必须保留依据、版本和置信度，不能因为设备属于用户就把所有声音都归给用户。无法确定“我”是谁时，使用unresolved人物实体，保留原文，不猜测承诺主体。

claim_mode=explicit_statement 表示音频中说话人直接陈述；reported_statement 表示转述他人说法；inference 表示分析中心的推断。三者都不证明现实事实已经发生。ASR分数不能当成事实真实性或人物身份分数；Event.confidence 非null时必须给出confidence_basis说明结论评分方法。

occurred_at 对承诺创建默认表示承诺被说出的时刻，由录音起点与片段偏移得到；“明天发BOM”的明天属于data.deadline，不能把创建时刻移到明天。无法确定录音绝对时间、时区或相对日期时，相关时间返回null，并在data中保留原始时间表达。observed_at 是分析中心产出这条观察的时刻，recorded_at 是Context Layer收到它的时刻。

音频观察必须有origin=audio、claim_mode与非空audio_evidence；feedback.recorded必须有origin=feedback，只引用已有事件/状态，不能伪装为音频观察或直接完成承诺。生命周期、事实、更正与撤回的现实依据均须追溯到音频片段。重新转写或重新提取须产生新版本和新来源记录ID，通过显式更正关联旧结论，不能覆盖原事件。同一音频片段可提取多条事件，各条有自己的source_event_id，共享音频定位。

## 4. 类型注册表与最小 data 契约

每个类型必须声明名称、版本、所需字段、字段类型、实体引用、状态影响及是否属于报告或操作反馈。
未知类型或未知版本不得参与状态投影；未注册类型必须拒绝进入规范事件库。
扩展采用带命名空间的类型，例如 `audio.topic.observed`，并先注册契约。
不得依靠事件名称包含某个单词，自动推断其维度、完成状态或撤回语义。
下面类型的 `schema_version` 均为 `1`。

| 类型 | 必需 `data` | 投影语义 |
| --- | --- | --- |
| `commitment.created` | `commitment_id`、`description`、`owner_ids`；可选 `deadline` | 创建承诺，状态 `active` |
| `commitment.completed` | `commitment_id`；可选 `note` | 记录完成报告，状态 `completed` |
| `commitment.cancelled` | `commitment_id`；可选 `reason` | 记录取消报告，状态 `cancelled` |
| `commitment.deadline_changed` | `commitment_id`、`deadline` | 修改期限，保留生命周期状态 |
| `fact.observed` | `fact_id`、`predicate`、`value` | 为同一事实槽追加观察，可选 `unit` |
| `fact.retracted` | `target_event_id`、`reason` | 撤回指定事实观察的贡献 |
| `event.corrected` | `target_event_id`、`replacement_event_id`、`reason` | 原事件保留，投影使用已追加的替代事件 |
| `event.retracted` | `target_event_id`、`reason` | 原事件保留，其投影贡献撤回 |
| `conversation.session_recorded` | `participant_ids`、`started_at`、`ended_at`、`coverage`、`completeness` | 保存会话范围与参与者，不投影事实 |
| `conversation.turn_recorded` | `turn_id`、`turn_index`、`text`、`speaker_entity_id` | 保存逐轮沟通文本，不投影事实 |
| `conversation.summary_recorded` | `summary`、`covered_event_ids`、`key_points`；可选 `mentioned_entity_ids` | 保存衍生摘要，不投影事实或完成承诺 |
| `feedback.recorded` | `action`、`target_ids`、`result` | 保存操作或代理反馈，默认不修改现实状态 |

`commitment_id` 必须引用承诺实体，`owner_ids` 必须引用主体实体。
`deadline` 必须为 RFC3339 时间；期限未知时创建事件可以省略该字段，变更事件不得省略。
生命周期事件不得另带冲突的 `status` 或主体类别；状态由注册类型决定。
同一承诺在一个投影快照中只能有一个有效创建；重复来源重放不算第二次创建。显式 event.corrected 可以引用兼容的替代创建事件，历史保留两条，但只有替代事件参与更正后的投影；孤立的第二次创建必须拒绝。
完成、取消和期限变更必须引用已有创建事件，逻辑发生时间不得早于创建时间。
完成与取消是终态；期限变更不得重开承诺。更正终态必须走显式更正或撤回，不得暗中覆盖。
终态之间的冲突报告必须保留并标记状态冲突，不得仅凭更高分数裁定现实。

`fact_id` 是稳定事实槽 ID，与固定 `subject_ids` 和 `predicate` 绑定；不得把不同主体或谓词塞入同一槽。
事实槽可以描述人、任务、项目或主题；不另增加“任务表”“人物事实表”等强制核心对象。
事实观察必须保留“来源报告”性质；观察的新旧不会自动证明哪个值正确。
`fact.retracted` 必须指向 `fact.observed`；更正或撤回目标必须已经存在，且不得形成引用循环。
`event.corrected` 的替代事件必须与目标业务身份兼容，并在同一事务或此前写入。
撤回／更正记录不得被再次作为撤回目标；如需修订撤回决定，追加新的兼容业务事件并明确说明。

工具返回“动作成功”只能产生 origin=feedback 的 feedback.recorded。例如发出提醒不等于承诺完成。
只有有音频片段依据的完成陈述才能写入 commitment.completed；说话人陈述“已经发了”仍是完成报告，不是外部交付验证。

## 4.1 会话历史：沟通过什么

会话为 `kind=conversation` 的 Entity，使用稳定 `conversation_id`；逐轮记录和摘要仍是 Event，不增加第四种业务对象。
承诺、事实等相关事件可以带同一个 `conversation_id`，但关联本身不使它们成为转写或已验证事实。
一个会话可以跨多个录音；同一录音也可以包含多个会话。录音 ID、音频局部说话人 ID 和会话 ID 不得混用。
会话边界未知时，`conversation.turn_recorded` 可以暂不带 `conversation_id`；`turn_id` 必须稳定且在来源内唯一。
随后确定会话关联必须追加替代事件与 `event.corrected`，不得覆盖原事件或靠动态聚类静默改变历史。

`conversation.session_recorded` 必须带 `conversation_id`，其 `data` 如下：

| 字段 | 约束 |
| --- | --- |
| `participant_ids` | 人物实体 ID 数组；身份未知可用 unresolved 人物实体，参与者尚不明确可为空 |
| `started_at` / `ended_at` | RFC3339 或 null；都已知时结束不得早于开始，未知不得猜测 |
| `coverage` | 非空音频范围数组，每项 `{recording_id,start_ms,end_ms}`，遵守音频偏移校验 |
| `completeness` | `partial` 或 `complete`，只描述声明 coverage 内分析记录的覆盖情况 |

参与者指实际加入会话的人；被提及的人只能进入相关主体或 `mentioned_entity_ids`，不得因此列为参与者。
`complete` 只表示声明范围内待导入的会话分析记录已全部保存，不保证设备捕获全部现实谈话，也不保证转写正确。
只有部分逐轮转写时必须为 `partial`；来源范围扩展或覆盖情况变化必须追加新会话范围版本并显式更正旧版本。
会话范围、参与者及时间不直接投影人物事实；需要事实时须另追加有音频依据的 `fact.observed`。

`conversation.turn_recorded.data.text` 必须保存非空实际转写文本，而不是摘要或外部 URL；`content` 可以是展示截句。
`turn_id` 是稳定逻辑轮次 ID；已关联会话时以 `(conversation_id,turn_id)` 标识轮次，否则以 `(source_id,turn_id)` 标识。
`turn_index` 为非负整数，是声明会话内的逻辑顺序；暂未关联时为来源声明顺序。同一有效会话版本中不得有重复轮次 ID 或重复序号。
来源的序号不连续可以保留，用 `partial` 表达覆盖缺口；不得为了补齐序号编造话语。
`speaker_entity_id` 为人物实体 ID 或 null；未知身份使用 null，音频内的局部 `speaker_id` 必须保留在 `audio_evidence`，两者不得互相代替。
已确认或 unresolved 的人物映射须保留映射依据与置信度；局部 speaker 标签不能跨录音推定同一个人。
每条 turn 都必须有 `origin=audio` 和非空 `audio_evidence`；文本版本由证据中的 `transcript_version`、`analysis_version` 标识。
转写修订必须沿用逻辑 `turn_id`，追加新事件并用 `event.corrected` 关联旧事件；未声明更正的同轮第二份有效文本必须拒绝。

`conversation.summary_recorded` 必须带 `conversation_id`，保存非空 `summary`、非空 `covered_event_ids` 和 `key_points` 数组。
`covered_event_ids` 必须引用同一会话内已存的 `conversation.turn_recorded`；证据必须限定为这些轮次，不得包含会话外内容。
摘要为衍生表述，`claim_mode` 必须为 `inference`；`audio_evidence` 必须可追溯到覆盖轮次的音频范围。
`mentioned_entity_ids` 可列出被提及的人或组织，不意味着他们参加了会话；每条 `key_points` 也只是摘要，不直接写入事实或承诺状态。
摘要不能代替逐轮记录，不能自行证明承诺完成；摘要修订也须通过不可变更正机制关联旧摘要。
被覆盖轮次更正或撤回后，旧摘要必须标记为证据已变更，不得作为默认有效摘要继续交付；重新分析须追加兼容替代摘要。

沟通历史查询默认展示快照中的有效版本；会话详情先列 session、再列 summary、最后列 turn，各类型内分别按有效发生时间／事件 ID、有效发生时间／事件 ID、`turn_index`／事件 ID 升序排序。
一般 event 查询沿用其统一时间排序；会话详情的分组次序不得改变分页中途排序。重新分析不得把同一轮显示两次。
会话 session、turn、summary 均不生成 State；相关承诺只能由独立生命周期事件生成，并可用 `conversation_id` 回连证据。
历史文本的 TTL 到期只限制进入当前上下文，不得删除过去说过什么的记录；`get_conversation_context` 与显式 event 历史查询可以读取过期的有效版本，返回既有 `valid_until`／`expires_at` 供调用方辨识，不增加 history_mode 或 expired 字段。
已撤回／被更正的原文仍可按事件 ID 追溯，显式审计读取可保留原版本、替代关系和原因；默认历史列表不得把它们重复显示为当前文本。
会话历史仍须固定 `known_at`、`recorded_seq` 水位及目录／注册表版本；更正前的快照必须返回当时可知的原版本。

## 5. 身份、去重与原子写入

`id` 与 `(source_id, source_event_id)` 都必须唯一。
同一来源记录的相同规范化载荷重放必须返回原事件，不新增事件、不刷新 `recorded_at`、`recorded_seq` 或过期时间。
来源键已存在而请求 ID 不同时，返回 `SOURCE_EVENT_CONFLICT` 并给出原有 ID，调用方使用该 ID 重试；不得创建第二条记录。
同一 ID 或来源记录键对应不同载荷必须拒绝，要求调用者追加更正事件。
比较载荷包括来源提供的字段；接收层生成的 ID、`recorded_at`、`recorded_seq` 不参与来源重放内容差异判断。
规范化仅可处理时间格式、对象键顺序及无序引用数组，不得改写内容意义。

批量写入必须先校验类型、引用、时间、数值和所有唯一约束，然后原子提交；任一失败不得部分落库。
批内前向引用允许在整个批次通过引用和无环校验后提交。
错误必须区分无效载荷、未知引用、重复冲突和存储失败。

## 6. State：可重建的派生状态

State 必须由有效事件确定性重放而来，允许缓存；缓存不得成为唯一真相来源。
删除缓存后，使用相同规范版本与查询时刻必须重建相同状态。

| 字段 | 类型与约束 |
| --- | --- |
| `id` | 稳定状态 ID，例如 `state_commitment_ent_commit_bom` 或 `state_fact_f001` |
| `kind` | `commitment` 或 `fact` |
| `subject_ids`、`topic_ids` | 稳定实体引用，不得聚合无关主体 |
| `content`、`data` | 从贡献事件生成的摘要与状态值 |
| `confidence` | `0..1` 或 `null`；继承内容须保守处理 |
| `priority` | 贡献信息的显著性提示，不决定正确性 |
| `occurred_at`、`observed_at` | 最新状态贡献的发生／来源观察时间 |
| `recorded_at`、`recorded_seq` | 最新已纳入贡献的接收时间与最大接收序号 |
| `valid_from`、`valid_until`、`expires_at` | 当前状态的有效性边界 |
| `provenance` | 全部实际贡献事件和来源引用，不得只有最后一条 ID |
| `projection_version` | 本次重放使用的规范／投影版本 |

承诺 `data.status` 为 `active`、`completed`、`cancelled` 或 `conflicted`。
事实 `data` 至少包含 `fact_id`、`predicate`、`value`；冲突时须保留 `alternatives` 和各自证据。
0.1 对同一事实槽先确定每个来源最后一次观察。同源后续观察替代先前观察；该最新观察过期或撤回时，此来源无当前值，不得退回旧值。各来源在 as_of 下有效的最新报告值相同则合并证据；值不同或同源同一发生时刻出现不同值时，返回 `data.status=conflicted`、`value=null` 与带证据的 alternatives。JSON 对象键顺序不影响值比较，数组顺序有意义。发生时间未知时按来源观察时间比较；这些规则只确定当前报告，不证明现实真相。
反馈补充元数据时，必须保留创建及被继承字段的证据；继承字段的置信度不得因新反馈变高而自动提高。
如多个贡献字段评分不同，0.1 单一状态分数使用已评分贡献的最小值；存在未评分贡献时状态分数为 `null`。
有效区间和过期约束必须保留被继承字段的限制；不得用期限变更或反馈为过期事实续命。

投影查询必须明确 `as_of`（有效时间）和 `known_at`（知识截止时间）；默认两者为响应 `generated_at`。
查询开始必须固定 `snapshot_watermark`（当时最大已提交 `recorded_seq`），后续分页使用同一水位。
快照同时必须固定实体目录修订、类型注册表修订和 `projection_version`；名称更新不得改变已有分页快照的内容。
辅助元数据修订应进入同一单调序号体系，或保存可按快照解析的不可变修订版本；不允许只读取最新展示名。
仅使用 `recorded_at <= known_at`、`observed_at <= known_at` 且 `recorded_seq <= snapshot_watermark` 的事件。
已知 `occurred_at` 必须不晚于 `as_of`；未知发生时间使用 `observed_at` 判断此限制。
声明的开始边界之后、结束边界之前的信息才可作为有效贡献。
历史 `as_of` 可以包含后来获知的报告；要读取当时已知状态，调用方必须同时设置 `known_at=as_of`。
按 `occurred_at`（未知时使用 `observed_at`）、`observed_at`、`recorded_seq`、`id` 升序重放。
更正和撤回先按知识截止时间确定目标贡献集合，再重放业务状态；它们不得把更正前的历史读取静默改写。
同一瞬时的冲突值不得靠字典序假装达成一致；字典序只保证输出顺序，业务冲突仍须标记。
终态必须压制同一承诺的旧创建记录；迟到、回填或新索引不得使已完成承诺重新变为 `active`。
最新观察过期、撤回或失效时，应呈现无当前值或明确未知；不得自动把过时旧值作为当前事实复活。

## 7. 交付层共用 ContextItem

以下是完整（full）交付投影形状，不是第四个存储对象，也不要求复制一套业务数据库。
MCP 交付规范可以包装此形状，但不得更改字段含义。

```json
{
  "id": "state_commitment_ent_commit_bom",
  "kind": "state",
  "subject_ids": [
    "ent_person_demo",
    "ent_commit_bom"
  ],
  "topic_ids": [
    "ent_topic_bom"
  ],
  "content": "合成示例：BOM 核对承诺尚未完成。",
  "data": {
    "state_kind": "commitment",
    "status": "active",
    "deadline": "2026-10-04T10:00:00Z"
  },
  "confidence": 0.8,
  "priority": "high",
  "occurred_at": "2026-10-03T11:00:00Z",
  "observed_at": "2026-10-03T11:05:00Z",
  "valid_from": null,
  "valid_until": null,
  "expires_at": null,
  "provenance": [
    {
      "event_id": "evt_demo_bom",
      "source_id": "src_demo_recording",
      "source_event_id": "rec_demo_segment_01",
      "source_ref": "synthetic://recording/segment/01",
      "origin": "audio",
      "claim_mode": "explicit_statement",
      "audio_refs": [
        {
          "device_id": "wearable_demo_01",
          "recording_id": "rec_demo_20261003",
          "segment_id": "segment-01",
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

`kind` 仅为 `event` 或 `state`；状态的业务类别放入 `data.state_kind`，事件的类型与版本放入 `data.event_type`、`data.schema_version`，其原始业务 data 放入 `data.payload`。
ContextItem 可以带顶层 `conversation_id`，含义与 Event 的稳定会话引用一致。
默认上下文集合必须排除逐轮事件，显式 `include_turns=true` 或按事件 ID 读取才可交付；full turn 的 `data.payload.text` 是本层保存的转写，不等同于默认省略的 `raw_excerpt`。
ContextItem 的 `recorded_at`、`recorded_seq` 为可选服务元数据，不属于完整交付的必需字段；查询仍必须固定服务端快照水位。
`occurred_at`、`confidence` 和三项有效性时间允许 `null`；数组必须返回数组，未评分不得省略成隐式高可信。
provenance每项必须含event_id、source_id、source_event_id、origin。audio来源还必须带claim_mode和audio_refs数组，保留设备、录音、片段、时间偏移、说话人标识与处理版本；不含全文转写。source_ref可省略。ContextItem必须回显origin；派生state只采用音频依据，feedback以独立event交付。
compact 交付只可以省略 `content` 和业务 `data`；身份、时间、置信度、优先级与 provenance 必须保留。
full 与 compact 均不得嵌入音频、二进制附件或 base64 媒体；仅使用来源引用。
默认不交付 `raw_excerpt`；证据读取可以单独按事件 ID 返回可访问的来源片段。
用途 `purpose` 是下游选择约束，显著性 `priority` 是信息属性；二者不得混为同一个字段。

## 8. 合成输入例

以下姓名、承诺和录音位置均为合成数据，不表示任何真实个人作出承诺。
假设所引用的人物、承诺和主题实体已注册。此例展示接收后已保存的事件，`recorded_at` 与 `recorded_seq` 由服务端生成，不属于追加输入；输入中提供这些字段必须拒绝。

```json
{
  "id": "evt_demo_bom",
  "type": "commitment.created",
  "schema_version": 1,
  "source_id": "src_demo_recording",
  "source_event_id": "rec_demo_segment_01",
  "source_ref": "synthetic://recording/segment/01",
  "subject_ids": [
    "ent_person_demo",
    "ent_commit_bom"
  ],
  "topic_ids": [
    "ent_topic_bom"
  ],
  "content": "合成人物承诺明天核对 BOM。",
  "data": {
    "commitment_id": "ent_commit_bom",
    "description": "核对 BOM",
    "owner_ids": [
      "ent_person_demo"
    ],
    "deadline": "2026-10-04T10:00:00Z"
  },
  "confidence": 0.8,
  "priority": "high",
  "occurred_at": "2026-10-03T11:00:00Z",
  "observed_at": "2026-10-03T11:05:00Z",
  "recorded_at": "2026-10-03T11:06:00Z",
  "recorded_seq": 1,
  "valid_from": null,
  "valid_until": null,
  "expiry_seconds": null,
  "classification": "private",
  "origin": "audio",
  "claim_mode": "explicit_statement",
  "confidence_basis": "合成示例：语义提取评分，不表示事实已验证",
  "audio_evidence": [
    {
      "device_id": "wearable_demo_01",
      "recording_id": "rec_demo_20261003",
      "segment_id": "segment-01",
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

后续完成报告追加 `commitment.completed`，使用新的事件／来源记录 ID，并引用 `data.commitment_id=ent_commit_bom`。
不得修改 `evt_demo_bom`；完成状态证据至少包含创建与完成报告两条事件。

## 9. 合成沟通历史例与验收

以下是合成追加输入，所有引用实体、设备和录音均假定已登记；不包含服务端生成的 `recorded_at`／`recorded_seq`。
会话 `ent_conv_demo`、人物 `ent_person_demo`、被提及人物 `ent_person_li` 与主题 `ent_topic_bom` 使用不同稳定 ID。
录音目录：`rec_demo_20261003` 属于 `wearable_demo_01`，起点 `2026-10-03T10:59:00Z`，长度至少 68000ms。
这里只导入一轮话语，因此会话为 partial，开始／结束边界未知仍可入库。

```json
[
  {
    "schema_version": 1,
    "source_id": "src_demo_recording",
    "origin": "audio",
    "conversation_id": "ent_conv_demo",
    "subject_ids": ["ent_person_demo"],
    "topic_ids": ["ent_topic_bom"],
    "confidence": null,
    "priority": "normal",
    "observed_at": "2026-10-03T11:05:00Z",
    "valid_from": null,
    "valid_until": null,
    "expiry_seconds": null,
    "classification": "private",
    "audio_evidence": [{"device_id": "wearable_demo_01","recording_id": "rec_demo_20261003","segment_id": "segment-01","start_ms": 60000,"end_ms": 68000,"speaker_id": "speaker_0","transcript_version": "asr-demo-1","analysis_version": "conversation-demo-1","asr_confidence": null,"extraction_confidence": null,"speaker_confidence": null}],
    "id": "evt_conv_session",
    "type": "conversation.session_recorded",
    "source_event_id": "evt_conv_session-v1",
    "content": "合成会话：已导入一轮，边界未知。",
    "data": {"participant_ids": ["ent_person_demo"],"started_at": null,"ended_at": null,"coverage": [{"recording_id": "rec_demo_20261003","start_ms": 60000,"end_ms": 68000}],"completeness": "partial"},
    "claim_mode": "inference",
    "occurred_at": null
  },
  {
    "schema_version": 1,
    "source_id": "src_demo_recording",
    "origin": "audio",
    "conversation_id": "ent_conv_demo",
    "subject_ids": ["ent_person_demo"],
    "topic_ids": ["ent_topic_bom"],
    "confidence": null,
    "priority": "normal",
    "observed_at": "2026-10-03T11:05:00Z",
    "valid_from": null,
    "valid_until": null,
    "expiry_seconds": null,
    "classification": "private",
    "audio_evidence": [{"device_id": "wearable_demo_01","recording_id": "rec_demo_20261003","segment_id": "segment-01","start_ms": 60000,"end_ms": 68000,"speaker_id": "speaker_0","transcript_version": "asr-demo-1","analysis_version": "conversation-demo-1","asr_confidence": null,"extraction_confidence": null,"speaker_confidence": null}],
    "id": "evt_conv_turn_01",
    "type": "conversation.turn_recorded",
    "source_event_id": "evt_conv_turn_01-v1",
    "content": "合成转写：明天核对 BOM，之后与小李沟通。",
    "data": {"turn_id": "turn-demo-01","turn_index": 0,"text": "我明天核对 BOM，也想之后和小李谈谈。","speaker_entity_id": "ent_person_demo"},
    "claim_mode": "explicit_statement",
    "occurred_at": "2026-10-03T11:00:00Z"
  },
  {
    "schema_version": 1,
    "source_id": "src_demo_recording",
    "origin": "audio",
    "conversation_id": "ent_conv_demo",
    "subject_ids": ["ent_person_demo"],
    "topic_ids": ["ent_topic_bom"],
    "confidence": null,
    "priority": "normal",
    "observed_at": "2026-10-03T11:05:00Z",
    "valid_from": null,
    "valid_until": null,
    "expiry_seconds": null,
    "classification": "private",
    "audio_evidence": [{"device_id": "wearable_demo_01","recording_id": "rec_demo_20261003","segment_id": "segment-01","start_ms": 60000,"end_ms": 68000,"speaker_id": "speaker_0","transcript_version": "asr-demo-1","analysis_version": "conversation-demo-1","asr_confidence": null,"extraction_confidence": null,"speaker_confidence": null}],
    "id": "evt_conv_summary",
    "type": "conversation.summary_recorded",
    "source_event_id": "evt_conv_summary-v1",
    "content": "合成衍生摘要；只覆盖已导入的一轮。",
    "data": {"summary": "说话人提到明天核对 BOM，并计划之后与小李沟通。","covered_event_ids": ["evt_conv_turn_01"],"key_points": ["提到核对 BOM","提到后续与小李沟通"],"mentioned_entity_ids": ["ent_person_li"]},
    "claim_mode": "inference",
    "occurred_at": null
  }
]
```

本层必须保存 `evt_conv_turn_01.data.text`。只返回录音链接、上述 content 截句或 summary 不满足逐轮持久化要求。
小李只是被提及人，不在 session 的 `participant_ids` 中；speaker_0 仅为本录音的局部标签。
如果“我”无法确认身份，turn 的 `speaker_entity_id` 必须为 null，session 可登记 unresolved 参与者；不得沿用示例中的已识别人名。
若另有 BOM 承诺提取，追加独立 `commitment.created`，带 `conversation_id=ent_conv_demo`，并引用同一段音频；摘要本身不创建承诺。
若 v2 将“明天”更正为“下周”，追加同 `turn_id` 的新 turn 事件及 `event.corrected`，明确替代 evt_conv_turn_01；旧 summary 随之成为证据已变更。
之后追加引用 v2 轮次的新 summary，并显式更正旧 summary；默认列表每个逻辑轮次只显示 v2，旧事件仍按 ID 可读。

| 验收输入／操作 | 必须观察到的结果 |
| --- | --- |
| 导入上述 partial 会话 | 不等待完整录音；本地可取回逐轮 text、摘要、范围与音频证据 |
| 将小李只放进 mentioned_entity_ids | 不自动列为参与者，不生成关于小李的新事实 |
| speaker_entity_id=null 且 speaker_id=speaker_0 | 接受未知身份，保留局部 speaker，不自动归给设备主人 |
| 同一会话跨两段已登记的不同录音 | 引用同一 conversation_id，保留各段独立 recording_id／speaker_id |
| 同一录音的两段话属于不同会话 | 返回两个 conversation_id，不按录音强制合并 |
| 先导入未关联 turn，再确定会话 | 通过替代事件和更正关联；原事件不可变，默认不显示双份 |
| 重放相同 source_event_id 与载荷 | 返回原事件，不增加文本、不过期续期、不改变接收序号 |
| 未更正就提交相同逻辑 turn_id 的 v2 | 拒绝冲突；显式更正后只显示有效 v2，原文可按 ID 追溯 |
| 摘要引用空 covered_event_ids 或其他会话的 turn | 原子拒绝，不能写入无证据／跨会话摘要 |
| v1 轮次被撤回或更正 | v1 摘要标记证据变更，默认有效摘要不得继续交付 |
| 已导入 turn 的 TTL 到期 | 当前上下文排除；历史读取仍有 text 及 valid_until／expires_at，原事件未删除 |
| 查询更正前 known_at 与快照水位 | 返回当时可知 v1；当前默认读取返回 v2，审计按 ID 可追原文 |
| 只导入 session／turn／summary | State 中不新增事实或已完成承诺；默认集合排除 turn，显式读取可返回文本 |
| 文本子串检索命中某一轮 | 只返回命中且允许读取的事件，不隐式扩展同会话其他内容 |

## 10. 分类、主体范围与存储边界

所有对象默认 `private`。派生状态取贡献信息最严格分类；公开摘要不得带出私有主体或来源引用。
`classification=public` 不构成公开发布授权，不触发上传，也不改变访问控制；规范和代码公开与使用者的音频数据公开分别处理。
`subject_ids` 是相关性过滤条件，不是权限范围；匹配多主体事件可能返回其共同内容。
本规范不提供按主体实施访问控制的保证；如未来增加权限限制，必须另定义可访问主体范围与跨主体内容处理。
0.1 的单用户／本地假设不是身份认证或权限隔离声明；分类字段也不等于已经具备访问控制。

逻辑存储可包含实体目录、事件日志、来源目录、类型注册表和派生状态缓存。
建议接口为：注册／解析实体、注册类型、原子追加事件、按来源键去重、读取事件证据、按双时间重放状态。
实现可以使用关系数据库、文件日志或其他存储；唯一约束、不可变性与重建语义必须保持一致。
物理表名、JSON 存储方式、索引和事务技术由实现决定，不属于 0.1 外部契约。
