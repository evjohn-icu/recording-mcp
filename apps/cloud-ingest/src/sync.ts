// 事件同步、快照恢复、ack 与音频下载（契约 §4.7–§4.10）。仅 sync 凭据可用。

import type { Env } from "./index";
import { ApiError, PROTOCOL_VERSION, intField, isUuidV4, json, requireProtocolVersion } from "./errors";
import { authenticate, requireRole } from "./auth";
import { MAX_JSON_BODY_BYTES, RATE_LIMITS, rateLimit, readJsonWithLimit } from "./limits";
import { loadRecording, loadChunks, recordingDetail, type RecordingRow } from "./recordings";

interface EventRow {
  id: number;
  type: string;
  recording_id: string;
  seq: number;
  payload_json: string;
  created_at: string;
}

const ACK_STATES = ["pending", "ingested", "failed", "skipped"] as const;

function queryInt(url: URL, name: string, fallback: number, min: number, max: number): number {
  const raw = url.searchParams.get(name);
  if (raw === null) return fallback;
  if (!/^-?\d+$/.test(raw)) throw new ApiError("validation_failed", `${name} must be an integer`);
  const value = Number(raw);
  if (!Number.isInteger(value) || value < min || value > max) {
    throw new ApiError("validation_failed", `${name} must be an integer in [${min}, ${max}]`);
  }
  return value;
}

export async function handleSyncEvents(request: Request, env: Env): Promise<Response> {
  const principal = await authenticate(request, env);
  requireRole(principal, "sync");
  await rateLimit(env, `json:${principal.tokenHash}`, RATE_LIMITS.jsonPerMinute);

  const url = new URL(request.url);
  const cursor = queryInt(url, "cursor", 0, 0, Number.MAX_SAFE_INTEGER);
  const limit = queryInt(url, "limit", 200, 1, 500);

  // 游标超出事件保留窗口 → 410（契约 §4.7：cursor > 0 且 cursor < MIN(id)-1）。
  const minRow = await env.DB.prepare("SELECT MIN(id) AS min_id FROM events WHERE owner = ?")
    .bind(principal.owner)
    .first<{ min_id: number | null }>();
  const minId = minRow?.min_id ?? null;
  if (cursor > 0 && minId !== null && cursor < minId - 1) {
    throw new ApiError("cursor_expired", "cursor is outside the event retention window", { min_available_cursor: minId });
  }
  const maxRow = await env.DB.prepare("SELECT MAX(id) AS max_id FROM events WHERE owner = ?")
    .bind(principal.owner)
    .first<{ max_id: number | null }>();
  const maxId = maxRow?.max_id ?? 0;

  // 严格 id > cursor 升序；多取一行判定 has_more。
  const page = await env.DB
    .prepare("SELECT id, type, recording_id, seq, payload_json, created_at FROM events WHERE owner = ? AND id > ? ORDER BY id LIMIT ?")
    .bind(principal.owner, cursor, limit + 1)
    .all<EventRow>();
  const hasMore = page.results.length > limit;
  const rows = hasMore ? page.results.slice(0, limit) : page.results;
  const events = rows.map((row) => ({
    id: row.id,
    type: row.type,
    recording_id: row.recording_id,
    seq: row.seq,
    payload: JSON.parse(row.payload_json) as Record<string, unknown>,
    created_at: row.created_at,
  }));
  const nextCursor = rows.length > 0 ? rows[rows.length - 1]!.id : cursor;
  return json({
    events,
    next_cursor: nextCursor,
    has_more: hasMore,
    max_event_id: maxId,
    server_protocol_version: PROTOCOL_VERSION,
  });
}

