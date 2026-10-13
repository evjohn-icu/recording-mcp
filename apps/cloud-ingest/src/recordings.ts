// 录音创建 / 详情 / 删除 / finish（契约 §4.3、§4.5、§4.6、§4.11）。
// 幂等矩阵（契约 §5）：同 ID 同数据重放 → 成功且不产生重复记录/事件；同 ID 异数据 → conflict_payload。

import type { Env } from "./index";
import { ApiError, intField, isRfc3339, isUuidV4, json, requireProtocolVersion } from "./errors";
import { authenticate, requireRole } from "./auth";
import { MAX_JSON_BODY_BYTES, RATE_LIMITS, rateLimit, readJsonWithLimit } from "./limits";

export interface RecordingRow {
  owner: string;
  recording_id: string;
  device_id: string;
  codec: string;
  sample_rate: number;
  channels: number;
  recorded_at: string | null;
  timezone: string | null;
  state: "recording" | "complete" | "deleted";
  total_chunks: number | null;
  total_samples: number | null;
  total_duration_ms: number | null;
  synced_at: string | null;
  transcription_state: "none" | "pending" | "ingested" | "failed" | "skipped";
  created_at: string;
  finished_at: string | null;
  deleted_at: string | null;
}

export interface ChunkRow {
  seq: number;
  bytes: number;
  sample_offset: number;
  sample_count: number;
  sha256: string;
  uploaded_at: string;
}

export async function loadRecording(env: Env, owner: string, recordingId: string): Promise<RecordingRow | null> {
  return env.DB.prepare("SELECT * FROM recordings WHERE owner = ? AND recording_id = ?").bind(owner, recordingId).first<RecordingRow>();
}

export async function loadChunks(env: Env, owner: string, recordingId: string): Promise<ChunkRow[]> {
  const result = await env.DB
    .prepare("SELECT seq, bytes, sample_offset, sample_count, sha256, uploaded_at FROM chunks WHERE owner = ? AND recording_id = ? ORDER BY seq")
    .bind(owner, recordingId)
    .all<ChunkRow>();
  return result.results;
}

// openapi RecordingDetail 形状（端点 6 / 端点 9 复用）。
export function recordingDetail(rec: RecordingRow, chunks: ChunkRow[]) {
  return {
    recording_id: rec.recording_id,
    state: rec.state,
    codec: rec.codec,
    sample_rate: rec.sample_rate,
    channels: rec.channels,
    recorded_at: rec.recorded_at,
    created_at: rec.created_at,
    finished_at: rec.finished_at,
    total_chunks: rec.total_chunks,
    total_samples: rec.total_samples,
    total_duration_ms: rec.total_duration_ms,
    chunks: chunks.map((chunk) => ({
      seq: chunk.seq,
      bytes: chunk.bytes,
      sample_offset: chunk.sample_offset,
      sample_count: chunk.sample_count,
      content_sha256: chunk.sha256,
      uploaded_at: chunk.uploaded_at,
    })),
    sync: { synced_at: rec.synced_at, transcription_state: rec.transcription_state },
  };
}

// 事件表写入（4 处调用共用：created / committed / finished / deleted）。
export function eventStatement(
  env: Env,
  owner: string,
  type: string,
  recordingId: string,
  seq: number,
  payload: Record<string, unknown>,
  createdAt: string,
): D1PreparedStatement {
  return env.DB
    .prepare("INSERT INTO events(owner, type, recording_id, seq, payload_json, created_at) VALUES(?,?,?,?,?,?)")
    .bind(owner, type, recordingId, seq, JSON.stringify(payload), createdAt);
}

