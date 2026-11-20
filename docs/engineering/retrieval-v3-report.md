# Retrieval v3（证据准入）报告

状态：**实现完成；真实模型门禁在上次运行中 FAIL（p95），本轮无受信模型配置因此 profile/calibration/holdout
均为 BLOCKED/NOT_RUN，默认值未改变**。
本文件只记录实际执行过的检查；没有运行的真实模型/门禁一律写 BLOCKED 或 NOT_RUN。

## 1. 交付内容

| 项 | 位置 |
|---|---|
| 准入类型、策略、决策适配器 | `internal/contextbuild/admission.go` |
| 候选证据水合、最终证据元数据 | `internal/contextbuild/admission_inputs.go` |
| Build 集成（排序后、预算前） | `internal/contextbuild/build.go` |
| `EvidenceRef` 可选来源元数据 | `internal/contextbuild/build.go` |
| 评估器调用缓存（仅 calibration） | `internal/eval/admission_cache.go` |
| 生命周期遗忘与泄漏指标 | `internal/eval/harness.go`、`internal/eval/retrieval.go` |
| v3 评测协议（阈值网格、锁、一次性 holdout） | `scripts/run-retrieval-quality-v2.py --protocol v3` |
| v3 冻结夹具 | `fixtures/eval/quality-v3-r1/`（生成器与 REVIEW 见该目录） |
| 无模型协议运行记录 | `docs/engineering/retrieval-v3-metrics.json` |
| 时延 profile 协议路径 | `docs/engineering/followup/v3-profile-notrun.json` |
| lock v2 与 holdout 恢复 | `scripts/run-retrieval-quality-v2.py`、`scripts/test_retrieval_quality_v2.py` |
| 服务入口 | `cmd/context-eval|context-memory|context-web --admission-policy` |
| Web 面板 | `web/src/api/types.ts`、`SearchPage.tsx`、`SufficiencyPanel.tsx` |

## 2. 行为与安全边界

- 默认检索为 `v1` + hash provider。搜索页只在能力探测确认准入策略与模型匹配时提供 v3 选项。URL 显式请求 `retrieval_version=v3` 时，搜索页保留该值并发送给服务端，由服务端返回准入结果或降级信息。
- 缺 policy、标识不匹配、`phase=calibration` 被服务端拒绝、评估失败 → v3 返回空 Items/Evidence、
  sufficiency=unknown、degraded 与 `admission_unavailable|admission_failed`；不回退 v1/v2。
- 空 query 的显式导航返回导航上下文，`assessment_query_missing`，不声称回答问题。
- 64 KiB 输入按稳定顺序分批；单个候选完整证据放不下时拒绝该候选，不截断正文。
- 准入概率不参与排序；准入项保持原序，配置 reranker 时按合法 permutation 重排。
- 证据元数据（recording/revision/时间/confirmed speaker）来自当前 store，未确认身份保持 unknown。

## 3. 实际执行的检查

| 命令 | 退出码 | 结果 |
|---|---|---|
| `go test -count=1 ./internal/contextbuild -run TestBuildV3AdmissionAndSufficiency` | 0 | 16 个子用例通过（拒绝/负向证据/缺 ref/proposal/失败/取消/policy/阈值/pool/计数/空 query） |
| `go test -count=1 ./internal/eval` | 0 | 既有评估协议回归通过 |
| `python3 -m unittest scripts.test_retrieval_quality_v2` | 0 | v2 协议 19 项 + v3 协议 5 项通过 |
| `python3 -m unittest scripts.test_long_audio_reliability` | 0 | 8 项通过（D 线 manifest 诚实性） |
| `npm run typecheck --prefix web` / `npm run build --prefix web` | 0 / 0 | v3 UI 类型与构建通过 |
| `node --experimental-strip-types --test src/api/retrieval-version.test.mjs` | 0 | 6 项通过 |

## 4. 真实门禁状态

| 项 | 状态 | 原因 |
|---|---|---|
| 真实 embedding（Qwen3-Embedding-0.6B CPU） | **CONFIGURED** | 本地服务，`97b0c614...`，1024 维，请求成功 |
| 真实 assessor（magpie Jev） | **CONFIGURED** | `typesafe/jev-latest` → `jev-1.14.0`，`magpie /v1/systemone`，batch=32 |
| calibration 阈值 0（openai 后端） | **FAIL（p95）** | Recall@5=0.875（v1 0.7933）、MRR@10=0.8786（v1 0.2539）、FP=0、FalseSufficient=0；p95=2444.7ms vs v1 308.5ms（1.5× 上限 462.7ms） |
| 其余阈值 | **NOT_RUN** | 准入成本与阈值无关，p95 不会因阈值变化；网格在阈值 0 后停止 |
| selection-lock 生成 | **NOT_RUN** | 无候选通过全部门禁 |
| holdout 一次性执行 | **NOT_RUN** | 无 lock；未生成任何 holdout 请求 |
| 默认值切换 | **未发生**（`default_changed=false`） | 门禁未通过 |

合成/夹具测试只能证明协议、过滤与失败路径，不能替代真实 embedding/assessor 门禁；本报告不据此声称
v3 质量达标。通过真实门禁后也只建议显式启用 v3，不自动改默认。

## 4b. 本地运行时 spike（可行性，未进入正式门禁）

按计划做了一次有界 spike，只验证“本地低时延 decision 运行时能否把 admission 压进预算”，没有改冻结夹具。

