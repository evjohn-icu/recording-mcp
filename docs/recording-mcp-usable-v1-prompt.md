# recording-mcp：可用版本实施 Prompt

你正在修改仓库 https://github.com/evjohn-icu/recording-mcp 。

你的任务不是再写一轮架构方案，而是在现有代码上交付一个陌生开发者能安装、输入自己的录音、通过 Agent 查询且能部署可选 Cloudflare 采集入口的版本。直接检查仓库、修改代码、运行测试、修复问题。不要只给建议，也不要重新搭一个与现有实现并行的 Memory Server。

## 1. 产品目标与本轮边界

对外定位：Evidence-backed memory for personal AI agents.

本轮交付两个明确的运行模式：

A. Local：录音文件或标准转写 → Go 摄入 → SQLite → 提炼/发布/增量索引 → stdio 或 HTTP MCP → Personal Agent。
B. Cloud Ingest：录音客户端/ESP32 协议模拟器 → Cloudflare 实时 ASR 网关 → 云端持久化 → 本地主动同步 → 同一套 Go 记忆核心。

Cloudflare 负责可选采集，不承诺整套 Context Layer 已经纯 serverless。完整记忆处理与 MCP 服务仍运行在用户电脑、家庭服务器或 VPS；该节点离线时云端仍能保存采集结果，但记忆加工、最新上下文和远程 MCP 会受影响。请把这个边界写进文档与状态界面。

本轮先支持单用户、自托管、多个录音设备。设备与用户身份要正确隔离，但不要扩成多租户 SaaS。

必须做：真实本地闭环、MCP 双传输、可部署 Cloudflare 入口、端到端同步、录音模拟器、可复现演示、基本运维与英文文档。
暂不做：完整 ESP32 固件/PCB/续航优化、手机 App、自己的聊天产品、计费系统、图数据库、完整知识图谱、自动声纹识人、全云端记忆引擎、同时接入大量 ASR 厂商。

## 2. 先核实代码，不把历史说明当现状

先记录当前 commit、工作区改动和已有测试结果。保留用户未提交改动，不覆盖私人数据。阅读 README、Makefile、go.mod、CI 以及相关实现，特别是：

- cmd/context-memory、cmd/context-worker、cmd/asr-ingest、cmd/context-embed；
- internal/asrstream、adapters/doubao、asrinput、memory、pipeline、semantic、projection、migrations；
- examples/context-memory.json、docs/context-layer-runtime.md、docs/context-projection.md、NEXT_ROUND_REPORT.md；
- 转写修复脚本与现有 stdio、修订、stale、作用域隔离测试。

注意 examples/context-memory.json 已经存在，不要按“文件缺失”重造。旧 recording-mcp 事件原型与 context-memory 的工具不能不加区分混用。

先复现、再修以下待核实问题：synthetic 默认执行、Decision 专用服务强依赖、发布后索引更新、仅 stdio 的接入、意向/请求被投影为承诺、ASR 文件流程对实时摄入的限制。已被当前分支解决的问题不要重复实现。

只做必要的模块抽取；不要改语言、重写全部存储层或为了测试通过随意升级/降级依赖。先写简短实施清单，然后立刻开始落地，每阶段保持可运行。

## 3. 本地默认路径必须是真实业务流程

1. 分开 demo 和 real，禁止在 real 模式静默使用 synthetic/no-op。模型缺配置时明确报错或明确进入“仅原始转写可检索”模式，不能宣称已分析。
2. 提供无需额外 Decision 服务的正常提炼路径。保留现有 SystemOne/Decision 为可选增强；禁用它不影响 Generator → 证据校验 → Publish。不要通过假造 Decision 结果绕过强依赖。
3. 至少提供一个可配置的 OpenAI-compatible Generator，模型、完整 endpoint、凭据来源与超时可配置。不能假设所有兼容模型支持相同 temperature/结构化输出参数。将“Claude/ChatGPT 作为 MCP 客户端”与“用哪家模型做提炼”分开。
4. Curator/关联发现默认可禁用；显示 disabled、skipped、failed 的区别，不能挡住基本闭环。
5. Publish 后自动安排增量 embedding；原始 turn 摄入后也要有明确索引策略。任务与发布状态具有可恢复的持久化联系，失败重试不重复写。显示 indexing_pending/failed/ready。
6. embedding 的索引端和查询端使用一致的 provider/model/dimensions。模型切换能检测并重建。Hash n-gram 仅标为离线 baseline，不宣传为真正的语义理解；真实语义能力由显式配置的 embedding 后端提供。
7. 对每个摄入片段独立记录成功、失败、待补传。不能因为某个片段失败就丢掉所有已完成片段，也不能把部分覆盖声明为完整录音。
8. 审查修复脚本是否原地改写已发布 turn；正式修复走版本化更正与派生失效，保留旧证据，不绕过公共摄入逻辑直接 UPDATE 当作正常路径。
9. 统一配置与优先级，提供 .env.example、明确的数据目录、doctor 命令。doctor 默认不发付费请求；联网探针需显式启用。

