# Retrieval quality v3 r1 authoring notes

本目录的 corpus.json / retrieval.json / lifecycle.json 全部由确定性模板生成器合成，
生成命令：`python3 scripts/generate-quality-v3-fixture.py --out fixtures/eval/quality-v3-r1`。

- 话轮与查询都是模板生成的合成文本，用于 evidence admission / assessor / lifecycle 的**协议校准**；
- **不是人工裁定（human-adjudicated）的真实数据**，不能作为真实检索质量或模型语义能力的证据；
- 全部人物、地点与事件均为虚构，不含任何私人录音或生产数据；
- 生成过程不含时间戳与随机数，重跑输出逐字节一致，SHA256SUMS.json 固定三个 JSON 的哈希。

## Bucket composition

| bucket | 说明 | 每组问题数 | 该 bucket 总问题数 |
|---|---|---|---|
| direct_fact | 同义直问：查询直接指向事实话轮 | 2 | 60 |
| synonym | 同义改写：查询换一种说法指向同一事实 | 2 | 60 |
| negation | 否定排除：事实话轮包含明确的否定或排除表述 | 2 | 60 |
| time_change | 时间变化：r1 旧值与 r2 当前值冲突，complete 金标只指向当前话轮 | 3（含 1 个 partial） | 90 |
| cross_session | 跨会话：两个会话各提供一半事实，complete 金标引用两个 conversation | 2 | 60 |
| multi_source | 同会话多来源：complete 金标引用同一会话的两个话轮 | 2 | 60 |
| revocation | 撤回：r1 旧通知被 r2 撤回公告取代；旧话轮列入 lifecycle.json | 3（含 1 个 partial） | 90 |
| missing_evidence | 证据缺口：partial 问题只有一半材料，按协议标记 insufficient | 3（含 1 个 partial） | 90 |
| wrong_identity_proposal | 身份提议：未确认 speaker 提出人员提议，complete 金标不断言身份已确认 | 3（含 1 个 partial） | 90 |
| numeric_constraint | 数值约束：查询中的数字出现在其话轮文本中 | 2 | 60 |
| decision_commitment | 决定承诺：事实话轮是明确的最终决定表述 | 2 | 60 |

## Totals

- 组：330（train/calibration/test 各 110；11 个 bucket × 10 组 × 3 split）。
- 问题：780（每 split 260 = complete 110 + none 110 + partial 40）；
  全量 complete 330、none 330、partial 120。
- partial 仅出现在 time_change / revocation / missing_evidence / wrong_identity_proposal。
- lifecycle.json：30 条来源，全部为 revocation 组的 r1 旧话轮。

## Conventions

- 每组唯一锚点：`编号{code}`，code 由组序号决定（101..430），出现在事实话轮与该组全部问题文本中；
  查询中的三位数字字面量即该组唯一编号。
- time_change / revocation 组为同一 segment 的 r1/r2 两个修订（old/current 冲突事实），
  complete 金标只指向 r2 当前话轮。
- cross_session 组由两个独立会话（两个 conversation）组成，complete 金标各引用其一。
- 标签约定：complete=unanswerable=false/expected_sufficiency=sufficient；none=unanswerable=true/
  expected_sufficiency=insufficient 且无金标引用；partial=unanswerable=false/
  expected_sufficiency=insufficient，可引用一个相关话轮。
- revocation 组三个问题均带 `expect_forgotten_excluded: true`。
