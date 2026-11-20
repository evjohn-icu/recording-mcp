# Spec Delta
## ADDED Requirements
### Requirement: 显式 v3 候选准入
系统 MUST 仅在显式 `retrieval_version=v3` 时启用候选准入，并保留 v1/v2 与默认 v1+hash。准入 MUST 位于融合与硬过滤之后、dedupe 与预算之前；只有准入项可以进入 Items/Evidence，被拒候选 MUST NOT 把正文写入答案证据。
#### Scenario: CE04-B1-01
- **WHEN** v3 候选与问题话题相似但缺少所问事实
- **THEN** 候选被拒，Items/Evidence 为空或被拒候选不出现，trace 记录候选与被拒计数
#### Scenario: CE04-B1-02
- **WHEN** 候选原文直接否定问题前提
- **THEN** relation=refutes 且达到阈值时准入，作为负向证据
#### Scenario: CE04-B1-03
- **WHEN** 候选 pool 超过 `RerankCandidateLimit`
- **THEN** 超出项记入 `pool_limited_ids` 与 `admission_pool_limited` gap，不进入答案

### Requirement: 准入需要完整当前证据
准入 MUST 使用 item 自身引用 turn 的当前 revision 证据，逐条执行 scope/ambient 检查并按时间与 turn ID 稳定排序；缺少任一必需 ref MUST 拒绝完整性；MUST NOT 用邻居 turn 冒充引用。conversation 项 MUST 只作导航，必须有合法 source refs。
#### Scenario: CE04-B1-04
- **WHEN** 跨 session 答案缺少一个必需 ref
- **THEN** 该候选不 sufficient，缺失 ref 被记录
#### Scenario: CE04-B1-05
- **WHEN** 候选引用了过期 revision 或 scope 外 turn
- **THEN** 该候选被拒，不进入证据
#### Scenario: CE04-B1-06
- **WHEN** 候选是未确认身份或未决提议
- **THEN** 不得当作已确认事实，评估状态保留 unknown speaker/current 来源

### Requirement: 策略、失败与降级
`AdmissionPolicy` MUST 含 `schema_version:"evidence-admission-policy-v1"`、retrieval_version、provider/model/revision、assessor_model、fixture_sha256、threshold 与 `phase:"calibration"|"selected"`。缺 policy、标识不匹配或评估失败 MUST 使 v3 返回空 Items/Evidence、sufficiency=unknown、degraded 与 `admission_unavailable|admission_failed`，MUST NOT 静默回退 v1。
#### Scenario: CE04-B1-07
- **WHEN** assessor 调用失败或 policy 缺失/不匹配
- **THEN** v3 输出 unknown 与明确 gap，不返回未经准入的证据
#### Scenario: CE04-B1-08
- **WHEN** 服务收到 `phase:"calibration"` 策略
- **THEN** Web/MCP 拒绝启用，仅 context-eval 在显式校准模式下接受

### Requirement: 概率、阈值与排序
准入概率 MUST 为 P(supports)+P(refutes)，关系 MUST 为概率分布 argmax（tie 固定 supports/refutes/related/unknown 顺序）；只有 supports/refutes 且概率达到校准 threshold 才准入。准入概率 MUST NOT 用于排序；准入项保持原序，配置 reranker 时 MUST 按合法 permutation 重排。
#### Scenario: CE04-B1-09
- **WHEN** 模型输出非法 choice、非有限概率或未归一分布
- **THEN** 评估失败并返回 `admission_failed`，不产生准入项
#### Scenario: CE04-B1-10
- **WHEN** 单候选完整证据超过 64KiB
- **THEN** 该候选被拒绝，不截断正文

### Requirement: 空查询导航与充分性
空 query 的显式导航请求 MUST 仍可返回导航上下文，并标 sufficiency=unknown 与 `assessment_query_missing`；MUST NOT 声称回答问题。sufficient MUST 具备有效 refs；缺证据为 insufficient，服务不可用为 unknown。
#### Scenario: CE04-B1-11
- **WHEN** 空 query 的导航请求
- **THEN** 返回导航上下文且 sufficiency=unknown
#### Scenario: CE04-B1-12
- **WHEN** 两条 current 相反陈述或未知 recording time
- **THEN** 可同时准入但不足以回答“最终结论”，sufficiency=insufficient

### Requirement: v3 冻结评测与默认值
v3 评测 MUST 使用 `fixtures/eval/quality-v3-r1/`（11 类 × 每 split 10 组，每 split 260 问题，共 780），split=train/calibration/test（test 即 holdout），问题 MUST 声明 `evidence_completeness`、`expect_forgotten_excluded`，并使用 `lifecycle.json` 对临时库真实遗忘后检索。threshold 网格 MUST 为整数 `threshold_hundredths` 0..100，仅 calibration 选择。
#### Scenario: CE04-B2-01
- **WHEN** 校验 v3 语料
- **THEN** 跨 split 的 group/source/conversation/别名/规范化 query 重复被拒绝，SHA 与 split 隔离通过
#### Scenario: CE04-B2-02
- **WHEN** calibration 网格评估
- **THEN** 按 Recall@5↓、MRR@10↓、p95↑、threshold_hundredths↓ 选择唯一候选

### Requirement: 门禁、锁与一次消费
calibration 筛出的唯一候选 MUST 在相同硬件、禁用评估缓存、一致 warmup 下完整复跑一次用于门禁与 lock；lock MUST 原子写并钉住 fixture/binary/policy hash、模型/prompt 版本、阈值与预算。holdout MUST 在 lock 同目录以 `O_CREAT|O_EXCL` 原子 claim 后运行，失败不可重用。
#### Scenario: CE04-B2-03
- **WHEN** 任一 gate 失败或缺指标/零分母/调用失败
- **THEN** 不生成 lock，holdout NOT_RUN，default_changed=false
#### Scenario: CE04-B2-04
- **WHEN** holdout 已消费或 lock 被修改
- **THEN** 运行拒绝启动，不产生 test 模型请求

### Requirement: v3 门禁指标
v3 MUST 满足 Recall@5≥.85、无证据 query FP≤.05（任一准入 item 即 FP）、FalseSufficient≤.05、stale/ambient/scope/forgotten/invalid-ref/cross-split 泄漏全 0，并保留相对门禁：Recall 严格优于同 provider v1、MRR 不退化、端到端 p95≤v1×1.5。MUST 报告逐 bucket 指标、冻结模型 cold-load 与 query warm latency 边界。
#### Scenario: CE04-B2-05
- **WHEN** 真实 embedding/assessor 不可用
- **THEN** 该配置记 BLOCKED，合成或 hash 测试不能替代真实门禁
#### Scenario: CE04-B2-06
- **WHEN** 全部 gate 通过
- **THEN** 生成 selected policy 与 selection-lock，仅建议显式启用 v3，默认仍为 v1
