#!/usr/bin/env python3
"""Private 90-second SAUC ASR validation. Reads the API key with hidden input."""
from __future__ import annotations

import asyncio
import getpass
import gzip
import json
import os
import struct
import sys
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path

import websockets

URL = "wss://openspeech.bytedance.com/api/v3/plan/sauc/bigmodel_async"
RESOURCE = "volc.seedasr.sauc.duration"
PCM_PATH = Path(__file__).resolve().parents[1] / "data/sample-validation/intro-90s-16k-mono.pcm"
OUT = Path(__file__).resolve().parents[1] / "data/sample-validation"
CHUNK_BYTES = 6400  # 200 ms at 16 kHz, mono, signed 16-bit PCM


def pack_header(message_type: int, flags: int) -> bytes:
    return bytes((0x11, (message_type << 4) | flags, 0x11, 0x00))


def full_request(seq: int, payload: dict) -> bytes:
    body = gzip.compress(json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode())
    return pack_header(0x1, 0x1) + struct.pack(">iI", seq, len(body)) + body


def audio_request(seq: int, pcm: bytes, final: bool = False) -> bytes:
    body = gzip.compress(pcm)
    signed_seq = -seq if final else seq
    return pack_header(0x2, 0x3 if final else 0x1) + struct.pack(">iI", signed_seq, len(body)) + body


def parse_response(frame: bytes) -> dict:
    if len(frame) < 4:
        raise ValueError("response frame shorter than header")
    header_size = (frame[0] & 0x0F) * 4
    msg_type, flags = frame[1] >> 4, frame[1] & 0x0F
    serialization, compression = frame[2] >> 4, frame[2] & 0x0F
    offset = header_size
    result = {"message_type": msg_type, "flags": flags, "sequence": None, "is_last_package": bool(flags & 0x02), "code": 0}
    if flags & 0x01:
        result["sequence"] = struct.unpack_from(">i", frame, offset)[0]
        offset += 4
    if msg_type == 0x9:
        result["payload_size"] = struct.unpack_from(">I", frame, offset)[0]
        offset += 4
    elif msg_type == 0xF:
        result["code"] = struct.unpack_from(">i", frame, offset)[0]
        offset += 4
        result["payload_size"] = struct.unpack_from(">I", frame, offset)[0]
        offset += 4
    body = frame[offset:]
    if compression == 1 and body:
        body = gzip.decompress(body)
    if serialization == 1 and body:
        result["payload"] = json.loads(body.decode("utf-8"))
    elif body:
        result["payload_base64"] = __import__("base64").b64encode(body).decode("ascii")
    return result


def extract_result(payload: dict | None) -> tuple[str, list[dict]]:
    if not isinstance(payload, dict):
        return "", []
    result = payload.get("result") or {}
    text = result.get("text") or ""
    utterances = result.get("utterances") or []
    return text, utterances if isinstance(utterances, list) else []


