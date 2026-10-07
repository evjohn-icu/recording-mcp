# 混合检索 v0 设计说明

本文件是本轮“混合检索（hybrid retrieval）”的审计与设计契约：先记录改动前各检索面的真实实现，再给出目标数据流、融合公式、降级模式与必须保持的不变量。后续代码必须与本文件一致；改变融合权重或分支语义属于检索语义变更，必须同步更新本文件。

## 1. 审计现状

改动前，检索由若干彼此独立、各自实现过滤的读取面组成，没有任何统一的语义召回、融合打分或逐条来源标注。

| 读取面 | 实现位置 | Go 符号 | 数据流 |
|---|---|---|---|
| 字面检索 | `internal/memory/retrieval.go` | `Store.SearchMemory`（:513） | 大小写不敏感的子串匹配，先扫 `notes.summary`（命中 `Score=1.0`），再扫 `turns.content`（命中 `Score=0.5`），各自按 kind 排序后合并分页。 |
| 笔记子串扫描 | `internal/memory/retrieval.go` | `Store.searchNotes`（:576） | `notes JOIN conversations LEFT JOIN recordings`，默认追加 `NOT NoteStalePredicate(n)` 与“主证据轮次非 ambient”子句，再用 Go 侧 `strings.Contains` 过滤。 |
| 轮次子串扫描 | `internal/memory/retrieval.go` | `Store.searchTurns`（:653） | `turns JOIN recordings`，默认追加 `CurrentTurnPredicate(t)` 与 `turn_classes.class='ambient'` 排除子句，再逐条投影已确认说话人身份。 |
| 导航查询 | `internal/memory/store.go` | `Store.Query`（:348） | 按 conversation/topic/recording/speaker 过滤读取 turns 与 notes，同样内联 stale/ambient 子句，并用 `literalMatch` 做可选子串收窄。 |
| 组装入口 | `internal/contextbuild/build.go` | `Build`（:250） | 依次执行字面召回 → topic/conversation 导航 → 已确认链接扩展 → 排序去重 → 预算 → 证据窗口 → 可选重排，全部候选经 `addItem` 进入 `candidateSet`。 |
| 作用域硬过滤 | `internal/contextbuild/scope.go` | `requestScope.allows` | 每个候选在进入集合前检查 conversation/topic/time；时间未知的会话在有界请求中被排除。 |
| 已确认链接遍历 | `internal/contextbuild/build.go`（步骤 3） | `pipeline.ListLinks`（`internal/pipeline/associations.go:555`） | 仅 `status=confirmed` 且 `!link.Stale` 的链接参与导航；proposed/rejected 不参与。 |
| 说话人投影 | `internal/memory/speakers.go` | `Store.SpeakerProjection`（:420） | 按 (recording, transcript revision) 投影 confirmed 绑定到 person id，`speakerProjectionCache`（`store.go:604`）缓存结果。 |
| ambient 过滤 | `internal/turnclass/turnclass.go`、检索 SQL | `turn_classes` + 三处 SQL 子句 | 轮次 ambient 由 `turn_classes.class='ambient'` 决定；笔记 ambient 由 ordinal 0 的证据轮次决定。 |
| stale 过滤 | `internal/memory/turnscope.go` | `CurrentTurnPredicate` / `NoteStalePredicate` | 轮次 stale 来自 `attachment_turns JOIN source_attachments.active=0`；笔记 stale 来自 `derived_artifacts.stale=1`。 |

结论：字面后端正确且有测试覆盖，但缺少语义召回、融合打分和“为什么召回这一条”的逐条来源。

## 2. 数据流

MCP 输入 → `Package` 的有序流水线（每一步都命名了实现函数）：

1. **字面召回** `Store.SearchMemory`（`retrieval.go:513`）→ `addHits`（`build.go`）。
2. **语义召回**（本轮新增）`semantic.SemanticRetriever.Search` → `addItem`（`SourceSemantic`）。
3. **导航** `Store.Query`（`store.go:348`）→ `addQueryPage`（`SourceNav`）。
4. **已确认链接扩展** `pipeline.ListLinks` → `addQueryPage`（`SourceLink`）。
5. **排序去重** `candidateSet.sorted` → `dedupeBySourceEvidence`。
6. **预算** 预算循环（`build.go` 步骤 6）。
7. **证据窗口** `Store.GetCurrentEvidenceContext`（仅当前轮次）。
8. **可选重排** `Reranker.Rerank`（缺失即降级，不清空结果）。

所有分支都必须经过 `addItem`：它先拒绝 `item.Stale`，再经 `requestScope.allows` 施加 conversation/topic/time 硬过滤。

## 3. Hybrid 设计

### 3.1 边界

