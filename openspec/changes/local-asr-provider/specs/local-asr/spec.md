# Spec Delta

## ADDED Requirements

### Requirement: 本地 ASR provider

系统 MUST 支持通过 `--asr-provider ark|qwen|local` 在本地以 float16 运行离线 ASR，并复用现有 `cloudsync.Transcriber` 与 ingest 写入路径。

#### Scenario: local-asr.provider-selection
- **WHEN** 配置 `ark`、`qwen` 或 `local` 并指定合法 backend
- **THEN** web 与 worker 使用同一个构造器得到 `LocalTranscriber`，默认模型分别为 `Edge0/ARK-ASR-0.6B` 与 `Qwen/Qwen3-ASR-1.7B`，并由 `internal/cloudsync/select_test.go` 验证

#### Scenario: local-asr.float16-window-mapping
- **WHEN** runner 返回多个窗口的 JSON
- **THEN** 每个窗口映射为一个 `asrinput.Result`，provider 为 `local-ark`/`local-qwen`，偏移、时长、文本与模型 id 正确，且不伪造 confidence，并由 `internal/cloudsync/local_asr_test.go` 验证

#### Scenario: local-asr.invalid-runner-output
- **WHEN** runner 以非零退出、返回非 JSON 或越界窗口
- **THEN** 转写返回错误且不写任何记录，错误包含 stderr 摘要，并由 `internal/cloudsync/local_asr_test.go` 验证

#### Scenario: local-asr.startup-probe
- **WHEN** 启动时缺少 runner 脚本或 Python 依赖
- **THEN** 进程在启动阶段失败并指出缺失项，而不是等到首个任务，并由 `internal/cloudsync/select_test.go` 验证