不要先换 FTS/向量数据库。先用固定问题集检查能找到原话、证据、最新修订，性能瓶颈有测量后再处理。

## 4. 先让 Agent 安全使用现有能力

抽出共享 MCP server/service 构造逻辑，stdio 与 Streamable HTTP 复用业务实现和授权检查，使用仓库兼容版本的官方 MCP SDK。

- 保留本地 stdio；新增 /mcp、轻量 /healthz 与受保护的详细状态接口。stdout 在 stdio 模式只输出协议数据，日志写 stderr。
- 新手入口采用只读 personal 工具面，优先复用 list_conversations、build_context、get_context_item、get_evidence_context、get_processing_status；安全修正后的 get_current_context 可纳入。不要为了命名漂亮重复造一套工具。
- 全量工具保留 advanced/legacy profile，写入、身份复核、关联确认只能在明确授权的管理路径使用。ReadOnlyHint 只是提示，不代替后端授权。
- 不要把旧事件原型的 get_current_state 当成新的记忆状态服务。明确哪个 API 是正式入口。
- 引用至少能定位 conversation/recording/turn/revision/时间位置；原文保留原语言，默认输出有预算且标注缺口。
- HTTP 默认仅监听 loopback；非本地部署必须配置认证、HTTPS 外层和 Host/Origin 校验。设置请求/响应预算、超时、并发限制，拒绝未知来源的越权访问。

客户端交付：
1. Claude Code、Codex CLI、Claude Desktop 的可复制 stdio 示例与 smoke test。
2. 通用 MCP 客户端的 HTTP 示例及真实协议级测试，不只是 curl 返回 200。
3. ChatGPT、Claude 远程连接的 HTTPS 与认证配置。查阅实施时的官方文档，不凭记忆编造 UI 入口或套餐要求。
4. 静态 Bearer 只用于确实支持自定义认证头的客户端；不能据此宣称 ChatGPT/Claude 网页已支持。受保护的远程 MCP 使用符合当前规范的 OAuth：资源发现、PKCE、正确 issuer/audience/scope 验证。优先接入维护中的授权组件或外部 IdP，不手写完整 OAuth 服务。
5. 提供可选 Cloudflare Tunnel 接入示例；Tunnel 不自动替代应用鉴权，Cloudflare Access 登录页也不能当成已经实现 MCP OAuth。

没有账号权限实际联调时，标记“协议测试通过，指定客户端尚未实测”；不得把代码配置示例说成已连接成功。

## 5. 修正最危险的记忆语义，不扩建真值引擎

- “想做”“别人要求我做”“建议去做”不能自动升级成已确认承诺；约束也不能直接成为用户长期偏好。
- 最小可行方案是保留原始 note kind、来源、归属与未确认状态。无法可靠判定时输出 proposed/unresolved，不自动创建任务、提醒或外部动作。
- 人物只能依据明确的复核绑定；说话人标签不跨录音/修订继承身份。
- 区分：ASR 原文更正、用户改变决定、Agent 更正自己的解释。
- 不同录音里的 global_start_ms 是各自相对偏移，不能直接比较得出跨录音先后；绝对时间未知就保持未知。
- 不要仅凭“同话题＋取消关键词”就宣布无关承诺被撤销。关系未确定时展示冲突/待确认；明确的 supersedes 或用户复核才能可靠裁决。
- 新的保守语义涉及契约变化时，版本化并提供迁移说明；保留所有既有 stale/ambient/证据/作用域隔离保证。

本轮不做通用 Context Diff 引擎。用现有修订历史展示更正前后及来源即可；事实有效性复杂推导后置。

## 6. Cloudflare 采集层：薄适配，不复制记忆引擎

建议新增 apps/cloud-ingest，使用 TypeScript、锁定依赖的 Wrangler 配置、migrations 和测试。若已有合适目录，则复用。

