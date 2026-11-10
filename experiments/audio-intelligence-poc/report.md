# 音频前端方案 PoC 与选型报告

分支：`experiment/audio-intelligence-poc`（基于 `main@51a73a1`）
日期：2026-11-10
范围：VAD、环境声标签、说话人分离、声纹；不改动 ASR、Evidence、Worldline、MCP 契约。

## 1. 结论

| 能力 | 选择 | 关键依据 | 备选/限制 |
|---|---|---|---|
| VAD | Silero VAD ONNX（官方 `silero_vad.onnx`） | 20 session CPU RTF 0.0046；ROC-AUC 0.935；PR-AUC 0.993；2.3 MB；MIT | 默认 0.5 的 F1 0.831，0.01 时 0.958；GPU 无收益（比 CPU 慢 12–13 倍）；远场需按设备校准阈值 |
| 环境声标签 | sherpa-onnx Zipformer Audio Tagging | mAP 0.928、Top-1 0.914；CPU RTF 0.018；Apache-2.0 | CED 精度略高但上游 GPL-3.0，不能直接用于非 GPL 产品；PANNs 作为 GPU/服务端备选 |
| 说话人分离 | pyannote Community-1（GPU） | 20 session 聚合 DER 7.96%；GPU RTF 0.038；峰值 436 MB；官方 gated 权重（`3533c8c`）在 L 上复现同一 DER | 授权 CC-BY-4.0（需接受条款）；CPU RTF 1.14（慢 36×），边缘不实用 |
| 声纹 | 3D-Speaker CAM++（ONNX） | 4008 trials / 113 人 EER 3.24%（sherpa 提取器）：CPU RTF 0.024；28 MB；Apache-2.0 | 显式 fbank 路径同权重可到 2.30%，EER 取决于前端而非 provider；WeSpeaker 3.49% 且 minDCF 更低；2 session 的 1.51% 属偏乐观样本 |
| 边缘/离线组合 | Silero + Zipformer + CAM++ + 可选 sherpa-onnx diarization | 全部 ONNX/CPU，可迁移 macOS / RK | sherpa oracle 人数下 20 session DER 0.2114（confusion 主导）；需可靠人数估计或人工提示，当前估计器不够用 |
| 服务端高精度组合 | pyannote Community-1 + PANNs（可选） | 8 GB 显存足够，峰值 <0.5 GB/模型 | PyTorch 依赖重；官方模型需接受 gated 条款 |

## 2. 环境

| 项 | 实测值 |
|---|---|
| 主机 | Ubuntu 24.04.5 LTS on WSL2，kernel `6.18.58.1-microsoft-standard-WSL2` |
| CPU | AMD Ryzen 5 5600X 6-Core（12 线程） |
| 内存 | 23 GB |
| GPU | NVIDIA GeForce RTX 2070，8192 MiB，驱动 610.88，CUDA UMD 13.3 |
| Docker | 29.9.2（Docker Desktop 4.98.0，WSL2 engine） |
| Python | 3.12.13（PoC venv） |
| 数据 | AISHELL-4 test split 全部 20 个 session；ESC-50 2000 clips |

Docker Desktop 原有手动代理 `172.22.23.142:7890` 已失效，导致 `docker build/pull` 失败。构建两个 PoC 镜像期间把 `ProxyHTTPMode` 改为 `system`；镜像构建与全部容器验证完成后，已按备份 `settings-store.json.bak-audio-poc` 还原原始 `manual` 代理配置并重启 Docker Desktop，`docker info` 正常应答、两个镜像仍在本地。还原后该代理依然不可达，这是使用者的原始状态，不是本次改动引入的。before/after 记录在 `results/docker_settings_restore.json`，早先的探针在 `results/docker_gpu_probe.json`。

GPU 侧 ONNX Runtime 有一处必须记录的版本约束：`onnxruntime-gpu==1.31.1` 要求 CUDA 13 运行时，而 PyPI 的 `nvidia-*-cu13` 轮子只是 `0.0.1` 占位包，本机 CUDA 12.6（torch cu126 自带 cuBLAS 12 / cuDNN 9）无法满足；因此 GPU 环境固定 `onnxruntime-gpu==1.23.2`（最后一个 CUDA 12 构建），并在脚本内先调用 `onnxruntime.preload_dlls()` 让 ORT 加载 torch 自带的 `nvidia/*/lib`。`get_available_providers()` 只反映编译期能力，CUDA 运行库缺失时会静默回退 CPU；脚本因此在 `InferenceSession` 创建后检查 `get_providers()`，不含 `CUDAExecutionProvider` 直接 `SystemExit`，避免把 CPU 数字标成 GPU 结果。

## 3. 数据与协议

### 3.1 中文多人音频

- AISHELL-4（OpenSLR SLR111）test split，**全部 20 个 session**（`results/aishell4_download.json`、`results/manifest_all.json`）：
  - 总时长 45,811.1 s（约 12.7 h），单 session 2,195.4–2,393.9 s。
  - 每 session 5–7 个说话人（合计 116 个 session-local 说话人）；重叠语音合计 2,124.4 s；RTTM speech union 合计 41,440.9 s。
  - 下载后 WAV 合计 1398.0 MiB（16 kHz mono s16）；RTTM 末尾留白 1.8–114.1 s，因此下载脚本只校验「RTTM 不超出音频」，不要求两端接近。
