# Retrieval Quality v2 验证记录

日期：2026-10-25。全部数据为仓库合成夹具或测试临时目录；未读取私人资料和录音，未修改 `contracts/ingest-v1/`。默认检索路径保持 v1。

## 测试先行记录

以下为本次实际执行顺序的记录，OpenSpec 和 test-map 的结构检查本身不能证明 TDD。

| 范围 | 首次失败命令与结果 | 后续验证 |
|---|---|---|
| 中文 lexical API | `go test ./internal/memory -run TestSearchMemoryV2ChineseRankingAndConversationIsolation -count=1`：API/字段尚不存在，编译失败 | `GOCACHE=/tmp/recording-mcp-go-cache go test ./internal/memory -count=1` 通过 |
| 截断前评分回归 | 超过512条干扰项的新增测试首先返回 `t:noise-0000`，未返回后面的相关项 | 移除评分前ID截断后，同包测试通过 |
| v2 builder | `go test ./internal/contextbuild -run 'TestBuildV2' -count=1`：新增API尚不存在，编译失败 | `GOCACHE=/tmp/codex-contextbuild-cache go test ./internal/contextbuild -count=1` 通过 |
| Web版本入口 | `go test ./internal/web -run TestHybridRetrievalVersionSelection -count=1`：未知版本错误返回200 | 实现参数转发/校验后通过；补充省略版本与显式v1的JSON等价断言 |
| MCP入口 | 新增v2 smoke断言后首次运行遇到并行builder尚未完成的编译错误，不能作为独立行为失败证明 | `go test ./cmd/context-memory -run TestBuildContextToolSmoke -count=1` 通过 |

后续回归覆盖非法排列、模型修改候选、模型输入上限、未配置模型时不应用模型字节限制、模型故障保留结果，以及短摘要引用的长原文不得绕过最终字节预算。

## 已执行的集成检查

- `go vet ./...`：通过。首次执行因默认Go缓存只读而失败，重跑后成功；权限问题不算代码检查通过。
- `go test -race -count=1 ./...`：通过，所有包完成；当时的eval后续记账修正另行验证。
- `make spec-validate`：8项通过，39个场景映射通过；原有 `workflow.no-tdd-inference` 仍明确 pending。
- `python3 -m unittest discover -s scripts -p 'test_*.py' -v`：共64项：63项通过、1项真实模型smoke按配置跳过；后续新增测试另行验证。
- 初次相关包集成测试中，既有 `TestWorldlineMutationVersionIsAtomicAcrossServiceInstances` 出现 `SQLITE_BUSY`，单独3次复跑也出现一次。工作区随后20次通过，导出HEAD的临时目录3次和20次均通过，全仓race运行也通过。未修改worldline代码，保留该间歇性失败记录，不据此声称已修复。

## 首批语料否决

首次240问冻结集虽通过数量、分组、引用和哈希校验，随后语义内容审查发现部分否定/时间变化/跨会话金标不足。其 train/calibration 分数不得作为有效质量提升证据。原冻结字节保留，原始执行命令、二进制哈希、报告及否决说明归档于 [首批诊断记录](evals/retrieval-quality-v2-rejected/REJECTED.md)。原test检索分区没有单独评分，但旧classic问题包含全部概念，不能声称该批holdout保持独立。新runner会显式关闭classic pass，避免间接执行test概念问题。

新语料使用独立版本 `quality-v2-r2`，逐组审查后另行冻结；不能把修正后的集合与首批成绩混用。

## R3 格式修复与预检

R2首次正式运行在导入第一条过期干扰修订时失败，8次run均没有进入检索阶段。保留 [R2失败记录](evals/retrieval-quality-v2-r2-import-failure/README.md)。R3只将每个segment的original_index规范为唯一的1起始整数；问题和金标与语义审查后的R2字节相同。

新增 `test_source_original_index_must_be_positive` 首次失败（没有抛出ValueError），添加校验后通过。真实导入预检命令：

```sh
/tmp/context-eval-quality-v2-final --corpus fixtures/eval/quality-v2-r3/corpus.json --questions '' --retrieval '' --projection '' --links '' --embed-provider off --out /tmp/r3-ingest-only.json
```

退出0，导入180个会话、204个segment；questions/retrieval_questions/retrieval_requests/embedding/model_calls均为0。预检不涉及任何查询评分。

最终新增修正验证：`go test -race -count=1 ./internal/contextbuild ./internal/web ./cmd/context-memory`、`go test -race -count=1 ./internal/eval ./cmd/context-eval` 均通过。`go vet ./...` 重跑通过；Python全量更新到共72项：71项通过、1项模型smoke跳过，增加索引校验后v2专项19项通过。

最终 `make spec-validate` 再次通过8项规范与39个场景映射。`python3 -m unittest discover -s scripts -p 'test_*.py'` 最终共73项：72通过、1项真实模型smoke按配置跳过。既有浏览器runner测试产生未关闭socket的ResourceWarning，命令退出0；未将该警告隐藏为没有发生。

## 最终 R3 校准结果

真实执行命令（均退出0）：

```sh
go build -o /tmp/context-eval-quality-v2-final ./cmd/context-eval
python3 scripts/run-retrieval-quality-v2.py --binary /tmp/context-eval-quality-v2-final --out /tmp/retrieval-quality-v2-r3-final
```

4次train与4次calibration运行完成。每次校准包含40个正例、20个负例，MRR和Recall按正例计，负例误召回按至少返回一项的负例查询计；p95按该模式60次检索请求计。

| 路径 | Recall@5 | MRR@10 | 负例误召回 | p95 ms |
|---|---:|---:|---:|---:|
| v1-lexical | 0.0000 | 0.0000 | 2/20 (10%) | 2.470 |
| v1-hash | 0.1500 | 0.1052 | 19/20 (95%) | 21.892 |
| v2-lexical | 0.8750 | 0.3721 | 19/20 (95%) | 59.885 |
| v2-hash | 0.1625 | 0.0784 | 19/20 (95%) | 82.536 |

所有实际运行的stale/ambient/scope/invalid-evidence泄漏计数均为0，这是该次实验的观测值，不替代隔离回归测试。v2 lexical提升词面召回但误召回与延迟不达标；v2 hash的MRR退步，误召回与延迟也不达标。没有合格校准候选，未生成selection lock、未运行test、未切换默认。

真实embedding、reranker和充分性模型均没有显式配置，状态为NOT_RUN；接口与故障路径只经过合成测试，不能声明真实模型质量通过。hash是确定性本地基线，不是真实语义embedding。充分性指标没有配置模型时不以0%误判宣称成功。

原始逐问指标、各桶分母、调用计数、命令、退出码、报告哈希和门槛逐项结果见 [最终manifest](evals/retrieval-quality-v2-r3/manifest.json)，环境信息见 [运行元数据](evals/retrieval-quality-v2-r3/run-metadata.json)。训练与校准分数不能作为独立holdout成绩；后续不得根据test回调本版参数。

冻结输入：
- `corpus.json`: `b95ade6df3221b8c0fd4653d88d2761da33c15efed5aa4be3cd9402b3fb63dc2`
- `retrieval.json`: `03e8ed678f35cc6632092daacb0f031871048f596853e01cf664d2997b3cc4d3`
