# 音频前端方案 PoC 与选型报告

分支：`experiment/audio-intelligence-poc`（基于 `main@51a73a1`）
日期：2026-11-10
范围：VAD、环境声标签、说话人分离、声纹；不改动 ASR、Evidence、Worldline、MCP 契约。

## 1. 结论

| 能力 | 选择 | 关键依据 | 备选/限制 |
|---|---|---|---|
| VAD | Silero VAD ONNX（官方 `silero_vad.onnx`） | CPU RTF 0.0030；ROC-AUC 0.971；2.3 MB；MIT | 远场会议需按设备校准阈值；默认 0.5 的 F1 0.881，0.02 时 0.980 |
| 环境声标签 | sherpa-onnx Zipformer Audio Tagging | mAP 0.928、Top-1 0.914；CPU RTF 0.018；Apache-2.0 | CED 精度略高但上游 GPL-3.0，不能直接用于非 GPL 产品；PANNs 作为 GPU/服务端备选 |
| 说话人分离 | pyannote Community-1（GPU） | 7 人会议 DER 9.95%；RTX 2070 峰值 436 MB | 官方仓库 gated；授权 CC-BY-4.0；CPU RTF 0.666，边缘设备不实用 |
| 声纹 | 3D-Speaker CAM++（ONNX） | EER 1.51%，minDCF 0.085；CPU RTF 0.017；28 MB；Apache-2.0 | WeSpeaker CN-Celeb 同测试 EER 4.52% |
| 边缘/离线组合 | Silero + Zipformer + CAM++ + 可选 sherpa-onnx diarization | 全部 ONNX/CPU，可迁移 macOS / RK | sherpa 聚类在未知人数时严重过分割；需人数估计或人工提示 |
| 服务端高精度组合 | pyannote Community-1 + PANNs（可选） | 8 GB 显存足够，峰值 <0.5 GB/模型 | PyTorch 依赖重；官方模型需接受 gated 条款 |

## 2. 环境

| 项 | 实测值 |
|---|---|
| 主机 | Ubuntu 24.04.5 LTS on WSL2，kernel `6.18.54.1-microsoft-standard-WSL2` |
| CPU | AMD Ryzen 5 5600X 6-Core（12 线程） |
| 内存 | 23 GB |
| GPU | NVIDIA GeForce RTX 2070，8192 MiB，驱动 610.88，CUDA UMD 13.3 |
| Docker | 29.9.2（Docker Desktop 4.98.0，WSL2 engine） |
| Python | 3.12.13（PoC venv） |
| 数据 | AISHELL-4 2 个 test session；ESC-50 2000 clips |

Docker Desktop 原有手动代理 `172.22.23.142:7890` 已失效，导致 `docker build/pull` 失败。将 `ProxyHTTPMode` 改为 `system` 并重启 Docker Desktop 后恢复；原设置已备份为 `settings-store.json.bak-audio-poc`，验证后保留 `system` 模式。该设置已记录在 `results/docker_gpu_probe.json`。

## 3. 数据与协议

### 3.1 中文多人音频

- AISHELL-4（OpenSLR SLR111）test split：
  - `L_R004S03C01`：2227.768 s，7 说话人，重叠语音 142.5 s。
  - `S_R004S03C01`：2327.119 s，5 说话人，重叠 0 s。
- 使用 8 通道录音的 **channel 0**，16 kHz mono；RTTM 作为 diarization 参考。
- 官方许可证是 **CC BY-SA 4.0**（OpenSLR SLR111）；HF 数据集卡写 `apache-2.0`，以 OpenSLR 为准。PoC 不重分发音频。
- VAD/diarization/voiceprint 共用同一批 AISHELL-4 音频。
- Ambient tagging 必须使用带环境声类别标签的语料，AISHELL-4 没有这类标签；因此 ambient 用 ESC-50，而所有语音任务仍使用同一批中文多人音频。报告没有把 ESC-50 的指标混入语音任务。

### 3.2 环境声标签

AISHELL-4 没有环境声类别标签，因此 ambient tagging 使用 ESC-50：
- 2000 clips × 5 s，CC BY-NC 3.0（仅研究/评测，不得用于商业分发）。
- 选取 21 个可映射到 AudioSet 的类别，每类 40 clips，共 840 clips。
- Top-1/Top-3 是这 21 类的闭集指标，不是 527 类 AudioSet 全量指标。

### 3.3 指标定义