| 实验 | 结果 |
|---|---|
| 现有本地 2B Jev 服务（:8765，llama.cpp Q8，串行） | 单题约 29ms；4 题 110ms、8 题 231ms、16 题 428ms、32 题 856ms、40 题 1073ms |
| 0.8B-v3 Q4 GGUF + 32k context + prompt 前缀复用 | 4 个长候选约 93ms（warm）；但真实评估 admission p95 711ms、最终 assessor p95 373ms、总 p95 1882ms |
| 把 admission pool 降到 4（train 开发集，threshold 0） | Recall@5 **0.386**、MRR@10 0.155；质量门禁 0.85 远不可达 |
| 0.8B GGUF 的 letter-logprob 读数 | 短请求返回 0.25/0.25/0.25/0.25 均匀分布，说明该 GGUF 未按 letter 读数协议工作，不能直接作为 admission 模型 |

结论：**pool 不能降到 4，质量会崩；pool 保持 40 时本地串行/批处理都超出 150ms 准入预算。**
现有本地运行时方案不成立，spike 停止，不生成 lock，不进入 holdout；v3 维持关闭。若未来重启 v3，
需要真正的“共享状态 + 多 verdict slot 一次前向”运行时，或由产品决定调整 1.5× 时延预算。

## 5. 复跑方式

```sh
go build -o "$CE04_TMP/context-eval" ./cmd/context-eval
python3 scripts/run-retrieval-quality-v2.py --protocol v3 \
  --fixture-dir fixtures/eval/quality-v3-r1 \
  --binary "$CE04_TMP/context-eval" --out "$CE04_TMP/retrieval-v3" --phase calibrate \
  --embed-endpoint http://127.0.0.1:8790/v1/embeddings \
  --embed-model Qwen/Qwen3-Embedding-0.6B --embed-dims 1024 --embed-input-type \
  --embed-revision <受信revision> \
  --assessor-url http://127.0.0.1:3425/v1/systemone --assessor-model typesafe/jev-latest \
  --admission-batch-questions 32 \
  --flush-embed-cache-url http://127.0.0.1:8790/admin/flush
```

真实运行使用本机 magpie 网关的 Jev 决策端点（`typesafe/jev-latest`，实际 `jev-1.14.0`），
准入批大小 32；同一 provider 的 v1 基线为 308.5ms p95。所有正确性/泄漏门禁通过，唯一失败项是
时延：v3 p95 2444.7ms 超过 v1 的 1.5 倍（462.7ms）。准入调用与阈值无关，因此其余 100 个阈值
不会改变该结论；openai 网格在阈值 0 后停止，未生成 lock。另以 hash 后端筛查阈值 0..17：Recall@5
0.829–0.836，低于 0.85 绝对门禁，故 hash 候选也不会被选中。

最后一轮真实运行还暴露并修复了四个校准路径缺陷，均有回归测试：lifecycle forget 需要现场
preview 的 `expected_state`；已遗忘来源的 q3 gold ref 需要在生命周期集合中解析；只有全部 gold
ref 都被遗忘的问题才退出 recall 分母（`recall_eligible`），带 flag 但仍有 live gold 的 revocation
问题必须保留；准入批大小默认 1，因为本地 Jev 适配器对每个 question 串行前向，多问题批会把
prompt 成本乘以批大小。embedding 服务增加进程内 memo，并在最终未缓存门禁前通过
`/admin/flush` 清空，保证时延门禁不含缓存命中。

`--admission-probability-cache` 默认写在输出目录，只用于 calibration 筛选；最终候选必须在相同硬件、
禁用缓存下复跑一次，门禁与 lock 只取该次结果。

---

## 6. 第二轮更新（2026-11-13）

上一轮记录有两处口径需要修正，本轮已按代码如实改写：

1. **“阈值 0 之后停止”是那次运行的操作事实，不是代码保证。** 现在
   `scripts/run-retrieval-quality-v2.py --protocol v3 --phase calibrate` 对每个筛选后端执行完整
   0..100 网格（101 个阈值），无论阈值 0 的结果如何；最终门禁只取“同一候选、禁用概率缓存”的一次复跑。
   测试 `test_v3_calibration_executes_every_threshold_and_one_uncached_rerun` 固定了这个行为。
2. **首行状态与正文不一致**已修正：p95 门禁 FAIL 是上次运行的实际结论，本轮没有重跑真实门禁。

本轮新增内容：

- `--phase profile`：仅 train、threshold=0、不写 lock、不碰 test；逐 query 记录
  `decision.Engine.Evaluate` 的累计墙钟时间与准入请求数/候选数/字节数。判定规则：引擎时间
  p95 已超过同次 v1 的 1.5 倍即 `model-bound`，此时删掉客户端构造成本也不可能达标。
- `internal/contextbuild/admission.go` 的唯一改动：每候选只渲染 block 与 question 一次，
  批次字节数由转义长度累加得到，与整包 `json.Marshal` 逐字节相等（Go 测试固定）。
  它降低构造成本，但**不改变**上一轮 p95 结论，也不代表 v3 已达标。
- lock schema 升为 `retrieval-v3-selection-lock-v2`（完整 configuration、identity、budget、warmup），
  holdout 从 lock 恢复参数并拒绝冲突/概率缓存/旧 schema；每个 v3 phase 只允许新目录或空目录。

本轮实际执行：`--phase profile` 协议路径（无 assessor）→ `runs.profile=NOT_RUN`、无 lock、
`test_touched=false`（产物 `docs/engineering/followup/v3-profile-notrun.json`）。本机没有受信的
embedding/assessor 端点，按计划不扫描缓存、不下载模型，因此真实 profile、完整校准与 holdout 仍为
BLOCKED/NOT_RUN；在拿到受信配置与事先约定的质量阈值前，v3 继续保持关闭。
