# Local ASR Provider Change

## Why

The beta only supported `doubao` (cloud, paid, key-bound) and `fixture` (synthetic text).
Users with an NVIDIA GPU asked for offline recognition. Two Apache-2.0 checkpoints cover the
need: `Edge0/ARK-ASR-0.6B` (fast, Chinese/English) and `Qwen/Qwen3-ASR-1.7B` (higher accuracy).

## What Changes

- Add `--asr-provider ark|qwen|local` to both `context-web` and `context-worker`.
- `internal/cloudsync.LocalTranscriber` runs `scripts/local-asr.py` per recording and maps
  window output to immutable `asrinput.Result` records. Inference is float16 only.
- Startup probes the interpreter, runner script and required Python modules, and fails with a
  clear message instead of failing the first job.
- `internal/cloudsync.SelectTranscriber` is the single provider constructor shared by web and
  worker, so both accept the same values.

## Non-goals

- No default change: `ASR_PROVIDER` stays `off`.
- No model weights in the repository or image.
- No timestamps finer than the configured window.
- The Docker image does not ship Python/torch; local ASR runs on a host worker with a GPU.
