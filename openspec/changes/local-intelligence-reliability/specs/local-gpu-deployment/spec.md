# local-gpu-deployment 规范增量

## ADDED Requirements

### Requirement: 单命令本机 GPU 启动
系统 SHALL 提供一个命令完成前置检查、ASR 模型探针、web/worker 启动、健康等待与受控关闭；启动脚本 MUST NOT 在探针失败时报告就绪。

#### Scenario: local-gpu.preflight
- **WHEN** 机器没有可用 GPU 或空闲显存低于所选后端阈值
- **THEN** 启动脚本 MUST 以非零状态退出并说明缺失的前置条件，且不启动 web/worker

#### Scenario: local-gpu.cold-start
- **WHEN** 前置检查、模型快照与 profile 都存在
- **THEN** 启动脚本 SHALL 完成 ASR 探针后启动子进程，并只在 web 与 worker 健康检查都通过后打印就绪地址

#### Scenario: local-gpu.five-cycles
- **WHEN** 以 `--cycles 5` 运行
- **THEN** 脚本 SHALL 连续完成五次启动-健康-关闭循环，全部通过才退出 0，并记录每轮冷启动、模型加载、显存与关闭耗时

#### Scenario: local-gpu.concurrency
- **WHEN** 两个本机推理进程使用同一个 GPU 锁路径
- **THEN** 第二个进程 MUST 等待第一个释放独占锁，不得并发进入推理

#### Scenario: local-gpu.degraded
- **WHEN** 本机 ASR 选择失败或 CUDA 探针失败
- **THEN** worker 与 web SHALL 继续启动并可服务其他阶段，能力接口 MUST 返回 `available=false` 与具体原因、GPU 名称、显存和错误文本

#### Scenario: local-gpu.vram-diagnostics
- **WHEN** 执行 `--probe`
- **THEN** 探针 SHALL 输出模型加载毫秒数与总/空闲/峰值显存，且 stdout 只有这一行 JSON

#### Scenario: local-gpu.no-paid-fallback
- **WHEN** 父进程环境包含 `VOLC_API_KEY`、`EMBED_API_KEY`、`GENERATOR_API_KEY` 或 `MEMORY_PROFILE_PASSWORD`
- **THEN** 本机 ASR runner MUST NOT 继承这些变量，且 SHALL 保留 `PATH`、`HOME`、`CUDA_VISIBLE_DEVICES`、`HF_HOME`
