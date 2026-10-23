# Topic 世界线

当前版本已接入本机真实 Jev-Style Decision-v3 推理，启动模型和启用参数见 [本地 JEV 接入](worldline-jev.md)。下方基础启动命令不带决策参数时，只提供浏览和规则投影。

世界线默认按具体来源 Topic 展示跨会话的提及脉络，大类仅用于分组；事实适用时间保留为独立视图。录音是证据，不是事实变化本身。原有录音列表、搜索、转写、归纳纠正、时间历史与 MCP 保留。

本轮约束：仅在本地资料库副本生成；不调用外部模型，不重跑 ASR，不把真实资料、截图或生成结果提交仓库。原数据库与录音保持不变。

## 时间语义

支持 `YYYYMMDDHHmmss` 和 `PHYYMMDDHHmmss` 文件名，默认上海时区。开始时间来自原文件名；原录音修改时间按用户提供的语义作为结束时间线索。分段 PCM、转写或复制文件的修改时间不能替代原录音修改时间。找不到原文件时可由已有媒体时长计算播放区间，并明确标注依据。

录音开始/结束、原话提及时间、事实生效时间、系统记录时间分别处理。无法确定事实生效时间但有录音时间的陈述仍显示在提及脉络中；只有提及时间也未知的内容才单列。提及节点只能表示说过这件事。开放区间的结束未知，不表示一直持续到今天。更正记录不能画成现实再次发生变化。

## 生成边界

只复用 current、非 ambient、未遗忘/未抑制的已有归纳与真实 source turn/revision。首轮不使用语义模型自动编造事实或合并身份：确定性规则用于大主题分类和可验证的日期解析，歧义进入待归类。所有新生成陈述待确认；计划、请求、约束与建议不自动成为现实事实。冲突并列，人工核对表示核对了来源，不保证客观真实性。

世界线是现有追加式事件与记忆来源的投影，不是另一个独立事实库。重命名、主题归并与拆分、归属调整和事实反馈保存历史并进行乐观版本校验；来源更新或遗忘后不能复活旧内容。

## 数据与回退

真实本地副本、密码、来源校验清单与实际运行报告放在被 Git 忽略的 `data/worldline-local/` 下。文件权限限制到当前用户；密码保护不是磁盘加密。回退可以使用原数据库和旧页面，世界线副本不反写原库。执行任何永久遗忘后的备份恢复仍必须遵守既有删除屏障。

## 启动本轮本地版本

在仓库根目录执行：

```sh
make build
cd web && npm ci && npm run build && cd ..
./bin/context-web --profiles-root data/worldline-local/profiles --addr 127.0.0.1:8790 --web-dir web/dist
```

打开 <http://127.0.0.1:8790>，真实副本的资料库名为 `worldline`；本机密码保存在 `data/worldline-local/worldline-password.txt`。合成演示资料库名为 `demo`，密码为 `synthetic-worldline-demo`。不要把真实资料库、密码或截图提交 Git。

为已经通过 `context-profile adopt` 建立的本地副本准备录音时间和浏览器音频：

```sh
export MEMORY_PROFILE_PASSWORD="$(cat data/worldline-local/worldline-password.txt)"
./bin/context-worldline --prepare --profile worldline \
  --profiles-root data/worldline-local/profiles \
  --library-dir data/asr/library --timezone Asia/Shanghai
unset MEMORY_PROFILE_PASSWORD
```

如果知道原录音目录，可追加 `--audio-root /实际原录音目录`。工具验证原文件 hash 后才使用它的修改时间；没有原文件时使用媒体时长，来源依据写入该资料库 `tasks/recording-time-manifest.json`。不得用缓存 PCM 的修改时间替代原文件时间。

## 本轮真实本地验收

2026-10-23，所有真实内容只在本机副本处理：

