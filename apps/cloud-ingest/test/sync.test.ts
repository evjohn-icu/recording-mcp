// 同步端点：游标分页 / max_event_id / 410 cursor_expired / 快照恢复 / ack / 音频下载（契约 §4.7–§4.10）。
// 每个用例使用独立 owner（isolatedStorage 只按文件隔离，owner 隔离避免跨用例串扰）；
// 事件 id 为库级自增，断言一律基于 owner 内的相对顺序。
import { describe, expect, it } from "vitest";
import { SELF, env } from "cloudflare:test";
import pcm440Vector from "../../../contracts/ingest-v1/vectors/pcm_1s_440hz.json";
import {
  BASE,
  VERSION_HEADERS,
  createRecording,
  decodeBase64,
  errorCodeOf,
  errorOf,
  finishRecording,
  getRecording,
  isJsonObject,
  jsonObjectOf,
  mintSyncToken,
  pairDevice,
  putChunk,
} from "./helpers";

const PCM = decodeBase64(pcm440Vector.pcm_base64);
const SHA = pcm440Vector.content_sha256;

interface SyncEventShape {
  id: number;
  type: string;
  recording_id: string;
  seq: number;
  payload: Record<string, unknown>;
  created_at: string;
}

interface SyncPage {
  events: SyncEventShape[];
  next_cursor: number;
  has_more: boolean;
  max_event_id: number;
  server_protocol_version: string;
}

// 运行时校验响应形状后再按 SyncPage 读取。
function isSyncPage(value: unknown): value is SyncPage {
  if (!isJsonObject(value)) return false;
  if (!Array.isArray(value.events) || typeof value.next_cursor !== "number" ||
      typeof value.has_more !== "boolean" || typeof value.max_event_id !== "number" ||
      value.server_protocol_version !== "ingest-v1") {
    return false;
  }
  return value.events.every((event) =>
    isJsonObject(event) && typeof event.id === "number" && typeof event.type === "string" &&
    typeof event.recording_id === "string" && typeof event.seq === "number" &&
    isJsonObject(event.payload) && typeof event.created_at === "string");
}

async function fetchSyncPage(syncToken: string, query: string): Promise<SyncPage> {
  const res = await SELF.fetch(`${BASE}/v1/sync${query}`, {
    headers: { ...VERSION_HEADERS, authorization: `Bearer ${syncToken}` },
  });
  expect(res.status).toBe(200);
  const body: unknown = await res.json();
  if (!isSyncPage(body)) throw new Error(`unexpected sync page shape: ${JSON.stringify(body)}`);
  return body;
}

// 建一条 2 分块 + finish 的录音（独立 owner）：owner 内事件共 4 条
// created, committed(seq0), committed(seq1), finished，按 id 升序。
async function setupRecording(): Promise<{ devToken: string; syncToken: string; recordingId: string; baseId: number; owner: string }> {
  const owner = `owner-${crypto.randomUUID()}`;
  const dev = await pairDevice(owner);
  const sync = await mintSyncToken(owner);
  const recordingId = crypto.randomUUID();
  expect((await createRecording(dev, recordingId)).status).toBe(201);
  expect((await putChunk(dev, recordingId, 0, PCM, SHA, { sampleOffset: 0 })).status).toBe(200);
  expect((await putChunk(dev, recordingId, 1, PCM, SHA, { sampleOffset: 16000 })).status).toBe(200);
  expect((await finishRecording(dev, recordingId, 2, 32000, 2000)).status).toBe(200);
  const full = await fetchSyncPage(sync.token, "?cursor=0&limit=500");
  expect(full.events).toHaveLength(4);
  const baseId = full.events[0]!.id;
  expect(full.events.map((e) => e.id)).toEqual([baseId, baseId + 1, baseId + 2, baseId + 3]);
  expect(full.max_event_id).toBe(baseId + 3);
  return { devToken: dev.token, syncToken: sync.token, recordingId, baseId, owner };
}

