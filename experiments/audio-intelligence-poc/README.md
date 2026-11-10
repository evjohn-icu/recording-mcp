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
  run_vad_silero.py         Silero VAD ONNX, CPU
  run_audio_tagging.py      sherpa-onnx CED/Zipformer or PANNs
  run_diarization.py        sherpa-onnx or pyannote Community-1
  run_voiceprint.py         CAM++ or WeSpeaker ONNX, CPU
  env_report.py             host/Docker/GPU probes + model inventory
docker/
  Dockerfile.cpu.portable   Debian + portable CPython 3.12
  Dockerfile.gpu.portable   Debian + portable CPython 3.12 + torch cu126
  build-portable.sh         stages the interpreter and builds the image
models/                     downloaded weights (gitignored)
data/                       AISHELL-4 / ESC-50 (gitignored)
results/                    measured JSON, RTTM, environment probes
```

## Data

- **AISHELL-4** test split, channel 0, 16 kHz mono:
  `L_R004S03C01` (7 speakers) and `S_R004S03C01` (5 speakers).
  Official license: **CC BY-SA 4.0** (OpenSLR SLR111).
- **ESC-50** for ambient tagging: CC BY-NC 3.0, evaluation only.
- Audio and weights are not committed. The report records the exact URLs,
  sizes, and SHA-256 values.

## Reproduce

```bash
python3.12 -m venv .venv-cpu
.venv-cpu/bin/pip install -r requirements-cpu.txt

# Optional GPU environment for pyannote/PANNs:
python3.12 -m venv .venv-gpu
.venv-gpu/bin/pip install torch --index-url https://download.pytorch.org/whl/cu126
.venv-gpu/bin/pip install pyannote.audio==4.0.0 panns_inference \
  numpy scipy scikit-learn soundfile librosa psutil pandas tqdm \
  huggingface_hub pyannote.metrics kaldi-native-fbank

.venv-cpu/bin/python scripts/prepare_eval.py
.venv-cpu/bin/python scripts/run_vad_silero.py --model models/silero-official/model.onnx
.venv-cpu/bin/python scripts/run_audio_tagging.py --engine sherpa --sherpa-model zipformer
.venv-gpu/bin/python scripts/run_audio_tagging.py --engine panns --device cuda
.venv-cpu/bin/python scripts/run_voiceprint.py --model campplus
.venv-gpu/bin/python scripts/run_diarization.py --engine pyannote --device cuda
.venv-cpu/bin/python scripts/run_diarization.py --engine sherpa --num-clusters 7
.venv-cpu/bin/python scripts/env_report.py
```

`run_diarization.py` uses `--num-clusters` when the speaker count is known;
without it, sherpa's threshold clustering over-segments AISHELL-4. This is a
measured finding, not a default recommendation.

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
  --sessions S_R004S03C01 --out results/vad_silero_docker.json
```

On WSL2, `--gpus all` alone did not expose the driver. The working invocation
also mounts the host WSL driver libraries:

```bash
docker run --rm --gpus all --network=none \
  -v /usr/lib/wsl/lib:/usr/lib/wsl/lib:ro \
  -e LD_LIBRARY_PATH=/usr/lib/wsl/lib \
  recording-mcp-audio-poc:gpu -c "import torch; print(torch.cuda.is_available())"
```

No user data or private recordings are used; all audio comes from the public
corpora above.
