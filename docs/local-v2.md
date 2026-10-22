# 本地 v2：本机使用说明

本文仅说明当前代码路径；本地数据和密码不要贴入 issue、日志或报告。建议仅在受信任的本机运行，Web 默认绑定 loopback。

## 构建与启动

在仓库根目录构建 Go 命令及前端：

```sh
make build
make web   # npm ci + web production build，生成 web/dist
```

建立私有 profiles 根目录，并分别创建 profile。密码从环境变量读取，不会作为命令行参数传递：

```sh
export MEMORY_PROFILE_PASSWORD='替换为本地强密码'
./bin/context-profile create --profiles-root ./data/profiles --name home
# 换成第二个 profile 的不同密码再执行
export MEMORY_PROFILE_PASSWORD='另一个本地强密码'
./bin/context-profile create --profiles-root ./data/profiles --name work
```

`context-profile list --profiles-root ./data/profiles` 可查看名称。profile 目录保存各自的 SQLite、事件库、音频、任务、索引和托管备份；不要手工复制不同 profile 的文件互相混用。密码验证凭据不是数据库加密：profile 密码**不提供静态数据加密**，磁盘、备份及操作系统权限仍需自行保护。

启动允许在登录界面切换 profile 的 Web：

```sh
./bin/context-web --profiles-root ./data/profiles
```

访问终端提示的 `http://127.0.0.1:8788`。profile 选择模式会在 Web 登录时验证所选 profile 密码；Web 不是远程多用户服务，不要改为公网监听。

MCP 服务及处理 worker 则应明确使用同一 profile 和 profiles root，并在**各自进程环境**提供正确密码：

```sh
export MEMORY_PROFILE_PASSWORD='home 的本地密码'
./bin/context-memory --profile home --profiles-root ./data/profiles
# 另一个终端 / 服务进程：
export MEMORY_PROFILE_PASSWORD='home 的本地密码'
./bin/context-worker --profile home --profiles-root ./data/profiles
```

同样的 profile 参数适用于其他注册了 profile CLI flags 的命令。不要同时指定 `--db` / `--audio-dir` 覆盖 profile 路径。worker 默认 `--generator off`：队列中有任务并不表示会产生候选或被处理；必须主动运行 worker 才会消费任务，而默认 off 的生成器只留下可检索的原始 turns、不生成候选。hash embedding 是离线确定性特征，不是外部语义模型。不要用 `--generator synthetic` 冒充真实分析；合成生成器仅供演示/测试并要求显式 `--allow-synthetic`。

可用 `context-profile adopt` 把旧版本地数据库迁入一个**全新** profile。数据库以只读 SQLite 连接制作一致快照；源数据库和旧音频不会被删除或改写。音频只复制普通文件，不跟随符号链接；如果旧数据库记录了托管音频路径，需提供对应旧目录：

```sh
export MEMORY_PROFILE_PASSWORD='迁移后 profile 的本地强密码'
./bin/context-profile adopt --profiles-root ./data/profiles --name home \
  --db ./data/memory.sqlite --audio-dir ./data/audio --events-db ./data/events.sqlite
```

`--audio-dir` 和 `--events-db` 可省略（没有需要迁移的托管音频或旧事件库时）。命令不会合并已存在的 profile，也不会迁移可重建 sidecar；失败时只回滚本次新建的目标目录。保留源文件并在实际使用前核实 profile 内容。

## 审阅、修正与删除

