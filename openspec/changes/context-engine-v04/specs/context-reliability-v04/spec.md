# Spec Delta
## ADDED Requirements
### Requirement: 可降级语义故障分类
系统 MUST 仅把 provider timeout、连接拒绝、502/503/504 与结构化 `model_not_found` 归类为可降级 outage，其它 HTTP、协议与向量形状错误 MUST 继续失败；分类 MUST NOT 依赖响应正文字符串匹配。
#### Scenario: CE04-A1-01
- **WHEN** embedding provider 超时、连接被拒、返回 503 或结构化 `model_not_found`
- **THEN** 返回 `ProviderUnavailableError`，Reason 属于固定集合，错误文本不含响应正文、token 或 URL 参数
#### Scenario: CE04-A1-02
- **WHEN** provider 返回 400/500、畸形 JSON 或错误向量维度
- **THEN** 返回非可降级错误，Build 不把它当 outage 降级
#### Scenario: CE04-A1-03
- **WHEN** 请求 ctx 已取消或已超时
- **THEN** 原样返回 `ctx.Err()`，provider 自身 timeout 与请求取消可区分

### Requirement: 检索就绪接口显式报告错误
`SemanticRetriever.Ready` MUST 接受 ctx 并返回 `(bool,error)`；SQL 与索引错误 MUST 返回 error，只有真正空索引返回 `false,nil`。所有实现与调用 MUST 迁移，不保留旧签名兼容层。
#### Scenario: CE04-A1-04
- **WHEN** 数据库不可用、索引损坏或加载 graph 出错
- **THEN** `Ready` 返回 error，Build 标记 `readiness_db_error` 并记录 gap，而不是 `semantic_index_empty`
#### Scenario: CE04-A1-05
- **WHEN** 索引确实为空
- **THEN** `Ready` 返回 `false,nil`，Build 记录 degraded 与 `semantic_index_empty`

### Requirement: 降级不伪造成功且保留字面结果
Build MUST 在可降级 outage 时保留已有的字面、导航与确认链接结果，并设置 degraded 与稳定 `degrade_reason`、`semantic_unavailable` gap；成功但空的检索结果 MUST NOT 被视为 outage；SQL、索引解码、cosine 与 scope/ambient 过滤错误 MUST 继续失败。
#### Scenario: CE04-A1-06
- **WHEN** 语义 provider outage 但字面命中存在
- **THEN** 返回字面结果、degraded=true、`semantic_unavailable` gap，并在 rerank/assessor 调用后与返回前再次检查 ctx
#### Scenario: CE04-A1-07
- **WHEN** 搜索成功但无命中
- **THEN** 不新增 outage/degraded 标记
#### Scenario: CE04-A1-08
- **WHEN** scope/ambient 过滤或索引解码出错
- **THEN** Build 返回错误，不输出降级成功结果

## ADDED Requirements
### Requirement: 预算跳过超大候选并保持完整项
Build MUST 先把 items 装入预算再装证据窗口；超出剩余预算的单个 item MUST 被跳过并记录 `skipped_item_ids`，MUST NOT 截断字符串或把文本按相同内容合并不同来源。`max_items` 只计 items。
#### Scenario: CE04-A2-01
- **WHEN** 一个超大 note 与其后的小 turn 同时命中
- **THEN** 超大 note 被跳过并记入 trace，小 turn 正常进入结果
#### Scenario: CE04-A2-02
- **WHEN** 同一来源同时有超大 note 与小 turn
- **THEN** 小 turn 仍被接受，不因同源被去重丢弃
#### Scenario: CE04-A2-03
- **WHEN** 中文与 emoji 内容导致预算恰好用尽或差 1 字节
- **THEN** 字节口径与边界行为一致，差 1 字节即不装入

### Requirement: 证据窗口按中心优先完整装入
Build MUST 对每个被接受 item 使用 `GetCurrentEvidenceWindow` 取完整 radius=1 窗口，按 center→previous→next 尝试预算，至多 3 个成功窗口；中心缺失/被过滤/超预算时 MUST NOT 加入其邻居。证据按 TurnID 去重，相同文本不同 TurnID MUST 分别保留与计费。
#### Scenario: CE04-A2-04
- **WHEN** 窗口仅有 center 与一个邻居可装入
- **THEN** previous 优先入选，next 记入 `skipped_evidence_turn_ids`
#### Scenario: CE04-A2-05
- **WHEN** center 超预算而邻居很短
- **THEN** 整个窗口不当作该 item 的有效支持，继续处理下一 item
#### Scenario: CE04-A2-06
- **WHEN** 4 个合法 cited items 竞争窗口名额
- **THEN** 按最终稳定顺序前 3 个成功中心获得窗口，其余记录 evidence gap
#### Scenario: CE04-A2-07
- **WHEN** 不同 TurnID 携带相同文本，或多个窗口引用同一 turn
- **THEN** 文本分别保留，被多次引用只计费一次且 `UsedByItemIDs` 仅含实际引用者
#### Scenario: CE04-A2-08
- **WHEN** note 缺少预算内证据
- **THEN** 该 item 仍可相关，但不得声称 sufficient

