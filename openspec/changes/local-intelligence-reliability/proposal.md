# Proposal
## Why
Beta 已有本机 ASR、持久化队列、v1/v2 检索与决策轨迹，但缺少可交付的本地智能可靠性证据：启动需要多条手工命令，长录音转写中断后无法说明已完成多少窗口，真实中文 embedding 未评估，检索界面不展示证据是否充分，也没有可复现的发布验证清单。这些问题让"本机可用"停留在手工演示，不能作为私人 Beta 的验收结论。

## What Changes
- SMART-01：新增单命令离线 GPU 启动器与 ASR 探针/流式/跳过/超时协议，worker 与 web 在本机 ASR 不可用时降级启动并在能力接口说明原因、GPU、显存与错误。
- SMART-02：ASR 按窗口计划分块执行，以数据库附件为检查点、`sync_recordings` 为进度投影；中断后只补未完成窗口，覆盖时长按区间并集计算，OOM/超时/模型加载失败给出可执行动作。
- SMART-03：新增角色化 embedding 边界（document/query）与本地 OpenAI 兼容 Qwen3-Embedding 服务；真实 embedding 只作为可选路径评估，默认仍为 v1 + hash。
- SMART-04：提供 20 个具名证据充分性场景（含 ≥5 个不可回答），并在既有 Search UI 展示三种充分性状态与被引用证据。
- SMART-05：新增 12 步演示脚本、发布命令清单和发布报告，逐条记录实际执行结果与 `NOT_RUN` 原因。
- 不修改 `contracts/ingest-v1/`、Event Plane/Worldline 架构、队列模型，不新增 MCP 工具族或向量数据库，不改变默认检索版本与默认 embedding provider。

## Capabilities
### New Capabilities
- `local-gpu-deployment`: 单命令本机 GPU 启动、探针、并发锁、五轮启停与降级能力报告。
- `long-audio-reliability`: 长录音窗口级检查点、覆盖记账、失败分类与索引重启续跑。
- `real-embeddings`: 角色化 embedding 边界、本地 Qwen3-Embedding 服务与冻结评测门槛。
- `evidence-sufficiency`: 三种充分性状态、有效证据引用与不可回答场景。
- `release-validation`: 12 步演示、发布命令清单与结果记账。

### Modified Capabilities
无。本轮新增能力不改变既有 `retrieval`、`recovery`、`semantic-job` 的默认行为。

## Impact
`scripts/local-asr.py`、`scripts/local-gpu-up.sh`、`scripts/local-start.sh`、`internal/cloudsync`、`internal/ingest`、`internal/process`、`internal/app`、`internal/semantic`、`internal/migrations`、`internal/contextbuild`、`web/src`、`scripts/*.py`、`docs/productization/*`。数据库新增迁移 v22（`sync_recordings` 与 `worker_heartbeats` 附加列），对旧库为增量列，不删除既有数据。私有录音仅以授权夹具（120 秒与 36.95 分钟）做聚合指标验证，不复制进仓库。