- 使用 8 通道录音的 **channel 0**，16 kHz mono；RTTM 作为 diarization 参考。
- 官方许可证是 **CC BY-SA 4.0**（OpenSLR SLR111）；HF 数据集卡写 `apache-2.0`，以 OpenSLR 为准。PoC 不重分发音频。
- VAD/diarization/voiceprint 共用同一批 AISHELL-4 音频。
- Ambient tagging 必须使用带环境声类别标签的语料，AISHELL-4 没有这类标签；因此 ambient 用 ESC-50，而所有语音任务仍使用同一批中文多人音频。报告没有把 ESC-50 的指标混入语音任务。

### 3.2 环境声标签

AISHELL-4 没有环境声类别标签，因此 ambient tagging 使用 ESC-50：
- 2000 clips × 5 s，CC BY-NC 3.0（仅研究/评测，不得用于商业分发）。
- 选取 21 个可映射到 AudioSet 的类别并按类取样；实际落盘 **20 类 × 40 clips = 800 clips**（`thunder` 在 ESC-50 子集中没有样本，未进入指标）。
- mAP 在 20 个有样本类别上求均值；Top-1/Top-3 在 21 个可映射候选标签上计算（`thunder` 无 ESC-50 样本，只被排除在 AP 均值之外）。两者都不是 527 类 AudioSet 全量指标。

### 3.3 指标定义

- VAD：32 ms 帧；F1@0.5、Precision、Recall、ROC-AUC、PR-AUC；另给阈值扫描的最佳 F1。RTTM 的 speaker turn 含 turn 内停顿，Recall 会被低估。
- Diarization：`pyannote.metrics` DER，collar 0.25 s，`skip_overlap=False`；同时记录 FA/Miss/Confusion。
- Voiceprint：固定 4008 trials（2004 正 / 2004 负），113 个 speaker，覆盖 20 个 session；2 段 enrollment，余弦打分；EER、minDCF（p_target=0.01）、EER 阈值准确率。
- 资源：`RTF = 推理耗时 / 音频秒数`；模型加载时间单独统计；CPU 用峰值 RSS；GPU 用 `torch.cuda.max_memory_allocated`。
- 所有未真实执行的指标在 §8 标记 `NOT_RUN`。

## 4. 结果

### 4.1 VAD

模型：`silero_vad.onnx`（官方 `silero-vad` 包，SHA256 `1a153a22…78e3`）。协议：64 sample context + 512 sample chunk = 576；图输出已是 `[0,1]` 概率，不能再做 sigmoid。

全部 20 个 session（45,811.1 s，1,431,607 帧）CPU 聚合：

| 指标 | 聚合（20 session） |
|---|---:|
| F1 @0.5 | 0.8305 |
| Precision @0.5 | 0.9959 |
| Recall @0.5 | 0.7122 |
| ROC-AUC | 0.9351 |
| PR-AUC | 0.9925 |
| 最佳 F1（阈值） | 0.9575（0.01） |
| RTF | 0.00455 |
| 加载 | 0.088 s |
| 峰值 RSS | 631 MB |

逐 session F1 范围 0.6729（`M_R003S01C01`）–0.8929（`S_R003S01C01`）；最差 5 个是 `M_R003S01C01` 0.6729、`L_R003S01C02` 0.7396、`M_R003S02C01` 0.7407、`L_R003S02C02` 0.7593、`M_R003S05C01` 0.7806。原 2 session 子集（L+S）的 F1 0.8810 属于偏乐观样本：扩展到 20 session 后默认 0.5 的 recall 掉到 0.71。

GPU 行（自包含镜像内，`results/vad_silero_gpu.json`，同一对 L/S session）：

| 指标 | GPU（CUDAExecutionProvider） | CPU（同 2 session） |
|---|---:|---:|
| F1 @0.5（L / S） | 0.8819 / 0.8801 | 0.8819 / 0.8801 |
| RTF（L / S） | 0.0543 / 0.0598 | 0.00440 / 0.00445 |
| 显存增量（nvidia-smi 全卡） | 78 MB | — |
| session 内 provider | `['CUDAExecutionProvider','CPUExecutionProvider']` | `['CPUExecutionProvider']` |

解读：AISHELL-4 的 speech 帧占比 90.5%（1,295,006/1,431,607），F1 会被类别不平衡抬高；AUC/PR-AUC 更能反映排序能力。默认 0.5 偏保守，远场会议建议按麦克风/房间校准到 0.01–0.05，或做 hangover/能量门限。逐 chunk 调用的 Python 循环 + GPU 同步使 CUDA 比同两个 session 的 CPU 慢约 12–13 倍（0.054/0.060 vs 0.0044），且显存只需 78 MB：**Silero VAD 没有 GPU 收益，CPU 就是部署目标**。另已下载 TextGrid 并核对：非空 interval 的 union（S 2074.4 s、L 2172.3 s）与 RTTM 基本一致，TextGrid 不提供独立的词级静音切分，因此 RTTM 仍是可用的最佳帧级标签。20 session 上 4 线程约实时 220×（RTF 0.00455，无并发负载；同样的 20 session 在 5 个任务并发时测得 0.00919，报告采用无负载值）。

### 4.2 环境声标签

800 clips，20 类闭集（映射 21 个 AudioSet 标签；`thunder` 无 ESC-50 样本）。