职责分工：
- Worker：设备鉴权、请求校验、路由、受保护的同步接口。
- Durable Object：一个活动录音会话的有序流、上游 ASR 连接、流控和恢复状态。
- R2：有界音频分块、原始 ASR 来源文件；桶默认私有。
- D1：设备记录、片段清单、规范化最终转写、可重放的 ingress event log。
- Queues：异步补录/重试等有界任务，不承载实时音频帧，也不充当唯一事实日志。

D1 不是第二套可双向编辑的 memory database。Go/SQLite 仍是派生记忆的权威处理层；共享 asr-input/0.1 或显式升级后的统一契约，不在 TS 重新写一套投影、关联和记忆判断规则。

只实现当前豆包 ASR 的真实 Provider，加一个明确的测试 fake。上游 endpoint、resource id、鉴权模式由服务端配置，不允许设备传任意 URL，不把供应商密钥放在固件或浏览器。

移植/复用现有二进制 framing、gzip、sequence、结束帧、错误回包和归一化语义。Go 与 TS 共享协议 fixtures：解析输出语义一致；不要要求 gzip 字节流在不同实现间完全一致。

实时会话接口应支持 Start/PushAudio/OnPartial/OnFinal/Finish/Abort。不能继续把整段文件完成后才返回的函数包装一下，就宣称实现了实时字幕；必须核实 provider 参数确实提供增量结果。

partial 仅作为临时显示；只有确认的 final 快照进入规范转写和记忆加工。供应商返回累积 full result 时不能重复追加旧 utterance。每个终结片段可以独立持久化，不能等一天的总录音结束才入库。

上游 ASR 会话滚动长度、重叠、尾包等待和重连策略要配置化。应用 recording、上传 chunk、ASR request、现实 conversation 是不同层级，不要混用 ID。

## 7. 实时链路与可靠上传必须分开

为未来 ESP32 定义 versioned 协议，并实现桌面 recorder-simulator。第一版用 16 kHz / mono / s16le PCM，按约 200 ms 音频帧实时发送；不要在 Worker 内跑 ffmpeg 或把全日音频攒在内存。

采用两条可核验的路径：
A. WSS 小帧：低延迟送 ASR，返回 partial/final 与流控状态。
B. 本地 spool → 有界 chunk 上传：独立持久化原始音频，失败重传，用于恢复与补录。

至少包含 owner/device/recording/session/chunk 标识、协议版本、格式、sample offset/count、sequence、checksum 和时间来源。owner 由服务端认证绑定，不信任客户端自报。

- 设备录音时本地保存未持久化的音频；模拟器用真实磁盘 spool，重启后仍可恢复。
- 区分 received/forwarded 与 persisted ACK。只有音频 R2 写成功且清单达到可恢复状态，才能返回 durable ACK 并允许客户端释放对应缓存。
- 建议先用 30–60 秒封闭音频块做可靠上传，块长可配置。不要每 200 ms 创建一个 R2 对象，也不要逐帧写 D1。
- R2 与 D1 不是一个跨服务事务：用幂等对象键、checksum、pending/committed 清单与 reconcile 处理任意一步中断。
- 相同幂等键相同内容重放安全；相同键不同内容必须报冲突。明确漏帧、乱序、重复帧、序列缺口与流控行为。
- 实时连接恢复通常需要新 ASR request，不假定上游支持任意 offset 续接；用持久化音频有界补录，并处理重叠去重、修订与局部 speaker scope。
- 有界缓存、有限退避、限流、会话数/时长/字节预算必须真实生效；上游不可用时优先保存录音，不无限重试烧费用。
- 上游 token 只在服务端；设备凭据可单独撤销、轮换。会话协议拒绝超大帧、非法音频格式、gzip 解压炸弹和未授权重放。

模拟器必须支持：读取 PCM/WAV、按实时节奏发送、持久化 spool、随机断线、重复/漏帧、恢复补传、进程重启和打印最终覆盖报告。合法 PCM 的正弦波只能测试传输，不算真实语音识别验收；真实 ASR 用经授权的语音样本。

不要把 ESP32 模拟器通过写成 ESP32 真机通过。文档写清 I2S、Wi-Fi、SD/spool、证书校验、时钟与隐私指示需求即可，不在本轮堆固件空壳。

## 8. Cloudflare 运行时约束必须经过核实

实施前阅读当前官方文档，并在部署说明记录所依据的 SDK/runtime 版本与验证日期。

