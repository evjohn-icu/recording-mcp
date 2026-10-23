# 规范驱动工作流

## Why

仓库需要让 OpenSpec 场景与自动化验证可追溯，同时避免把映射脚本、结构校验误读为测试通过或 TDD 证据。

## What Changes

- 行为提案以 GIVEN/WHEN/THEN 描述可观察结果，并使用稳定场景 ID。
- `openspec/test-map.json` 显式区分 covered 与 pending；covered 表示精确测试锚点存在，不表示已运行或充分。
- 验证记录分开列出 OpenSpec CLI、映射检查和实际测试命令/结果；不得推断测试先于实现。

## Capabilities

- `workflow`

## Impact

影响 OpenSpec 提案格式、场景映射和校验记录。冻结的 `contracts/ingest-v1/` 仍是采集协议权威；`docs/specs/` 的 Context Layer 0.1 仍是未来草案。
