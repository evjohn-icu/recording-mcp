# Tasks
## 1. 基线与门禁
- [x] 1.1 记录分支/SHA/工具链与基线命令退出码到 `docs/engineering/context-engine-v04-baseline.md`
- [x] 1.2 `git status` 干净时在 `engineering/context-engine-v04` 执行；main 变化则按计划隔离复核
- [x] 1.3 建立 `CE04_TMP` 临时目录，所有新 profile/DB/音频/日志只写入该目录

## 2. Sprint A：可靠性
- [x] 2.1 A1 `ProviderUnavailableError` 分类 + `Timeout` 字段 + 结构化 `code`
- [x] 2.2 A1 `SemanticRetriever.Ready(ctx) (bool,error)` 迁移全部实现与调用（含 sidecar/HNSW/fake/测试）
- [x] 2.3 A1 Build 降级路径、degrade_reason、`semantic_unavailable` gap、ctx 检查
- [x] 2.4 A1 `TestSemanticGracefulDegradation` 覆盖 11 个具名故障
- [x] 2.5 A2 `GetCurrentEvidenceWindow` + 预算先 items 后窗口 + 跳过超大候选
- [x] 2.6 A2 BudgetTrace skip 字段与三类新 gap，保留 `budget_exhausted`
- [x] 2.7 A2 UTF-8 校验 `invalid_utf8_content`
- [x] 2.8 A2 `TestContextBudgetSkipsOversizedCandidates`、`TestContextBudgetEvidenceWindows`
- [x] 2.9 A3 `internal/backup` + `cmd/context-backup`（backup/restore/verify，manifest v1）
- [x] 2.10 A3 屏障集合计算、`backup_predates_forget`、`forget_barrier_unverifiable`、`writers_active`
- [x] 2.11 A3 `scripts/backup-profile-volume.py` + `docs/deployment.md` 备份恢复段替换
- [x] 2.12 A3 `TestBackupRestoreIntegrityAndAudio`、`TestBackupRejectsPreForgetArchive` 与部署测试扩展

## 3. Sprint B：证据准入
- [x] 3.1 B1 `internal/contextbuild/admission.go` 类型、接口、policy 校验
- [x] 3.2 B1 `DecisionCandidateAssessor` 复用 decision.Engine，64KiB 稳定分批
- [x] 3.3 B1 v3 准入接入 Build（pool、hard filter、证据完整性、排序、rerank 子集）
- [x] 3.4 B1 `TestBuildV3AdmissionAndSufficiency`
- [x] 3.5 B1 cmd/context-eval、internal/eval、app、web、MCP、types.ts、SearchPage、SufficiencyPanel 集成 `--admission-policy`
- [x] 3.6 B2 v3 协议、validator、fixture `fixtures/eval/quality-v3-r1/`（780 问题）、`lifecycle.json`
- [x] 3.7 B2 threshold 网格、calibration lock、holdout 原子消费、失败不生成 lock
- [x] 3.8 B2 真实模型校准/门禁运行（2026-11-13 实际运行：FAIL p95；无 lock）与 `retrieval-v3-report.md` + metrics JSON

## 4. Sprint C：音频分析
- [x] 4.1 C1 `docs/engineering/audio-analysis-v1.md` 契约 + `internal/audioanalysis` types/validation
- [x] 4.2 C1 migration v23 五张表与索引，旧数据升级保留
- [x] 4.3 C1 store：幂等 enqueue、running 快照、原子提交、head/current 过滤
- [x] 4.4 C1 revision 失效、forget/redaction/purge 集成
- [x] 4.5 C1 `TestAudioAnalysisLifecycle`、`TestAudioAnalysisForgetDuringCommit`、`TestAudioAnalysisRevisionInvalidation`、`TestAudioAnalysisModelReplacement`
- [x] 4.6 C2 `scripts/audio-analysis.py`、`scripts/audio_analysis_models.py`、`requirements-audio-analysis.txt`
- [x] 4.7 C2 配置加载与模型 hash 校验、能力 unavailable 语义
- [x] 4.8 C2 `internal/gpulock` 统一锁与 cloudsync/ASR/embedding/audio 接线
- [x] 4.9 C2 `scripts/test_audio_analysis.py`（silence/tone/短尾/重叠/坏 JSON/hash/维度）
- [x] 4.10 C3 `process.Spec.Dependencies` + Claim 依赖选择 + 环检测 + `CompleteTx` finalized 分支
- [x] 4.11 C3 ingest 接线（vad/tagging/diarization/speaker 依赖与失败隔离）、worker 启动恢复
- [x] 4.12 C3 `cmd/context-audio` 与幂等重跑
- [x] 4.13 C3 `app.AudioAnalysisView`、`GET /api/audio-analysis/events/{id}/audio`、Web/MCP 消费
- [x] 4.14 C3 `TestAudioAnalysisIdentityNeedsReview`、`TestAudioAnalysisEndToEnd`、`TestJobDependenciesWaitWithoutAttempts`、e2e revision 扩展

## 5. Sprint D：真实验收
- [x] 5.1 D `scripts/audio_acceptance_metrics.py` 与 ASR 脚本复用
- [x] 5.2 D v04 协议、manifest/gold 校验、NOT_RUN/BLOCKED 记账
- [x] 5.3 D `scripts/test_audio_acceptance_metrics.py`、`scripts/test_long_audio_reliability.py`
- [x] 5.4 D synthetic 闭环运行与 real NOT_RUN 运行

## 6. 发布与回归
- [x] 6.1 Makefile/Docker 加入 `context-backup`、`context-audio`
- [x] 6.2 Web/MCP 兼容性与浏览器流程证据（合成数据）
- [x] 6.3 最终 CI 等价检查与 Docker/Android/cloud E2E 状态记录
- [x] 6.4 `docs/engineering/context-engine-v04-release-report.md` + `context-engine-v04-release-results.json`
- [x] 6.5 `make spec-validate` 与 `openspec/test-map.json` 场景映射