- VAD：32 ms 帧；F1@0.5、Precision、Recall、ROC-AUC、PR-AUC；另给阈值扫描的最佳 F1。RTTM 的 speaker turn 含 turn 内停顿，Recall 会被低估。
- Diarization：`pyannote.metrics` DER，collar 0.25 s，`skip_overlap=False`；同时记录 FA/Miss/Confusion。
- Voiceprint：固定 398 trials（199 正 / 199 负），11 个 speaker；2 段 enrollment，余弦打分；EER、minDCF（p_target=0.01）、EER 阈值准确率。
- 资源：`RTF = 推理耗时 / 音频秒数`；模型加载时间单独统计；CPU 用峰值 RSS；GPU 用 `torch.cuda.max_memory_allocated`。
- 所有未真实执行的指标在 §8 标记 `NOT_RUN`。

## 4. 结果

### 4.1 VAD

模型：`silero_vad.onnx`（官方 `silero-vad` 包，SHA256 `1a153a22…78e3`）。协议：64 sample context + 512 sample chunk = 576；图输出已是 `[0,1]` 概率，不能再做 sigmoid。

| 指标 | 聚合（L+S） | L_R004S03C01 | S_R004S03C01 |
|---|---:|---:|---:|
| F1 @0.5 | 0.8810 | 0.8819 | 0.8801 |
| Precision @0.5 | 0.9981 | 0.9967 | 0.9997 |
| Recall @0.5 | 0.7885 | 0.7908 | 0.7861 |
| ROC-AUC | 0.9712 | 0.9173 | 0.9798 |
| PR-AUC | 0.9977 | 0.9973 | 0.9975 |
| 最佳 F1（阈值） | 0.9804（0.02） | 0.9879（0.01） | 0.9726（0.02） |
| RTF | 0.00301 | 0.00295 | 0.00307 |
| 加载 | 0.048 s | | |
| 峰值 RSS | 1119 MB | | |

解读：AISHELL-4 的 speech 帧占比 93.2%，F1 会被类别不平衡抬高；AUC/PR-AUC 更能反映排序能力。默认 0.5 偏保守，远场会议建议按麦克风/房间校准到 0.02–0.05，或做 hangover/能量门限。另已下载 TextGrid 并核对：非空 interval 的 union（S 2074.4 s、L 2172.3 s）与 RTTM 基本一致，TextGrid 不提供独立的词级静音切分，因此 RTTM 仍是可用的最佳帧级标签。CPU 侧 4 线程即可实时 300× 以上。

### 4.2 环境声标签

840 clips，21 类闭集。

| 模型 | mAP | Top-1 | Top-3 | RTF | 加载 | 峰值 RSS | GPU 峰值 | 权重大小 | 许可证 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| sherpa-onnx Zipformer | 0.9282 | 0.9137 | 0.9875 | 0.0181（CPU） | 0.865 s | 1119 MB | N/A | 259 MB | Apache-2.0 |
| sherpa-onnx CED base | 0.9309 | 0.9000 | 0.9900 | 0.0153（CPU） | 0.683 s | 1119 MB | N/A | 343 MB | 上游 CED **GPL-3.0** |
| PANNs CNN14（CPU） | 0.9070 | 0.8975 | 0.9675 | 0.0503 | 0.949 s | 1537 MB | N/A | 327 MB | 代码 MIT / 权重 CC-BY-4.0 |
| PANNs CNN14（GPU） | 0.9070 | 0.8975 | 0.9675 | 0.00208 | 1.125 s | 1511 MB | 424.7 MB | 327 MB | 代码 MIT / 权重 CC-BY-4.0 |

结论：CED 的 mAP 最高，但只比 Zipformer 高 0.0027，且 CED 上游是 GPL-3.0；Zipformer 的 Top-1 更高、权重大小更小、许可证干净。PANNs 的 mAP 低约 2.2 个百分点，CPU RTF 高 2.8–3.3 倍，但 GPU 上最快；适合服务端备选。PANNs 权重来自 HF 镜像 `ayushrsys/panns-cnn14`，md5 `541141fa2ee191a88f24a3219fff024e` 与 Zenodo 3987831 一致。

### 4.3 说话人分离

完整 `L_R004S03C01`（2227.8 s，7 人，含重叠）。

| 方案 | DER | FA | Miss | Confusion | 说话人（参考/输出） | RTF | 加载 | 峰值 RSS | GPU 峰值 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| pyannote Community-1 GPU | **0.0995** | 60.18 s | 36.24 s | 108.39 s | 7 / 5 | 0.0316 | 1.744 s | 2323 MB | 436.4 MB |
| sherpa-onnx（oracle 人数=7） | 0.1404 | 65.56 s | 32.32 s | 191.09 s | 7 / 7 | 0.0914 | 0.266 s | 1119 MB | N/A |
| sherpa-onnx（默认 threshold=0.5） | 0.4904 | 68.63 s | 33.31 s | 907.39 s | 7 / **114** | 0.0830 | 0.268 s | 2201 MB | N/A |
| sherpa-onnx（threshold=0.4） | 0.6073 | — | — | — | 7 / **157** | 0.0706 | 0.329 s | — | N/A |