| 模型 | mAP | Top-1 | Top-3 | RTF | 加载 | 峰值 RSS | GPU 峰值 | 权重大小 | 许可证 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| sherpa-onnx Zipformer | 0.9282 | 0.9137 | 0.9875 | 0.0181（CPU） | 0.865 s | 1119 MB | N/A | 259 MB | Apache-2.0 |
| sherpa-onnx CED base | 0.9309 | 0.9000 | 0.9900 | 0.0153（CPU） | 0.683 s | 1119 MB | N/A | 343 MB | 上游 CED **GPL-3.0** |
| PANNs CNN14（CPU） | 0.9070 | 0.8975 | 0.9675 | 0.0503 | 0.949 s | 1537 MB | N/A | 327 MB | 代码 MIT / 权重 CC-BY-4.0 |
| PANNs CNN14（GPU） | 0.9070 | 0.8975 | 0.9675 | 0.00208 | 1.125 s | 1511 MB | 424.7 MB | 327 MB | 代码 MIT / 权重 CC-BY-4.0 |

结论：CED 的 mAP 最高，但只比 Zipformer 高 0.0027，且 CED 上游是 GPL-3.0；Zipformer 的 Top-1 更高、权重大小更小、许可证干净。PANNs 的 mAP 低约 2.2 个百分点，CPU RTF 高 2.8–3.3 倍，但 GPU 上最快；适合服务端备选。PANNs 权重来自 HF 镜像 `ayushrsys/panns-cnn14`，md5 `541141fa2ee191a88f24a3219fff024e` 与 Zenodo 3987831 一致。

### 4.3 说话人分离

全部 20 个 session（45,811.1 s）聚合：

| 方案 | DER | FA | Miss | Confusion | 人数（参考/输出，逐 session 求和） | RTF | 加载（最大） | GPU 峰值 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| pyannote Community-1 GPU（自包含镜像） | **0.0796** | 849.2 s | 1151.5 s | 1164.0 s | 116 / 113 | 0.0382 | 5.96 s | 436.4 MB |
| sherpa-onnx CPU（oracle 人数） | 0.2114 | 1038.8 s | 920.8 s | **6447.8 s** | 116 / 115 | 0.0910 | 0.75 s | — |

pyannote 逐 session DER 0.0252（`L_R004S06C01`）–0.1332（`L_R003S01C02`），20 个 session 中 14 个的人数与参考完全一致；分歧集中在 `L_R004S02C01`（7→4）与 `L_R004S03C01`（7→5）。

sherpa 的人数在 20 个 session 里有 19 个与参考完全一致（逐 session 的 `num_clusters` 都等于 RTTM 人数），但 DER 仍有 0.2114，误差几乎全部来自 **confusion 6447.8 s**：说话人数量对了，标签分配却大面积错位。逐 session DER 0.0858–0.3839，最差 `L_R003S01C02` 0.3839、`S_R003S04C01` 0.3573。pyannote 的对照是 confusion 1164.0 s。

单个完整 `L_R004S03C01`（2227.8 s，7 人，含重叠）：

| 方案 | DER | FA | Miss | Confusion | 说话人（参考/输出） | RTF | 加载 | 峰值 RSS | GPU 峰值 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| pyannote Community-1 GPU | **0.0995** | 60.18 s | 36.24 s | 108.39 s | 7 / 5 | 0.0316 | 1.744 s | 2323 MB | 436.4 MB |
| sherpa-onnx（oracle 人数=7） | 0.1404 | 65.56 s | 32.32 s | 191.09 s | 7 / 7 | 0.0914 | 0.266 s | 1119 MB | N/A |
| sherpa-onnx（默认 threshold=0.5） | 0.4904 | 68.63 s | 33.31 s | 907.39 s | 7 / **114** | 0.0830 | 0.268 s | 2201 MB | N/A |
| sherpa-onnx（threshold=0.4） | 0.6073 | — | — | — | 7 / **157** | 0.0706 | 0.329 s | — | N/A |

补充：
- **官方 gated 权重复核**：用 HF token 下载 `pyannote/speaker-diarization-community-1`（revision `3533c8cf8e36`）在 L 上重跑，DER 0.0995 与公开镜像**数值完全相同**，RTF 0.0315（镜像 0.0316，属测量噪声）；说明本次镜像结果可以作为该 session 的官方等价证据。
- pyannote CPU 完整 L：DER 0.0995（与 GPU 相同），RTF **1.136**，加载 1.183 s，峰值 RSS 1322 MB；同一 session GPU RTF 0.0316，即 CPU 慢 36 倍。
- pyannote CPU 子集（S session 前 300 s）：DER 0.0276，参考 5 / 输出 2，RTF 0.666，加载 0.734 s，RSS 1474 MB。该子集只有 2 个主要说话人，DER 不能与完整 L 直接比较，仅用于 CPU 资源。
- sherpa 在 L 前 300 s（7 人）上阈值扫描：0.45→DER 0.316/21 人，0.5→0.266/18 人，0.55→0.266/17 人，0.6→0.263/16 人，0.7→0.168/11 人；oracle 7 人才到 0.140。说明 **未知人数时 threshold 聚类不可靠**。
- pyannote 在 L 上只输出 5 个 speaker，漏掉 2 个；混淆 108 s。sherpa oracle 7 人时人数正确，但混淆 191 s。
- 人数估计器（Silero 1.5 s 窗 + CAM++ 余弦层次聚类，`results/speaker_count_estimator.json`）：L 估计 5（参考 7，silhouette 0.254，threshold 0.8），S 估计 7（参考 5，silhouette 0.342，threshold 0.7），两次都被判 `reliable`。用估计值驱动 sherpa：L 参考 7 / 输出 5，DER **0.2612**，同 session oracle 7 人是 0.1404，明显退化；S 参考 5 / 输出 4，DER **0.2409**，与同 session oracle 结果**完全相同**（`num_clusters=7` 与 5 在该 session 收敛到同一分割），因此 S 只能算持平，不能作为估计值有害的证据。**silhouette ≥ 0.05 不足以证明人数正确**：估计器"自信地"在两个方向上都报错，`reliable` 只能当作"聚类结构清晰"，不能当作人数正确性的证据。

