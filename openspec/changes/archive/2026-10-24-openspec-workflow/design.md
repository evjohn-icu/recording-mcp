# 设计：规范驱动与证据边界

## Context

仓库以 OpenSpec 记录变更和现有实现基线。脚本可检测场景 ID 与精确测试函数锚点存在，但并不执行这些测试，也无法证明测试充分、测试先于代码编写或行为正确。

## Decisions

- 场景采用稳定 ID 与具体 GIVEN/WHEN/THEN，不使用“执行场景”或重复 boilerplate 替代预期结果。
- 每个 change 场景必须映射 covered 或 pending；covered 需列文件与函数锚点，pending 不伪造锚点。
- `make spec-validate` 仅验证 OpenSpec 结构及映射完整性；实际测试必须单独执行并准确记录命令和结果。
- Tasks 仅在有执行证据时勾选完成；代码/测试文件存在不是验证完成证据。
- 不修改冻结 ingest-v1 契约；Context Layer 0.1 仍是未来草案，不将计划描述成当前能力。

## Risks

映射脚本只能检查结构和锚点存在。若记录者把 covered 写成测试通过或 TDD 证明，将造成错误审计结论。