补充：
- pyannote CPU 子集（S session 前 300 s）：DER 0.0276，参考 5 / 输出 2，RTF 0.666，加载 0.734 s，RSS 1474 MB。该子集只有 2 个主要说话人，DER 不能与完整 L 直接比较，仅用于 CPU 资源。
- sherpa 在 L 前 300 s（7 人）上阈值扫描：0.45→DER 0.316/21 人，0.5→0.266/18 人，0.55→0.266/17 人，0.6→0.263/16 人，0.7→0.168/11 人；oracle 7 人才到 0.140。说明 **未知人数时 threshold 聚类不可靠**。
- pyannote 只输出 5 个 speaker，漏掉 2 个；混淆 108 s。sherpa oracle 7 人时人数正确，但混淆 191 s。

结论：精度优先选 pyannote Community-1；边缘/CPU 可选 sherpa-onnx，但必须有人数估计或人工指定 `num_clusters`，不能直接用默认 threshold。

### 4.4 声纹

398 trials，11 speakers，余弦打分。

| 模型 | EER | minDCF（归一化） | EER 阈值准确率 | RTF | 加载 | 峰值 RSS | 维度 | 大小 | 许可证 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| 3D-Speaker CAM++ | **1.51%** | **0.0854** | 98.49% | 0.0174 | 0.604 s | 1119 MB | 192 | 28.3 MB | Apache-2.0 |
| WeSpeaker CN-Celeb ResNet34-LM | 4.52% | 0.1407 | 95.48% | 0.0304 | 0.112 s | 1119 MB | 256 | 26.5 MB | Apache-2.0 |

说明：测试是 AISHELL-4 同 session、远场、非重叠片段，难度与 VoxCeleb/CN-Celeb 官方 trial 不同；1.51% 不能直接与其他论文数字横比。CAM++ 在中文会议条件明显优于 WeSpeaker 的 CN-Celeb 模型。CAM++ ONNX 是第三方导出（`Luigi/campplus-zh-en-onnx`，Apache-2.0）；生产应固定官方权重或校验导出。

## 5. Docker + WSL2 + RTX 2070

### 5.1 构建与 CPU 容器

- 镜像 `recording-mcp-audio-poc:cpu`，1.28 GB，基于 `debian:bookworm-slim` + 便携 CPython 3.12.13（Docker Hub/apt 代理不稳定时仍可构建）。
- 容器内已验证：
  - Silero VAD（S session）：F1 0.8801，RTF 0.00533，加载 0.086 s，RSS 477 MB。
  - Zipformer tagging（42 clips 冒烟）：mAP 0.9458，Top-1 0.900，RTF 0.0163，加载 0.793 s，RSS 542 MB。
  - CAM++ voiceprint：EER 1.51%，RTF 0.0124，加载 0.649 s，RSS 556 MB。
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

- 首次构建因 `panns_inference==0.2.1` 不存在而失败；已把 pin 修正为 `0.1.1` 并重建；重建未在本次会话内完成（已停止以避免继续占用资源），因此 **自包含 GPU 镜像标记 `NOT_RUN`**。
- 上述 §5.2 的所有 GPU 结果均在真实容器内执行：使用 CPU 镜像，挂载宿主 `.venv-gpu` 与 WSL 驱动库。`results/docker_gpu_validation.json` 保存了成功/失败命令与指标，未把宿主进程结果混入 Docker 结论。

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
| pyannote Community-1 | CC-BY-4.0 | 官方仓库 gated；需接受条款 |
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

- pyannote Community-1 **官方 gated 仓库**：无 HF token，`NOT_RUN`；PoC 使用公开镜像 `pyannote-community/speaker-diarization-community-1`（CC-BY-4.0）。镜像无官方 checksum，文件完整性无法对 pyannote 校验；生产必须使用官方 gated 仓库。
- Silero VAD / sherpa diarization / CAM++ / WeSpeaker 的 **GPU 执行**：`NOT_RUN`（目标为 CPU/ONNX；Docker GPU 已用 PANNs 与 pyannote 验证）。
- pyannote CPU **完整 2227.8 s**：`NOT_RUN`；只跑了 S session 前 300 s。
- 自包含 GPU 镜像：`NOT_RUN`；首次构建因 PANNs pin 错误失败，修正后重建未在本次会话完成。GPU 容器验证使用 CPU 镜像 + 宿主 `.venv-gpu`（真实容器执行，见 §5.2）。
- macOS / RK 系列真机：`NOT_RUN`（无硬件）。
- AISHELL-4 全部 20 个 test session：`NOT_RUN`；只测 2 个。
- 商业授权的 ambient 语料：`NOT_RUN`；只测 ESC-50（非商业）。
- 未知说话人数估计器：`NOT_RUN`；这是 sherpa 边缘方案的前置条件。
- 端到端接入 recording-mcp 的 ASR/Evidence/Worldline/MCP：`NOT_RUN`；本次只做隔离 PoC，未改契约。
- ASR/Evidence/Worldline/MCP 回归测试：`NOT_RUN`；因为没有改这些代码路径。