结论：精度优先选 pyannote Community-1（20 session DER 0.0796，GPU RTF 0.038）；边缘/CPU 可选 sherpa-onnx，但必须有人数估计或人工指定 `num_clusters`，不能直接用默认 threshold；当前人数估计器还不足以自动供给该参数。

### 4.4 声纹

4008 trials（2004 正 / 2004 负），113 个 speaker，覆盖全部 20 个 session，余弦打分。

| 模型 | EER | minDCF（归一化） | EER 阈值准确率 | RTF | 加载 | 峰值 RSS | 维度 | 大小 | 许可证 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| 3D-Speaker CAM++ | **3.24%** | 0.373 | 96.76% | 0.0244 | 0.777 s | 3117 MB | 192 | 28.3 MB | Apache-2.0 |
| WeSpeaker CN-Celeb ResNet34-LM | 3.49% | **0.305** | 96.51% | 0.0305 | 0.266 s | 3109 MB | 256 | 26.5 MB | Apache-2.0 |

说明：测试是 AISHELL-4 同 session、远场、非重叠片段，难度与 VoxCeleb/CN-Celeb 官方 trial 不同。**扩展到 20 个 session 后两个模型基本打平**：2 session 子集上 CAM++ 1.51% vs WeSpeaker 4.52%，全量上变成 3.24% vs 3.49%，且 WeSpeaker 的 minDCF 更低（0.305 vs 0.373）——说明单 session 结论不能外推，选型应看全量分布。CAM++ ONNX 是第三方导出（`Luigi/campplus-zh-en-onnx`，Apache-2.0）；生产应固定官方权重或校验导出。峰值 RSS 3.1 GB 来自把 20 个 session 的 float32 波形整段驻留（45,811 s ≈ 2.8 GB），不是模型常驻内存；2230 段 192 维嵌入本身只有约 1.6 MiB。

同一条 4008-trial 协议在同权重、同脚本（`run_voiceprint_onnx.py`：Kaldi 80 维 fbank + 逐句均值归一）下换 provider 重跑，用于隔离"前端"和"provider"两个变量：

| 模型 | 路径 | EER | minDCF（归一化） | RTF | 全卡显存增量 |
|---|---|---:|---:|---:|---:|
| CAM++ | sherpa-onnx 提取器（CPU，部署参考路径） | 3.24% | 0.373 | 0.0244 | — |
| CAM++ | 显式 fbank + ONNX Runtime CPU | **2.30%** | 0.358 | 0.0081 | — |
| CAM++ | 显式 fbank + ONNX Runtime CUDA | **2.30%** | 0.358 | 0.0232 | 228 MB |
| WeSpeaker | 显式 fbank + ONNX Runtime CPU | 3.49% | **0.305** | 0.0155 | — |
| WeSpeaker | 显式 fbank + ONNX Runtime CUDA | 3.49% | **0.305** | 0.0046 | 362 MB |

三点必须分开说，不能混成一句"GPU 更准"：

1. **provider 不改变分数**：同一脚本下 CAM++ 的 CPU 与 CUDA EER 都是 2.2954%（minDCF 0.3583），WeSpeaker 都是 3.4930%（minDCF 0.3049），四位有效数字完全相同。此前把 3.24%→2.30% 归因于"CUDA kernel 数值差异"是错的，实际是**前端不同**。
2. **前端才是差异来源**：sherpa-onnx 的 CAM++ 提取器给出 3.24%，显式 fbank+CMN 的 ORT 路径给出 2.30%，同权重下差 0.94 个百分点。选型必须固定前端并写清归一化方式，否则 EER 会随接入方式漂移。
3. **吞吐取决于模型**：CAM++ 在 CPU 上反而更快（0.0081 vs 0.0232，小模型被 kernel launch 与逐段同步拖慢）；WeSpeaker 在 CUDA 上快 3.4×（0.0155 → 0.0046）。GPU 只对较大的 WeSpeaker 有意义，两个模型的全卡显存增量分别 228 MB / 362 MB。

## 5. Docker + WSL2 + RTX 2070

### 5.1 构建与 CPU 容器

- 镜像 `recording-mcp-audio-poc:cpu`，1.28 GB，基于 `debian:bookworm-slim` + 便携 CPython 3.12.13（Docker Hub/apt 代理不稳定时仍可构建）。
- 容器内已验证：
  - Silero VAD（S session）：F1 0.8801，RTF 0.00533，加载 0.086 s，RSS 477 MB。
  - Zipformer tagging（42 clips 冒烟）：mAP 0.9458，Top-1 0.900，RTF 0.0163，加载 0.793 s，RSS 542 MB。
  - CAM++ voiceprint（当时 398 trials 的 2 session 子集）：EER 1.51%，RTF 0.0124，加载 0.649 s，RSS 556 MB。
- 运行命令使用 `--network=none`，证明推理阶段完全离线。

