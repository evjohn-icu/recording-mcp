# Tasks
## 1. 中文检索
- [x] 1.1 先写失败测试，再实现v2 lexical与隔离，运行memory测试并记录命令。
## 2. 组装与充分性
- [x] 2.1 先写失败测试，再实现有界预算前重排与充分性状态，运行contextbuild测试并记录命令。
- [x] 2.2 贯通调用入口和版本参数，以入口测试验证默认v1和显式v2。
## 3. 冻结评测
- [x] 3.1 独立创作冻结240问与来源组，先写校验失败测试再校验哈希和分区。
- [x] 3.2 扩展CLI和评测矩阵、门禁、故障记账，以Go/Python测试验证并记录实验结果。
## 4. 集成
- [x] 4.1 更新运行文档与测试映射，执行spec-validate、go vet、race测试及质量实验，记录真实命令和限制。

## Workflow follow-up
- 实际命令、红绿测试、失败与限制见 docs/retrieval-quality-v2-validation.md。
- 质量门槛未通过，默认保持v1。真实embedding/reranker/assessor未配置，记NOT_RUN；test按门禁未运行。上述状态不是发布验收通过。
- 后续有模型配置时可另做校准；通过门槛前不得替换默认。归档不在本轮请求内。