| 项目 | 状态 | 结果 |
| --- | --- | --- |
| 录音准备 | PASS | 47 个录音时间与 47 份拼接 PCM；无音频缺口 |
| 世界线生成与显示 | PASS | 8 个大主题、5,043 条 current 非 ambient 陈述 |
| 明确事实时间 | PASS | 3 条有来源日期；5,040 条保持未知，未自动确认为现实事实 |
| 真实 Chromium 浏览器 | PASS | 主题、原文详情、音频片段播放；无页面脚本错误 |
| 原始资料未修改 | PASS | 原 DB SHA-256 一致，25,113 个源文件大小/mtime 不变 |
| 外部模型、重新 ASR | NOT_RUN | 本轮只复用已有本地归纳与原文 |
| 原始录音修改时间 | NOT_RUN | 尚未取得原音频目录；结束时间采用媒体时长 |

真实运行明细与截图仅保存在忽略目录 `data/worldline-local/`。第一轮本机世界线查询约 5.5–5.8 秒；第二轮索引优化与对照见下文，尚未进行大规模并发验收。合成验证见 [worldline-validation.md](worldline-validation.md)。

## 迁移、回退与限制

新增迁移 17：只添加 `attachment_turns(turn_id, attachment_id)` 反向索引，不新增业务表或更换驱动。正常启动自动迁移，可保留索引运行旧版本；如需撤销性能变更，可停止服务后在副本删除 `attachment_turns_by_turn` 索引（不删除迁移记录，不影响数据语义）。世界线反馈和生成状态使用现有 plane 追加事件，录音准备复用现有 recording/sync 表；归纳仍引用当前 source turn/revision。来源版本变化会使旧反馈不再参与当前投影。遗忘还会清理跨会话反馈的派生正文。

停止新服务后可运行旧二进制并访问旧录音页；准备的数据均位于独立资料库，原库不需要恢复。不要通过恢复删除前快照绕过遗忘屏障。当前改动未自动提交、推送或部署。

后续工作：确认主体后的跨会话事实合并；大规模查询性能验证；在取得原文件后核实结束时间。语义关系仍是可审计建议，不会自动改写事实或合并身份。

构建与回归结果：`make build` **PASS**；`go test ./...` **PASS**；`go test -race ./...` **PASS**；最终遗忘清理及时间/搜索过滤修复后的 `go test -race ./internal/app ./internal/web` **PASS**；`git diff --check` **PASS**。这些检查不代表重新验证了外部 Provider、真实 ASR 准确率或大规模检索性能。

## 跨会话提及脉络（第二轮）

默认视图使用具体来源 Topic 的标题进行规范化精确分组，优先展示跨会话 Topic。相同主题只表示讨论对象的组织线索，不代表相同说话人或相同事实。每条陈述保留独立 claim、来源修订和原文；支持从节点打开原文与音频。事实适用时间仍可切换查看，没有生效日期的陈述也能按录音提及时间进入脉络。

关系标签分为首次提及、后续提及、计划、相同已确认主体的重复陈述，以及人工修正/现实变化。只有原文和归纳都有同一明确变化措辞，才显示“变化线索，待核对”；这不会自动确认现实事实、合并身份或把计划升格为事实。“现在”一个词不能单独证明变化。异名 Topic 的语义归并仍未运行，当前分组不是完整语义理解。

2026-10-23 本地真实 API 检查：过滤旧修订和 ambient 后共 2,762 个具体 Topic，其中 34 个跨会话。第一页 8 个 Topic 各涉及 3–5 个会话；抽查 46 条陈述全部有提及时间，仍保留未知事实生效时间。接口与明细保存在私有本地目录，真实原文不写入报告。

同一资料库的 `/api/worldline` 三次查询耗时：索引前 5.8252 / 5.4034 / 5.8845 秒；索引后 0.1990 / 0.1994 / 0.1946 秒。统计和分页预览中的 claim/version/evidence 标识哈希一致。新 `/api/worldline/threads` 首次查询约 0.227 秒。这是本机串行测量，不能推导并发或百万级性能。

对全部 34 个跨会话 Topic 的本地明细核验（145 条陈述）：145 条都有提及日期，来源证据非空；关系为 25 次首次提及、97 次后续提及、23 条计划。这批数据未触发保守的明确变化措辞规则，因此**不宣称已识别出真实事实变化或矛盾**。这轮交付解决跨会话排列和原文核对；更广泛的语义理解仍需后续可解释评测。