export async function handleCreateRecording(request: Request, env: Env): Promise<Response> {
  const principal = await authenticate(request, env);
  requireRole(principal, "device");
  await rateLimit(env, `json:${principal.tokenHash}`, RATE_LIMITS.jsonPerMinute);

  const body = await readJsonWithLimit(request, MAX_JSON_BODY_BYTES);
  requireProtocolVersion(body);
  if ("owner" in body) throw new ApiError("validation_failed", "owner is assigned by the server and must not be sent");

  const recordingId = body.recording_id;
  if (!isUuidV4(recordingId)) throw new ApiError("validation_failed", "recording_id must be a UUIDv4 string");
  if (body.codec !== "pcm_s16le") throw new ApiError("validation_failed", 'codec must be "pcm_s16le"');
  if (body.sample_rate !== 16000) throw new ApiError("validation_failed", "sample_rate must be 16000");
  if (body.channels !== 1) throw new ApiError("validation_failed", "channels must be 1");

  let recordedAt: string | null = null;
  if (body.recorded_at !== undefined && body.recorded_at !== null) {
    if (!isRfc3339(body.recorded_at)) throw new ApiError("validation_failed", "recorded_at must be RFC3339 or null");
    recordedAt = body.recorded_at;
  }
  let timezone: string | null = null;
  if (body.timezone !== undefined && body.timezone !== null) {
    if (typeof body.timezone !== "string" || body.timezone.length === 0 || body.timezone.length > 64) {
      throw new ApiError("validation_failed", "timezone must be a non-empty string of at most 64 chars or null");
    }
    timezone = body.timezone;
  }

  const sameMetadata = (rec: RecordingRow): boolean =>
    rec.codec === "pcm_s16le" && rec.sample_rate === 16000 && rec.channels === 1 &&
    rec.recorded_at === recordedAt && rec.timezone === timezone;

  const existing = await loadRecording(env, principal.owner, recordingId);
  if (existing) {
    if (sameMetadata(existing)) {
      return json({ recording_id: recordingId, state: existing.state, created_at: existing.created_at, duplicate: true });
    }
    throw new ApiError("conflict_payload", "recording exists with different metadata");
  }

  const now = new Date().toISOString();
  try {
    await env.DB.batch([
      env.DB.prepare(
        "INSERT INTO recordings(owner, recording_id, device_id, codec, sample_rate, channels, recorded_at, timezone, state, created_at) " +
          "VALUES(?,?,?,?,?,?,?,?,'recording',?)",
      ).bind(principal.owner, recordingId, principal.deviceId, "pcm_s16le", 16000, 1, recordedAt, timezone, now),
      eventStatement(env, principal.owner, "recording.created", recordingId, -1,
        { codec: "pcm_s16le", sample_rate: 16000, channels: 1, recorded_at: recordedAt, timezone }, now),
    ]);
  } catch (err) {
    // 并发同 ID 创建：读回已存在行，等价重放 → 200 duplicate；不同元数据 → 冲突。
    const raced = await loadRecording(env, principal.owner, recordingId);
    if (raced) {
      if (sameMetadata(raced)) {
        return json({ recording_id: recordingId, state: raced.state, created_at: raced.created_at, duplicate: true });
      }
      throw new ApiError("conflict_payload", "recording exists with different metadata");
    }
    throw err;
  }
  return json({ recording_id: recordingId, state: "recording", created_at: now, duplicate: false }, 201);
}

export async function handleGetRecording(request: Request, env: Env, recordingId: string): Promise<Response> {
  const principal = await authenticate(request, env); // device 或 sync 均可（契约 §4.6）
  await rateLimit(env, `json:${principal.tokenHash}`, RATE_LIMITS.jsonPerMinute);

  const rec = await loadRecording(env, principal.owner, recordingId);
  if (!rec) throw new ApiError("recording_not_found", "recording does not exist for this owner");
  return json(recordingDetail(rec, await loadChunks(env, principal.owner, recordingId)));
}

export async function handleDeleteRecording(request: Request, env: Env, recordingId: string): Promise<Response> {
  const principal = await authenticate(request, env);
  requireRole(principal, "device");
  await rateLimit(env, `json:${principal.tokenHash}`, RATE_LIMITS.jsonPerMinute);

  const rec = await loadRecording(env, principal.owner, recordingId);
  if (!rec) throw new ApiError("recording_not_found", "recording does not exist for this owner");

  if (rec.state !== "deleted") {
    const now = new Date().toISOString();
    try {
      await env.DB.batch([
        env.DB.prepare("UPDATE recordings SET state = 'deleted', deleted_at = ? WHERE owner = ? AND recording_id = ? AND state != 'deleted'")
          .bind(now, principal.owner, recordingId),
        // 幂等：并发重复删除由 events_identity 唯一索引兜底（整批回滚后按已删除状态返回）。
        eventStatement(env, principal.owner, "recording.deleted", recordingId, -1, { reason: "client_delete" }, now),
      ]);
    } catch {
      // 已被并发删除：视为幂等成功。
    }
  }

  // 尽力删除 R2 分块对象（契约 §4.11；失败不影响幂等语义）。
  const keys = await env.DB.prepare("SELECT r2_key FROM chunks WHERE owner = ? AND recording_id = ?")
    .bind(principal.owner, recordingId)
    .all<{ r2_key: string }>();
  for (let i = 0; i < keys.results.length; i += 1000) {
    try {
      await env.AUDIO.delete(keys.results.slice(i, i + 1000).map((row) => row.r2_key));
    } catch {
      // best-effort
    }
  }

  const updated = await loadRecording(env, principal.owner, recordingId);
  if (!updated) throw new ApiError("internal", "recording row vanished during delete");
  return json(recordingDetail(updated, await loadChunks(env, principal.owner, recordingId)));
}

