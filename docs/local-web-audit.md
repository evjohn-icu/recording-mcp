# 本地 Web / 记忆系统审计（只读代码审阅）

> 范围：当前工作树的实际源码、合成评测报告（`/tmp/recording-mcp-{head-audit-eval,current-original-fixtures-eval,current-audit-eval}.json`）及同级 `../recording-mcp-references`。未读取项目 `data/`、私密资料、凭证或环境设置；未访问外网。本文件是审计/设计建议，不代表实现已完成。
>
> 被审计仓库 HEAD：`b43125874f5be73c15d4cc648b08319a62af13eb`。工作树已有大量其他修改，本文没有更改这些文件。

## 1. 系统现状与代码入口

| 层次 | 代码入口 / 符号 | 实际职责与观察 |
|---|---|---|
| 业务 API | `internal/app/service.go: Service` | MCP 与本地 Web 共用业务 facade；DB 读取/改写具体逻辑仍由内存 store 承担。 |
| 本地 Web | `internal/web/server.go: New`, `Server.wrap`；`internal/web/handlers.go` | 注册会话、会话/turn、注释历史/编辑、音频、搜索、hybrid、topics 与 health 路由。写请求经 session、Origin、CSRF 保护；SPA 静态目录可选。Web 会话目前是本地 bootstrap token 换取的会话，不等于多租户身份系统。 |
| 原始记忆 | `internal/memory/retrieval.go: SearchMemory`, `searchNotes`, `searchTurns` | 字面不区分大小写的子串检索；默认排除 superseded 与 ambient 内容，可显式包含。排序是 note 优先再按 ID，而非相关性排序。 |
| 混合检索 | `internal/contextbuild/build.go: Request`, `branchWeights`, `Build` | 词面、可选 semantic、导航、确认关系分支融合；带证据/预算/coverage，reranker 只能重排。Hybrid 模式是默认值，navigation/link 并不会因关闭 semantic 而一同关闭。 |
| 向量 | `internal/semantic/provider.go: HashProvider`, `OpenAICompatProvider`; `index.go: Indexer`; `search.go: SQLiteRetriever` | 默认配置是无模型、无权重的 `hash-ngram-v1` 确定性 token/ngram 特征；embedding blob 存在 SQLite 普通表，检索逐条解码后算 cosine。`OpenAICompatProvider` 是另一条显式配置路径。当前不是 ANN 索引。 |
| 事件平面 | `internal/plane/plane.go: Event`, `Open`, `Append`, `normalize`；`internal/mcpserver/server.go: Register` | SQLite append-only event archive，ID 重放幂等、冲突拒绝；支持 commitment lifecycle/feedback 的确定性投影。Event 同时有 `Timestamp`、`ObservedAt`、`ValidFrom/Until`、TTL，表达了部分 event-time/observation-time 语义。 |
| ASR | `internal/asrstream/client.go: SessionConfig`, `Transcribe` | WebSocket PCM 分帧、最终负序号结束帧、响应收集；`PacketInterval < 0` 按代码的 `if cfg.PacketInterval > 0` 跳过 pacing。此值是发送节奏配置，不是识别质量的负面指标。 |

### 现存问题（源码/报告可直接验证）

