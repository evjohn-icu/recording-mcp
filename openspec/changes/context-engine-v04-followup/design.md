# Design

## 1. 人工金标：三步、每步只写新目录

`scripts/review-audio-acceptance.py` 把“人工复核”拆成三个不可跳过的状态迁移：

1. `prepare`：显式路径的一条授权录音 → 匿名 `audio.wav`（单声道 16 kHz PCM；已是该格式则按字节复制，
   否则用 ffmpeg 转换）+ 离线 `annotate.html` + `gold-draft.json`（`provenance:"draft"`，
   `annotator`/`reviewed_at`/`category` 为空、`scored_regions` 为空）。目录 0700、文件 0600，
   目标目录已存在即拒绝。页面不引用任何外部资源，草稿与参考转写以内联 JSON 注入，导出通过 Blob 下载。
2. `finalize`：只接受 `provenance:"human"` 的复核稿。先在内存中调用 `acceptance.validate_gold`
   完成全部校验，通过后才创建输出目录，并把音频复制到新根、写入冻结 gold 与
   `context-engine-acceptance-v1` manifest。任何校验失败都不会留下半个目录。
3. `score`：只读冻结 gold 与冻结预测，输出到新目录；不启动服务、不加载模型、不跑 Go 测试。

参考转写单独放在 `reference-draft.json`（`provenance:"machine"`），永不进入 gold；页面把它作为
“机器草稿”展示，避免把 ASR 输出误当人工金标。

### 语义边界

- 时间一律毫秒整数，`bool` 拒绝；`0 <= start_ms < end_ms <= audio_duration_ms`；`scored_regions` 不重叠。
- 数组省略 = 未标注；空数组 = 人工确认没有内容。复核界面导出时保留这个区别。
- `speaker_trials.left/right` 必须引用同一 manifest 授权过的 `sample_id`；不自动推断 `same_speaker`。
- `queries.evidence_completeness:"none"` 不得带 `required_spans`；`"complete"` 必须带。
- `claims` 是人工预期，绝不代替系统预测；预测来自 bundle。

## 2. 预测 bundle 与独立裁决

`context-engine-predictions-v1` 字段固定为：`run_id`、`audio_sha256`、`gold_sha256`、`config_sha256`、
`binary_sha256`、`model`（实际 provider/revision）、`samples`（每样本的 conversation/recording/revision
与当前 turns）、`query_outputs`（Web 与 MCP 各自的 refs 与 sufficiency）、`claims`、`stages`、`resources`。
不含声纹向量。

`prediction_digest` = 对规范化 JSON（`sort_keys=True`、`separators=(",",":")`、`ensure_ascii=False`）
的 `{claims, queries}` 取 SHA-256，排除计时、资源采样与 digest 自身。`context-engine-adjudications-v1`
必须带同一 digest，否则 FAIL；未知或重复的 claim/ref ID 一律拒绝。

评分规则：

| 指标 | 口径 |
|---|---|
| CER/WER | 只在 `scored_regions` 内评分；gold turn 必须整段落在某个区间内，跨边界 turn 记入 `lost_gold_ms` 而不是按字符截断；分子/分母汇总 |
| 事件 PR | 同 label 最大基数匹配（复用了 DER 的匈牙利分配），输出 TP/FP/FN 与 per-label |
| 证据准确率 | 分母 = 全部返回 refs；非法/过期/错 recording 或 revision/越 scope/ambient/forgotten 计错；空返回 null，全非法非空 0；另有结构合法率与 required 覆盖率 |
| 语义准确率 | 需要每个返回 ref 的人工裁决，未全裁决即 NOT_RUN |
| 决策精确率 | 某类全部预测裁决完成才出正式分数，否则 null + 待复核计数；零输出 NOT_EVALUABLE |
| DER | 区间裁剪后 collar=0/skip_overlap=false；pyannote 仅在配置的音频解释器里可用，不可用即 BLOCKED |
| EER | 至少两名说话人、各自有跨录音 positive 与 negative，否则 NOT_EVALUABLE；向量只在内存比较 |

## 3. 真实执行

`long-audio-reliability.py --protocol context-engine-v04 --mode real` 逐样本启动隔离 Stack
（profile/DB/日志都在临时目录），要求显式 `--asr-backend ark|qwen --asr-python --asr-revision`；
`test-stub`/`fixture` 等测试 provider 被拒绝。流程：上传 → 等待所有 enabled stage 终态 →
校验 `expected_chunks>0`、`completed==expected`、`failed==0`、ASR 覆盖率、turn WAV 长度
`44+32*(end-start)`、当前附件唯一 → 对每个 gold query 同时调用 `GET /api/search/hybrid`
与 MCP `build_context`（MCP 放在独立子进程里，超时即放弃，避免 readline 无限阻塞）→
采样进程树 CPU/RSS 与设备 VRAM。

合成/夹具运行只证明协议与失败路径；真实质量必须有授权录音与人工 gold。生成器默认 off，
claims 因此 NOT_RUN；只有显式 `--generator-config`（仅 off/openai、loopback URL、key 只给变量名）
才采集 claims。

## 4. v3 时延：先证明是不是模型受限

`--phase profile` 固定 train 全集、threshold=0、不写 lock、不送 test。每 query 用
`internal/eval.EngineTiming` 累计 `decision.Engine.Evaluate` 的墙钟时间（含准入与最终评估），
再加 `AdmissionRequestStats` 的请求数/候选数/字节数。判定：

- `engine_p95 > v1_p95 * 1.5` → `model-bound`：即使把客户端构造成本降到 0 也进不了预算。
- 否则 → `construction-bound`：客户端构造值得一次语义不变优化。

两种结论都只对当前 train 配置、当前 provider 与当前二进制成立，不外推。

唯一实现改动在 `admission.go`：`admissionRequest` 每候选渲染 block 与 question 各一次，
并用 `escapedStringLen` 计算转义后长度，批次 size 由累加得出。JSON 字符串转义是逐字符的，
因此 `size(batch)` 与整包 `json.Marshal` 完全相等；Go 测试对多组批次逐一比对，并固定了
“恰好 64 KiB 通过、多 1 字节拆分”“超大候选被拒且后续小候选仍提交”两个边界。

## 5. lock v2 与一次性 holdout

`retrieval-v3-selection-lock-v2` 保存：完整 `configuration` 与其 hash（含 `embedding_input_type`）、
`identity`（provider/model/revision/dims/binary/prompt 身份）、fixture/binary/policy template hash、
`budget`、`warmup`、`selected`、`calibration_metrics`、`calibration_gate`。

holdout 流程：校验 lock schema 与全部 hash → 从 lock 恢复参数（显式不同先失败）→ 用恢复后的配置重算
identity/健康状态（不向 test 发任何探测请求）→ `O_EXCL` claim + fsync → 原子写 `holdout_started`
→ 才发 test 请求。崩溃或超时后 `.consumed` 与 `holdout_started` 都保留，不能删标记重开。
概率缓存只用于 calibration 筛选，holdout 传该 flag 直接拒绝；flush URL 必须指向实际 embedding 服务
的 host:port，避免照抄历史端口造成“看似无缓存”。

每个 v3 phase 只允许新目录或空目录：覆盖已有 calibration/holdout 目录等于销毁已消费的证据。