describe("GET /v1/sync", () => {
  it("pages events strictly by ascending id and reports max_event_id", async () => {
    const { syncToken, recordingId, baseId } = await setupRecording();

    const page1 = await fetchSyncPage(syncToken, "?cursor=0&limit=2");
    expect(page1.events.map((e) => e.id)).toEqual([baseId, baseId + 1]);
    expect(page1.events.map((e) => e.type)).toEqual(["recording.created", "chunk.committed"]);
    expect(page1.next_cursor).toBe(baseId + 1);
    expect(page1.has_more).toBe(true);
    expect(page1.max_event_id).toBe(baseId + 3);
    expect(page1.server_protocol_version).toBe("ingest-v1");

    const page2 = await fetchSyncPage(syncToken, `?cursor=${baseId + 1}&limit=2`);
    expect(page2.events.map((e) => e.id)).toEqual([baseId + 2, baseId + 3]);
    expect(page2.has_more).toBe(false);
    expect(page2.next_cursor).toBe(baseId + 3);
    expect(page2.events.map((e) => e.recording_id)).toEqual([recordingId, recordingId]);
  });

  it("reports seq -1 for non-chunk events and chunk seq on committed events", async () => {
    const { syncToken } = await setupRecording();
    const page = await fetchSyncPage(syncToken, "?cursor=0&limit=500");
    expect(page.events.map((e) => e.seq)).toEqual([-1, 0, 1, -1]);
  });

  it("returns frozen payload shapes for each event type", async () => {
    const { syncToken, recordingId, owner } = await setupRecording();
    const page = await fetchSyncPage(syncToken, "?cursor=0&limit=500");
    expect(page.events[0]!.payload).toEqual({ codec: "pcm_s16le", sample_rate: 16000, channels: 1, recorded_at: null, timezone: null });
    expect(page.events[1]!.payload).toEqual({
      seq: 0,
      sample_offset: 0,
      sample_count: 16000,
      bytes: 32000,
      duration_ms: 1000,
      content_sha256: SHA,
      r2_key: `owners/${owner}/recordings/${recordingId}/chunks/00000000.pcm`,
    });
    expect(page.events[3]!.payload).toEqual({ total_chunks: 2, total_samples: 32000, total_duration_ms: 2000 });
  });

  it("returns an empty page with next_cursor unchanged", async () => {
    const { syncToken, baseId } = await setupRecording();
    const page = await fetchSyncPage(syncToken, `?cursor=${baseId + 3}`);
    expect(page.events).toEqual([]);
    expect(page.next_cursor).toBe(baseId + 3);
    expect(page.has_more).toBe(false);
  });

  it("returns 410 cursor_expired with min_available_cursor after retention trim", async () => {
    const { syncToken, baseId } = await setupRecording();
    // 模拟保留期裁剪：删除该 owner 的前三条事件。
    await env.DB.prepare("DELETE FROM events WHERE id <= ?").bind(baseId + 2).run();

    const expired = await SELF.fetch(`${BASE}/v1/sync?cursor=${baseId + 1}`, {
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${syncToken}` },
    });
    expect(expired.status).toBe(410);
    const err = await errorOf(expired);
    expect(err.code).toBe("cursor_expired");
    expect(err.details.min_available_cursor).toBe(baseId + 3);

    // cursor == MIN(id)-1 恰好存活边界 → 正常返回。
    const boundary = await fetchSyncPage(syncToken, `?cursor=${baseId + 2}`);
    expect(boundary.events.map((e) => e.id)).toEqual([baseId + 3]);
  });

  it("validates cursor and limit query params", async () => {
    const owner = `owner-${crypto.randomUUID()}`;
    const sync = await mintSyncToken(owner);
    const badLimit = await SELF.fetch(`${BASE}/v1/sync?cursor=0&limit=0`, {
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${sync.token}` },
    });
    expect(badLimit.status).toBe(400);
    const bigLimit = await SELF.fetch(`${BASE}/v1/sync?cursor=0&limit=501`, {
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${sync.token}` },
    });
    expect(bigLimit.status).toBe(400);
    const negativeCursor = await SELF.fetch(`${BASE}/v1/sync?cursor=-1`, {
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${sync.token}` },
    });
    expect(negativeCursor.status).toBe(400);
  });

  it("reports zero state for an owner without events", async () => {
    const owner = `owner-${crypto.randomUUID()}`;
    const sync = await mintSyncToken(owner);
    const page = await fetchSyncPage(sync.token, "?cursor=0");
    expect(page.events).toEqual([]);
    expect(page.next_cursor).toBe(0);
    expect(page.has_more).toBe(false);
    expect(page.max_event_id).toBe(0);
  });
});