1. **无答案的 hybrid 召回造成误报。** 当前 synthetic audit 的 `unanswerable-nowhere`（query `量子纠缠实验室`）在 lexical 模式返回 0，而 hybrid 返回 7 条。报告对 3 个 unanswerable 问题合计记了 15 个 hybrid false-positive items，`unanswerable_clean_rate=0.6667`；这是需要独立计分的无证据误召回问题，不能用 evidence recall 掩盖。`contextbuild` 固定 branch weights 本身不是校准过的置信度，候选分支任一命中即可进入候选，再经 byte/item budget 选择；没有问答生成模块，因此这里只评价检索空结果，不评价模型拒答。
2. **所谓语义能力需准确命名。** 默认 `HashProvider` 只做 token/CJK bigram 的 FNV-1a 映射和归一化；可重复、可算 cosine，但没有训练语义空间，碰撞也不可避免。把 `semantic_assisted_recall` 当成已测得的通用语义理解或把 hash 当作学习到的 embedding 都不成立。即使开启 OpenAI-compatible provider，也需按对应模型/数据另行实测。
3. **文本检索质量有结构性限制。** `SearchMemory` 是全量收集字面子串后按种类/ID 排序，适合作为精确兜底而非相关性 ranker；常见词会产生很多看似有效的候选。Hybrid 的检索质量应看负例与相关性，而不只是证据 recall。
4. **时间字段虽存在，事件平面的隔离/删除尚不完整。** `plane.Event` 没有 tenant/user/owner scope 字段；`Repository.Events()` 全量取出事件；表有禁止 update/delete 的触发器。保留不可变审计记录有价值，但现有模型没有 forget/tombstone 事件及其向 derived/semantic/search 索引传播的契约。不能据此声称支持用户级删除。
5. **多用户范围不能只靠 Web 登录态。** memory 的 conversation ID 过滤与单用户部署可用，但事件 archive 的接口是全局 `Events(ctx)`，Event 也无主体 scope；需明确 tenant 边界贯穿 DB 主键、查询、embedding、缓存、音频、导出与后台任务，避免一个 transport 的认证误被当成存储隔离。
6. **向量当前是精确线性扫描。** `SQLiteRetriever.Search` 对 SQL 返回的所有符合 provider/model/dims 的候选逐行 Decode/Cosine，再排序截断。小型本地 corpus 简单可靠；规模上来后成本随候选数线性增加。`sqlite-vec` 是 pre-v1 扩展，且 Go 工程使用 `modernc.org/sqlite`（纯 Go SQLite 驱动）；不能假设 C 扩展能直接通过当前 driver 的连接加载、版本/ABI 与构建配置兼容。尚未看到集成或基准证据。

## 2. Synthetic 评测：测量值与不可推出的结论

以下数值是读取本地已有 JSON 报告得到的，不是本次运行测试的结果，也不是线上/自然流量结论；各报告所用 fixture 集不同，数值不能当作严格同基准前后对照。

| 已有报告 | corpus / questions | 实测字段（报告值） | 限定解释 |
|---|---:|---|---|
| `/tmp/recording-mcp-head-audit-eval.json` | 24 segments / 42 questions | evidence recall@5 `0.9286`；lexical `0.75`；hybrid `1.0`；semantic-assisted `0.25`；stale leakage `0`；model calls `0`；平均延迟 `2.095ms` | 这是旧报告配置；不能据此说 semantic 是训练模型，也不是 wall-clock 用户体验 SLA。 |
| `/tmp/recording-mcp-current-original-fixtures-eval.json` | 24 / 42 | 与 head 报告主要指标相同；unanswerable questions `0` | 此集合没有负例，不能验证拒答质量。 |
| `/tmp/recording-mcp-current-audit-eval.json` | 32 / 46 | evidence recall@5 `0.9348`；lexical recall `0.6667`；hybrid recall `0.6667`；semantic-assisted `0.1667`；3 unanswerable；hybrid false-positive items `15`；unanswerable clean rate `0.6667`；stale leakage `0`；model calls `0`；平均延迟 `3.109ms` | 多出的 fixture/question 改变集合；具体 `unanswerable-nowhere` lexical 0、hybrid 7。这个负例应作为门槛回归测试。 |

投影报告里的 `evidence_valid=1`、`stale_not_current=1`、`superseded_not_current=1`、`conflicting_honest=1`、`deterministic=true` 是该报告的合成 harness 对有限夹具的断言/计数，不是全量正确率。`latency_ms_*` 是本地 harness 的计时样本，非生产性能承诺。报告中的召回命中也不等于最终回答正确、证据充分或无泄漏。`semantic_assisted_recall` 只是 harness 按分支归因统计到的辅助比例，不意味着向量表示具有语义理解能力。


## 3. 参考项目映射（SHA、许可证与可借鉴边界）

SHAs 是本地 checkout 的 `git rev-parse HEAD` 实际结果；许可证以各仓库根目录的 license 文件名/正文标识为准。映射只总结读到的少数源码锚点，不代表完整产品审计或依赖关系。

