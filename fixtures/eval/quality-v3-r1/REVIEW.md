# v3 r1 冻结前自检记录

以下各项由生成器自检程序逐项确认（生成器结构自检，非人工语义审查）。
自检命令：`python3 scripts/generate-quality-v3-fixture.py --out fixtures/eval/quality-v3-r1 --self-check`

- [x] 组与问题总数：330 组、780 题；train/calibration/test 各 110 组、260 题；
  每 bucket × split 恰 10 组。
- [x] 组内构成：每组 1 个 complete + 1 个 none；partial 仅出现于
  time_change/revocation/missing_evidence/wrong_identity_proposal（每 split 40 题、全量 120 题）。
- [x] 分区隔离：group_id / source_group_id / segment_id / conversation_id / 金标引用均不跨 split。
- [x] 问题唯一性：strip+casefold 后 780 条查询互不重复，均为中文，且每组查询携带唯一的三位编号字面量
  （组间唯一，同时出现在金标话轮文本中）。
- [x] cross_session complete 问题引用两个不同会话；multi_source complete 问题引用至少两个话轮。
- [x] time_change / revocation 组含 r1 旧话轮与 r2 当前话轮的冲突事实；complete 金标只指向当前话轮。
- [x] wrong_identity_proposal 组含未确认 speaker（speaker_entity_id 为空）的提议话轮；
  complete 金标答案只报告“尚未确认”，不断言身份已确认。
- [x] numeric_constraint 查询中的数字（二十）出现在其话轮文本中。
- [x] lifecycle.json：30 条来源，全部为 revocation 组的 r1 旧话轮并带 reason；
  对应组三个问题均带 `expect_forgotten_excluded`；旧话轮文本与组查询共享编号与场地，遗忘缺失可观察。
- [x] SHA256SUMS.json 覆盖 corpus.json / retrieval.json / lifecycle.json，与文件实际哈希一致。

本记录仅证明生成器结构自检通过；不代表任何模型质量门槛通过，也不替代人工语义审查。