第二轮验证：`go test ./...`、`go test -race ./...`、`make build` 均 **PASS**；未登录访问两个新线程接口均返回 401（**PASS**）。真实 Chromium 首屏 8 个具体 Topic、默认展开 5 条提及，首次可见约 1.907 秒；点击陈述可播放真实 1.5 秒原文片段，播放时钟前进且无解码/页面错误（**PASS**）。这是浏览器媒体管线验证，不代表物理扬声器听音检查。原 DB 哈希及 25,113 个源文件再次校验无变化（**PASS**）。

## 稳定主题、关系复核与队列分析

具体主题的 ID 由来源 Topic 身份生成，因此手动改名和来源标题变化不会改变当前主题 ID。旧标题派生的线程 ID 作为重定向保留。用户可把不同来源 Topic 手动组成一个展示分组，也可撤销；这只组合提及视图，不修改 claim、事实或说话人身份。改名、分组和撤销事件记录当前来源引用、来源 note 版本，并使用跨进程的 `expected_version` guard；来源被修改或遗忘后，旧呈现事件不再参与当前投影。

每条语义关系建议都有独立的 suggestion ID、来源 claim、turn/revision、model、weights 和 rubric 版本。复核记录支持接受、拒绝、证据不足和重新打开；这些操作只改变复核状态，不会把关系建议转成事实。列表中的待复核计数跟随最新状态。相同 request ID 的原请求可安全重试；不同内容复用 request ID 会冲突。复核提交遇到版本冲突时保留决定，取回当前来源摘要和证据供对照后再重试。

`POST /api/worldline/analysis` 将固定的来源对和版本引用写入现有 processing 队列，`GET /api/worldline/analysis/{job_id}` 返回状态及进度。worker 只读取本次启动时解析好的 profile event-plane 路径，拒绝 payload 中不匹配的 profile scope。profile worker 默认启用 Worldline stage；legacy 数据库必须显式配置 `--worldline-events-db` 后才能领取该 stage。决策 endpoint 只接受 loopback HTTP，不使用代理或重定向。原有同步 `POST /api/worldline/generate` 保留用于兼容。

分析以不超过 128 对、每批最多 45 秒执行；每对完成后先将带来源引用、speaker 快照、model/weights 与 rubric 的结果写入 event plane。worker 重启或 lease 重试后按来源、model、weights 和 rubric 命中已保存结果并继续未完成的尾部；source note、turn revision 或 speaker identity 已变化的对会跳过，不发布过期建议。队列 payload 保存 source snapshot、来源会话、turn/revision 和 note 版本，不保存转录文本。以上 Worldline 行为复用 event plane 与 processing queue，没有新增 Worldline 专用数据库表。

本次本地实现验证：`go test ./internal/app ./internal/web ./internal/profile ./cmd/context-worker ./cmd/context-web ./internal/decision -count=1` **PASS**；`cd web && npm run typecheck` **PASS**。app 测试包含 300 对合成候选的重启续跑、缓存仅失效变更来源对、旧线程 ID 解析、改名与分组撤销、首 100 项后的改名、复核接受/重新打开/幂等重试/过期身份拒绝，以及两个 service 实例竞争同一版本时恰好一个写入成功。该验证使用合成测试输入，没有访问私有资料或调用模型服务。

Detail 页处理状态另有合成 Chromium 回归 `python3.14 scripts/processing-poll-browser.py`：pending 状态持续轮询、终态停止轮询、空 `jobs`/`workers` 数组不崩溃、无存活 worker 时显示“候选生成器状态未知”，重试请求发生一次网络失败后仍复用原 `request_id`，且成功后重启状态轮询均 **PASS**。该脚本用生产构建预览与 mock API，不连接本地资料库。

最终 managed browser run `python3.14 scripts/run-local-browser-tests.py --artifacts /tmp/recording-worldline-browser-final --browser /usr/bin/google-chrome` **PASS**：合成 profile/browser 20 项通过，Worldline mock browser 22 项通过，processing poll 回归通过（pending 连续查询、终态等待超过 10 秒仍无新查询、两次 retry 复用同一 request ID、无页面脚本错误）。runner 使用 `vite preview` 提供已构建 bundle，避免 API route mock 拦截开发服务器的 `/src/api/` 模块。
