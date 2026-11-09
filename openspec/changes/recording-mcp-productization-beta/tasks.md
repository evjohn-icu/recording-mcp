# Tasks

## 0. 基线、安全与 OpenSpec

- [x] 0.1 创建分支并记录基线提交 `3f2865e9d2f41f4d03bc982d1ed8a2958e450433`
- [x] 0.2 运行基线审计命令并写入 `docs/productization/baseline-audit.md`
- [x] 0.3 `scripts/demo.sh` 改为 `mktemp` 临时目录，验证 `data/demo` 指纹不变
- [x] 0.4 建立 `docs/productization/tdd-evidence.md`
- [x] 0.5 建立本 OpenSpec 变更与 test-map pending 条目，`make spec-validate` 通过

## 1. 部署与运行时

- [ ] 1.1 失败测试：WAL 并发读写、`/healthz` 数据库失败、worker 能力心跳、`context-profile verify`
- [ ] 1.2 SQLite/事件平面 WAL、`busy_timeout`、迁移 v20
- [ ] 1.3 `GET /api/capabilities`、处理投影能力字段
- [ ] 1.4 `Dockerfile`、`.dockerignore`、`docker-compose.yml`、`docker-profile-init.sh`
- [ ] 1.5 部署验证测试与 `deployment*.md`

## 2. 导入与 ingest/asr

- [ ] 2.1 失败测试：8 个导入场景 + 真实格式解码
- [ ] 2.2 迁移 v21、`EnsurePendingRecordingTx`、`RecordingView` 元数据
- [ ] 2.3 `internal/ingest`（decoder/service）
- [ ] 2.4 `POST /api/imports` 与错误映射
- [ ] 2.5 worker `ingest/asr` stage 与 fatal 错误语义
- [ ] 2.6 `ProcessingSnapshot.phases` 与测试
- [ ] 2.7 窄测试全绿，test-map 对应项转 covered

## 3. Web 导入与处理体验

- [ ] 3.1 失败测试/脚本：产品浏览器工作流
- [ ] 3.2 类型、client、ImportPanel、Timeline/Detail 阶段 UI、响应式
- [ ] 3.3 `run-product-browser.py` 与 `product-workflow-browser.py`
- [ ] 3.4 8/8 浏览器步骤与附加检查通过

## 4. 音频 E2E 与检索质量

- [ ] 4.1 `scripts/product-audio-e2e.py` synthetic 三段运行
- [ ] 4.2 `fixtures/eval/product-review-v1/` 与检索报告
- [ ] 4.3 检索测试与泄漏计数
- [ ] 4.4 real 模式 `NOT_RUN` 记录与命令

## 5. 决策轨迹后端

- [ ] 5.1 失败测试：24 个具名场景断言
- [ ] 5.2 `WorldlineService.Snapshot`
- [ ] 5.3 `DecisionTrailService` Build/Review、字节预算、时间语义
- [ ] 5.4 `internal/web/decision_trail.go` 路由与错误映射
- [ ] 5.5 24 个测试全绿

## 6. 决策轨迹 Web

- [ ] 6.1 API client 与 `DecisionTrailPanel`
- [ ] 6.2 Worldline 页面集成与样式
- [ ] 6.3 `decision-trail-browser.py` 8 项 + 4 附加断言

## 7. MCP 可信上下文

- [ ] 7.1 失败测试：15 个 MCP 契约测试
- [ ] 7.2 `get_decision_trail` 只读工具与 profile 事件平面修复
- [ ] 7.3 工具清单断言与文档更新
- [ ] 7.4 15 个测试全绿

## 8. 演示与发布

- [ ] 8.1 `fixtures/productization/beta-demo/` 与 `scripts/beta-demo.py`
- [ ] 8.2 `beta-release-checklist.md`、`beta-release-report.md`
- [ ] 8.3 演示 10/10 与重启持久化

## 9. 回归与交付

- [ ] 9.1 全量回归命令并记录退出码
- [ ] 9.2 CI 新增 product-browser 与 docker-deployment job
- [ ] 9.3 test-map 全部转 covered，`make spec-validate`
- [ ] 9.4 分里程碑 commit，记录未推送状态
