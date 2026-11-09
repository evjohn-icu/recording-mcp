#!/usr/bin/env python3
"""Offline ASR runner for the recording-mcp worker.

Reads a 16 kHz mono S16LE PCM file, splits it into overlapping windows, runs a
local model in float16, and prints one JSON document:

    {"backend": "...", "model": "...", "language": "...",
     "windows": [{"index": 1, "start_ms": 0, "end_ms": 30000, "text": "..."}]}

With --stream the document is replaced by one JSON line per completed window;
with --skip those 1-based window indexes are not decoded, so a restarted job
only pays for work it has not committed yet. --probe loads the model and prints
one JSON object with device, dtype and VRAM, so a launcher can prove the model
works before starting the stack.

Progress goes to stderr; stdout stays machine-readable. Model weights are never
bundled here: pass a local directory or a Hugging Face id.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import signal
import sys
import tempfile
import threading
import time
import wave

SAMPLE_RATE = 16000
BYTES_PER_MS = 32  # S16LE mono 16 kHz

EXIT_MODEL_LOAD = 4
EXIT_WINDOW_TIMEOUT = 5

# Pinned snapshots shared with internal/cloudsync/select.go and
# scripts/model-revisions.json; the runner must not silently follow main.
DEFAULT_MODELS = {
    "ark": "Edge0/ARK-ASR-0.6B",
    "qwen": "Qwen/Qwen3-ASR-1.7B",
}
DEFAULT_REVISIONS = {
    "ark": "45776b56d58cdfb2e2eb632f7e110f38684633e0",
    "qwen": "7278e1e70fe206f11671096ffdd38061171dd6e5",
}

MODEL_LOAD_TIMEOUT = 300
WINDOW_TIMEOUT = 300


class WindowTimeout(Exception):
    """Raised by the SIGALRM handler when one window exceeds its budget."""


def plan_windows(duration_ms: int, window_ms: int, overlap_ms: int) -> list[tuple[int, int]]:
    if window_ms <= 0:
        raise ValueError("window-ms must be positive")
    if overlap_ms < 0 or overlap_ms >= window_ms:
        raise ValueError("overlap-ms must be in [0, window-ms)")
    step = window_ms - overlap_ms
    out: list[tuple[int, int]] = []
    start = 0
    while start < duration_ms:
        end = min(start + window_ms, duration_ms)
        out.append((start, end))
        if end >= duration_ms:
            break
        start += step
    return out


def parse_skip(raw: str, window_count: int) -> set[int]:
    """Parse --skip into validated 1-based indexes; anything else is an error."""
    out: set[int] = set()
    for part in (raw or "").split(","):
        part = part.strip()
        if not part:
            continue
        try:
            index = int(part)
        except ValueError:
            raise ValueError(f"--skip value {part!r} is not an integer")
        if index < 1 or index > window_count:
            raise ValueError(f"--skip index {index} is outside the {window_count}-window plan")
        out.add(index)
    return out


def write_wav(path: str, samples) -> None:
    with wave.open(path, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(SAMPLE_RATE)
        w.writeframes(samples.tobytes())


def clean_text(text: str) -> str:
    text = re.sub(r"<\|[^|]*\|>", " ", text or "")
    return re.sub(r"\s+", " ", text).strip()


def log(message: str) -> None:
    print(f"local-asr: {message}", file=sys.stderr, flush=True)


def load_ark(model_path: str, device: str, revision: str):
    import torch
    from transformers import AutoModelForCausalLM, AutoProcessor, AutoTokenizer

    log(f"loading ARK-ASR from {model_path}@{revision or 'main'} (float16, {device})")
    revision = revision or None
    processor = AutoProcessor.from_pretrained(model_path, trust_remote_code=True, revision=revision)
    tokenizer = AutoTokenizer.from_pretrained(model_path, trust_remote_code=True, revision=revision)
    try:
        model = AutoModelForCausalLM.from_pretrained(
            model_path, trust_remote_code=True, dtype=torch.float16, attn_implementation="sdpa", revision=revision
        )
    except TypeError:
        model = AutoModelForCausalLM.from_pretrained(
            model_path, trust_remote_code=True, torch_dtype=torch.float16, attn_implementation="sdpa", revision=revision
        )
    model = model.to(device).eval()
    eos = tokenizer.eos_token_id
    keep = {eos} if isinstance(eos, int) else set(eos or [])
    bad_ids = set(tokenizer.all_special_ids) - keep
    bad_ids.update(
        token_id
        for token, token_id in tokenizer.get_added_vocab().items()
        if token.startswith("<") and token.endswith(">") and token_id not in keep
    )
    bad_words_ids = [[token_id] for token_id in sorted(bad_ids)]
    return processor, tokenizer, model, bad_words_ids


def transcribe_ark_window(args, bundle, wav_path: str) -> str:
    import torch

    processor, tokenizer, model, bad_words_ids = bundle
    conversation = [
        {
            "role": "user",
            "content": [
                {"type": "audio", "path": wav_path},
                {"type": "text", "text": (f"Please transcribe this audio in {args.language}."
                                          if args.language else "Please transcribe this audio.")},
            ],
        }
    ]
    inputs = processor.apply_chat_template(
        conversation,
        add_generation_prompt=True,
        return_tensors="pt",
        sampling_rate=SAMPLE_RATE,
        audio_padding="longest",
        text_kwargs={"padding": "longest"},
        audio_max_length=args.window_ms * SAMPLE_RATE // 1000,
    )
    inputs = inputs.to(args.device)
    if "audios" in inputs:
        inputs["audios"] = inputs["audios"].to(dtype=torch.float16)
    with torch.inference_mode():
        outputs = model.generate(
            **inputs,
            do_sample=False,
            max_new_tokens=args.max_new_tokens,
            pad_token_id=tokenizer.pad_token_id,
            eos_token_id=tokenizer.eos_token_id,
            bad_words_ids=bad_words_ids,
        )
    text = tokenizer.batch_decode(
        outputs[:, inputs.input_ids.shape[1]:], skip_special_tokens=True
    )[0]
    del inputs, outputs
    torch.cuda.empty_cache()
    return clean_text(text)


def run_ark(args, wavs: list[str]) -> tuple[list[str], str]:
    bundle = load_ark(args.model, args.device, args.revision)
    texts: list[str] = []
    for i, wav_path in enumerate(wavs, 1):
        log(f"window {i}/{len(wavs)}")
        texts.append(transcribe_ark_window(args, bundle, wav_path))
    return texts, args.language or ""


def load_qwen(model_path: str, device: str, max_new_tokens: int, revision: str):
    import torch
    from qwen_asr import Qwen3ASRModel

    log(f"loading Qwen3-ASR from {model_path}@{revision or 'main'} (float16, {device})")
    model = Qwen3ASRModel.from_pretrained(
        model_path,
        dtype=torch.float16,
        device_map=device,
        max_inference_batch_size=1,
        max_new_tokens=max_new_tokens,
        revision=revision or None,
    )
    return model


def transcribe_qwen_window(args, model, wav_path: str) -> tuple[str, str]:
    import torch

    langs = [args.language] if args.language else None
    results = model.transcribe(audio=[wav_path], language=langs)
    text = ""
    language = args.language or ""
    for result in results:
        text = clean_text(getattr(result, "text", ""))
        if not language:
            language = getattr(result, "language", "") or ""
    torch.cuda.empty_cache()
    return text, language


def run_qwen(args, wavs: list[str]) -> tuple[list[str], str]:
    model = load_qwen(args.model, args.device, args.max_new_tokens, args.revision)
    texts: list[str] = []
    language = args.language or ""
    for i, wav_path in enumerate(wavs, 1):
        log(f"window {i}/{len(wavs)}")
        text, detected = transcribe_qwen_window(args, model, wav_path)
        texts.append(text)
        if not language:
            language = detected
    return texts, language


def load_model(args):
    """Load the selected model once and return a backend handle."""
    if args.backend == "ark":
        return ("ark", load_ark(args.model, args.device, args.revision))
    return ("qwen", load_qwen(args.model, args.device, args.max_new_tokens, args.revision))


def transcribe_window(args, handle, wav_path: str) -> tuple[str, str]:
    kind, payload = handle
    if kind == "ark":
        return transcribe_ark_window(args, payload, wav_path), args.language or ""
    return transcribe_qwen_window(args, payload, wav_path)


def load_model_with_timeout(args):
    """Load the model in a daemon thread so a wedged import cannot hang startup.

    The thread cannot be joined after the deadline without reintroducing the
    hang, so the process exits with a code the Go side maps to
    asr_model_load_failed.
    """
    outcome: dict = {}

    def target():
        try:
            outcome["value"] = load_model(args)
        except BaseException as exc:  # noqa: BLE001 - reported at the boundary
            outcome["error"] = exc

    worker = threading.Thread(target=target, daemon=True)
    worker.start()
    worker.join(args.load_timeout)
    if worker.is_alive():
        print(f"local-asr: model load timed out after {args.load_timeout}s", file=sys.stderr, flush=True)
        os._exit(EXIT_MODEL_LOAD)
    if "error" in outcome:
        raise outcome["error"]
    return outcome["value"]


def _alarm(_signum, _frame):
    raise WindowTimeout()


def with_window_timeout(seconds: int, fn, *fn_args):
    if seconds <= 0:
        return fn(*fn_args)
    previous = signal.signal(signal.SIGALRM, _alarm)
    signal.setitimer(signal.ITIMER_REAL, seconds)
    try:
        return fn(*fn_args)
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous)


def cuda_stats(device: str) -> tuple[int, int, int]:
    """Return (total_mb, free_mb, peak_mb); zeros on CPU devices."""
    if not device.startswith("cuda"):
        return 0, 0, 0
    import torch

    if not torch.cuda.is_available():
        return 0, 0, 0
    index = 0
    if ":" in device:
        try:
            index = int(device.split(":", 1)[1])
        except ValueError:
            index = 0
    free, total = torch.cuda.mem_get_info(index)
    peak = torch.cuda.max_memory_allocated(index)
    mib = 1024 * 1024
    return total // mib, free // mib, peak // mib


def probe(args) -> int:
    started = time.monotonic()
    try:
        load_model_with_timeout(args)
    except BaseException as exc:  # noqa: BLE001 - reported at the boundary
        print(f"local-asr: model load failed: {exc}", file=sys.stderr, flush=True)
        return EXIT_MODEL_LOAD
    elapsed = int((time.monotonic() - started) * 1000)
    total, free, peak = cuda_stats(args.device)
    payload = {
        "backend": args.backend,
        "model": args.model,
        "revision": args.revision,
        "device": args.device,
        "dtype": args.dtype,
        "model_load_ms": elapsed,
        "vram_total_mb": total,
        "vram_free_mb": free,
        "vram_peak_mb": peak,
    }
    json.dump(payload, sys.stdout, ensure_ascii=False)
    sys.stdout.write("\n")
    sys.stdout.flush()
    return 0


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="offline ASR runner (float16)")
    p.add_argument("--backend", choices=["ark", "qwen"], required=True)
    p.add_argument("--model", default="", help="local model dir or Hugging Face id")
    p.add_argument("--revision", default="", help="Hugging Face revision (commit); empty uses the pinned snapshot of the built-in model")
    p.add_argument("--pcm", default="", help="16 kHz mono S16LE PCM input (required unless --probe)")
    p.add_argument("--dtype", default="float16", help="only float16 is supported")
    p.add_argument("--device", default="cuda:0")
    p.add_argument("--language", default="", help="optional language hint, e.g. Chinese")
    p.add_argument("--window-ms", type=int, default=30000)
    p.add_argument("--overlap-ms", type=int, default=2000)
    p.add_argument("--max-new-tokens", type=int, default=512)
    p.add_argument("--max-windows", type=int, default=0, help="debug: limit processed windows")
    p.add_argument("--dry-run", action="store_true", help="emit the window plan without loading a model")
    p.add_argument("--probe", action="store_true", help="load the model and print device/dtype/VRAM diagnostics only")
    p.add_argument("--stream", action="store_true", help="emit one JSON line per completed window")
    p.add_argument("--skip", default="", help="comma-separated 1-based window indexes already committed")
    p.add_argument("--load-timeout", type=int, default=MODEL_LOAD_TIMEOUT, help="seconds allowed for model loading")
    p.add_argument("--window-timeout", type=int, default=WINDOW_TIMEOUT, help="seconds allowed per window")
    return p.parse_args()


def main() -> int:
    args = parse_args()
    if args.dtype != "float16":
        raise SystemExit("local-asr: only --dtype float16 is supported")
    if args.model == "" and args.backend in DEFAULT_MODELS:
        args.model = DEFAULT_MODELS[args.backend]
    if not args.revision and args.model == DEFAULT_MODELS.get(args.backend):
        args.revision = DEFAULT_REVISIONS[args.backend]
    if args.probe:
        if args.pcm:
            raise SystemExit("local-asr: --probe does not read --pcm")
        return probe(args)
    if not args.pcm:
        raise SystemExit("local-asr: --pcm is required unless --probe is set")
    if not os.path.exists(args.pcm):
        raise SystemExit(f"local-asr: PCM file not found: {args.pcm}")
    size = os.path.getsize(args.pcm)
    duration_ms = size // BYTES_PER_MS
    if duration_ms <= 0:
        raise SystemExit("local-asr: empty PCM input")
    windows = plan_windows(duration_ms, args.window_ms, args.overlap_ms)
    if args.max_windows > 0:
        windows = windows[: args.max_windows]
    try:
        skip = parse_skip(args.skip, len(windows))
    except ValueError as exc:
        raise SystemExit(f"local-asr: {exc}")
    if args.dry_run:
        emit(args, [(i, start, end, "") for i, (start, end) in enumerate(windows, 1)], "")
        return 0
    import numpy as np

    samples = np.memmap(args.pcm, dtype="<i2", mode="r")
    log(f"{duration_ms}ms audio, {len(windows)} window(s), backend={args.backend}")
    try:
        handle = load_model_with_timeout(args)
    except BaseException as exc:  # noqa: BLE001 - reported at the boundary
        print(f"local-asr: model load failed: {exc}", file=sys.stderr, flush=True)
        return EXIT_MODEL_LOAD
    log(f"model loaded from {args.model}@{args.revision or 'main'}")
    results: list[tuple[int, int, int, str]] = []
    language = args.language or ""
    tmp_parent = os.path.dirname(os.path.abspath(args.pcm))
    with tempfile.TemporaryDirectory(prefix=".local-asr-", dir=tmp_parent) as tmp:
        for index, (start_ms, end_ms) in enumerate(windows, 1):
            if index in skip:
                log(f"window {index}/{len(windows)} skipped (already committed)")
                continue
            path = os.path.join(tmp, f"window-{index:04d}.wav")
            write_wav(path, samples[start_ms * 16:end_ms * 16])
            log(f"window {index}/{len(windows)}")
            try:
                text, detected = with_window_timeout(
                    args.window_timeout, transcribe_window, args, handle, path
                )
            except WindowTimeout:
                print(f"local-asr: window timeout after {args.window_timeout}s", file=sys.stderr, flush=True)
                return EXIT_WINDOW_TIMEOUT
            except Exception as exc:  # noqa: BLE001 - one bad window fails the run
                detail = str(exc)
                if "out of memory" in detail.lower() or "cudaerrormemoryallocation" in detail.lower():
                    print(f"CUDA out of memory: {detail}", file=sys.stderr, flush=True)
                raise
            finally:
                if os.path.exists(path):
                    os.unlink(path)
            if not language and detected:
                language = detected
            results.append((index, start_ms, end_ms, text))
            if args.stream:
                line = {
                    "index": index,
                    "start_ms": start_ms,
                    "end_ms": end_ms,
                    "text": text,
                    "language": language,
                }
                sys.stdout.write(json.dumps(line, ensure_ascii=False) + "\n")
                sys.stdout.flush()
    if args.stream:
        return 0
    emit(args, results, language)
    return 0


def emit(args: argparse.Namespace, results: list[tuple[int, int, int, str]], language: str) -> None:
    payload = {
        "backend": args.backend,
        "model": args.model,
        "language": language,
        "windows": [
            {"index": index, "start_ms": start, "end_ms": end, "text": text}
            for index, start, end, text in results
        ],
    }
    json.dump(payload, sys.stdout, ensure_ascii=False)
    sys.stdout.write("\n")
    sys.stdout.flush()


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        raise SystemExit(130)
    except SystemExit:
        raise
    except Exception as exc:  # noqa: BLE001 - runner boundary
        import traceback

        traceback.print_exc()
        raise SystemExit(f"local-asr: {exc}")
