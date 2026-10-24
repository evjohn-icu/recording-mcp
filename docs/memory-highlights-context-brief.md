# 记忆重点与上下文简报

两项功能使用当前 Profile 的 SQLite 和托管音频，不调用外部模型，不会确认候选记忆或合并人物身份。标签表示用户关注度，来源有效也不代表陈述已证实。

## 构建与合成体验

执行 `make build web`，再执行 `make web-demo`。只使用该命令创建的临时合成录音，不把真实资料库传给演示脚本。启动与 Profile 登录方式沿用 `docs/local-v2.md`。

录音详情页的“记忆重点”面板支持从轮次创建、编辑备注和标签、查看原话、播放音频、删除及人工重新关联。全局播放器播放期间可标记当前完整录音时间。转写被修订后标记保留并显示“需要重新关联”，旧原话不再显示为当前证据；重新关联不确认事实。并发保存返回 409 时界面保留草稿，刷新服务器版本后由用户再次提交。

导航“上下文简报”支持 Person、Topic、Conversation 和 Project。Project 是本次显式选择的 Topic/Conversation 并集，不创建独立项目事实。选择范围和可选录音时间区间后生成，展开证据并播放原音。未确认的跟进内容标为 proposed；其他来源陈述默认 unverified。无证据栏目显示缺口。未知录音时间不会猜测，有时间筛选时排除并报告。

Markdown 复制仅把用户选择的简报复制到剪贴板。永久遗忘清理本机托管数据及备份，不能自动删除用户已复制到外部系统的内容。

## 应用契约

所有请求复用 Web session、Origin 和 CSRF 保护，JSON 写请求带 `Content-Type: application/json` 和 `X-CSRF-Token`。Profile 由已认证会话绑定，参数不能选择另一个数据库。

`highlights/0.1`：

| 方法与路径 | 用途 |
| --- | --- |
| POST `/api/recordings/{id}/highlights` | 创建；request_id 必填，重复相同内容幂等 |
| GET `/api/recordings/{id}/highlights` | 列出该录音标记 |
| GET `/api/highlights/{id}` | 读取标记、版本、来源状态和当前原话 |
| PATCH `/api/highlights/{id}` | expected_version 加 tag/note_text 或 relink |
| DELETE `/api/highlights/{id}?expected_version=N` | 按版本删除，保留不含正文的请求拒绝标识 |
| GET `/api/highlights/{id}/audio` | 根据服务器保存的锚点播放托管 PCM |

创建字段：`request_id`、`anchor_kind`（point/range/turn）、`start_ms/end_ms`（完整录音毫秒）、可选 `turn_id`、`source_attachment_id`、`transcript_revision`、`tag`（important/follow_up/question）、`note_text`。来源附件 ID 可从当前 turn 的 transcript-source 接口取得；不能用 segment ID 代替。turn 缺少时间时可以保留证据标记，但不能播放。

PATCH 的 `relink` 包含当前 `turn_id/source_attachment_id/transcript_revision`，不悄悄改变时间锚点。版本冲突为 `409 version_conflict`，不同创建载荷重放为 `409 request_conflict`；来源错误为 `409 source_mismatch`，遗忘为 `410 source_forgotten`，已删除请求为 `410 highlight_gone`，退休为 `410 profile_retired`，未知标记为 404。备注上限 8192 UTF-8 字节。

音频不接受客户端路径；区间单次最多播放前 120 秒，时间点默认前后各 5 秒并限制在录音内，窗口与遗忘区间重叠时拒绝播放。响应头 `X-Audio-Start-MS`、`X-Audio-End-MS`、`X-Recording-ID` 表示实际片段位置；播放器 currentTime 仅是片段内偏移。

`POST /api/briefs/preview` 使用 `context-brief/0.1`，输入：

```json
{"scope":{"kind":"conversation","conversation_ids":["显式会话ID"]},"max_items":40,"max_evidence":80,"max_bytes":65536}
```

`GET /api/briefs/people` 返回当前有效的 confirmed 人物关联，供页面选择；没有当前确认依据的人物不会出现在列表中。

其他 scope：person/person_ids、topic/topic_ids、project/topic_ids + conversation_ids。`started_after/started_before` 是可选 RFC3339 录音时间边界。默认预算 40 条内容、80 条证据和 64 KiB；最大 200/400/256 KiB，超出或无法容纳响应骨架的预算返回校验错误。响应包括 snapshot_id、scope、sections、evidence、coverage、gaps、truncated、degraded。证据引用、范围和版本参与确定性快照；简报正文不另行缓存。读取最多 5000 个范围内轮次，达到上限时明确报告截断；字节预算可能缩短摘录，并携带 text_truncated，界面提供完整来源链接。

