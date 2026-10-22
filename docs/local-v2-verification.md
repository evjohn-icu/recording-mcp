# 本地 v2 交付与验证记录

基线 HEAD：`59eb934370a46c4ff41c518a8b2a19dbe285ffb5`。本轮基于本地实际代码；没有回退到早期远端审查基线。测试仅使用临时合成资料，没有操作真实记忆、调用外部模型或部署生产。

## 功能落点

| 范围 | 落点与验证依据 | 边界 |
| --- | --- | --- |
| 原始转写纠错 | `internal/app/correction.go`、`memory/transcript_correction.go`、`web/src/components/CorrectionPanels.tsx` | 完整 segment 新 revision；旧派生内容 stale；冲突保留草稿 |
| 说话人确认 | 同一 correction 应用服务与 HTTP handlers；现有绑定提议/确认/拒绝/撤销 | 当前录音与 revision 内确认，不跨修订继承 |
| 本地永久遗忘 | `memory/forget.go`、`app/forget_cleanup.go`、`cloudsync/forget.go` | turn/录音/会话/整个 profile；先屏障后清理；云端和非托管副本除外 |
| 统一时间历史 | `plane/time.go`、`app/time.go`、HistoryPage | 沿用事件原型，区分 valid time 与服务端 recorded time，未知不编造；人工报告不等于真相确认 |
| 大规模检索 | `semantic/search.go` 的有界 top-K、分页索引；可选纯 Go HNSW | 默认 exact；ANN 未达质量与百万规模验收门槛 |
| 独立本地账户 | `internal/profile`、`web/profiles.go`、`profile_auth.go` | 密码、会话撤销、固定 DB/音频/任务路径；不提供磁盘加密或远程多租户 |

启动和只读迁入旧数据库的准确命令见 [local-v2.md](local-v2.md)。现有 MCP 和 ingest-v1 继续使用相同数据库与业务状态。

## 可复查证据

- **PASS**：`make build`，所有 Makefile 中的 Go 命令构建。
- **PASS**：`go test ./...` 与 `go test -race ./...`，全仓回归。
- **PASS**：最终遗忘历史版本与待处理任务列表改动后，重新执行 `go test -race ./internal/memory ./internal/app ./internal/web` 与 `make build`。
- **PASS**：`go vet ./...`；`CGO_ENABLED=0 go test ./internal/semantic ./cmd/context-embed ./cmd/context-bench`。
- **PASS**：遗忘单条后邻居音频仍可通过 `Service.AudioClip` 播放且 PCM 字节不变。
- **PASS**：profile 清理失败后重启、拒绝普通登录、凭密码重试完成；持有音频读锁期间，数据库和事件查询立即不再返回内容。
- **PASS**：AI 归纳抑制清理失败后关闭/重开数据库，待处理任务可列出并重试；同会话无关事实保留。
- **PASS**：只读 snapshot 迁入新 profile，源 DB/音频不变，拒绝音频符号链接。
- **PASS**：真实 Chrome 浏览器 19 项流程检查。归纳抑制、单条遗忘和资料库删除核对了真实合成服务响应；待处理归纳清理的刷新/重试界面使用模拟任务列表，真实落盘/重启恢复另由后端测试覆盖。逐项结果见 [local-v2-browser-results.json](local-v2-browser-results.json)；使用本机 Chrome、合成 PCM 与临时 profile，不能外推为真实语音质量。
- **NOT_RUN**：真实 ASR、真实 embedding/generator、云端 provider 调用和私人录音播放。

## 检索基线与比较

[local-v2-evaluation.json](local-v2-evaluation.json) 保存同配置、每次新数据库的三组前后比较（hash provider，256 维，无外部调用）。Evidence Recall@5 = 0.9348、lexical = 0.6154、hybrid = 0.6923，前后一致。stale、ambient、scope 泄漏均为 0；hybrid 无证据问题仍误召回 7 条，**FAIL**，未宣称验证模型拒答。运行时间有噪声，不宣称加速。

[local-v2-retrieval.md](local-v2-retrieval.md) 与 JSON 保存向量算法测量：100k 的 ANN recall@10=.735、@40=.633，**FAIL**；百万 ANN 构建超 600 秒预算，**BLOCKED**。百万精确扫描测量已执行，但仅为内存合成向量，SQLite 全链路百万检索 **NOT_RUN**。因此不默认启用 ANN，也没有接入 sqlite-vec 或替换驱动。

## 迁移、恢复与剩余工作

新增迁移 14、15、16 在临时数据库回归测试中执行；未迁移任何真实数据。升级前对自有数据备份，先在副本验证。未执行遗忘时可保留原数据库副本回退；执行遗忘后不得恢复旧副本或旧程序绕过屏障。检索 sidecar 可丢弃重建，不是事实源。

清理包含该 profile 的全部托管备份，预览会明确告知。进行中的清理必须等到 complete；失败时使用界面重试，不把 pending 当成删除完成。已经交付给浏览器的字节、手工导出、云端和系统备份无法由本地清理追回。

后续 backlog：

1. 为百万规模建立 SQLite + 当前状态过滤 + profile 生命周期的端到端基准，再选择 ANN 参数或替代索引；sqlite-vec 单独验证 modernc/平台 ABI，不引入隐式 CGO。
2. 用已授权真实 embedding 评估中文别名、ASR 误差和无证据问题，解决现有 hybrid 误召回后再调整默认检索。
3. 云端删除/恢复协议携带持久 tombstone，跨设备恢复先应用屏障；当前只保证受管理的本地路径。
4. 若扩展远程多用户，另行设计租户授权、进程/文件/任务隔离与密钥管理；当前独立 profile 是本机隔离边界。

参考源码版本、许可证和借鉴映射沿用 [local-web-audit.md](local-web-audit.md)，本轮没有引入这些参考库为运行依赖。