### 5.2 GPU 直通

- `docker run --gpus all` 单独使用时容器内没有 `/dev/nvidia*`，`NVIDIA_VISIBLE_DEVICES=all` 但无法访问驱动。
- 在 WSL2 下追加宿主驱动库后成功：
  ```bash
  -v /usr/lib/wsl/lib:/usr/lib/wsl/lib:ro \
  -e LD_LIBRARY_PATH=/usr/lib/wsl/lib
  ```
- 容器内 `nvidia-smi` 显示 RTX 2070 8192 MiB；PyTorch CUDA matmul 成功，峰值 16.1 MB。
- 容器内真实模型：
  - PANNs CNN14 GPU（42 clips）：mAP 0.8819，Top-1 0.850，RTF 0.00552，加载 2.076 s，GPU 峰值 424.7 MB。
  - pyannote Community-1 GPU（前 60 s）：DER 0.0660，RTF 0.0947，加载 5.333 s，GPU 峰值 436.4 MB。该 60 s 片段只有 2 个主要说话人，DER 仅作冒烟证据。
- 8 GB 显存足够：单模型峰值 <0.5 GB；pyannote 整条流水线峰值约 0.44 GB（不含其他进程）。主机当前桌面占用约 0.7 GB，仍有充足余量。

### 5.3 自包含 GPU 镜像

- 镜像 `recording-mcp-audio-poc:gpu` 构建成功（`docker/build-portable.sh gpu`，构建 732 s），内容为 Debian bookworm-slim + 便携 CPython 3.12.13 + `torch/torchaudio 2.14.1+cu126` + `onnxruntime-gpu==1.23.2` + `pyannote.audio==4.0.0` + `panns_inference==0.1.1` + `kaldi-native-fbank`。
- 容器内自检：`torch.cuda.is_available() == True`（RTX 2070），`onnxruntime` 1.23.2 的 `CUDAExecutionProvider` 实际生效（`session.get_providers()` 含 CUDA）。
- 容器内真实模型行（全部 `--network=none`，真实容器执行）：
  - Silero VAD（L+S，`results/vad_silero_gpu.json`）：F1 0.8819/0.8801，RTF 0.0543/0.0598，全卡显存增量 78 MB。
  - PANNs CNN14 GPU（40 clips，`results/audio_tagging_panns_docker_gpu_smoke.json`）：mAP 0.8819，Top-1 0.850，RTF 0.00549，加载 1.69 s，GPU 峰值 424.7 MB。
  - pyannote Community-1 GPU（L 前 60 s，`results/diarization_pyannote_docker_gpu_smoke.json`）：DER 0.0660，RTF 0.0548，加载 2.82 s，GPU 峰值 436.4 MB。
- 两个必须记录的容器细节：
  - **ONNX Runtime 版本**：`onnxruntime-gpu==1.31.1` 要求 CUDA 13 运行时，PyPI 的 `nvidia-*-cu13` 只是 `0.0.1` 占位包；镜像与 venv 因此固定 `1.23.2`（最后一个 CUDA 12 构建），并在脚本里 `onnxruntime.preload_dlls()` + `get_providers()` 断言，避免静默回退 CPU。
  - **PANNs 离线**：`panns_inference` 的标签/权重默认落在 `~/panns_data` 并用 `wget` 拉取；离线运行需 `-e HOME=/tmp` 且只读挂载宿主 `panns_data`。
- `results/docker_gpu_validation.json` 保存了完整命令、失败/成功对照与所有行指标。

## 6. 许可证核对

详见 `LICENSES.md`。关键结论：

| 资产 | 许可证 | 风险/动作 |
|---|---|---|
| Silero VAD | MIT | 可商用 |
| sherpa-onnx | Apache-2.0 | 可商用 |
| Zipformer tagging 权重 | Apache-2.0 | 可商用，推荐 |
| CED tagging 权重 | 上游 CED **GPL-3.0** | 不建议进入非 GPL 产品；仅作基线 |
| PANNs | 代码 MIT；权重 CC-BY-4.0 | 可商用，需署名 |
| pyannote.audio | MIT | 可商用 |
| pyannote Community-1 | CC-BY-4.0 | 官方 gated 仓库已接受条款并实测（revision `3533c8c`），与公开镜像结果一致 |
| AISHELL-4 | **CC BY-SA 4.0** | 音频分发需署名+相同方式共享；本 PoC 不重分发 |
| ESC-50 | CC BY-NC 3.0 | 仅评测，不可商用/再分发 |
| CAM++ / WeSpeaker | Apache-2.0 | 可商用 |
| Debian / CPython | DFSG / PSF-2.0 | 可分发 |

## 7. 可迁移性（Mac / RK）

- 全部 ONNX/CPU 组件（Silero、Zipformer、CAM++、sherpa diarization）可直接走 ONNX Runtime；macOS 可用 CoreML/CPU EP，RK 可用 ARM CPU EP。模型总大小约 2.3 MB + 259 MB + 28 MB + 6 MB + 28 MB，边缘存储可接受。
- pyannote Community-1 和 PANNs 是 PyTorch 路线；Mac 可用 MPS/CPU，但 RK 不现实，建议放服务端/GPU。
- Zipformer 259 MB 对资源极紧的 RK 仍偏大；后续应测试 int8/更小模型（`NOT_RUN`）。
- 会议远场 VAD 阈值、sherpa 聚类人数是迁移时最先需要重新校准的两个参数。

