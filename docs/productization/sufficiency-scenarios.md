# 证据充分性场景（SMART-04）

21 个具名场景位于 `fixtures/sufficiency/v1/scenarios.json`，通过真实 v2 构建器执行。每个场景使用确定性评估器：只接收最终预算后的证据，按 `required_terms`/`forbidden_terms` 返回三种状态；错误、越界引用、空引用由固定脚本注入。因此期望状态不依赖真实评估器实现，真实评估器质量在 `docs/productization/real-embedding-evaluation.md` 单独记录。

## 执行命令

```sh
SUFFICIENCY_REPORT="$PWD/artifacts/sufficiency/v1/results.json" \
  go test ./internal/contextbuild -run TestSufficiencyScenarios -count=1
go test ./internal/app -run TestDecisionTrailSufficiencyScenarios -count=1
```

`SUFFICIENCY_REPORT` 必须是绝对路径（`go test` 的工作目录是包目录）。未设置时只写 `t.TempDir()`。

## 结果

2026-11-09 运行：21 个场景，21/21 通过，其中 11 个不可回答（要求 ≥5）。`artifacts/sufficiency/v1/results.json` 记录每个场景的期望/实际状态、证据引用与 gap。

| 场景 | 类别 | 期望状态 | 关键断言 |
|---|---|---|---|
| direct-factual | 直接事实 | sufficient | 引用具体金额与日期所在轮次 |
| partial-factual | 部分证据 | insufficient | 相关证据存在但缺少截止日期 |
| cross-session-decision | 跨会话 | sufficient | 两个会话中只引用目的地所在轮次 |
| changed-decision | 决策变更 | insufficient | 过期归纳被排除，但旧轮次仍是转写证据，评估器拒绝把新旧决定合并 |
| unconfirmed-proposal | 未确认提案 | insufficient | 提议不等于已确认 |
| contradictory-statements | 矛盾陈述 | insufficient | 不给出任意单一答案 |
| negation | 否定 | sufficient | 按字面匹配"不再采用" |
| unknown-date | 未知时间 | sufficient | 证据时间保持 null，不推断 |
| missing-evidence | 无证据 | insufficient | 无任何引用 |
| stale-transcript | 过期归纳 | insufficient | 过期 note 不提供证据 |
| forgotten-source | 已忘记来源 | insufficient | 已删除轮次不可引用 |
| ambient-exclusion | 环境音 | insufficient | 环境音轮次不可引用 |
| scope-isolation | 会话隔离 | insufficient | 不泄漏另一会话证据 |
| budget-exclusion | 字节预算 | insufficient | 超出预算的证据不得声称充分 |
| assessor-failure | 评估器失败 | unknown | gap `evidence_assessment_failed` |
| assessor-invalid-refs | 非法引用 | unknown | 越界引用使评估无效 |
| no-assessor | 未配置评估器 | unknown | gap `evidence_assessment_unavailable` |
| no-evidence | 空证据 | insufficient | 有评估器也不猜测 |
| sufficient-empty-refs | 空引用充分 | unknown | `sufficient` 且无引用被判无效 |
| proposal-vs-confirmed | 提案/确认 | sufficient | 只以已确认记录满足"已确认" |
| forgotten-decision | 忘记决定 | insufficient | 过期决定不被复活 |

决策轨迹语义另由 `internal/app` 的 `TestDecisionTrailSufficiencyScenarios` 验证：未确认提案的 `Current` 为空、已忘记来源的确认不再当前、未知录音时间时 `valid_from` 保持 null。

## 界面

`scripts/sufficiency-browser.py` 在既有 Search 页面用 Playwright mock 三种状态，验证标签、证据文本、时间、链接、gap 与 v1 不渲染：

```sh
cd web && npm exec vite -- preview --host 127.0.0.1 --port 4173 --strictPort &
BROWSER_ARTIFACT_DIR=artifacts/sufficiency-browser BASE_URL=http://127.0.0.1:4173 \
  python scripts/sufficiency-browser.py
```

2026-11-09 本地运行：12/12 检查通过（新增 390px 移动布局检查，断言无横向溢出且面板可见）；截图写入运行时的 `BROWSER_ARTIFACT_DIR`（本机为临时目录，`artifacts/` 不入库），发布报告记录命令与检查数。

## 边界

这些是契约场景，不是真实评估器的质量指标。假充分率、不可回答率等统计只来自冻结评测的真实评估器运行；未配置评估器时记 `NOT_RUN`，不记 0%。
