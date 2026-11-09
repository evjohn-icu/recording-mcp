# Sprint 3 UI/UX 检查

## 范围

本轮新增或调整的用户可见面：

| 界面 | 位置 | 内容 |
|---|---|---|
| 搜索 | `SearchPage` | 证据充分性面板：充分/部分/不足/未评估、理由、可点击证据引用 |
| 录音详情 | `DetailPage` 后台处理 | 转写覆盖 `已完成/预期窗口` 与时长、本地 GPU 名称/显存/设备、ASR 错误 |
| 导入面板 | `ImportPanel` | ASR 能力状态与失败原因（未配置、worker 离线、GPU 不可用、模型初始化失败） |

## 本轮修复

1. **转写覆盖可见性**：覆盖信息原在折叠的“诊断（实验）”内，用户默认看不到。现移到“后台处理”标题下方，与阶段进度同级；诊断仅保留索引与轮次计数。
2. **移动端搜索表单空白**：`@media (max-width: 480px)` 将 `.search-controls` 改为纵向后，`.search-input-label` 的 `flex: 1 1 220px` 被解释为 220px 高度，关键词输入框下方出现大块空白。移动端覆盖为 `flex: 0 1 auto`，空白消失。

## 证据

| 检查 | 结果 | 说明 |
|---|---|---|
| `scripts/product-workflow-browser.py` | 14/14 | 新增 `web-asr-coverage`：覆盖行可见且含“窗口”；移动布局检查同时要求覆盖行可见 |
| `scripts/sufficiency-browser.py` | 12/12 | 新增 `sufficiency-mobile-layout`：390×844 下无横向溢出、面板可见 |
| 移动截图 | 通过 | `sufficiency-panel-mobile.png`、`product-workflow-mobile.png`（运行时 `BROWSER_ARTIFACT_DIR`） |

实际命令（Playwright 1.60.0 venv，本地 vite preview）：

```sh
BROWSER_ARTIFACT_DIR=/tmp/uiux/suff BASE_URL=http://127.0.0.1:4173 \
  python scripts/sufficiency-browser.py
python scripts/run-product-browser.py --artifacts /tmp/uiux/product
```

## 观察与限制

- 充分性面板在桌面与 390px 宽度下均无溢出；证据引用为短标签，长窗口仍由现有结果卡片限宽。
- 三种充分性状态与“未评估”文案由浏览器脚本覆盖；颜色依赖现有 `--success`/`--warning`/`--danger`，深色模式未截图。
- `ImportPanel` 的 GPU 不可用/模型初始化失败文案只做代码与 Go handler 测试核对，浏览器套件未伪造能力接口，因此未在浏览器中断言。
- 决策轨迹页面仅桌面截图；移动端布局沿用既有响应式规则，本轮未改动。
