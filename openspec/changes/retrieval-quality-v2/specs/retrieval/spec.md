# Spec Delta
## ADDED Requirements
### Requirement: v2 中文召回保持隔离与兼容
系统 MUST 显式选择 v2 才启用中文 token 召回，保留来源版本、scope、ambient、stale 过滤和默认 v1。
#### Scenario: retrieval.v2-lexical-isolation
- **WHEN** v2 查询中文改写且含过期、环境声及范围外干扰
- **THEN** 返回相关当前证据，不发生过滤泄漏
### Requirement: 有界候选先重排再预算
系统 MUST 对有界候选池重排后才执行最终条目和字节预算，模型不得新增或修改来源对象。
#### Scenario: retrieval.v2-prebudget-rerank
- **WHEN** 重排将候选池尾部项提升且最终预算较小
- **THEN** 提升项能进入最终结果，非法排列明确失败并回退
### Requirement: 充分性与故障明确区分
系统 MUST 返回 sufficient、insufficient 或 unknown 及合法证据引用，模型故障不能报告正常证据不足，部分证据保留。
#### Scenario: retrieval.v2-sufficiency
- **WHEN** 模型判不足、调用失败或引用范围外证据
- **THEN** 正常不足与故障分别记录，失败输出 unknown
### Requirement: 独立冻结评测与默认门禁
v2 MUST 使用独立240条中文问题和来源组隔离的50/25/25分区，冻结哈希并在校准后锁定候选。报告Recall@5、MRR@10、负例查询误召回率、p95及分母。缺配置模型记NOT_RUN，失败不得通过，默认不自动切换。
#### Scenario: retrieval.v2-frozen-evaluation
- **WHEN** 校验并评估v2冻结语料
- **THEN** 跨分区来源组不泄漏，真实模型状态诚实记录，质量门槛失败不改变默认
