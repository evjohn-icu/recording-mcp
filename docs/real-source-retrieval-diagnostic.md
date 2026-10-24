# 真实来源检索诊断与有限调优（2026-10-25）

## 授权与边界

用户在原合成测试要求之后，明确授权读取所有真实 Profile，并允许使用已有模型。此次真实库均用 SQLite `mode=ro` / `query_only` 检查和制作一致临时快照；没有调用会重建数据库的 `context-eval` / `eval.Run`。新代码的回归测试仍只用合成数据库。

检查了项目 data 目录内可发现的库、Profile 和正在运行服务的数据库路径，区分演示/e2e 与真实来源。主要真实 Profile 和旧 library 库有相同的转写内容摘要，分别为 schema v17/v12；两者时间元数据不同，未合并 Profile、人物、Topic 或事实。实际检索矩阵针对主要真实 Profile 的隔离副本；没有宣称每一个历史库都完成了相同评测。

只在副本中由现有 migration runner 升级到 v19、调用现有 `contextbuild.Build` / `LoadRetrievalQuestions`，并用现有 `semantic.Indexer` 构建离线 hash 对照。未引入检索引擎、HNSW/JEV 调优或真实神经 embedding。未修改 ingest-v1。

## 数据覆盖

| 指标 | 值 |
| --- | ---: |
| 会话 / 录音 | 47 / 47 |
| 原始转写片段 / 归纳 | 7,233 / 6,176 |
| 有转写的会话 / 没有转写的会话 | 36 / 11 |
| 当前非 ambient 片段 / ambient 片段 | 6,924 / 309 |
| 已确认 speaker binding | 0 |
| 原始 embedding 行数 | 0 |

没有 embedding 意味着语义分支缺少索引，不等于补索引必然提高检索质量。没有确认人物绑定时，Person Brief 必须保留缺口，不能猜身份。旧 library 的录音时间均未知，不能与主 Profile 的已保存时间元数据混为一谈。对选取的 23 个会话构建 Brief 未发生错误，返回共 775 条预算内证据；这不是逐条内容准确性验收。

## 方法与指标限制

从有足够长当前非 ambient 片段的 23 个会话各选一个来源，固定本轮问题集：23 条原话短语、18 条原文关键词、18 条模型改写问题，共 59 条。5 个模型拟稿缺失或未通过格式/字面关键词检查，不加入自然问题组。

外部调用只发送选中的 23 个片段和不透明来源 ID 给用户已配置的 `gpt-6-luna`，用于拟题；没有上传全部数据库或音频。模型输出不写入 Memory、Worldline 或事实状态。

下表是**指定来源命中数**，目标是检查检索能否定位已知来源；它不是人工标注的整体 Recall/Precision。每题只有一个已知目标，没有穷举其他同样相关的来源，没有人工判定无答案问题，也没有独立调参/盲测拆分。不能据此宣称真实生产召回率达到某个百分比。

## 结果

所有条件采用同一固定问题集，max_items=10、max_bytes=24000、返回原话证据，未配置重排或充分性模型。

| 条件 | 全部目标命中@5 | 自然问题目标命中@5 | 全部目标命中@10 |
| --- | ---: | ---: | ---: |
| v1 全库，无索引 | 19/59 | 0/18 | 41/59 |
| v1 限定会话，无索引 | 23/59 | 4/18 | 44/59 |
| v2 全库，无索引 | 26/59 | 8/18 | 28/59 |
| v2 限定会话，无索引 | 44/59 | 14/18 | 47/59 |
| v1 全库，hash 混合 | 8/59 | 3/18 | 9/59 |
| v1 限定会话，hash 混合 | 30/59 | 7/18 | 37/59 |
| v2 全库，hash 混合 | 25/59 | 8/18 | 27/59 |
| v2 限定会话，hash 混合 | 44/59 | 14/18 | 45/59 |

无索引时 lexical/hybrid 的命中和排名一致，均实际执行过。hash 对照索引包含 13,409 行（6,176 notes / 7,233 turns），仍由查询路径过滤 ambient/stale。全部矩阵调用无错误，本诊断的范围/当前来源检查计数为 0；安全结论仍依赖独立的合成安全回归，不能从样本计数推及所有数据。

v2 对改写问题更有帮助，但全库原话排序也有退化；hash 对 v1 全库的总体目标命中明显下降。因此本轮没有改默认版本，也没有把对照索引写回原库。

