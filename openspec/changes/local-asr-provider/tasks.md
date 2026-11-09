# Tasks

- [x] Add `LocalTranscriber` and window mapping (`internal/cloudsync/local_asr.go`).
- [x] Add `SelectTranscriber` shared by web and worker, with float16 enforcement and startup probe.
- [x] Add `scripts/local-asr.py` (ARK + Qwen backends, float16, window JSON).
- [x] Wire `--asr-provider ark|qwen|local` and local flags into both binaries.
- [x] Unit tests: mapping, float16 flags, runner failure, invalid output, provider selection, probe.
- [x] Real-model smoke: run both checkpoints on a speech sample and record the transcript
  (`docs/productization/local-asr-smoke.md`, 2026-11-09, RTX 2070, float16).
- [x] Score a real 120 s recording against an existing transcript: Qwen CER 5.6%, ARK 10.4%, magpie doubao-lite 3.8%
  (`docs/productization/real-audio-local-asr.md`; audio/transcript not committed).
- [x] Document install, download, flags, VRAM and limitations (`docs/productization/local-asr.md`).
