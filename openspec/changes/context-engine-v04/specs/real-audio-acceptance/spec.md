# Spec Delta
## ADDED Requirements
### Requirement: v04 Harness 协议与隔离
`scripts/long-audio-reliability.py` MUST 支持 `--protocol legacy|context-engine-v04`（默认 legacy）与 `--acceptance-manifest`、`--audio-analysis-config`、`--out`。v04 real 模式无 manifest MUST 直接输出四类真实质量指标 NOT_RUN，MUST NOT 访问 legacy 默认 data 路径或探测文件存在；每个样本独立计账，缺一类不阻断其它授权类。
#### Scenario: CE04-D-01
- **WHEN** v04 real 模式无 manifest
- **THEN** 四类指标全部 NOT_RUN，不读取任何默认数据目录
#### Scenario: CE04-D-02
- **WHEN** manifest 只提供部分类别
- **THEN** 提供类别按规则执行，缺失类别 NOT_RUN，其余不受影响

### Requirement: 验收与金标 manifest
manifest MUST 使用 `context-engine-acceptance-v1`：samples[] 含 id、category（quiet_two_person/far_field_meeting/overlapping_speech/media_interference）、audio_path、audio_sha256、authorization{allowed:true,reference}、gold_path、gold_sha256；路径限定在 manifest 所属授权样本根目录，MUST NOT 自动扫描。gold MUST 使用 `context-engine-gold-v1` 且 `provenance:"human"`，含匿名 annotator ID、reviewed_at、sample_id、scored_regions、transcript turns、acoustic events、speaker trials（分 calibration/evaluation）、queries、claims 与可选 adjudications。
#### Scenario: CE04-D-03
- **WHEN** manifest 缺授权或路径逃逸
- **THEN** 在打开音频前拒绝
#### Scenario: CE04-D-04
- **WHEN** 提供 hash/路径/格式错误
- **THEN** FAIL；依赖或权限不足记 BLOCKED；缺样本记 NOT_RUN

### Requirement: 固定指标定义
Harness MUST 使用 `scripts/audio_acceptance_metrics.py` 的共享纯指标（ASR 脚本同样调用，不复制编辑距离）：CER/WER 的 normaliz 与分母固定；DER 用 collar=0、skip_overlap=false 的最优 speaker 映射；EER 仅用人工 same/different trials 并在 evaluation 集扫描；acoustic event 为同标签 IoU≥0.5 一对一最大匹配；evidence accuracy 要求 current 合法 recording/revision 且在 gold span 内（端点容差 250ms）；decision/commitment precision 只按绑定 prediction digest 的人工 adjudication。零分母 MUST 输出 NOT_EVALUABLE，MUST NOT 填 0 或 1。
#### Scenario: CE04-D-05
- **WHEN** gold empty 或 hypothesis empty
- **THEN** gold empty 为 NOT_EVALUABLE，非空 reference 的空 hypothesis 计全部删除错误
#### Scenario: CE04-D-06
- **WHEN** 缺人工 adjudication
- **THEN** 语义指标 NOT_RUN，结构引用有效率仍可测

### Requirement: 资源与延迟测量
Harness MUST 报告 RTF（提交 import 到所有启用 stage 终态的 wall 秒/音频秒）、每 stage 耗时、p95（按完整运行取值并列 n）、进程树 CPU 峰值、树 RSS 峰值、设备 VRAM 采样峰值与模型分配峰值；每 0.5s 采样，缺 telemetry MUST 输出 null 且原因 NOT_RUN，MUST NOT 伪造 0。缓存命中 MUST NOT 用于生产门禁延迟。
#### Scenario: CE04-D-07
- **WHEN** 缺少 VRAM telemetry
- **THEN** 字段为 null 并标 NOT_RUN
#### Scenario: CE04-D-08
- **WHEN** 统计端到端 p95
- **THEN** 使用完整运行 wall time，并另报各阶段延迟

### Requirement: v04 协议成功条件与诚实状态
v04 协议成功 MUST 要求终态 job 成功、`expected_chunks>0`、`completed==expected`、`failed==0`、权威 ASR 覆盖完整、唯一有效附件、合法 evidence 与可读音频；MUST NOT 以 turn 数超出窗口数推断重复，MUST NOT 以 turn 覆盖替代合法零 ASR coverage。未设质量阈值的模型结果 MUST 标 EXPERIMENTAL；MUST NOT 据此建议 Production Ready。
#### Scenario: CE04-D-09
- **WHEN** 协议运行完成但未设质量阈值
- **THEN** 结果标 EXPERIMENTAL，报告不声称 Production Ready
#### Scenario: CE04-D-10
- **WHEN** 真实样本/金标/模型缺失
- **THEN** 对应指标 NOT_RUN 或 BLOCKED，合成闭环仍必须完成

### Requirement: 私密产物与报告边界
prediction bundle 与待复核文件 MUST 存到操作者指定的私密输出目录（目录 0700、文件 0600、Git 排除）；公共报告 MUST 只含匿名 ID、计数与指标，MUST NOT 含正文或向量。adjudication MUST 引用 prediction digest，二次评分 MUST NOT 修改冻结 gold。
#### Scenario: CE04-D-11
- **WHEN** 生成报告与产物
- **THEN** 报告无正文/向量，产物权限为 0700/0600
#### Scenario: CE04-D-12
- **WHEN** gold 与 adjudication 同时存在
- **THEN** 语义判定以人工 gold/adjudication 为准，不用 ASR 或其它模型充当裁判