## 8. NOT_RUN

以下项目没有真实执行，未用推测数字填充：

- pyannote Community-1 **官方 gated 仓库**：已用 HF token 接受条款并下载官方权重（`pyannote/speaker-diarization-community-1`，revision `3533c8cf8e36`），在 `L_R004S03C01` 上跑出 DER 0.0995，与公开镜像**逐位相同**（`results/diarization_pyannote_official_cuda_L.json`，状态见 `results/pyannote_official_status.json`）。镜像 checksum 风险不再影响该 session 的结论；其余 19 个 session 仍只用镜像权重测量。
- sherpa-onnx diarization 的 **GPU 执行**：`NOT_RUN`。PoC 用的 `sherpa-onnx 2.0.1` 是 CPU wheel，`--provider cuda` 会被库自己回退并打印：
  `Please compile with -DSHERPA_ONNX_ENABLE_GPU=ON. Available providers: CPUExecutionProvider, . Fallback to cpu!`
  `run_diarization.py` 捕获 fd 2 上的这条日志并在 `--provider cuda` 时 `SystemExit`，因此不会把 CPU 结果标成 CUDA。Silero、CAM++、WeSpeaker 的 GPU 行已真实执行（见 §4.1、§4.4）。
- macOS / RK 系列真机：`NOT_RUN`（无硬件）。
- 商业授权的 ambient 语料：`NOT_RUN`；只测 ESC-50（非商业）。
- 端到端接入 recording-mcp 的 ASR/Evidence/Worldline/MCP：`NOT_RUN`；本次只做隔离 PoC，未改契约。
- ASR/Evidence/Worldline/MCP 回归测试：`NOT_RUN`；因为没有改这些代码路径。

## 9. 复现

```bash
cd experiments/audio-intelligence-poc
python3.12 -m venv .venv-cpu
.venv-cpu/bin/pip install -r requirements-cpu.txt

# 1) 数据：全部 20 个 test session，channel 0，16 kHz mono
.venv-cpu/bin/python scripts/fetch_aishell4_all.py
.venv-cpu/bin/python scripts/prepare_eval.py \
  --out results/manifest_all.json --trials-out results/trials_all.json

# 2) CPU 指标（20 session）
.venv-cpu/bin/python scripts/run_vad_silero.py \
  --out results/vad_silero_all_cpu.json
.venv-cpu/bin/python scripts/run_diarization_all.py --engine sherpa --device cpu \
  --num-clusters-mode oracle --out results/diarization_sherpa_cpu_all.json
.venv-cpu/bin/python scripts/run_voiceprint.py --model campplus \
  --trials results/trials_all.json --out results/voiceprint_campplus_all_cpu.json
.venv-cpu/bin/python scripts/run_voiceprint.py --model wespeaker \
  --trials results/trials_all.json --out results/voiceprint_wespeaker_all_cpu.json
.venv-cpu/bin/python scripts/estimate_speakers.py \
  --session L_R004S03C01 S_R004S03C01 --out results/speaker_count_estimator.json

# 3) GPU 行（自包含镜像）
docker/build-portable.sh gpu
docker run --rm --gpus all --network=none --user $(id -u):$(id -g) \
  -v /usr/lib/wsl/lib:/usr/lib/wsl/lib:ro -e LD_LIBRARY_PATH=/usr/lib/wsl/lib \
  -v "$PWD/data:/poc/data:ro" -v "$PWD/models:/poc/models:ro" -v "$PWD/results:/poc/results" \
  recording-mcp-audio-poc:gpu scripts/run_vad_silero.py --provider cuda \
  --sessions S_R004S03C01 L_R004S03C01 --out results/vad_silero_gpu.json
docker run --rm --gpus all --network=none --user $(id -u):$(id -g) \
  -v /usr/lib/wsl/lib:/usr/lib/wsl/lib:ro -e LD_LIBRARY_PATH=/usr/lib/wsl/lib \
  -v "$PWD/data:/poc/data:ro" -v "$PWD/models:/poc/models:ro" -v "$PWD/results:/poc/results" \
  recording-mcp-audio-poc:gpu scripts/run_diarization_all.py --engine pyannote \
  --device cuda --out results/diarization_pyannote_cuda_all.json
docker run --rm --gpus all --network=none --user $(id -u):$(id -g) \
  -v /usr/lib/wsl/lib:/usr/lib/wsl/lib:ro -e LD_LIBRARY_PATH=/usr/lib/wsl/lib \
  -v "$PWD/data:/poc/data:ro" -v "$PWD/models:/poc/models:ro" -v "$PWD/results:/poc/results" \
  recording-mcp-audio-poc:gpu scripts/run_voiceprint_onnx.py --model campplus \
  --device cuda --trials results/trials_all.json \
  --out results/voiceprint_campplus_cuda_all.json
# 同一调用换 --model wespeaker 即可得到 voiceprint_wespeaker_cuda_all.json

# 说明：pyannote 在容器里会尝试写 $HOME/.cache/matplotlib 与 torch kernel cache；
# 上述 diarization 调用建议追加 -e HOME=/tmp -e MPLCONFIGDIR=/tmp/mpl
# （结果不受影响，只是消除每次进进程的缓存告警）。
# PANNs 容器行还需要 panns_inference 的标签表与权重：
#   -e HOME=/tmp -v "$HOME/panns_data:/tmp/panns_data:ro"

# 4) 汇总与自带环境探针
.venv-cpu/bin/python scripts/summarize.py
.venv-cpu/bin/python scripts/env_report.py
```

