# Tasks

- [x] Add `LocalTranscriber` and window mapping (`internal/cloudsync/local_asr.go`).
- [x] Add `SelectTranscriber` shared by web and worker, with float16 enforcement and startup probe.
- [x] Add `scripts/local-asr.py` (ARK + Qwen backends, float16, window JSON).
- [x] Wire `--asr-provider ark|qwen|local` and local flags into both binaries.
- [x] Unit tests: mapping, float16 flags, runner failure, invalid output, provider selection, probe.
- [x] Real-model smoke: run both checkpoints on a speech sample and record the transcript
  (`docs/productization/local-asr-smoke.md`, 2026-11-09, RTX 2070, float16).
- [x] Document install, download, flags, VRAM and limitations (`docs/productization/local-asr.md`).
