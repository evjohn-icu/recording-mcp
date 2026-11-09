# Design

## Data flow

```
upload -> ingest stage (ffmpeg -> 16k mono PCM)
       -> asr stage -> LocalTranscriber.Transcribe
            -> python scripts/local-asr.py --backend ark|qwen --dtype float16
                 -> window WAVs -> model.generate -> JSON windows
            -> asrinput.Result per window (provider local-ark / local-qwen)
       -> memory.Store.IngestTranscript (unchanged)
```

## Decisions

- **One runner, two backends.** Both models expose different APIs; the runner hides that and
  prints one JSON contract. Go never imports Python libraries.
- **Window plan owned by the runner.** Default 30 s window / 2 s overlap (ARK's audio limit is
  30 s). Go validates bounds against the PCM duration and rejects duplicates.
- **float16 only.** Turing (RTX 2070) has no native bf16; accepting `--asr-dtype bfloat16`
  would silently fall back or fail at generation time.
- **No fabricated metadata.** `confidence` stays nil, `valid_from`/recording start stay
  unknown, `resource_id` is not invented. Only the model id is recorded.
- **Startup probe.** `SelectTranscriber(ProbePython: true)` checks the interpreter and modules
  (`torch`, `transformers`, `numpy`, plus `qwen_asr` for Qwen) before the first job.
- **Default interpreter.** `./.venv-asr/bin/python` when present, otherwise `python3`. The
  system Python may be newer than torch/qwen-asr wheels.

## Failure semantics

- Missing script or Python modules -> startup error for web/worker; an unreachable model id/path fails on the first job.
- Runner non-zero exit -> job error carrying the tail of stderr (retryable).
- Invalid JSON, duplicate window index, or out-of-range bounds -> job error; no rows written.
- Empty transcription text is still ingested as a window with no retained turn, preserving
  coverage accounting.