官方 gated 仓库（需要 HF token，且需在网页接受条款）：

```bash
HF_TOKEN=... .venv-gpu/bin/python scripts/fetch_pyannote_official.py
```

## 10. 契约影响

- 分支只新增 `experiments/audio-intelligence-poc/`；没有修改 `contracts/ingest-v1/`、ASR provider、Evidence、Worldline、MCP handler 或数据库 schema。
- 所有脚本独立运行，不 import 主应用代码；后续接入应通过 adapter/worker，而不是改核心数据结构。
- `git diff --stat` 在提交前只应出现本实验目录。

## 11. 风险与后续

1. **CED GPL-3.0**：若产品需要非 GPL 分发，禁止采用 CED；Zipformer 是当前最合适的替代。
2. **AISHELL-4 CC BY-SA 4.0**：评测可以，音频再分发需遵守 share-alike；生产训练/评测数据需单独法务确认。
3. **ESC-50 NC**：当前 ambient 指标不能作为商用结论；需换商业授权语料复测。
4. **pyannote gated**：生产需正式接受官方条款并固定权重 revision/hash；本次仍用公开镜像。
5. **sherpa 未知人数**：边缘 diarization 必须先解决人数估计；默认 threshold 的 DER 不可接受，而现有估计器两次都给出错误的"reliable"人数（L 5≠7、S 7≠5）。
6. **VAD 阈值**：不同麦克风/房间需重新校准；20 session 上 0.5 的 F1 只有 0.831，0.01 才到 0.958。
7. **前端数值一致性**：CAM++ 同权重下 sherpa-onnx 提取器 3.24%、显式 Kaldi fbank+CMN 2.30%，差 0.94 个百分点；同脚本的 CPU/CUDA 四位有效数字完全相同，说明差异来自前端而不是 provider。接入前必须固定前端与归一化，并用金标 trial 复核。
8. **样本量与结论稳定性**：扩展后（20 session / 4008 trials / 800 ambient clips）多个 2-session 结论被推翻——CAM++ 优势缩小、sherpa 人数估计不可靠；报告中的数字都以全量为准，单 session 数字仅作对照。
9. **sherpa CUDA 不可用**：`sherpa-onnx 2.0.1` 是 CPU wheel，GPU 行保持 `NOT_RUN`；边缘方案按 CPU 资源规划。

## 12. 验收索引（结论 → 证据 → 范围）

每条结论只对应一个权威结果文件；`results/summary.json` 是这些文件的自动汇合，不产生新数字。

| 结论 | 证据文件 | 测量范围与条件 | 复算方式 |
|---|---|---|---|
| 数据 20/20 session | `results/aishell4_download.json`、`results/manifest_all.json` | 20 个 WAV + RTTM，45,811.1 s，channel 0 | 下载脚本自校（单向 RTTM 校验）+ `prepare_eval.py` |
| 固定 trial 集 | `results/trials_all.json` | 4008 trials（2004/2004）、113 speaker、seed 20261110 | `prepare_eval.py --trials-out` |
| VAD 全量 CPU | `results/vad_silero_all_cpu.json` | 20 session、4 线程、无并发负载 | `binary_metrics` + `best_f1_threshold` |
| VAD GPU | `results/vad_silero_gpu.json` | L/S 两个 session、自包含镜像、CUDA provider | 同脚本 `--provider cuda`；`provider` 字段为硬证据 |
| 环境声标签 | `results/audio_tagging_{zipformer,ced,panns}_cpu.json`、`audio_tagging_panns_cuda.json`、`audio_tagging_panns_docker_gpu_smoke.json` | 800 clips / 20 个有样本类（21 个候选标签） | `evaluate()` 内 mAP/Top-k |
| pyannote 20 session | `results/diarization_pyannote_cuda_all.json` + `results/diarization_pyannote_*.rttm` | 镜像内 GPU，20 session 全量 | 聚合 DER = (FA+Miss+Conf)/Σtotal；逐 session 指标可从 RTTM 重新打分 |
| 官方 pyannote 权重 | `results/diarization_pyannote_official_cuda_L.json`、`results/pyannote_official_status.json` | 官方 gated 仓库、revision `3533c8cf8e36`、仅 L 一个 session | 状态文件记录 revision/returncode；DER 与镜像逐位相同 |
| pyannote CPU 资源 | `results/diarization_pyannote_cpu_L.json`、`diarization_pyannote_cpu_S_first300s.json` | 完整 L；S 前 300 s 子集 | 脚本直接输出 |
| sherpa oracle 人数 | `results/diarization_sherpa_cpu_all.json` + 20 个 `diarization_sherpa_cpu_<session>.json` | CPU、`num_clusters`=RTTM 人数 | 同上；逐 session `num_clusters` 与 `expected_speakers` 相等 |
| 人数估计器 | `results/speaker_count_estimator.json` + `results/diarization_sherpa_est_{L,S}_*.json` | 仅 L/S 两个 session | silhouette 选阈值；估计值驱动 sherpa 的 DER 对照 oracle |
| 声纹 sherpa 路径 | `voiceprint_campplus_all_cpu.json`、`voiceprint_wespeaker_all_cpu.json` | 4008 trials，CPU | `compute_eer` |
| 声纹显式前端 | `voiceprint_{campplus,wespeaker}_onnx_cpu_all.json`、`voiceprint_{campplus,wespeaker}_cuda_all.json` | 同 4008 trials，Kaldi fbank + CMN，CPU/CUDA | 同上；`provider` 字段为硬证据 |
| Docker / 环境 | `results/docker_gpu_validation.json`、`docker_settings_restore.json`、`environment.json` | 构建、容器内 CUDA 自检、设置还原 | 文件内命令与返回值 |
| 汇合 | `results/summary.json` | 上述文件的子集视图 | `scripts/summarize.py` |

