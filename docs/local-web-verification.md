# 本地 Web 验证与交付记录

本轮用户流程已实现：时间线 → 搜索 → 详情 → 原话证据 → 片段播放 → 带版本纠正 → 历史。
仓库 HEAD 为 `b43125874f5be73c15d4cc648b08319a62af13eb`；没有回退 HEAD、提交或覆盖原有修改。
实施前快照 `/tmp/recording-mcp-preimplementation-1uoq2yud`，本次关键修复前完整源码副本
`/tmp/recording-mcp-luna-baseline-58vafp9a`。这些临时路径不保证跨环境保留。

## 实际验证

| 状态 | 检查 | 证据 / 边界 |
|---|---|---|
| PASS | `make build`、前端类型检查、生产构建 | `npm --prefix web run typecheck`、`npm --prefix web run build` |
| PASS | 纯 Go 构建 | `CGO_ENABLED=0 go build ./cmd/context-web` 成功，保留 modernc 驱动 |
| PASS | Go 全仓普通测试 | `go test ./... -count=1 -timeout 120s`；之后修改的包另行回归 |
| PASS | 最终全仓 race 检查 | 收尾修改后重跑 `go test -race ./... -count=1 -timeout 180s`，全部通过；日志 `/tmp/recording-mcp-final-tests.log` |
| PASS | stale、scope、版本、队列与索引回归 | stale sibling 不复活；范围先于 unresolved/coverage；内嵌证据也过滤；新注释与 embed 队列同事务；旧向量不能覆盖新文本 |
| PASS | HTTP 与音频安全 | loopback bind、Host、bootstrap Origin、写入 CSRF、越界符号链接、PCM 偏移溢出；超大稀疏 PCM 在返回成功前被拒绝 |
| PASS | Chrome 完整用户流程 | 合成库 24 条时间线；搜索“场地”打开对应证据；从证据直接播放 3.5 秒 WAV，currentTime 正常推进、无媒体错误 |
| PASS | 真实并发版本冲突 | 在页面编辑时通过实际 API 写入另一版本；页面返回 409，展示当前摘要并保留草稿；显式重试保存成功，其他条目不变 |
| PASS | 历史、键盘与窄屏 | 保存后历史刷新；冲突焦点进入弹窗；390px 宽度无横向溢出；测试过程中无 JS 错误 |
| PASS | 无证据 UI 空结果 | 默认词面搜索“量子纠缠实验室”显示空结果 |
| FAIL | 实验混合检索无证据误召回 | 两个无证据问题中一个返回 7 项；保留实验标签，不宣称可靠拒答 |
| NOT_RUN | 真实 embedding / ASR / 云端 / 真实音频 | 未调用外部 Provider、未上传私人记忆、未部署；播放验证仅合成正弦波，不验证声学输出或 ASR 质量 |

浏览器截图：[窄屏历史验证](local-web-browser.png)。完整记录
[local-web-browser-results.json](local-web-browser-results.json)；本地测试日志
`/tmp/recording-mcp-luna-all-tests.log`、`/tmp/recording-mcp-luna-race.log`。

## 相同语料的修复前后比较

使用同一套已修正中文合成 fixtures；每个版本串行运行 3 次 `context-eval`。
32 segments、24 conversations、46 普通问题、15 retrieval 问题，其中 13 个正例、2 个无证据问题。
错误 ASR 样例“密钥 / 密月”按有证据正例计分；普通正例仍强制 gold 非空。
完整指标数组见 [local-web-evaluation.json](local-web-evaluation.json)。

| 指标 | 修复前 | 修复后 |
|---|---:|---:|
| 普通 evidence Recall@5 | 0.9348 | 0.9348 |
| lexical 模式 Recall@5 | 0.6154 | 0.6154 |
| hybrid 模式 Recall@5 | 0.6923 | 0.6923 |
| 有显式 top1 标注样例的排序命中率 | 1.0 | 1.0 |
| 无证据误召回项数：lexical / hybrid | 0 / 7 | 0 / 7 |
| stale / ambient / scope 泄漏计数 | 0 / 0 / 0 | 0 / 0 / 0 |
| 无效引用计数 | 0 | 0 |
| lexical 平均耗时的三次中位数 | 1.565 ms | 1.680 ms |
| hybrid 平均耗时的三次中位数 | 5.942 ms | 7.199 ms |

计时是同机器本地样本，不是 P95 或规模性能承诺。新增来源校验有开销；没有宣称性能提升。
修复后的 checker 会检测混合新旧证据和 evidence 范围，而旧 checker 可能漏检；因此修复前的零
不能证明此前没有缺陷，新增专门回归已经复现并验证修复。`lexical` 模式仍包含现有导航/关系扩展，
不是纯词面算法的消融实验。没有问答生成模块，未验证模型拒答。

本轮保留原融合与 SQLite 驱动，没有引入 RRF、FTS5 或 sqlite-vec。FTS5 本地探针显示
unicode61 不能满足任意中文子串召回，trigram 对两字词不命中；未证明收益，不更换现有路径。
默认 hash provider 是离线确定性特征，不等于训练得到的语义 embedding。

## 启动、迁移与未实现项

运行 `make web-demo`（可选 `PORT=8790`），打开日志中的单次登录 URL。
演示使用新建临时数据库及合成 PCM，不触碰 `data/`。已有本地库启动和 worker 配置见
[运行手册](local-web.md)。Web 不自动运行索引 worker；编辑立即保存，语义索引由已有队列更新。

无新增 schema migration，现有 MCP 与 ingest-v1 契约保留。回退先停进程，选择性恢复本轮源码/二进制，
不能 `git reset --hard` 或清理原有未跟踪文件；恢复数据库前使用 SQLite backup API，不覆盖实时 WAL 数据。

未实现：GUI 原始转写纠错、GUI 说话人确认、永久遗忘、多用户、向量扩展。
原始转写仍须新 revision → 旧派生 stale → 重新处理；说话人使用现有确认绑定机制。
人工修改归纳不确认现实事实，intention/request/constraint 仍是 proposed/unconfirmed。
录音删除仍仅移除音频，保留转写和派生内容；本轮没有包装成“永久忘记”操作。
后续时间模型、遗忘级联、规模优化和多用户设计见 [审计与参考映射](local-web-audit.md)。