- Web 详情页可查看转写证据、播放片段并纠正 segment 文本。提交携带预期 revision；并发版本冲突时草稿应保留，先重新检查当前版本再决定是否重试。修订会使依赖旧来源的派生内容过期；说话人绑定不会自动跨修订继承。
- 说话人按录音内标签提议并由用户确认、拒绝或撤销；确认身份是人工审阅，不是自动识别。
- 时间线中如出现事实时间，区分事实适用时间（valid time）与系统记录时间（recorded time）。未知的旧时间不应推造；“某人报告了某事”不等于已验证该事实。录音时间不确定时应选宽泛音频范围，不要把缺失时间静默扩展为更大删除范围；系统当前时间也不等于事实发生时间。
- GUI 的逐条遗忘操作先显示预览和将清理的托管备份数量，再确认提交；预览版本变化时必须重看预览。遗忘会写入 tombstone / replay fence，清理托管的音频、索引及备份副本；清理未完成会显示 pending，需重试/等待完成，不能当作已完成。待处理遗忘与归纳抑制任务保存在服务端；重新登录后可在页面提示中继续重试。AI-note suppression（抑制）只阻止同一来源、同类 note 再出现，保留来源；它不是删除或遗忘事实。
- 托管目录之外的云端副本、手工导出及操作系统/用户自管备份不在本地清理范围。发生遗忘后不可用旧二进制或旧备份绕过 tombstone；任何恢复都必须重新施加删除 fence。不能承诺外部副本已删除。
- Web profile 选择器支持整体永久删除：在当前 profile 的登录界面调用 `POST /api/profiles/retire`，提交 profile 名称、同名确认和当前密码。删除先持久化 fence 并撤销会话，再清理本地 SQLite、事件库及托管目录；响应 `pending` 表示仍被 fence 阻止访问，可用同名和密码重试，只有 `complete` 表示托管清理已完成。已退休名称不能重新创建。不要通过重建旧 profile 或还原备份规避。

## 检索及验证边界

精确 SQLite 检索是默认路径。可选纯 Go HNSW sidecar 属实验功能，必须通过 `--semantic-index` 显式启用；sidecar 可重建，SQLite 仍是事实源。构建配置/程序不能仅凭一次查询的速度判断 ANN 合格：设计门槛为 exact-oracle recall@10 ≥ .95、recall@40 ≥ .98。见 [`local-v2-retrieval.json`](local-v2-retrieval.json) 和 [`local-v2-retrieval.md`](local-v2-retrieval.md)：100k 的一组设置 recall@10=.735、@40=.633，未过门槛；另一设置只在 10k 得到 1.0；1m ANN 结果未完成。因此不能声称规模 ANN 已达标。报告中的向量算法测量是**内存中合成向量**，不包括 SQLite 过滤、权威状态复核、sidecar 加载、profile 生命周期或端到端 MCP 延迟；全链路性能未知，勿据此承诺响应时间。exact SQLite 查询仍可用且为默认。

浏览器状态以 [`local-v2-browser-results.json`](local-v2-browser-results.json) 为准：合成 profile 上的纠错重试与冲突合并、事实/归纳冲突、未编辑 sibling 保留、搜索空结果及命中详情、窄屏键盘、forget 完成和 profile retirement 均记录为 PASS。forget 验收来自真实 API 响应：POST 返回 200，tombstone 查询为 `complete`，被遗忘 turn 的 source 查询返回 404。归纳抑制也来自真实 API 响应：POST 返回 `200/complete`，目标归纳消失而来源仍返回 200。待清理列表刷新和 retry 控件则使用浏览器脚本注入的合成 pending 条目验证界面恢复；服务端持久化 pending/retry 由独立后端测试覆盖，不是该浏览器步骤制造的真实外部清理失败。搜索分别验证了真实无结果状态和命中后打开详情。该报告只使用合成 fixture，不是私人资料或真实用户验证；真实 ASR、embedding/generator provider、云端流程及真实录音测试均为 `NOT_RUN`，没有真实 provider 质量或真实录音性能结论。

## 数据库迁移状态

代码中注册了迁移 14（permanent forget）、15（profile retirement）和 16（suppression cleanup operations），但这不等于任何既有本地数据库已迁移，也不代表本轮所有迁移/恢复场景已验收。启动/worker 会应用其代码路径上的迁移；部署前须对数据库副本备份并核实实际 `schema_migrations` 状态及迁移成功日志。**不要在未核实前宣称迁移完成**，也不要拿生产/私人数据库做演示或验证。细节与范围参见 [`local-v2-plan.md`](local-v2-plan.md)。

## 合成演示

要验证浏览器交互，使用 `make web-demo` 启动临时合成数据演示，不要连接实际 profile、已有 `data/`、真实 provider 或私人音频。合成材料只验证界面和技术流程，不代表对真实转写或事实的验证。