// 运行时校验录音详情条目，取 recording_id。
function recordingIdOf(entry: unknown): string {
  if (!isJsonObject(entry) || typeof entry.recording_id !== "string") {
    throw new Error(`unexpected recording entry: ${JSON.stringify(entry)}`);
  }
  return entry.recording_id;
}

describe("GET /v1/sync/recordings", () => {
  it("returns full recording snapshots ordered by created_at, recording_id", async () => {
    const { syncToken, recordingId } = await setupRecording();
    const page = await jsonObjectOf(await SELF.fetch(`${BASE}/v1/sync/recordings`, {
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${syncToken}` },
    }));
    expect(page.has_more).toBe(false);
    expect(page.next_after).toBe(null);
    if (!Array.isArray(page.recordings) || page.recordings.length !== 1) throw new Error("expected exactly one recording");
    const detail = page.recordings[0]!;
    expect(detail).toMatchObject({ recording_id: recordingId, state: "complete", codec: "pcm_s16le" });
    expect(detail).toHaveProperty("chunks");
    expect(detail).toHaveProperty("sync");
  });

  it("paginates via next_after across multiple recordings", async () => {
    const owner = `owner-${crypto.randomUUID()}`;
    const dev = await pairDevice(owner);
    const sync = await mintSyncToken(owner);
    const idA = crypto.randomUUID();
    const idB = crypto.randomUUID();
    await createRecording(dev, idA);
    await createRecording(dev, idB);

    const page1 = await jsonObjectOf(await SELF.fetch(`${BASE}/v1/sync/recordings?limit=1`, {
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${sync.token}` },
    }));
    expect(page1.has_more).toBe(true);
    const nextAfter = page1.next_after;
    if (typeof nextAfter !== "string") throw new Error("next_after missing");

    const page2 = await jsonObjectOf(await SELF.fetch(`${BASE}/v1/sync/recordings?after=${nextAfter}&limit=1`, {
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${sync.token}` },
    }));
    expect(page2.has_more).toBe(false);
    if (!Array.isArray(page2.recordings) || page2.recordings.length !== 1) throw new Error("expected exactly one recording");
    const seen = [nextAfter, recordingIdOf(page2.recordings[0])].sort();
    expect(seen).toEqual([idA, idB].sort());
  });

  it("404s when the after anchor does not exist for this owner", async () => {
    const sync = await mintSyncToken(`owner-${crypto.randomUUID()}`);
    const res = await SELF.fetch(`${BASE}/v1/sync/recordings?after=${crypto.randomUUID()}`, {
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${sync.token}` },
    });
    expect(res.status).toBe(404);
    expect(await errorCodeOf(res)).toBe("recording_not_found");
  });
});