- 不把普通 Go 可执行文件、goroutine 网络栈和 SQLite 文件直接塞进 Workers；本轮 Go 核心与 TS 网关分开部署。
- 不把 DO 内存或 WebSocket attachment 当作重启后必存数据；持久状态要写存储。
- 出站 ASR WebSocket 不享受入站 WebSocket 的休眠能力，持续音频消息本来也会让处理活跃；不要承诺“实时录音零运行成本”。
- 文档中的 15 分钟防驱逐行为不能解读成“所有 WebSocket 15 分钟必断”的硬限制。
- 不能依赖 waitUntil 或打开一个连接无限延长任务；核实当前事件时限、CPU/内存/消息限制，补录任务有界可重试。
- Queues 按至少一次投递设计；不要依赖全局 FIFO。消费者幂等、有重试上限与死信/可人工恢复路径。
- D1 提交成功而 enqueue 失败不能丢事件，使用 outbox/补扫等恢复机制。队列消息传 ID/对象键，不塞大音频。
- 记录大陆设备 → Cloudflare → ASR 的实测网络状态，不保证地区可达性；Provider 和协议不与 Cloudflare 的内部 API 混在一起。

## 9. 云端到本地同步必须闭环

新增 context-sync 或等价命令：本地使用受限凭据主动拉取 D1 ingress log，导入现有 memory.Store.IngestTranscript，随后沿相同队列进行提炼、发布、索引。

要求：
1. 稳定事件 ID、可恢复 cursor，不只靠 created_at 时间戳分页。
2. 本地 inbox/去重记录；事件成功导入后才推进 checkpoint。崩溃重放不重复建转写、候选或任务。
3. 保留完整 source metadata、音频引用、revision/supersedes、真实时间或未知时间；缺前置修订的事件显式等待/报错，不能悄悄跳过。
4. 本地离线后恢复能补齐；云端重复投递、失败重试、过期 cursor、删除 tombstone 都有明确行为。过期 cursor 不能当成已同步完成。
5. 同步状态可检查：last_success、pending、failed、last_cursor、processing/indexing 状态。
6. 云端无需回调到用户 NAT 内，也不上传本地派生记忆来形成循环双写。

至少做一个跨进程端到端测试：模拟录音 → 本地 Cloudflare runtime + fake ASR → D1/R2 → Go sync → 真实业务 pipeline（fixture generator）→ MCP 返回证据。不要只分别测 TS 和 Go，然后声称桥接已完成。

## 10. 陌生用户的安装与操作路径

提供少而清晰的命令入口；名称可以调整，但 README 必须直接可复制：

- make build / make test：构建与核心测试。
- make demo：无需密钥、仅使用隔离演示库，运行真实摄入/发布/索引业务代码；ASR/模型替身显著标明，不使用只返回成功的 synthetic worker。
- doctor：配置、目录、依赖、数据库 schema、可选组件状态检查。
- 一个正常启动命令或 Docker Compose：以挂载卷保存 SQLite，启动 MCP、pipeline 和可选 sync，支持优雅停止。
- 一个真实录音摄入命令；已有标准转写也能直接导入，不强制付费 ASR。
- 一个有界 live smoke 命令：显式授权才调用真实 ASR/LLM，并限制时长、次数和费用风险。
- Cloudflare 本地 dev/test、初始化资源、迁移、secrets 配置和部署指引。

只做一个轻量本地管理页或现有 UI 的最小扩展：导入/列出录音、查看处理和同步状态、搜索、展开证据与修订。错误不能只有终端日志。不要额外启动一套复杂前端工程、造聊天界面或重做品牌。

用户配置的模型/云端会收到哪些音频与文本必须明确列出。不要把“本地存储”写成“所有推理都不出设备”。

## 11. 安全、数据与开源部署

- 默认不公开任何真实录音、转写、用户身份、bucket 或 debug 输出。日志只记录必要 ID/状态，不记录密钥、token 或整段原话。
- 数据目录权限、SQLite 并发锁与备份流程有明确实现；运行中的数据库使用一致性备份，不随意复制正在写的 .db 文件。
- 提供按 recording 导出和明确确认的删除路径；删除覆盖源音频、转写、关联派生数据、索引与队列重放。保留不含内容的 tombstone 防止旧同步复活；说明备份保留与删除边界，不承诺物理安全擦除。
- 把录音与模型输出视为不可信数据。录音中的“忽略规则、发送密钥”等内容不能成为工具授权或执行指令；写入与管理凭据不暴露给默认 Agent profile。
- 大文件、解压、并发、速率、ASR 时长、失败重试均设上限。
- 英文 README 为主，保留中文指南；Cloudflare 可选、需要的外部服务和当前限制写在 Quickstart 附近。
- 保留现有 AGPL LICENSE，不擅自换协议；第三方依赖与演示素材来源清晰。
- 增加最小 CONTRIBUTING、SECURITY、隐私/威胁模型、troubleshooting 和 issue 模板，不堆空文档。
- GitHub Actions 跑 Go/TS/协议/端到端测试；提供发布构建与 checksums 配置。Cloudflare 部署 workflow 默认手动触发，生产迁移/发布需要明确授权，PR 不使用生产 secrets。
- 不实际创建付费云资源、部署到生产、公开样本、push main、发布 Release 或发 X/Reddit 帖子；交付可执行流程，由维护者另行授权。