async def run() -> int:
    if not PCM_PATH.exists():
        raise FileNotFoundError(f"missing prepared audio: {PCM_PATH}")
    request_id, connect_id = uuid.uuid4().hex, uuid.uuid4().hex
    payload = {
        "user": {"uid": "recording-mcp-validation"},
        "audio": {"format": "pcm", "codec": "raw", "rate": 16000, "bits": 16, "channel": 1},
        "request": {
            "model_name": "bigmodel",
            "enable_nonstream": True,
            "enable_speaker_info": True,
            "show_utterances": True,
            "result_type": "full",
            "enable_itn": True,
            "enable_punc": True,
        },
    }
    # Hidden tty input avoids command arguments, environment dumps, and persisted credentials.
    api_key = getpass.getpass("Volcengine API key (hidden input): ")
    headers = {
        "X-Api-Key": api_key,
        "X-Api-Resource-Id": RESOURCE,
        "X-Api-Request-Id": request_id,
        "X-Api-Connect-Id": connect_id,
        "X-Api-Sequence": "-1",
    }
    del api_key
    OUT.mkdir(parents=True, exist_ok=True)
    os.chmod(OUT, 0o700)
    started = datetime.now(timezone.utc)
    meta = {
        "endpoint": URL,
        "resource_id": RESOURCE,
        "request_id": request_id,
        "connect_id": connect_id,
        "sample_file": str(PCM_PATH.name),
        "sample_duration_seconds": 90,
        "audio": payload["audio"],
        "request": payload["request"],
        "started_at": started.isoformat(),
        "result_frames": 0,
        "http_status": None,
        "server_log_id": None,
    }
    raw_path = OUT / "raw-responses.bin"
    jsonl_path = OUT / "parsed-responses.jsonl"
    try:
        async with websockets.connect(
            URL,
            additional_headers=headers,
            open_timeout=20,
            close_timeout=5,
            max_size=16 * 1024 * 1024,
            ping_interval=20,
        ) as ws:
            with raw_path.open("wb") as raw, jsonl_path.open("w", encoding="utf-8") as parsed:
                # Drop the reference to credentials as soon as the handshake is complete.
                headers.pop("X-Api-Key", None)
                meta["http_status"] = ws.response.status_code
                meta["server_log_id"] = ws.response.headers.get("X-Tt-Logid")
                await ws.send(full_request(1, payload))
                try:
                    ack = await asyncio.wait_for(ws.recv(), timeout=15)
                except asyncio.TimeoutError:
                    raise RuntimeError("WebSocket opened, but no protocol acknowledgement arrived within 15s")
                if not isinstance(ack, bytes):
                    raise RuntimeError("unexpected text frame before audio upload")
                ack_obj = parse_response(ack)
                raw.write(struct.pack(">I", len(ack)) + ack)
                parsed.write(json.dumps(ack_obj, ensure_ascii=False) + "\n")
                parsed.flush()
                if ack_obj["message_type"] == 0xF or ack_obj["code"]:
                    raise RuntimeError(f"service rejected initialization: code={ack_obj['code']} payload={ack_obj.get('payload')}")
                print(f"HANDSHAKE_OK status={meta['http_status']} log_id={meta['server_log_id'] or 'unavailable'}", flush=True)
                pcm_size = PCM_PATH.stat().st_size
                total_chunks = (pcm_size + CHUNK_BYTES - 1) // CHUNK_BYTES
                started_audio = time.monotonic()
                with PCM_PATH.open("rb") as pcm_file:
                    for chunk_index in range(total_chunks):
                        seq = chunk_index + 2  # full request consumed sequence 1
                        chunk = pcm_file.read(CHUNK_BYTES)
                        if not chunk:
                            break
                        last = chunk_index == total_chunks - 1
                        await ws.send(audio_request(seq, chunk, final=last))
                        if not last:
                            await asyncio.sleep(0.2)
                        # Drain any responses already available without stalling the upload.
                        while True:
                            try:
                                frame = await asyncio.wait_for(ws.recv(), timeout=0.005)
                            except asyncio.TimeoutError:
                                break
                            if not isinstance(frame, bytes):
                                continue
                            obj = parse_response(frame)
                            raw.write(struct.pack(">I", len(frame)) + frame)
                            parsed.write(json.dumps(obj, ensure_ascii=False) + "\n")
                            meta["result_frames"] += 1
                deadline = time.monotonic() + 20
                while time.monotonic() < deadline:
                    try:
                        frame = await asyncio.wait_for(ws.recv(), timeout=1)
                    except asyncio.TimeoutError:
                        continue
                    if not isinstance(frame, bytes):
                        continue
                    obj = parse_response(frame)
                    raw.write(struct.pack(">I", len(frame)) + frame)
                    parsed.write(json.dumps(obj, ensure_ascii=False) + "\n")
                    meta["result_frames"] += 1
                    if obj["message_type"] == 0xF or obj["code"]:
                        raise RuntimeError(f"service error: code={obj['code']} payload={obj.get('payload')}")
                    if obj["is_last_package"]:
                        break
                meta["audio_upload_seconds"] = round(time.monotonic() - started_audio, 2)
                raw.flush()
                parsed.flush()
    except Exception as exc:
        meta["error"] = f"{type(exc).__name__}: {exc}"
        meta["completed_at"] = datetime.now(timezone.utc).isoformat()
        (OUT / "run-metadata.json").write_text(json.dumps(meta, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        os.chmod(OUT / "run-metadata.json", 0o600)
        raise
    finally:
        headers.pop("X-Api-Key", None)

    responses = [json.loads(line) for line in jsonl_path.read_text(encoding="utf-8").splitlines() if line.strip()]
    texts, utterances = [], []
    final_text = ""
    for item in responses:
        text, utt = extract_result(item.get("payload"))
        if text:
            texts.append(text)
        if utt:
            utterances = utt
        if item.get("is_last_package") and text:
            final_text = text
    transcript = {
        "final_text": final_text or (texts[-1] if texts else ""),
        "utterances": utterances,
        "partial_text_updates": texts,
        "response_frame_count": len(responses),
    }
    transcript_path = OUT / "transcript.json"
    transcript_path.write_text(json.dumps(transcript, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    meta["completed_at"] = datetime.now(timezone.utc).isoformat()
    meta["final_package_received"] = bool(final_text)
    meta["utterance_count"] = len(utterances)
    meta["speaker_ids"] = sorted({str((u.get("additions") or {}).get("speaker_id", u.get("speaker_id"))) for u in utterances if (u.get("additions") or {}).get("speaker_id", u.get("speaker_id")) is not None})
    meta["speaker_id_field"] = "utterances[].additions.speaker_id"
    meta["definite_values"] = sorted({bool(u.get("definite")) for u in utterances if "definite" in u})
    meta["transcript_path"] = transcript_path.name
    (OUT / "run-metadata.json").write_text(json.dumps(meta, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    for path in (raw_path, jsonl_path, transcript_path, OUT / "run-metadata.json"):
        os.chmod(path, 0o600)
    print(f"ASR_DONE frames={len(responses)} utterances={len(utterances)} speakers={','.join(meta['speaker_ids']) or 'none'} final={meta['final_package_received']}", flush=True)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(asyncio.run(run()))
    except Exception as error:
        print(f"ASR_VALIDATION_FAILED {type(error).__name__}: {error}", file=sys.stderr, flush=True)
        raise SystemExit(1)
