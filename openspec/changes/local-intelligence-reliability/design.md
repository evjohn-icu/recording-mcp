# Design
## 部署形态
主形态为 Mode B：WSL 原生 web + worker + `.venv-asr` 本机 ASR。Docker Compose 保持非 GPU 部署；本轮不发布 GPU Compose profile，因为镜像刻意不含 Python/torch，GPU 镜像需要数 GB CUDA/torch 栈与模型缓存卷。`docker run --gpus all` 探针只记录事实结果，不据此声称容器 GPU 支持。

## ASR 进程协议
`scripts/local-asr.py` 保持"模型加载一次、窗口循环处理"的结构，并新增四个可观察行为：`--probe` 只加载模型并输出一行 JSON；`--stream` 每个完成窗口输出一行 JSON 并 flush；`--skip` 跳过已提交窗口且不生成 WAV；`--load-timeout`/`--window-timeout` 用守护线程与 `SIGALRM` 把加载与单窗口推理变成可诊断失败（退出码 4/5）。stdout 只承载机器可读输出，进度写 stderr。

## 窗口检查点
Go 侧 `PlanWindows` 与 Python `plan_windows` 必须给出同一计划，因此用 `--dry-run` 对照测试锁死漂移。`ChunkTranscriber` 逐行解析 `--stream` 输出，先校验索引在计划内、未跳过、未重复、边界不超出音频，再回调写入。数据库里的 `source_attachments` 是唯一检查点：附件已提交即视为该窗口完成，`sync_recordings.completed_chunks/failed_chunks/coverage_ms` 只是可读投影。覆盖时长按活跃区间并集计算，绝不用 `MAX(global_end_ms)` 推断完整性。上下文取消时保留 `partial` 以便重启续跑；真实失败写 `failed` 并保留已完成计数。

## GPU 独占
本机 ASR 与本地 embedding 共用 8 GB 显存，因此用 `flock` 的独占文件锁串行化推理。锁路径由启动器传入，Go 侧轮询 `LOCK_EX|LOCK_NB`，Python 侧 `--gpu-lock` 在模型加载与每批推理外加锁，超时返回 503 `GPU busy`。

## 降级
本机 ASR 选择失败或 CUDA 探针失败不阻止进程启动：worker 以 `transcriber=nil` 继续服务其他 stage，`RunASRJob` 继续返回 `asr_unavailable` 与动作；web 记录 `ASRInitError`。`Capabilities` 综合配置、心跳存活、worker 可用性与错误给出 `asr_not_configured`/`worker_offline`/`worker_asr_mismatch`/`gpu_unavailable`/`model_probe_failed`，并暴露 GPU 名称与显存。

## 角色化 embedding
`RoleProvider` 只在请求里加 `input_type`（`omitempty`），OpenAI 兼容远端会忽略；本地服务据 `query` 追加 Qwen3 指令前缀。索引写文档角色，检索写查询角色，hash provider 回退到 `Embed`。这样最小改动即可让真实模型与既有 provider 共存，不引入第二个 Go provider 实现。

## 评测与默认值
冻结评测协议与门禁不改。真实 embedding 只有在校准门禁全部通过时才创建 selection lock 并一次性运行 holdout；否则记录失败检查、保持 v1 + hash 默认。默认必须离线可用且不依赖第二个模型服务，所以即使通过也只记录为可选命令。

## 充分性
20 个场景以确定性测试评估器驱动：评估器只接收最终预算后的证据，按 `required_terms`/`forbidden_terms` 返回三种状态，错误注入返回 `unknown`。这样期望状态不依赖真实评估器实现，真实评估器质量另作指标报告。

## 数据与隐私
只使用授权夹具与合成夹具；不读取私有录音正文，只读取元数据/时长用于聚合指标。所有验证使用临时 profile 与临时数据库，不对真实 profile 执行破坏性迁移。
