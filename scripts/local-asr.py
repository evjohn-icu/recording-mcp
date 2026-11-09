#!/usr/bin/env python3
"""Offline ASR runner for the recording-mcp worker.

Reads a 16 kHz mono S16LE PCM file, splits it into overlapping windows, runs a
local model in float16, and prints one JSON document:

    {"backend": "...", "model": "...", "language": "...",
     "windows": [{"index": 1, "start_ms": 0, "end_ms": 30000, "text": "..."}]}

Progress goes to stderr; stdout stays machine-readable. Model weights are never
bundled here: pass a local directory or a Hugging Face id.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
import tempfile
import wave

SAMPLE_RATE = 16000
BYTES_PER_MS = 32  # S16LE mono 16 kHz


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


def write_wav(path: str, samples) -> None:
    with wave.open(path, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(SAMPLE_RATE)
        w.writeframes(samples.tobytes())


def clean_text(text: str) -> str:
    text = re.sub(r"<\|[^|]*\|>", " ", text or "")
    return re.sub(r"\s+", " ", text).strip()


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


def run_ark(args, wavs: list[str]) -> tuple[list[str], str]:
    import torch

    processor, tokenizer, model, bad_words_ids = load_ark(args.model, args.device, args.revision)
    texts: list[str] = []
    language = args.language or ""
    for i, wav_path in enumerate(wavs, 1):
        log(f"window {i}/{len(wavs)}")
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
        texts.append(clean_text(text))
        del inputs, outputs
        torch.cuda.empty_cache()
    return texts, language


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


def run_qwen(args, wavs: list[str]) -> tuple[list[str], str]:
    import torch

    model = load_qwen(args.model, args.device, args.max_new_tokens, args.revision)
    texts: list[str] = []
    language = args.language or ""
    batch = 1
    for start in range(0, len(wavs), batch):
        chunk = wavs[start:start + batch]
        log(f"window {start + 1}-{start + len(chunk)}/{len(wavs)}")
        langs = [args.language] * len(chunk) if args.language else None
        results = model.transcribe(audio=chunk, language=langs)
        for result in results:
            texts.append(clean_text(getattr(result, "text", "")))
            if not language:
                language = getattr(result, "language", "") or ""
        torch.cuda.empty_cache()
    return texts, language


def log(message: str) -> None:
    print(f"local-asr: {message}", file=sys.stderr, flush=True)


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="offline ASR runner (float16)")
    p.add_argument("--backend", choices=["ark", "qwen"], required=True)
    p.add_argument("--model", default="", help="local model dir or Hugging Face id")
    p.add_argument("--revision", default="", help="Hugging Face revision (commit); empty uses the default branch")
    p.add_argument("--pcm", required=True, help="16 kHz mono S16LE PCM input")
    p.add_argument("--dtype", default="float16", help="only float16 is supported")
    p.add_argument("--device", default="cuda:0")
    p.add_argument("--language", default="", help="optional language hint, e.g. Chinese")
    p.add_argument("--window-ms", type=int, default=30000)
    p.add_argument("--overlap-ms", type=int, default=2000)
    p.add_argument("--max-new-tokens", type=int, default=512)
    p.add_argument("--max-windows", type=int, default=0, help="debug: limit processed windows")
    p.add_argument("--dry-run", action="store_true", help="emit the window plan without loading a model")
    return p.parse_args()


def main() -> int:
    args = parse_args()
    if args.dtype != "float16":
        raise SystemExit("local-asr: only --dtype float16 is supported")
    if args.backend == "ark" and not args.model:
        args.model = "Edge0/ARK-ASR-0.6B"
    if args.backend == "qwen" and not args.model:
        args.model = "Qwen/Qwen3-ASR-1.7B"
    if not os.path.exists(args.pcm):
        raise SystemExit(f"local-asr: PCM file not found: {args.pcm}")
    size = os.path.getsize(args.pcm)
    duration_ms = size // BYTES_PER_MS
    if duration_ms <= 0:
        raise SystemExit("local-asr: empty PCM input")
    windows = plan_windows(duration_ms, args.window_ms, args.overlap_ms)
    if args.max_windows > 0:
        windows = windows[: args.max_windows]
    if args.dry_run:
        emit(args, windows, [""] * len(windows), "")
        return 0
    import numpy as np

    samples = np.memmap(args.pcm, dtype="<i2", mode="r")
    texts: list[str] = []
    language = ""
    tmp_parent = os.path.dirname(os.path.abspath(args.pcm))
    with tempfile.TemporaryDirectory(prefix=".local-asr-", dir=tmp_parent) as tmp:
        wavs = []
        for i, (start_ms, end_ms) in enumerate(windows):
            path = os.path.join(tmp, f"window-{i + 1:04d}.wav")
            write_wav(path, samples[start_ms * 16:end_ms * 16])
            wavs.append(path)
        log(f"{duration_ms}ms audio, {len(wavs)} window(s), backend={args.backend}")
        if args.backend == "ark":
            texts, language = run_ark(args, wavs)
        else:
            texts, language = run_qwen(args, wavs)
    emit(args, windows, texts, language)
    return 0


def emit(args: argparse.Namespace, windows: list[tuple[int, int]], texts: list[str], language: str) -> None:
    payload = {
        "backend": args.backend,
        "model": args.model,
        "language": language,
        "windows": [
            {"index": i + 1, "start_ms": start, "end_ms": end, "text": text}
            for i, ((start, end), text) in enumerate(zip(windows, texts))
        ],
    }
    json.dump(payload, sys.stdout, ensure_ascii=False)
    sys.stdout.write("\n")


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        raise SystemExit(130)
    except Exception as exc:  # noqa: BLE001 - runner boundary
        import traceback

        traceback.print_exc()
        raise SystemExit(f"local-asr: {exc}")