export async function handleFinish(request: Request, env: Env, recordingId: string): Promise<Response> {
  const principal = await authenticate(request, env);
  requireRole(principal, "device");
  await rateLimit(env, `json:${principal.tokenHash}`, RATE_LIMITS.jsonPerMinute);

  const body = await readJsonWithLimit(request, MAX_JSON_BODY_BYTES);
  requireProtocolVersion(body);
  const totalChunks = intField(body, "total_chunks", 1, 720);
  const totalSamples = intField(body, "total_samples", 1, Number.MAX_SAFE_INTEGER);
  const totalDurationMs = intField(body, "total_duration_ms", 1, Number.MAX_SAFE_INTEGER);

  const rec = await loadRecording(env, principal.owner, recordingId);
  if (!rec || rec.state === "deleted") throw new ApiError("recording_not_found", "recording does not exist for this owner");

  // 同 body 重放 → 200 且不重复发事件（契约 §4.5）。
  if (rec.state === "complete") {
    if (rec.total_chunks === totalChunks && rec.total_samples === totalSamples && rec.total_duration_ms === totalDurationMs) {
      return json({ state: "complete", total_chunks: totalChunks, total_samples: totalSamples, durable: true });
    }
    throw new ApiError("conflict_payload", "recording already complete with different totals");
  }

  const chunks = await loadChunks(env, principal.owner, recordingId);

  // 缺口优先于总量校验（契约向量 finish_gap：部分上传 → sequence_gap）。
  // chunks 按 seq 升序返回；单趟扫描找出 [0, totalChunks) 内缺失的 seq。
  const missing: number[] = [];
  let expected = 0;
  for (const chunk of chunks) {
    while (expected < chunk.seq && expected < totalChunks) missing.push(expected++);
    if (expected < totalChunks && chunk.seq === expected) expected++;
  }
  while (expected < totalChunks) missing.push(expected++);
  if (missing.length > 0) {
    throw new ApiError("sequence_gap", "finish refused: missing chunk sequences", { missing_sequences: missing });
  }
  if (chunks.length !== totalChunks) {
    throw new ApiError("conflict_payload", "manifest chunk count does not match total_chunks", {
      manifest_chunks: chunks.length,
      total_chunks: totalChunks,
      extra_sequences: chunks.filter((chunk) => chunk.seq >= totalChunks).map((chunk) => chunk.seq),
    });
  }
  const sumSamples = chunks.reduce((acc, chunk) => acc + chunk.sample_count, 0);
  if (sumSamples !== totalSamples) {
    throw new ApiError("conflict_payload", "sum of chunk sample_count does not match total_samples",
      { sum_samples: sumSamples, total_samples: totalSamples });
  }
  // offset 链：offset[0] == 0 且 offset[i] == offset[i-1] + count[i-1]；末块不参与链校验（契约 §4.5）。
  const badOffsets: number[] = [];
  if (chunks.length > 0 && chunks[0]!.sample_offset !== 0) badOffsets.push(chunks[0]!.seq);
  for (let i = 1; i < chunks.length - 1; i++) {
    const prev = chunks[i - 1]!;
    const cur = chunks[i]!;
    if (cur.sample_offset !== prev.sample_offset + prev.sample_count) badOffsets.push(cur.seq);
  }
  if (badOffsets.length > 0) {
    throw new ApiError("conflict_payload", "sample_offset chain is discontinuous", { bad_offset_sequences: badOffsets });
  }

  const now = new Date().toISOString();
  try {
    await env.DB.batch([
      env.DB.prepare(
        "UPDATE recordings SET state = 'complete', finished_at = ?, total_chunks = ?, total_samples = ?, total_duration_ms = ? " +
          "WHERE owner = ? AND recording_id = ? AND state = 'recording'",
      ).bind(now, totalChunks, totalSamples, totalDurationMs, principal.owner, recordingId),
      eventStatement(env, principal.owner, "recording.finished", recordingId, -1,
        { total_chunks: totalChunks, total_samples: totalSamples, total_duration_ms: totalDurationMs }, now),
    ]);
  } catch (err) {
    // 并发重复 finish：读回状态，同 totals → 幂等 200。
    const raced = await loadRecording(env, principal.owner, recordingId);
    if (raced && raced.state === "complete" && raced.total_chunks === totalChunks &&
        raced.total_samples === totalSamples && raced.total_duration_ms === totalDurationMs) {
      return json({ state: "complete", total_chunks: totalChunks, total_samples: totalSamples, durable: true });
    }
    throw err;
  }
  return json({ state: "complete", total_chunks: totalChunks, total_samples: totalSamples, durable: true });
}
