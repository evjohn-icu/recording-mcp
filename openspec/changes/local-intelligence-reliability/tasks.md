# Tasks
## 0. 基线与契约
- [ ] 0.1 从当前 HEAD 建分支，记录环境、模块状态与基线命令结果到 `docs/productization/sprint-3-baseline.md`。
- [ ] 0.2 新增 5 个 spec delta 与 pending 场景映射，`make spec-validate` 通过。
- [ ] 0.3 新增 `scripts/model-revisions.json` 与钉住一致性测试。
## 1. SMART-01 单命令离线 GPU 部署
- [ ] 1.1 先写失败测试，再为 `scripts/local-asr.py` 实现 `--probe`/`--stream`/`--skip`/加载与窗口超时。
- [ ] 1.2 先写失败测试，再实现 `PlanWindows`、`ChunkTranscriber`、`RunnerError`、GPU 文件锁与环境隔离断言。
- [ ] 1.3 worker/web 在本机 ASR 不可用时降级启动；心跳与能力接口报告 GPU/显存/错误。
- [ ] 1.4 新增 `scripts/local-gpu-up.sh` 与 `scripts/test_local_gpu_up.py`；扩展 `local-start.sh` 的 provider 透传。
- [ ] 1.5 记录真实探针、五轮启停、冷启动与显存到 `docs/productization/gpu-deployment.md`。
## 2. SMART-02 长音频可靠性
- [ ] 2.1 先写失败测试，再新增迁移 v22 与 `ProcessingStatus` ASR 字段。
- [ ] 2.2 先写失败测试，再把 `RunASRJob` 改为数据库检查点的窗口循环，并分类 OOM/超时/加载失败。
- [ ] 2.3 新增 `long_audio_test.go` 六个锚点与 `scripts/testdata/slow-asr.py` 故障注入 runner。
- [ ] 2.4 新增 `scripts/long-audio-reliability.py` 与 `docs/productization/long-audio-reliability.md`，记录 T1–T8。
## 3. SMART-03 真实中文 embedding
- [ ] 3.1 先写失败测试，再实现 `RoleProvider`/`EmbedTexts` 并接入索引与检索。
- [ ] 3.2 新增 `scripts/local-embed.py`、`requirements-local-embed.txt`、`scripts/fetch-local-models.sh` 与协议测试。
- [ ] 3.3 启动器接通 `EMBED_PROVIDER=local` 与 `--with-embedding`。
- [ ] 3.4 运行冻结评测，按门禁决定是否创建 lock；写出 `docs/productization/real-embedding-evaluation.md`。
## 4. SMART-04 证据充分性
- [ ] 4.1 先写失败测试，再新增 20 个场景夹具与 `TestSufficiencyScenarios`。
- [ ] 4.2 新增决策轨迹场景测试（未确认提案、忘记来源、未知时间）。
- [ ] 4.3 在既有 Search UI 新增 `SufficiencyPanel`，并用浏览器脚本验证三种状态与证据链接。
- [ ] 4.4 写出 `docs/productization/sufficiency-scenarios.md`。
## 5. SMART-05 发布验证
- [ ] 5.1 新增 12 步演示脚本与 `RUN_SPRINT3_DEMO=1` 测试。
- [ ] 5.2 新增 `docs/productization/release-commands.json` 与清单测试。
- [ ] 5.3 运行全部回归命令，写出 `docs/productization/sprint-3-release-report.md`。
- [ ] 5.4 按里程碑提交，不推送、不合并。

## Workflow follow-up
- 实际命令、红绿测试与限制记录在 `docs/productization/tdd-evidence.md` 与 release report。
- 真实模型/授权音频缺失时记 `NOT_RUN` 与原因，不用合成结果冒充真实 ASR。