describe("POST /v1/sync/recordings/{id}/ack", () => {
  it("records transcription state visible to the device via endpoint 6", async () => {
    const { devToken, syncToken, recordingId } = await setupRecording();

    const ack = await SELF.fetch(`${BASE}/v1/sync/recordings/${recordingId}/ack`, {
      method: "POST",
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${syncToken}`, "content-type": "application/json" },
      body: JSON.stringify({ protocol_version: "ingest-v1", last_event_id: 4, transcription_state: "ingested" }),
    });
    expect(ack.status).toBe(200);
    const ackBody = await jsonObjectOf(ack);
    expect(ackBody.transcription_state).toBe("ingested");
    expect(ackBody.synced_at).not.toBeNull();

    const detail = await jsonObjectOf(await getRecording(devToken, recordingId));
    const syncInfo = detail.sync;
    if (!isJsonObject(syncInfo)) throw new Error("missing sync info");
    expect(syncInfo.transcription_state).toBe("ingested");
    expect(syncInfo.synced_at).not.toBeNull();
  });

  it("rejects invalid transcription_state values", async () => {
    const { syncToken, recordingId } = await setupRecording();
    const res = await SELF.fetch(`${BASE}/v1/sync/recordings/${recordingId}/ack`, {
      method: "POST",
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${syncToken}`, "content-type": "application/json" },
      body: JSON.stringify({ protocol_version: "ingest-v1", last_event_id: 1, transcription_state: "none" }),
    });
    expect(res.status).toBe(400);
    expect(await errorCodeOf(res)).toBe("validation_failed");
  });

  it("404s for unknown recordings", async () => {
    const sync = await mintSyncToken(`owner-${crypto.randomUUID()}`);
    const res = await SELF.fetch(`${BASE}/v1/sync/recordings/${crypto.randomUUID()}/ack`, {
      method: "POST",
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${sync.token}`, "content-type": "application/json" },
      body: JSON.stringify({ protocol_version: "ingest-v1", last_event_id: 1, transcription_state: "pending" }),
    });
    expect(res.status).toBe(404);
    expect(await errorCodeOf(res)).toBe("recording_not_found");
  });
});

describe("GET /v1/recordings/{id}/chunks/{seq}/audio", () => {
  it("streams the committed PCM with audio/L16 and X-Content-SHA256", async () => {
    const { syncToken, recordingId } = await setupRecording();

    const res = await SELF.fetch(`${BASE}/v1/recordings/${recordingId}/chunks/0/audio`, {
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${syncToken}` },
    });
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toBe("audio/L16");
    expect(res.headers.get("x-content-sha256")).toBe(SHA);
    expect(new Uint8Array(await res.arrayBuffer())).toEqual(PCM);
  });

  it("404s for a missing seq", async () => {
    const { syncToken, recordingId } = await setupRecording();
    const res = await SELF.fetch(`${BASE}/v1/recordings/${recordingId}/chunks/99/audio`, {
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${syncToken}` },
    });
    expect(res.status).toBe(404);
    expect(await errorCodeOf(res)).toBe("recording_not_found");
  });

  it("404s after the recording is deleted (read-side tombstone)", async () => {
    const { devToken, syncToken, recordingId } = await setupRecording();
    const deleted = await SELF.fetch(`${BASE}/v1/recordings/${recordingId}`, {
      method: "DELETE",
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${devToken}` },
    });
    expect(deleted.status).toBe(200);

    const res = await SELF.fetch(`${BASE}/v1/recordings/${recordingId}/chunks/0/audio`, {
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${syncToken}` },
    });
    expect(res.status).toBe(404);
    expect(await errorCodeOf(res)).toBe("recording_not_found");
  });

  it("purges R2 chunk objects on delete", async () => {
    const { devToken, recordingId, owner } = await setupRecording();
    const key = `owners/${owner}/recordings/${recordingId}/chunks/00000000.pcm`;
    expect(await env.AUDIO.get(key)).not.toBeNull();
    await SELF.fetch(`${BASE}/v1/recordings/${recordingId}`, {
      method: "DELETE",
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${devToken}` },
    });
    expect(await env.AUDIO.get(key)).toBeNull();
  });
});
