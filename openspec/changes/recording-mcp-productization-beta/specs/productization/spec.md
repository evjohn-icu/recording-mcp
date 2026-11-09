# Spec Delta

## ADDED Requirements

### Requirement: 可复现部署
系统 MUST 提供单镜像多命令的容器部署，`web` 与 `worker` 共享一个命名数据卷，容器以非 root 用户运行，宿主只发布回环端口。

#### Scenario: productization.deploy-compose-config
- **WHEN** 运行 `docker compose config`
- **THEN** 配置解析成功且服务、卷、安全选项与端口符合设计，并由 `scripts/test_productization_deploy.py` 验证

#### Scenario: productization.deploy-clean-start
- **WHEN** 用临时 profile 与随机密码启动 compose 栈
- **THEN** `web` 与 `worker` 各自健康且互不依赖，并由对应部署测试验证

#### Scenario: productization.deploy-restart-persistence
- **WHEN** 在导入数据后连续重启 3 次
- **THEN** 会话与导入数据仍在，并由对应部署测试验证

#### Scenario: productization.deploy-no-external-api
- **WHEN** 未配置任何外部 API 启动
- **THEN** 服务可用，能力接口报告 ASR/生成器不可用而不是崩溃，并由对应部署测试验证

#### Scenario: productization.deploy-backup-restore
- **WHEN** 备份卷并恢复到新卷
- **THEN** 行数与哈希一致，并由对应部署测试验证

#### Scenario: productization.deploy-non-root
- **WHEN** 检查容器进程身份
- **THEN** UID/GID 为 10001 且非 root，并由对应部署测试验证

### Requirement: 导入幂等与安全
系统 MUST 通过 multipart 导入音频，流式写入 profile 私有目录，以 SHA-256 决定稳定 ID，并对重复导入保持幂等。

#### Scenario: productization.import-normal
- **WHEN** 导入一个合法 WAV 并执行 ingest/asr
- **THEN** conversation、recording、turn、attachment 与来源元数据存在，并由 `internal/ingest/import_test.go` 验证

#### Scenario: productization.import-duplicate
- **WHEN** 相同字节导入两次
- **THEN** conversation/recording/job 各只有一个，第二次返回 duplicate，并由对应测试验证

#### Scenario: productization.import-unsupported-format
- **WHEN** 上传非白名单扩展名
- **THEN** 返回 415 `unsupported_format` 且不写任何行，并由对应测试验证

#### Scenario: productization.import-corrupt-audio
- **WHEN** 扩展名合法但探测失败
- **THEN** 返回 422 `invalid_audio` 且不写任何行，并由对应测试验证

#### Scenario: productization.import-restart
- **WHEN** 导入后重启服务并重新打开数据库
- **THEN** 任务与记录仍在且可继续处理，并由对应测试验证

#### Scenario: productization.import-worker-crash
- **WHEN** worker 租约过期后重新领取 asr 任务
- **THEN** 任务可恢复且 turn 不重复，并由对应测试验证

#### Scenario: productization.import-asr-unavailable
- **WHEN** 未配置 ASR 时上传
- **THEN** 返回 409 `asr_unavailable` 且给出 action，不写记录，并由对应测试验证

#### Scenario: productization.import-retry
- **WHEN** asr 首次可重试失败后重跑
- **THEN** active attachment 与 turn 各一条，任务最终成功，并由对应测试验证

#### Scenario: productization.import-progress-projection
- **WHEN** 读取处理快照
- **THEN** `phases/current_stage/错误动作` 与 job 状态一致，并由 `internal/app/processing_test.go` 验证

### Requirement: Web 工作流
系统 MUST 让用户在浏览器完成导入、查看阶段进度、打开转写、搜索、查看证据与播放，并在刷新后保持状态。

#### Scenario: productization.web-workflow
- **WHEN** 执行 8 步产品浏览器操作
- **THEN** 全部通过且无 pageerror，并由 `scripts/product-workflow-browser.py` 验证

#### Scenario: productization.web-refresh-state
- **WHEN** 刷新详情与列表页
- **THEN** 标题、转写与阶段仍在，并由对应脚本验证

#### Scenario: productization.web-mobile-layout
- **WHEN** 使用 390×844 与 1440×900 视口
- **THEN** 无横向溢出且导入按钮可见，并由对应脚本验证

#### Scenario: productization.web-no-page-errors
- **WHEN** 走完整个工作流
- **THEN** 没有阻塞性 pageerror，并由对应脚本验证

#### Scenario: productization.web-stale-not-exposed
- **WHEN** 来源被遗忘
- **THEN** turn 不可搜索或播放，并由 `internal/app/forget_visibility_test.go` 验证

### Requirement: 检索质量报告
系统 MUST 用可复现的冻结集报告 v1/v2 与 lexical/hash 指标、分母、延迟与泄漏计数，并保持默认 v1。

#### Scenario: productization.retrieval-quality-report
- **WHEN** 生成检索质量报告
- **THEN** 必填指标与分母齐全，真实模型项为 `NOT_RUN`，并由 `scripts/test_productization_retrieval.py` 验证

#### Scenario: productization.retrieval-default-v1
- **WHEN** 未显式指定版本
- **THEN** 检索使用 v1，并由 `internal/web/web_test.go` 验证

#### Scenario: productization.retrieval-leakage-zero
- **WHEN** 统计 stale/ambient/scope/invalid evidence 泄漏
- **THEN** 报告中的泄漏计数为零，并由对应脚本验证

### Requirement: Beta 演示
系统 MUST 提供隔离、可复现的 10 步演示，且不读取或删除用户数据。

#### Scenario: productization.demo-ten-steps
- **WHEN** 运行 Beta 演示
- **THEN** 10 个步骤全部通过且 Web 与 MCP 同源，并由 `scripts/test_beta_demo.py` 验证

#### Scenario: productization.beta-checklist
- **WHEN** 检查发布材料
- **THEN** 清单、报告与证据路径齐全，并由对应脚本验证