## 9. 复现

```bash
# 1) 数据（AISHELL-4 channel 0）
curl -L -o L.flac https://huggingface.co/datasets/AISHELL/AISHELL-4/resolve/main/test/wav/L_R004S03C01.flac
curl -L -o L.rttm https://huggingface.co/datasets/AISHELL/AISHELL-4/resolve/main/test/TextGrid/L_R004S03C01.rttm
ffmpeg -i L.flac -af "pan=mono|c0=c0" -ar 16000 -sample_fmt s16 L.wav

# 2) 指标
.venv-cpu/bin/python scripts/prepare_eval.py
.venv-cpu/bin/python scripts/run_vad_silero.py --model models/silero-official/model.onnx
.venv-cpu/bin/python scripts/run_audio_tagging.py --engine sherpa --sherpa-model zipformer --out results/audio_tagging_zipformer_cpu.json
.venv-gpu/bin/python scripts/run_audio_tagging.py --engine panns --device cuda --out results/audio_tagging_panns_cuda.json
.venv-cpu/bin/python scripts/run_diarization.py --engine sherpa --num-clusters 7 --out results/diarization_sherpa_cpu_L_c7.json
.venv-gpu/bin/python scripts/run_diarization.py --engine pyannote --device cuda --out results/diarization_pyannote_cuda_L.json
.venv-cpu/bin/python scripts/run_voiceprint.py --model campplus
.venv-cpu/bin/python scripts/run_voiceprint.py --model wespeaker

# 3) Docker CPU
docker/build-portable.sh cpu
docker run --rm --network=none --user $(id -u):$(id -g) \
  -v "$PWD/data:/poc/data:ro" -v "$PWD/models:/poc/models:ro" -v "$PWD/results:/poc/results" \
  recording-mcp-audio-poc:cpu scripts/run_vad_silero.py \
  --model models/silero-official/model.onnx --sessions S_R004S03C01 \
  --out results/vad_silero_docker.json

# 4) Docker GPU（WSL2 需要挂载宿主驱动库）
docker run --rm --gpus all --network=none \
  -v /home/ev/code/recording-mcp:/home/ev/code/recording-mcp \
  -v /usr/lib/wsl/lib:/usr/lib/wsl/lib:ro -e LD_LIBRARY_PATH=/usr/lib/wsl/lib \
  --entrypoint /opt/python/bin/python3 recording-mcp-audio-poc:cpu \
  -c "import torch; print(torch.cuda.is_available(), torch.cuda.get_device_name(0))"
```

## 10. 契约影响

- 分支只新增 `experiments/audio-intelligence-poc/`；没有修改 `contracts/ingest-v1/`、ASR provider、Evidence、Worldline、MCP handler 或数据库 schema。
- 所有脚本独立运行，不 import 主应用代码；后续接入应通过 adapter/worker，而不是改核心数据结构。
- `git diff --stat` 在提交前只应出现本实验目录。

## 11. 风险与后续

1. **CED GPL-3.0**：若产品需要非 GPL 分发，禁止采用 CED；Zipformer 是当前最合适的替代。
2. **AISHELL-4 CC BY-SA 4.0**：评测可以，音频再分发需遵守 share-alike；生产训练/评测数据需单独法务确认。
3. **ESC-50 NC**：当前 ambient 指标不能作为商用结论；需换商业授权语料复测。
4. **pyannote gated**：生产需正式接受官方条款并固定权重 revision/hash。
5. **sherpa 未知人数**：边缘 diarization 必须先解决人数估计；否则默认 threshold 的 DER 不可接受。
6. **VAD 阈值**：不同麦克风/房间需重新校准；0.5 不是远场会议的最佳点。
7. **样本量**：2 个 AISHELL-4 session、398 trials、840 ambient clips，结论方向明确，但置信区间仍偏宽；扩展测试标 `NOT_RUN`。