| 项目 | 本地 HEAD SHA | 根许可证 | 代码锚点及可借鉴处 | 不应直接推断 |
|---|---|---|---|---|
| **omi** | `3df13eb507e18964317dab4e3c71fef0cedc4088` | `LICENSE` — MIT | `backend/services/conversation_keyframes.py` 中的 conversation 时间窗口/设备字段与持久化 job 体现了捕获来源上下文与生命周期锚点；可参考设备/会话范围设计。 | Omi 设备/应用体系不等于这里的本地 event schema；不能照搬其多服务架构。 |
| **memos** | `ccc1f82393ab50d2c8fea40865d8c2fbe05472a8` | `LICENSE` — MIT | `store/memo.go: Memo`, `MemoWritePolicy`, `MemoAccessScope`；`store/memo_relation.go: MemoRelation`。有作者、visibility、space 与写策略的显式模型，可供多用户授权边界参考。 | Memos 是协作笔记产品，不是带 ASR 时钟/事实有效期的记忆系统。 |
| **hindsight** | `eb104868f23068b480d907d4a6911f3102b30da2` | `LICENSE` — MIT | `hindsight-api-slim/hindsight_api/engine/document_history.py: DocumentEventKind`, `DocumentEvent`, `deletion_events`；删除事实时同时记录事实与 observation 的移除影响，是 tombstone/cascade 可追溯性的具体范例。 | 这里只能证明审计事件与影响计数/采样；不能保证其所有后端删除语义符合本项目要求。 |
| **graphiti** | `8e320f880e7d169a907dbf2a7ac4188586c03b55` | `LICENSE` — Apache-2.0 | `server/graph_service/routers/retrieve.py: search`, `get_episodes`：search 明确接受 `group_ids`，episode 查询也以 group ID 限定。可参考把 scope 作为检索输入的做法。 | 图检索结果并非天然时间正确或 tenant 安全；仍需本地过滤与证据校验。 |
| **LongMemEval** | `9e0b455f4ef0e2ab8f2e582289761153549043fc` | `LICENSE` — MIT | 根 `README.md` 描述信息提取、多 session 推理、知识更新、temporal reasoning、abstention 五类能力，并单列 abstention 与 evidence session 标注；可据此扩展负例/时序评估设计。 | 本项目小型合成报告不是 LongMemEval 成绩；没有下载/运行它的数据集，也未访问其 `data/`。 |
| **sqlite-vec** | `04d28bd21773981e2d266bbf6aa4efbd011eb4f6` | `LICENSE-MIT` / `LICENSE-APACHE` — MIT / Apache-2.0 | 根 `README.md` 的 `vec0` virtual table 与 `MATCH`/distance KNN 示例，可作为未来候选索引方向；README 明确标注 pre-v1、可能 breaking changes。 | 不能认为它已接入本项目、支持 modernc 当前构建或提升过本地数据集指标；必须做 driver/extension 兼容验证。 |

## 4. 下一阶段建议设计（非当前实现）

### A. 统一 Event 与时间语义

- 以 `plane.Event` 为事实输入契约，保留 `Timestamp`（事件发生/业务时间）与 `ObservedAt`（系统获知时间）分离；明确 `ValidFrom/ValidUntil` 是事实有效区间、`TTLSeconds` 是存储/检索政策还是有效期，避免两者混用。
- 增加不可变 `EventID`、`TenantID`/`UserID`、`ConversationID`、`Source`、schema/version、幂等键与稳定 provenance；敏感数据不要直接塞入不受限 `StructuredData`。时间比较、迟到反馈、as-of 查询分别用事件时间与观察时间定义并测试边界。
- 将用户/agent feedback 继续建模为追加事件，而不是改写历史：completed/cancelled/deadline.changed 应显式 target、actor、observed time、effective time、来源和冲突策略；投影需可重放，反馈不应倒灌到其 `Timestamp` 之前的 as-of 视图。现有 lifecycle 部分测试已示范幂等与历史查询，需推广到所有反馈类别。

### B. Forget / tombstone 与 cascade 同步

- 定义 `forget.requested`、`forget.completed`/`forget.cancelled` 之类不可变删除意图，携带主体范围、目标资源、request ID、时间、策略与审计关联；在权限验证后生成 tombstone，而非物理篡改 append-only 原始事件。
- 投影规则让 tombstone 默认压制所有之后读取：notes/facts、evidence links、topic/graph edges、FTS、embedding、缓存、摘要、导出与音频引用。每条派生记录应记录来源 IDs，删除用这些边做级联重算/清除；stale/tombstone 在所有 retrieval branch 统一过滤。证明原始数据是否需要加密擦除/物理 purge，应由产品/法务保留策略决定，不能仅靠逻辑 tombstone 声称合规删除。
- 建立 transactional outbox/同步游标或等价可恢复机制：原始事件提交与待处理投影任务原子关联；消费者幂等、按主体有序、可重放；状态暴露 `pending/complete/failed` 与每个下游的 ack。断网/进程中止后最终一致恢复，并确保 tombstone 在恢复期间仍然 fail closed。
- 添加 synthetic 用例：对单条事实、整段 conversation、用户全量忘记；并发/重复请求；派生 topic/关系和 embedding 清理；暂停下游后重启；旧快照、缓存、导出及 semantic/hybrid 检索均不得复活已删除内容。