### 12.1 三处口径修订（与原始计划的差异）

1. **WAV/RTTM 时长校验改为单向**：原计划要求两端相差 ≤2 s；实测 AISHELL-4 RTTM 末尾留白 1.8–114.1 s（`S_R004S03C01` 就有 21.0 s），对称判据会拒掉合法数据。现判据为「RTTM 不得超出音频 + 2 s」，并把 `rttm_tail_gap_seconds` 写入下载 JSON。
2. **聚合 DER 公式**：`pyannote.metrics` 的 `total` 等于 `correct + missed + confusion`（L oracle-7：1834.90 + 32.32 + 191.09 = 2058.31），假警报在 `total` 之外，所以 `(total-correct)/total` 会漏掉 FA。实现与报告统一用 `(FA + missed + confusion) / total`。
3. **声纹差异归因**：同脚本 CPU/CUDA 的 EER 完全一致（CAM++ 2.2954%、WeSpeaker 3.4930%），3.24% 与 2.30% 的差来自 sherpa-onnx 提取器 vs 显式 fbank 前端；原报告归因于 CUDA kernel 的说法已作废。

### 12.2 证据未归档（不当作已验证，也不当作失败）

- 398-trial 的 GPU 声纹冒烟、sherpa 60 s 冒烟、VAD GPU 冒烟三份 JSON 在全量运行后已删除；它们的口径已被全量文件覆盖，报告不再把这些数字当作独立证据。
- pyannote GPU 全量运行只有聚合 JSON 与逐 session RTTM：镜像内当时是旧版 `run_diarization_all.py`（读后即删 per-session JSON）。逐 session 指标可从 `results/diarization_pyannote_*.rttm` 用 `common.der_metrics` 重新打分——已抽查 `S_R004S04C01`（0.028814）与 `L_R003S01C02`（0.133192），与聚合 JSON 的 per_session 值在 6 位小数上一致。

## 13. 采用边界（有条件推荐）

| 能力 | 推荐执行路径 | 本次实际测量 | 采用前置条件 | 不得据此放行 |
|---|---|---|---|---|
| 说话人分离 | pyannote Community-1，服务端 GPU | 20 session；官方权重仅在 L 上实测（DER 0.0995，与镜像逐位相同）；峰值 436 MB | 固定官方 revision/hash（本次 `3533c8cf8e36`）；GPU 显存预算 ≥1 GB | 20 session 聚合 DER 仍来自镜像权重，官方权重只覆盖 1 个 session；Mac/RK 未验证 |
| 说话人分离（边缘） | sherpa-onnx CPU，**人数由外部给定** | 20 session、oracle 人数、DER 21.14% | 先有可靠人数来源（人工/会议先验） | 默认 threshold 聚类；估计器自动给人数 |
| VAD | Silero ONNX，CPU | 20 session、F1 0.831@0.5、RTF 0.0046 | 按设备/房间校准阈值（0.01–0.05） | 沿用 0.5 默认值上远场新设备；GPU 部署 |
| 声纹 | CAM++ 或 WeSpeaker + **固定显式 fbank+CMN 前端** | 4008 trials、同 session、EER 3.24%/3.49%（sherpa）与 2.30%/3.49%（显式前端） | 前端与归一化固定并经金标 trial 复核；权重来源校验 | 跨数据集 EER 横比；把 sherpa 与 ORT 的数字互换 |
| 环境声标签 | sherpa-onnx Zipformer | ESC-50 800 clips（CC BY-NC） | 换商用授权语料复测 | 用作商用结论 |
| 人数估计 | 不作为能力 | 仅 L/S 两个 session，两例都判 reliable 但人数都错 | — | 作为 `num_clusters` 或产品输出 |

## 14. 接入交接（交给独立工作项，不在本 PoC 内）

接入方需要先确定的目标（本 PoC 不预设、不承诺）：

- 延迟预算：单段（会议发言段）p95 处理时间，或每音频分钟的预算。
- 内存上限：常驻 RSS 与峰值；报告已给出各模型的实测值，但整段缓存波形的基准脚本不代表产品内存模型。
- 并发路数：pyannote GPU 单卡并发上限、sherpa/VAD/声纹的 CPU 线程预算。
- 准确率门限：按能力分别定（VAD F1/AUC、DER、EER/minDCF），并说明可接受的误报/漏报代价。
- 数据保留与隐私：音频与嵌入的存储位置、保留期、删除路径。

仍然缺失、会直接阻塞对应结论的资源：

- ~~HF token + 官方条款接受~~：已具备，官方权重已在 L 上实测；其余 session 若要全官方向量重跑，需要同样的 token 与约 25 分钟 GPU。
- **macOS / RK 真机**（迁移结论未验证）。
- **商用授权 ambient 语料**（ESC-50 为 CC BY-NC）。
- **产品侧 adapter/worker 设计与回归套件**（ASR/Evidence/Worldline/MCP 未改动，也未验证）。
