# Audio intelligence PoC

Isolated PoC for VAD, ambient audio tagging, speaker diarization, and
voiceprint. It does not import or modify the recording-mcp application, its
contracts, ASR provider, Evidence, Worldline, or MCP handlers.

Read the selection report: [`report.md`](report.md). License audit:
[`LICENSES.md`](LICENSES.md).

## Layout

```text
scripts/
  common.py                 shared metrics/IO
  prepare_eval.py           AISHELL-4 manifest + fixed voiceprint trials
  fetch_aishell4_all.py     download + convert the full test split
  run_vad_silero.py         Silero VAD ONNX, CPU or CUDA provider
  run_audio_tagging.py      sherpa-onnx CED/Zipformer or PANNs
  run_diarization.py        sherpa-onnx or pyannote Community-1, one session
  run_diarization_all.py    one diarization engine over every test session
  run_voiceprint.py         CAM++ or WeSpeaker ONNX via sherpa-onnx, CPU
  run_voiceprint_onnx.py    same models via onnxruntime CPU/CUDA
  estimate_speakers.py      Silero windows + CAM++ agglomerative speaker count
  fetch_pyannote_official.py gated pyannote download (needs HF token)
  summarize.py              primary results into results/summary.json
  env_report.py             host/Docker/GPU probes + model inventory
docker/
  Dockerfile.cpu.portable   Debian + portable CPython 3.12
  Dockerfile.gpu.portable   Debian + portable CPython 3.12 + torch cu126 + ORT GPU
  build-portable.sh         stages the interpreter and builds the image
models/                     downloaded weights (gitignored)
data/                       AISHELL-4 / ESC-50 (gitignored)
results/                    measured JSON, RTTM, environment probes
```

## Data

- **AISHELL-4** test split (20 sessions), channel 0, 16 kHz mono.
  Official license: **CC BY-SA 4.0** (OpenSLR SLR111).
- **ESC-50** for ambient tagging: CC BY-NC 3.0, evaluation only.
- Audio and weights are not committed. The report records the exact URLs,
  sizes, and SHA-256 values; `results/aishell4_download.json` records per-session
  durations and file sizes.

## Reproduce

```bash
python3.12 -m venv .venv-cpu
.venv-cpu/bin/pip install -r requirements-cpu.txt

# Optional GPU environment for pyannote/PANNs/ONNX GPU:
python3.12 -m venv .venv-gpu
.venv-gpu/bin/pip install torch torchaudio --index-url https://download.pytorch.org/whl/cu126
.venv-gpu/bin/pip install -r requirements-gpu.txt

# Data
.venv-cpu/bin/python scripts/fetch_aishell4_all.py
.venv-cpu/bin/python scripts/prepare_eval.py \
  --out results/manifest_all.json --trials-out results/trials_all.json

# VAD
.venv-cpu/bin/python scripts/run_vad_silero.py --model models/silero-official/model.onnx \
  --out results/vad_silero_all_cpu.json
.venv-gpu/bin/python scripts/run_vad_silero.py --provider cuda \
  --sessions S_R004S03C01 L_R004S03C01 --out results/vad_silero_gpu.json

# Diarization
.venv-cpu/bin/python scripts/run_diarization_all.py --engine sherpa --device cpu \
  --num-clusters-mode oracle --out results/diarization_sherpa_cpu_all.json
.venv-gpu/bin/python scripts/run_diarization_all.py --engine pyannote --device cuda \
  --out results/diarization_pyannote_cuda_all.json

# Voiceprint
.venv-cpu/bin/python scripts/run_voiceprint.py --model campplus \
  --trials results/trials_all.json --out results/voiceprint_campplus_all_cpu.json
.venv-cpu/bin/python scripts/run_voiceprint_onnx.py --model campplus --device cpu \
  --trials results/trials_all.json --out results/voiceprint_campplus_onnx_cpu_all.json
.venv-gpu/bin/python scripts/run_voiceprint_onnx.py --model campplus --device cuda \
  --trials results/trials_all.json --out results/voiceprint_campplus_cuda_all.json
# same two commands with --model wespeaker cover the WeSpeaker rows

# Speaker count estimate (edge precondition for sherpa)
.venv-cpu/bin/python scripts/estimate_speakers.py \
  --session L_R004S03C01 S_R004S03C01 --out results/speaker_count_estimator.json

.venv-cpu/bin/python scripts/summarize.py
```

`run_diarization.py` uses `--num-clusters` when the speaker count is known;
without it, sherpa's threshold clustering over-segments AISHELL-4. This is a
measured finding, not a default recommendation. `estimate_speakers.py` reports a
silhouette-based `reliable` flag, but that flag only says the clustering is well
separated: on the full test split both sessions were flagged reliable while the
counts were wrong (5 vs 7 and 7 vs 5), so it does not certify the speaker count.

## Docker

The build uses a portable CPython 3.12 because Docker Hub/apt access was
unreliable in the target WSL2 environment:

```bash
docker/build-portable.sh cpu
docker/build-portable.sh gpu

docker run --rm --network=none --user $(id -u):$(id -g) \
  -v "$PWD/data:/poc/data:ro" -v "$PWD/models:/poc/models:ro" \
  -v "$PWD/results:/poc/results" recording-mcp-audio-poc:cpu \
  scripts/run_vad_silero.py --model models/silero-official/model.onnx \
  --out results/vad_silero_all_cpu.json
```

On WSL2, `--gpus all` alone did not expose the driver. The working invocation
also mounts the host WSL driver libraries:

```bash
docker run --rm --gpus all --network=none \
  -v /usr/lib/wsl/lib:/usr/lib/wsl/lib:ro \
  -e LD_LIBRARY_PATH=/usr/lib/wsl/lib \
  recording-mcp-audio-poc:gpu -c \
  "import torch, onnxruntime as ort; print(torch.cuda.is_available(), ort.get_available_providers())"
```

No user data or private recordings are used; all audio comes from the public
corpora above.