### C. sqlite-vec 与 modernc 的兼容判定

- 当前 `go.mod` 是 `modernc.org/sqlite v1.60.1`，向量存储为普通表+Go cosine。先制作独立 spike：记录 modernc SQLite 版本/编译选项、extension loading 是否受支持且默认禁用、sqlite-vec 编译产物平台矩阵、初始化/关闭/迁移/备份行为；在各受支持 OS 上跑真实 `vec0` create/insert/KNN 与重开 DB 测试。不能只因 `database/sql` API 一致就认定 loadable extension ABI 兼容。
- 保留现有 `SemanticRetriever` 接口与 exact cosine 作为 fallback/oracle；用同一 synthetic fixtures 对比 top-k、过滤、并发、升级、空索引、损坏向量与 forget pruning，检查 ANN 召回差异及 P50/P95、DB 大小。要明确预-v1 依赖锁定与升级/回滚路径；没有收益证据前不要替换可靠路径。

### D. 多用户 scope 作为存储不变量

- tenant/user scope 必须进入 event、memory/artifact、embedding、topic relation、缓存 key 与 job payload；DB 查询默认强制 scope，不能由调用方漏传后变成全库读取。复合唯一键至少在 scope 内唯一；跨用户查询应 fail closed。
- 认证身份由服务端 session 推导，不能信任客户端任意提交的 `UserID`。授权检查需要贯穿写入、搜索、feedback、forget、音频路径解析、导出与后台 worker；graph 参考的 `group_ids` 可作为显式硬过滤思路。
- 添加两个 synthetic 用户的同名数据隔离测试，覆盖 raw/derived/vector/hybrid/graph、分页游标、缓存、链接、feedback target、删除、音频、重启与 sync replay。验证所有分支 scope 无旁路，再讨论单用户本地部署的默认便利性。

## 5. 本次变更与验证边界

本次只新增 `docs/local-web-audit.md`。没有修改源代码、测试、fixture 或报告，没有运行测试命令；上述 PASS/FAIL 仅是已读取报告内的数值或源码事实，不是本次实现/测试通过声明。评测合成数据本身显示明确 FAIL：`unanswerable-nowhere` hybrid 返回 7 项；请后续负责检索实现的 agent 与本审计对接拒答门槛、tombstone 及 scope 接口契约。

> 许可证只是根文件的识别，不是法律审查；若复用任何实现/代码，还应检查子目录依赖许可、NOTICE/版权归属及分发义务。

## 修复前复现的缺陷（历史记录，最终状态见验证记录）

| 状态 | 文件 / 符号 | 用户影响 |
|---|---|---|
| FAIL | `internal/memory/revisions.go: replaceNoteDerivedState` | 编辑同会话另一条归纳会复活显式 stale 的旧归纳；`TestEditNoteDoesNotReviveExplicitlyStaleSibling` 已复现。 |
| FAIL | `internal/projection/projection.go: Build` | conversation 范围之外的注释进入 unresolved 与 NotesScanned；`TestConversationScopeExcludesOtherConversationNotesFromAudit` 已复现。 |
| FAIL | `internal/app/service.go: GetItem`、`EvidenceContext` 与 `web/src/components/EvidencePanel.tsx` | 详情 item 不返回 audio_status，证据面板的回退查询无法获知可用音频；合成音频浏览器验证未出现播放按钮。 |
| 尚未修复 | `internal/app/audio.go: resolveAudioPath` | 路径前缀检查后 os.Stat 会跟随符号链接，不能作为目录隔离保证。 |
| 尚未修复 | `internal/web/server.go` 会话 bootstrap、Origin 校验 | bootstrap 与常规写接口防护不一致，Host 也需独立校验。 |

上述缺陷已在后续实现中修复并补充回归。保留此表作为修复前证据，最终状态以 [验证记录](local-web-verification.md) 为准。

