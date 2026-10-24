# Design
## Context
现有 SearchMemory 为子串匹配，Build 在预算后重排；质量脚本只比较 hash 阈值。旧 test 已暴露。
## Goals / Non-Goals
Goals: 实施可显式启用且可独立评测的 v2。
Non-Goals: 自动切换默认、自然语言答案生成、修改 ingest-v1、读取私人资料、数据库迁移。
## Decisions
- retrieval_version=v1|v2，空值为 v1。v2 中文双字 gram、英文数字 token 的 BM25（k1=1.2,b=.75）与精确命中加分；单字查询支持单字匹配，不改旧分页。
- 所有分支先过滤来源版本/scope/ambient/stale，融合去重后取默认64、最多128候选；模型输入64KiB上限。重排只允许输入排列，错误回退确定性顺序，随后执行最终max_items/max_bytes。
- 独立 EvidenceAssessor 判定最终证据充分性，输出 sufficient/insufficient/unknown 与受校验引用；保留部分证据，模型错误为unknown，未配置不得伪装成功。
- 独立240问，四个正例桶各40、负例80；概念及相关来源组50/25/25隔离。冻结SHA-256后才实验，训练开发、校准选择、锁定后test一次揭盲。
- 正式评测max_items=10,max_bytes=65536；比较v1/v2 lexical、hash、配置可用的真实embedding以及对应rerank开关。显式配置才调用模型，缺配置NOT_RUN。
- 门槛：Recall@5严格提升，MRR@10不退步，负例查询误召回率<=5%，零隔离/引用泄漏，p95<=同配置v1的1.5倍。充分性误判充分率<=5%。失败和缺指标不得通过。保持默认v1。
## Risks / Trade-offs
- 小样本与本地计时波动：公开分母、逐问结果及环境，不夸大质量。
- 模型不可用：真实模型记NOT_RUN，mock仅验证协议。
- test暴露：后续调参需新版本独立holdout。
## Migration Plan
仅显式启用v2，无迁移；关闭v2即回到旧路径。

## Review Corrections
首次冻结的 quality-v2 仅通过形状校验，语义审查发现否定/变化/跨会话证据不充分，故否决其作为验收集。保留旧字节和诊断报告；新 quality-v2-r2 逐概念独立编写并经过不参与调参的语义审查后冻结。不得将首批分数描述为质量提升证明。
充分性模型只读取最终预算内返回的 evidence 或 raw turn 文本；不能通过加载 note 引用的未返回原文绕过 max_bytes。配置 assessor 的 v2 请求会在同一预算中附加可审计的原文。v1 重排适配器行为保持原状，严格校验只在 v2 使用。

R2通过语义审查但首次真实导入因 original_index=0 失败，未发生检索评分。保留R2冻结文件，R3仅规范化turn索引，问题和金标逐字节不变；先进行零问题、零检索请求的导入预检，再冻结运行。当前默认评测入口使用 quality-v2-r3。