// 全量快照分页（游标过期后的恢复通道，契约 §4.9）：按 created_at, recording_id 升序。
export async function handleListRecordings(request: Request, env: Env): Promise<Response> {
  const principal = await authenticate(request, env);
  requireRole(principal, "sync");
  await rateLimit(env, `json:${principal.tokenHash}`, RATE_LIMITS.jsonPerMinute);

  const url = new URL(request.url);
  const limit = queryInt(url, "limit", 100, 1, 200);
  const after = url.searchParams.get("after");

  let rows: RecordingRow[];
  if (after !== null) {
    if (!isUuidV4(after)) throw new ApiError("validation_failed", "after must be a recording id (uuid)");
    const anchor = await env.DB
      .prepare("SELECT created_at, recording_id FROM recordings WHERE owner = ? AND recording_id = ?")
      .bind(principal.owner, after)
      .first<{ created_at: string; recording_id: string }>();
    if (!anchor) throw new ApiError("recording_not_found", "after recording does not exist for this owner");
    const page = await env.DB
      .prepare(
        "SELECT * FROM recordings WHERE owner = ? AND (created_at > ? OR (created_at = ? AND recording_id > ?)) " +
          "ORDER BY created_at, recording_id LIMIT ?",
      )
      .bind(principal.owner, anchor.created_at, anchor.created_at, anchor.recording_id, limit + 1)
      .all<RecordingRow>();
    rows = page.results;
  } else {
    const page = await env.DB
      .prepare("SELECT * FROM recordings WHERE owner = ? ORDER BY created_at, recording_id LIMIT ?")
      .bind(principal.owner, limit + 1)
      .all<RecordingRow>();
    rows = page.results;
  }

  const hasMore = rows.length > limit;
  if (hasMore) rows = rows.slice(0, limit);
  const recordings = [];
  for (const rec of rows) {
    recordings.push(recordingDetail(rec, await loadChunks(env, principal.owner, rec.recording_id)));
  }
  const nextAfter = hasMore && rows.length > 0 ? rows[rows.length - 1]!.recording_id : null;
  return json({ recordings, next_after: nextAfter, has_more: hasMore });
}

export async function handleAck(request: Request, env: Env, recordingId: string): Promise<Response> {
  const principal = await authenticate(request, env);
  requireRole(principal, "sync");
  await rateLimit(env, `json:${principal.tokenHash}`, RATE_LIMITS.jsonPerMinute);

  const body = await readJsonWithLimit(request, MAX_JSON_BODY_BYTES);
  requireProtocolVersion(body);
  intField(body, "last_event_id", 0, Number.MAX_SAFE_INTEGER);
  const state = body.transcription_state;
  if (typeof state !== "string" || !(ACK_STATES as readonly string[]).includes(state)) {
    throw new ApiError("validation_failed", `transcription_state must be one of ${ACK_STATES.join("|")}`);
  }
  if (body.detail !== undefined && body.detail !== null) {
    if (typeof body.detail !== "string" || body.detail.length > 512) {
      throw new ApiError("validation_failed", "detail must be a string of at most 512 chars");
    }
  }

  const rec = await loadRecording(env, principal.owner, recordingId);
  if (!rec) throw new ApiError("recording_not_found", "recording does not exist for this owner");

  const now = new Date().toISOString();
  await env.DB
    .prepare("UPDATE recordings SET synced_at = ?, transcription_state = ? WHERE owner = ? AND recording_id = ?")
    .bind(now, state, principal.owner, recordingId)
    .run();
  return json({ synced_at: now, transcription_state: state });
}

export async function handleChunkAudio(request: Request, env: Env, recordingId: string, seqText: string): Promise<Response> {
  const principal = await authenticate(request, env);
  requireRole(principal, "sync");
  await rateLimit(env, `json:${principal.tokenHash}`, RATE_LIMITS.jsonPerMinute);

  const seq = Number(seqText);
  if (!Number.isInteger(seq) || seq < 0 || seq > 9_999_999) {
    throw new ApiError("validation_failed", "seq must be an integer in [0, 9999999]");
  }

  // 删除后读侧一律 404（契约 §5）；不区分存在性。
  const rec = await loadRecording(env, principal.owner, recordingId);
  if (!rec || rec.state === "deleted") throw new ApiError("recording_not_found", "recording does not exist for this owner");

  const chunk = await env.DB
    .prepare("SELECT sha256, r2_key FROM chunks WHERE owner = ? AND recording_id = ? AND seq = ?")
    .bind(principal.owner, recordingId, seq)
    .first<{ sha256: string; r2_key: string }>();
  if (!chunk) throw new ApiError("recording_not_found", "chunk does not exist");

  const object = await env.AUDIO.get(chunk.r2_key);
  if (!object) throw new ApiError("recording_not_found", "chunk audio missing from storage");
  return new Response(object.body, {
    status: 200,
    headers: {
      "Content-Type": "audio/L16",
      "X-Content-SHA256": chunk.sha256,
      "Content-Length": String(object.size),
    },
  });
}