## 已落地的改进

1. v2 原先即使带 ConversationID，仍在全库读取后逐条查询证据、来源和说话人元数据，再过滤会话。现把同一个会话条件放到 SQL 来源查询中，保留范围内完整候选集、BM25 统计、排序、预算和其他安全规则。v1 显式保留其历史忽略 ConversationID 的 store 行为，contextbuild 的最终硬范围检查保持不变。
2. 这也修复了范围外录音时间字段损坏会阻断范围内 v2 查询的问题。测试分别覆盖原话和归纳两条路径。
3. 搜索页勾选「混合检索（实验）」后可显式选择检索版本；默认 v1，v2 选择保存到 URL，刷新恢复。没有将 v2 描述成对所有问题更优。

在同一 59 条限定会话的 v2 lexical 查询上，优化前后记录的目标排名、返回条数及缺口信息逐题相同，命中保持 44/59；单轮 P95 从 1435.542 ms 降至 591.765 ms（约减少 59%）。这是同机单轮诊断，不是延迟 SLA。索引条件与浏览器回归并行，其延迟不参与性能对比。

对已知会话，可通过现有 MCP `build_context` 或 Web `/api/search/hybrid` 显式传 `conversation_id`、`retrieval_version=v2`；需要排除向量影响时使用既有 `mode=lexical`。Brief 本身是按显式范围取当前来源，不应与问句检索的命中指标混同。

## 实际验证与测试先行

- 新 store 回归首次运行因范围外 `not-rfc3339` 时间导致限定查询报错而 FAIL；实现过滤后 PASS。随后补充归纳路径回归。有效来源比较用 ID 而非跨语料范围的 BM25 分数，v1 与不传范围结果保持一致。
- 新浏览器脚本在实现前登录合成 alpha，因不存在「检索版本」控件而 FAIL；实现后验证默认请求 v1、选择 v2 更新请求和 URL、刷新恢复，共 3/3 PASS。
- `go test ./internal/memory ./internal/contextbuild`：PASS。
- `go test ./...`、`go test -race ./...`、`go vet ./...`：PASS。
- `make build web`：PASS。
- `make spec-validate`：PASS：12 items、67 scenarios，仅既有 workflow.no-tdd-inference pending。
- `cd web && npm run typecheck`：PASS（子代理执行）。
- `python3 -m unittest discover -s scripts -p 'test_browser_runner.py'`：3/3 PASS，仍有既有 unclosed socket ResourceWarning。
- `python3.14 scripts/run-local-browser-tests.py --skip-build --browser /usr/bin/google-chrome --artifacts /tmp/recording-retrieval-regression-2dcy07a7`：退出 0；新增检索版本 3/3、Highlights/Brief 21/21，以及原有其他浏览器流程完成。
- 浏览器版本脚本补强 HTTP 200 与 v1/v2 sufficiency 响应断言后，在已有合成 alpha 演示服务独立复测 3/3 PASS。
- 真实诊断通过临时 Go 工具直接调用现有 Store/Build，按 prepare → baseline → index → optimized → indexed 执行。baseline 为 8×59，optimized 为 1×59，indexed 为 4×59，共 767 次 contextbuild 调用；没有用破坏性 eval CLI 读取原库。

安全审查检查了 v1 兼容、范围、ambient/stale、来源证据、说话人绑定、UI 版本更新与旧结果卸载；没有发现本轮改动放宽这些规则。真实数据未进入测试 fixture、OpenSpec 场景正文或 Git。

## 留存与未完成项

私人快照、抽样原文、问题集及临时诊断工具已清理；保留的 JSON 只有汇总数值，不含原话、问题正文、来源 ID 或密钥。两个原库的 schema 仍为 v17/v12，embedding 仍为 0，文件大小和修改时间与开始时相同。不能撤回用户已授权发送到模型服务的片段。

没有部署到真实 Profile，也没有补齐缺失转写、确认说话人、建立生产索引或改动全局排序权重。人工真实问题集、全部相关证据标注、真正语义 embedding 与重排的质量比较仍未完成，不能将本次探索性诊断称为全面调优完成。

汇总：[real-source-retrieval-diagnostic.json](evals/real-source-retrieval-diagnostic.json)。浏览器：[retrieval-version-browser.json](evals/retrieval-version-browser.json)。OpenSpec：`openspec/changes/2026-10-25-real-retrieval-validation/`。