## 本轮采用判断（机制，不复制参考实现）

| 当前问题 | 参考机制 / 实际源码 | 我们的落点 | 收益 / 代价 | 决定 |
|---|---|---|---|---|
| 归纳难以回到原话 | Omi `conversation_section_sources.dart` 的 segment 来源、`edit_segment_sheet.dart` 未保存保护 | 搜索 item 深链、证据面板、版本冲突保留草稿 | 可核对；需维护稳定 turn ID 与修订 | 采用，证据音频链路已由 Chrome 验证 |
| 翻页失败与首屏失败混淆 | Memos `web/src/components/PagedMemoList/` 首次/追加加载状态；`server/api/memo_access.go: newMemoAccessScope` | 时间线/搜索分页、过滤先于计数与 LIMIT | 重试不丢列表；需游标与请求关联 | 采用，后端 scope 已修复并补回归 |
| 多路召回结果难解释 | Hindsight `search/fusion.py: reciprocal_rank_fusion(k=60)`、`interleave_fusion` 及各路 rank | 保留现有融合，trace 放诊断层 | 可比较算法；需要同语料对照才能决定 | RRF 暂缓，无可证明收益 |
| 来源修订与现实变化混淆 | Graphiti `edges.py: EntityEdge` 的 created_at/valid_at/invalid_at/expired_at；temporal_edge_utils 时间规范化 | 复用 plane.Event 时间字段，设计来源修订与现实有效期关联 | 避免第三套生命周期；迁移与重放较复杂 | 本轮只设计 |
| 无证据问题被普通 recall 掩盖 | LongMemEval README 明确从 retrieval 正例评分排除 30 个 abstention 样例 | 显式无证据标记与独立误召回计分 | 正例 gold 校验不放宽；需修正错误 ASR 标注 | 采用，修订计分已完成；不验证模型拒答 |
| 向量线性扫描的规模成本 | sqlite-vec vec0 KNN、Go 集成采用其他驱动/构建路径 | 保留 modernc 与 exact cosine | 避免 CGO/驱动迁移；大规模性能仍待测 | 暂缓，语义质量与查询性能分别评测 |

## 最终实施状态与落点

| 状态 | 模块 / 符号 | 结果与用户影响 |
|---|---|---|
| PASS | memory.ApplyAnnotations / replaceNoteDerivedState；app.EditNote | 保留 stale 状态、完整视图替换、expected_version；同事务失效 note 向量并排队，兄弟条目保留。 |
| PASS | app.GetWebItem / WebEvidenceContext / TurnPage；web.handleItem / handleEvidence | 共享来源解析与当前状态，返回真实音频状态及当前修订确认身份；转写按 recording 分页。 |
| PASS | contextbuild.Build / requestScope.allowsEvidence；projection.Build | 内嵌和展开证据过滤，计数/unresolved 先应用范围；带时间筛选无嵌套连接死锁。 |
| PASS | semantic.Indexer / SQLiteRetriever.Search | 原子条件写入当前文本向量，检索核对内容 hash，旧任务不能覆盖新内容。 |
| PASS | eval.evaluateQuestion / countScopeEscapes | 混合旧证据、证据范围与无效引用独立检查；无证据按模式计分。 |
| PASS | web.wrap / originAllowed；context-web.validateListenAddress；app.AudioClip | loopback、Host、Origin、CSRF；按授权 recording 映射，根目录安全打开 PCM。 |
| 已实现，复用 | pipeline extract/publish、process 队列、cloudsync.handleDeleted | 保留幂等/恢复；没有提供误导性的永久忘记按钮。 |
| FAIL，已知限制 | 实验 hybrid 无证据召回 | 同语料前后仍误召回 7 项；默认词面路径空结果正常。 |
| NOT_RUN | 外部 Provider、真实音频、云部署 | 没有调用或部署，不由合成测试推定通过。 |

未来时间模型以现有 plane.Event 的 ID、Timestamp、ObservedAt、ValidFrom/Until 和反馈事件为基础，
增加到 recording/attachment revision 的显式来源关联，不另起第三套 lifecycle。来源修订只影响来源有效性；
现实事实变化使用有效时间区间与系统观察时间。永久遗忘需要数据物理清除、派生级联和同步 tombstone，
不能只在 GUI 隐藏或在 append-only 事件流追加删除意图；重同步必须先检查 tombstone。
