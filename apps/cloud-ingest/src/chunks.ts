// 音频分块上传（契约 §4.4）：校验 → R2 put → D1 batch(manifest + event) → durable ACK。
// 200 仅在 R2 对象、manifest 行、事件行三者全部持久化后返回（契约 §5 durable ACK）。

import type { Env } from "./index";
import { ApiError, isRfc3339, isSha256Hex, json } from "./errors";
import { authenticate, base64UrlDecode, requireRole, sha256Hex } from "./auth";
import { MAX_CHUNK_BODY_BYTES, RATE_LIMITS, rateLimit, readBodyWithLimit } from "./limits";
import { eventStatement } from "./recordings";
import { FAULT_FAILING_SQL, debugFault } from "./debugfaults";

const MAX_SEQ = 9_999_999;

// R2 key 模板（契约冻结）：owners/{owner}/recordings/{recording_id}/chunks/{seq:08d}.pcm
export function r2Key(owner: string, recordingId: string, seq: number): string {
  return `owners/${owner}/recordings/${recordingId}/chunks/${String(seq).padStart(8, "0")}.pcm`;
}

function parseSeq(text: string): number {
  if (!/^\d{1,7}$/.test(text) || Number(text) > MAX_SEQ) {
    throw new ApiError("validation_failed", "seq must be an integer in [0, 9999999]");
  }
  return Number(text);
}

interface ChunkMeta {
  sample_offset: number;
  sample_count: number;
  duration_ms: number;
  content_sha256: string;
  recorded_at: string | null;
}

function parseChunkMeta(request: Request): ChunkMeta {
  const header = request.headers.get("x-chunk-meta");
  if (!header) throw new ApiError("validation_failed", "missing X-Chunk-Meta header");
  let parsed: unknown;
  try {
    parsed = JSON.parse(new TextDecoder().decode(base64UrlDecode(header)));
  } catch {
    throw new ApiError("validation_failed", "X-Chunk-Meta must be base64url(JSON)");
  }
  if (typeof parsed !== "object" || parsed === null) {
    throw new ApiError("validation_failed", "X-Chunk-Meta must decode to a JSON object");
  }
  const meta = parsed as Record<string, unknown>;
  if (typeof meta.sample_offset !== "number" || !Number.isInteger(meta.sample_offset) || meta.sample_offset < 0) {
    throw new ApiError("validation_failed", "sample_offset must be a non-negative integer");
  }
  if (typeof meta.sample_count !== "number" || !Number.isInteger(meta.sample_count) || meta.sample_count < 1) {
    throw new ApiError("validation_failed", "sample_count must be a positive integer");
  }
  if (typeof meta.duration_ms !== "number" || !Number.isInteger(meta.duration_ms) || meta.duration_ms < 1000 || meta.duration_ms > 90000) {
    throw new ApiError("validation_failed", "duration_ms must be an integer in [1000, 90000]");
  }
  if (!isSha256Hex(meta.content_sha256)) {
    throw new ApiError("validation_failed", "content_sha256 must be 64 lowercase hex chars");
  }
  if (meta.recorded_at !== undefined && meta.recorded_at !== null && !isRfc3339(meta.recorded_at)) {
    throw new ApiError("validation_failed", "recorded_at must be RFC3339 or null");
  }
  return {
    sample_offset: meta.sample_offset,
    sample_count: meta.sample_count,
    duration_ms: meta.duration_ms,
    content_sha256: meta.content_sha256,
    recorded_at: meta.recorded_at === undefined ? null : (meta.recorded_at as string | null),
  };
}

export async function handlePutChunk(request: Request, env: Env, recordingId: string, seqText: string): Promise<Response> {
  const principal = await authenticate(request, env);
  requireRole(principal, "device");
  await rateLimit(env, `chunk:${principal.deviceId}`, RATE_LIMITS.chunkPutPerMinute);

  const seq = parseSeq(seqText);
  const meta = parseChunkMeta(request);
  const body = await readBodyWithLimit(request, MAX_CHUNK_BODY_BYTES);

  if (body.byteLength !== meta.sample_count * 2) {
    throw new ApiError("validation_failed", `body is ${body.byteLength} bytes, expected ${meta.sample_count * 2}`);
  }
  const expectedMs = meta.sample_count / 16;
  if (Math.abs(meta.duration_ms - expectedMs) > 20) {
    throw new ApiError("validation_failed", `duration_ms ${meta.duration_ms} deviates more than 20ms from ${expectedMs}`);
  }
  const checksum = await sha256Hex(body);
  if (checksum !== meta.content_sha256) {
    throw new ApiError("validation_failed", "content_sha256 does not match body bytes");
  }

  const recording = await env.DB
    .prepare("SELECT state FROM recordings WHERE owner = ? AND recording_id = ?")
    .bind(principal.owner, recordingId)
    .first<{ state: string }>();
  if (!recording || recording.state === "deleted") {
    throw new ApiError("recording_not_found", "recording does not exist for this owner");
  }

  const existing = await env.DB
    .prepare("SELECT sha256 FROM chunks WHERE owner = ? AND recording_id = ? AND seq = ?")
    .bind(principal.owner, recordingId, seq)
    .first<{ sha256: string }>();
  if (existing) {
    if (existing.sha256 === checksum) {
      // 同 (recording, seq) 同 sha 重放：不发新事件（契约 §5）。
      return json({ seq, state: "committed", r2_key: r2Key(principal.owner, recordingId, seq), checksum, durable: true, duplicate: true });
    }
    throw new ApiError("conflict_checksum", "chunk already committed with different content");
  }
  if (recording.state === "complete") {
    throw new ApiError("validation_failed", "recording is already complete; new chunks are rejected");
  }

  const fault = debugFault(request, env);
  const key = r2Key(principal.owner, recordingId, seq);

  // 第 1 步：R2 写（同 key 覆盖写幂等；失败可安全重试）。
  await env.AUDIO.put(key, body);
  if (fault === "r2-after-write") throw new ApiError("internal", "injected fault: r2-after-write");

  // 第 2 步：D1 batch 原子写 manifest 行 + 事件行。
  const now = new Date().toISOString();
  const payload = {
    seq,
    sample_offset: meta.sample_offset,
    sample_count: meta.sample_count,
    bytes: body.byteLength,
    duration_ms: meta.duration_ms,
    content_sha256: checksum,
    r2_key: key,
  };
  const statements = [
    env.DB.prepare(
      "INSERT INTO chunks(owner, recording_id, seq, sample_offset, sample_count, bytes, duration_ms, sha256, r2_key, uploaded_at) VALUES(?,?,?,?,?,?,?,?,?,?)",
    ).bind(principal.owner, recordingId, seq, meta.sample_offset, meta.sample_count, body.byteLength, meta.duration_ms, checksum, key, now),
    eventStatement(env, principal.owner, "chunk.committed", recordingId, seq, payload, now),
  ];
  if (fault === "d1-manifest") statements[0] = env.DB.prepare(FAULT_FAILING_SQL);
  if (fault === "d1-event") statements[1] = env.DB.prepare(FAULT_FAILING_SQL);
  await env.DB.batch(statements);

  // 第 3 步：全部落定后才响应（response-drop 注入点在提交之后）。
  if (fault === "response-drop") throw new ApiError("internal", "injected fault: response-drop");
  return json({ seq, state: "committed", r2_key: key, checksum, durable: true });
}