MCP 新增只读工具 `list_highlights(recording_id)` 和 `build_context_brief`（与 preview 同输入）。只读模式仍提供这两个工具；不会注册 Highlights 写工具。

## 升级与遗忘

SQLite v19 新增 memory_highlights 和 memory_highlight_requests，并安装退休写入屏障。已有 v18 数据库启动时通过原有 runner 升级；不需要新增服务。测试只对临时库验证升级，不能据此宣称任何真实用户库已经升级。上线前沿用既有 SQLite 一致备份流程，恢复必须保留遗忘和退休 fence。v19 之后不要用旧二进制执行遗忘或 Profile 退休：旧版本不知道新表的清理规则，不能保证其隐私删除语义。

永久遗忘把标记纳入预览状态摘要，编辑发生后旧预览不能提交。turn 遗忘同时删除重叠的无 turn_id 时间标记；无法判定时保守处理。已删除标记的请求账本不保存备注和 payload 摘要；重放不得复活。整个 Profile 退休清理新表并阻止新增、编辑和重放。

实际命令和结果见 `docs/memory-highlights-context-brief-validation.md`。

## P2：可选润色、导出与编辑体验

Brief 页面新增精简/详细 Markdown 模板、复制反馈与 `.md` 下载。两种模板均保留来源 revision、附件、证据 ID、来源/断言状态、未知时间、截断和数据缺口。详细模板另外保留来源说明、人物与 Topic ID。输入文本按纯文本转义；AI 候选不混入确定性导出。

Highlights 可按标签筛选，显示录音绝对时间。409 冲突显示服务器最新版本、标签和备注，同时保留本地草稿；点击「重新加载版本」保留草稿并更新编辑基准，随后保存，或显式点击「以服务器新版为基础重试」。保存期间暂时禁用该条目编辑，避免请求完成时清除新输入。

润色默认关闭，确定性 preview 和 MCP `build_context_brief` 不调用模型。要启用，在原来的 `context-web` 启动命令追加：

```bash
--brief-polish-endpoint http://127.0.0.1:8080/v1/chat/completions \
--brief-polish-model YOUR_LOCAL_MODEL
```

端点是管理员启动配置，不接受网页传入的 URL；HTTP 只接受 loopback，远端必须 HTTPS。需要鉴权时再传 `--brief-polish-key-env YOUR_KEY_ENV`，从指定环境变量读密钥。示例不表示该端口已有模型。Profile 选择模式和单 Profile/legacy 模式均支持这些参数。

用户生成简报后，必须每次勾选「同意发送」并点击「生成独立候选」。服务可能是外部模型，界面明确说明发送内容；默认不发送任何私人资料。本次实现/测试没有连接外部模型，仅使用合成内容和 loopback 假模型。

新增 API（均复用 session/CSRF/Origin/请求体限制）：

- `GET /api/briefs/polish/config` → `{"enabled":false}`（或 true），不公开端点或密钥。
- `POST /api/briefs/polish` → 输入 `{"request":{...原 preview 参数...},"snapshot_id":"...","consent":true}`。
- 输出 `snapshot_id/candidates/degraded/reason`；候选包含 `entry_id/text/evidence_ids/assertion_state`，状态只能是 `proposed`，只能引用原条目的完整证据集合。

未启用返回 `503 polish_disabled`；缺同意或快照返回 400；来源在调用前后变化返回 `409 snapshot_changed`，需重新生成。模型网络/结构/预算失败返回 `degraded=true, reason=polish_unavailable, candidates=[]`，不含模型错误正文。原确定性简报仍可使用。

适配器使用兼容 chat-completions 的 JSON 交换格式；没有新增事实引擎或生成任务队列。现有 `generate.Generator` 用于记忆候选抽取，提示词与领域含义不同，因此没有将润色送入持久化候选流水线，也没有修改其错误处理行为。润色传输是独立的小适配器，最多 40 条、每条 8192 字节、总正文 64 KiB、请求/响应 64 KiB、超时 30 秒，不跟随重定向。超过限制可缩小范围后重试；不会影响原 preview 的资源预算。

候选只在响应与浏览器内存中存在，不存数据库；前后重新构建快照检查修订、遗忘、绑定和范围。结构校验不能证明模型文本真实，必须人工对照关联证据。永久遗忘不能撤回已复制/下载或已明确发送到外部服务的内容。

本增量无数据库迁移，schema 仍为 v19；没有新增 MCP 写工具、依赖或专门的检索性能调优。取消过期前端请求、复用证据索引与模型调用预算减少无用工作。