## 12. 验收场景：以行为验证，不以文件数量验收

每个场景都要有自动测试或具体手工验收步骤，并在报告区分实际执行与尚未执行：

A. 新 clone：按 README 得到可查询的隔离 demo，演示代码不能污染私人库。
B. 正常提炼：关闭 Decision 服务，仅配置 Generator，仍产出有合法 evidence 的记忆。
C. 原话模式：不配 Generator 仍可导入/查原话，状态诚实显示未提炼。
D. 自动索引：新发布内容无需手动 context-embed 即可被正确检索；失败可恢复。
E. 修订：更新 ASR 原文后，所有当前查询和语义分支不再返回旧证据；审计仍可读旧版。
F. 语义：意向/请求不自动变承诺；不同录音相对时间不能制造错误的先后判断。
G. 断网/重启：模拟器保留未持久化音频，恢复后最终覆盖一致；没有 persisted ACK 的块不丢。
H. 幂等：重复上传、重复队列、同步进程崩溃、乱序修订不会重复记忆或跳过缺失数据。
I. 鉴权：设备不能读所有记忆；Agent 只读凭据不能写/复核；错误身份、超大帧、未知 provider 被拒绝。
J. 分布式失败：R2 写后 D1 失败、D1 成功后 enqueue 失败均能恢复；删除后旧任务不能复活内容。
K. MCP：stdio 与 HTTP 都经过 initialize、tools/list、tools/call；断线/取消无资源泄漏。
L. 真机/云端：真实 ASR、真正 Cloudflare 部署和指定客户端分别记录证据；未取得凭据时列 NOT_RUN，不伪造结果。

真实语义检索质量、真实 ASR 准确率、跨境网络延迟不能用 fake 或正弦波测试成绩替代。

## 13. 执行顺序与最终交付

严格按垂直闭环推进，不同时开十个半成品：
M1：本地真实流程、Decision 解耦、自动索引、语义安全、demo。
M2：MCP 共享构造与 HTTP、安全接入、最小状态/证据界面。
M3：Cloudflare 真实 Provider、可靠 chunk 上传、模拟器、同步桥接。
M4：跨进程失败测试、安装/部署/维护、开源文档与发布准备。

每阶段先让最小闭环通过，再做下一阶段。可以记录以后要做的事，但本轮关键路径不能用 TODO、NotImplemented 或只返回 success 的桩交差。

缺密钥/账号时仍完成可测实现与本地模拟验收，不要求用户逐项批准常规实现选择；涉及付费、生产、隐私上传和外部发布时保持未执行状态。

最终提供：
- 实际代码与测试，不只实施计划；
- 简短改动摘要和兼容性/迁移说明；
- 一套真实可复制的本地与 Cloudflare 命令；
- 验收表：PASS / FAIL / NOT_RUN / BLOCKED，附日志或测试证据与原因；
- 客户端、真实 ASR、云端与硬件模拟/真机各自的验证状态；
- 实際限制、数据出域路径、成本构成和下一步唯一最重要的阻塞。

不要凭生成了配置文件就说“已部署”，凭 HTTP 200 就说“ChatGPT 已连通”，凭 mock 成功就说“ESP32 实时录音已可用”。若本次执行额度不足，留下可运行的阶段成果和逐项剩余清单，不把所有里程碑都写成完成。

现在从检查仓库与执行基线测试开始，然后直接实现 M1。

---

## 实施时的官方参考入口

以下仅是查证入口，实施时确认最新版本，不将它们视为本项目已通过验收的证据：

- Cloudflare WebSocket / DO lifecycle:
  https://developers.cloudflare.com/durable-objects/best-practices/websockets/
- Cloudflare Queues delivery guarantees:
  https://developers.cloudflare.com/queues/reference/delivery-guarantees/
- OpenAI MCP authentication:
  https://developers.openai.com/plugins/build/auth
- MCP specification:
  https://modelcontextprotocol.io/specification/latest
