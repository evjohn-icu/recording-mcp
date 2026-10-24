# Proposal
## Why
旧检索依赖整句子串且重排晚于预算截断，旧评测 test 已暴露，不能支持新的质量声明。
## What Changes
- 新增显式 v2 中文召回、有界预算前重排、证据充分性状态。
- 独立冻结合成评测，比较 lexical/hash/真实 embedding/rerank，失败不得切换默认。
## Capabilities
### New Capabilities
无。
### Modified Capabilities
- `retrieval`: 增加 v2 质量、隔离与充分性行为。
## Impact
memory/contextbuild、调用入口、eval CLI、Python 评测与文档。默认 v1，不修改 ingest-v1 或私人数据。