### Requirement: 非法 UTF-8 明确失败
Build MUST 在硬 scope/current 过滤之后、预算选择之前校验候选与窗口内容；非法 UTF-8 MUST 返回 `invalid_utf8_content`，MUST NOT 静默修补或当成语义 outage。
#### Scenario: CE04-A2-09
- **WHEN** note 或 turn 文本含 `0xff`
- **THEN** 返回 `invalid_utf8_content` 错误

### Requirement: 预算 trace 向后兼容
BudgetTrace MUST 保留 `budget_exhausted` 通用 gap，并新增可选 `skipped_item_ids`、`skipped_evidence_turn_ids` 与 gap `item_budget_exceeded|evidence_budget_exceeded|evidence_missing`；纯 dedupe 省略 MUST NOT 置 Truncated。
#### Scenario: CE04-A2-10
- **WHEN** 因预算省略 item 或证据，或仅因同源去重省略
- **THEN** 前者置 Truncated 并记录细分 gap，后者不置 Truncated 也不记预算 gap

## ADDED Requirements
### Requirement: 停写备份一致性
`context-backup backup` MUST 只支持停写模式；原生模式 MUST 要求 `--confirm-writers-stopped`，并在拷贝前后比较 DB/WAL/SHM/音频完整文件清单与 hash，变化时 MUST 返回 `writers_active` 且不发布归档。归档 MUST 记录 `backup-archive-manifest-v1`（profile ID、相对路径、大小、hash、schema、音频引用状态），拒绝覆盖、路径逃逸、symlink 与绝对/`..` 条目。
#### Scenario: CE04-A3-01
- **WHEN** 备份期间 writer 仍在写入
- **THEN** 返回 `writers_active`，不生成或发布归档，原卷不被修改
#### Scenario: CE04-A3-02
- **WHEN** archive 含 symlink、绝对路径或 `..` 条目
- **THEN** 命令失败且不写目标目录

### Requirement: 恢复验证完整性与音频引用
`restore` MUST 在临时副本重放 WAL，对每个 profile/event DB 运行 `integrity_check`（唯一 `ok`）与 `foreign_key_check`，并检查所有 available 音频引用（缺文件、越界、hash 不符失败）；declared deleted/unavailable 不算缺失。
#### Scenario: CE04-A3-03
- **WHEN** 恢复的 DB 损坏或音频文件缺失/hash 不符
- **THEN** 恢复失败，报告具体失败类别
#### Scenario: CE04-A3-04
- **WHEN** 恢复成功
- **THEN** 恢复栈可检索合成内容，turn 与 audio-analysis event 片段 WAV 长度等于 `32*(end_ms-start_ms)`，遗忘后同一资源不可读

### Requirement: 遗忘屏障阻止旧备份复活
`restore` MUST 要求停写的受信 live root 作为 `--barrier-source`；archive 缺少其任一稳定屏障 MUST 返回 `backup_predates_forget`；archive 比 source 多屏障、profile 不匹配、缺 source、source 未完成 forget 或 barrier 变动 MUST 返回 `forget_barrier_unverifiable`。屏障集合为排序的 tombstone.id、fence(tombstone_id,provider_recording_id,device_id)、profile_control.tombstone_id/retired 与 retired-profiles catalog 条目，以固定字段顺序紧凑 JSON 的 SHA256 表示。
#### Scenario: CE04-A3-05
- **WHEN** 归档早于一次永久遗忘且屏障 source 已完成遗忘
- **THEN** 返回 `backup_predates_forget`，不恢复
#### Scenario: CE04-A3-06
- **WHEN** 两边屏障集合相等但任一侧 forget 未完成
- **THEN** 返回 `forget_barrier_unverifiable`
#### Scenario: CE04-A3-07
- **WHEN** 无存活可信 root
- **THEN** 工具明确不支持自动检测，该灾难恢复记 BLOCKED，不提供绕过开关

## ADDED Requirements
### Requirement: 备份工具命令与部署集成
系统 MUST 提供 `context-backup backup|restore|verify` 与 Docker 入口 `scripts/backup-profile-volume.py`；Docker 入口 MUST 先成功 stop 本 compose writer，枚举同卷其它 running 容器，存在其它 writer 时 MUST 拒绝而非替用户停止。部署文档 MUST 用受支持流程替换旧 tar 指令。
#### Scenario: CE04-A3-08
- **WHEN** Docker 同卷存在其它 running writer
- **THEN** 备份脚本拒绝执行并说明原因
#### Scenario: CE04-A3-09
- **WHEN** `verify --profiles-root` 检查恢复目录
- **THEN** 输出每个 profile 的 schema、DB 完整性与音频引用状态