```go
type Provider interface {
    ID() string
    Model() string
    Dims() int
    Embed(ctx context.Context, texts []string) ([]Vector, error)
}

type SemanticRetriever interface {
    Search(ctx context.Context, query string, opts SearchOptions) ([]SemanticHit, error)
    Ready() bool
    Describe() string
}
```

- `internal/semantic` 负责向量编解码、provider、索引与相似度检索；`contextbuild` 只依赖 `SemanticRetriever` 接口。
- 默认 provider 是离线确定性 `HashProvider`（`hash-ngram-v1`，256 维，FNV-1a 有符号哈希技巧 + CJK 2-gram）。真实 OpenAI 兼容 embedding 端点通过 `OpenAICompatProvider` 显式开启。
- 后端不可用（`Deps.Semantic == nil` 或 `!Ready()`）时**必须降级**，绝不向上抛错：hybrid 退化为字面检索并记录原因。

### 3.2 索引表

`memory_embeddings`（迁移 v12 `memory-embeddings`）：`PRIMARY KEY(kind, artifact_id, provider_id, model_id)`，`content_sha256` 使内容未变的重复索引成为 no-op。索引**包含 stale 与 ambient 工件**（查询路径负责过滤），从而让隔离不变量可被测试。索引写入由 CLI `bin/context-embed` 执行，不是 MCP 写工具。

### 3.3 融合

分支权重（固定、文档化）：

| 分支 | 常量 | 权重 |
|---|---|---|
| 字面 | `SourceLexical` | 1.0 |
| 语义 | `SourceSemantic` | 0.8 |
| 已确认链接 | `SourceLink` | 0.45 |
| 导航 | `SourceNav` | 0.30 |

融合公式：对同一 (kind, id) 候选，按分支取 `max` 原始分，`score = Σ branchWeights[b] * raw[b]`。可选 recency bonus（仅当请求带 `Now` 时）为 `0.10 * exp(-ageDays/30)`，未知开始时间不加分——因此不注入时钟时结果保持确定性。

### 3.4 降级模式

| 触发 | `Degraded` | `Gaps` kind | 行为 |
|---|---|---|---|
| 未配置语义后端 | true | `semantic_unavailable` | 仅字面检索 |
| 语义索引为空 | true | `semantic_index_empty` | 仅字面检索 |
| 未配置重排器 | true | —— | `DegradeReason` 保持 `degradeNoReranker` |
| 重排失败 | true | —— | 保留确定性结果 |

### 3.5 不变量与执行位置

| 不变量 | 执行位置 |
|---|---|
| stale 不进入结果 | `addItem` 拒绝 `item.Stale`；语义检索 SQL 内 `CurrentTurnPredicate`/`NoteStalePredicate` |
| ambient 不进入结果 | 语义检索 SQL 内 `turn_classes` 子句；笔记按 ordinal 0 判定 |
| 作用域不可逃逸 | `requestScope.allows`（conversation/topic/time） |
| 被取代的修订不复活 | 语义检索 SQL 的 stale 子句 + `addItem` |
| 仅 confirmed 链接参与 | `build.go` 步骤 3 的 `Status=confirmed` 与 `!link.Stale` |

## 4. 为什么不用 FTS5

`modernc.org/sqlite` 已编译 FTS5（其自带测试会建 `fts5` 表），但本轮明确不用：FTS5 会引入第二套索引，需要与 `turns`/`notes` 的修订、stale、ambient 生命周期同步，任何漏同步都会造成检索与当前状态不一致；而当前语料是本地小规模，字面全表扫描成本可接受。本轮字面分支保持其原有命中打分（笔记 1.0、轮次 0.5），未新增 token/n-gram 覆盖率打分；覆盖率打分与 FTS5 一样列为未来可选优化（见 README 的“计划”列表）。

## 5. 如何索引与运行

```sh
# 索引（hash provider，默认）
bin/context-embed --db data/context-memory.db --provider hash
# 仅索引一个会话
bin/context-embed --db data/context-memory.db --provider hash --conversation conv-xxx
# 清理工件已不存在的索引行
bin/context-embed --db data/context-memory.db --prune

# 真实 embedding 端点（opt-in）
bin/context-embed --db data/context-memory.db --provider openai \
  --model-url https://api.example.com/v1/embeddings --model text-embedding-3-small \
  --key-env OPENAI_API_KEY
```

`build_context` 新增 `mode` 字段：`""`/`"hybrid"`（默认，字面 + 语义 + 导航 + 已确认链接）与 `"lexical"`（关闭语义分支，导航与已确认链接保留）。

MCP server 新增标志：`--semantic-provider`（`off|hash|openai`，默认 `hash`）、`--semantic-dims`（默认 256）、`--semantic-endpoint`、`--semantic-model`、`--semantic-key-env`。`off` 时语义检索器为 nil，`build_context` 与 `get_current_context` 的查询路径退化为仅字面。
