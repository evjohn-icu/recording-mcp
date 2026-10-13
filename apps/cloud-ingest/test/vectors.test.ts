// 契约向量漂移校验（contracts/ingest-v1/vectors）：
// 1) pcm_1s_*：base64 解码 → 字节数 / SHA-256 / 首尾 8 字节；
// 2) chunk_meta_ok：base64url 解码 → 与 pcm 向量一致的规范 JSON；
// 3) error_codes.json：与 src/errors.ts 冻结表全等；
// 4) 端到端重放 put_chunk_ok → duplicate → conflict → finish_ok / finish_gap / sync_page。
import { describe, expect, it } from "vitest";
import { SELF } from "cloudflare:test";
import { base64UrlDecode, sha256Hex } from "../src/auth";
import { ERROR_STATUS } from "../src/errors";
import chunkMetaOkVec from "../../../contracts/ingest-v1/vectors/chunk_meta_ok.json";
import cursorExpiredVec from "../../../contracts/ingest-v1/vectors/cursor_expired.json";
import errorCodesVec from "../../../contracts/ingest-v1/vectors/error_codes.json";
import finishGapVec from "../../../contracts/ingest-v1/vectors/finish_gap.json";
import finishOkVec from "../../../contracts/ingest-v1/vectors/finish_ok.json";
import pcm440Vec from "../../../contracts/ingest-v1/vectors/pcm_1s_440hz.json";
import pcmSilenceVec from "../../../contracts/ingest-v1/vectors/pcm_1s_silence.json";
import putChunkConflictVec from "../../../contracts/ingest-v1/vectors/put_chunk_conflict.json";
import putChunkDuplicateVec from "../../../contracts/ingest-v1/vectors/put_chunk_duplicate.json";
import putChunkOkVec from "../../../contracts/ingest-v1/vectors/put_chunk_ok.json";
import syncPageVec from "../../../contracts/ingest-v1/vectors/sync_page.json";
import {
  BASE,
  VERSION_HEADERS,
  createRecording,
  decodeBase64,
  errorCodeOf,
  eventCount,
  finishRecording,
  getRecording,
  isJsonObject,
  jsonObjectOf,
  mintSyncToken,
  pairDevice,
  putChunk,
  toHex,
  errorOf,
} from "./helpers";

const PCM440 = decodeBase64(pcm440Vec.pcm_base64);
const SILENCE = decodeBase64(pcmSilenceVec.pcm_base64);
const VECTOR_RECORDING = "7e5a9c3f-2b1d-4e6f-8a9b-0c1d2e3f4a5b"; // 契约向量固定 UUID

interface VectorRequest {
  path: string;
  method: string;
  headers: Record<string, string>;
}

async function vectorPut(vec: VectorRequest, body: Uint8Array, deviceToken: string): Promise<Response> {
  return SELF.fetch(`${BASE}${vec.path}`, {
    method: vec.method,
    headers: {
      "x-ingest-version": "ingest-v1",
      "content-type": "application/octet-stream",
      authorization: `Bearer ${deviceToken}`,
      "x-chunk-meta": vec.headers["X-Chunk-Meta"],
    },
    body,
  });
}

describe("pcm vectors", () => {
  it("pcm_1s_440hz decodes with matching byte length, sha256, and edge bytes", async () => {
    expect(PCM440.byteLength).toBe(pcm440Vec.byte_len);
    expect(await sha256Hex(PCM440)).toBe(pcm440Vec.content_sha256);
    expect(toHex(PCM440.slice(0, 8))).toBe(pcm440Vec.first_8_bytes_hex);
    expect(toHex(PCM440.slice(PCM440.byteLength - 8))).toBe(pcm440Vec.last_8_bytes_hex);
  });

  it("pcm_1s_silence decodes with matching byte length and sha256", async () => {
    expect(SILENCE.byteLength).toBe(pcmSilenceVec.byte_len);
    expect(await sha256Hex(SILENCE)).toBe(pcmSilenceVec.content_sha256);
  });
});

describe("chunk_meta_ok vector", () => {
  it("decodes to canonical JSON whose sha matches the pcm vector", () => {
    expect(chunkMetaOkVec.x_chunk_meta_base64url).toBe(chunkMetaOkVec.per_seq["0"]);
    const decoded: unknown = JSON.parse(new TextDecoder().decode(base64UrlDecode(chunkMetaOkVec.x_chunk_meta_base64url)));
    expect(decoded).toEqual({
      sample_offset: 0,
      sample_count: 16000,
      duration_ms: 1000,
      content_sha256: pcm440Vec.content_sha256,
      recorded_at: null,
    });
    // 规范紧凑 JSON 字节一致（key 顺序保持）
    if (!isJsonObject(decoded)) throw new Error("chunk meta did not decode to an object");
    expect(JSON.stringify(decoded)).toBe(chunkMetaOkVec.chunk_meta_json);
  });
});

describe("error_codes vector", () => {
  it("equals the frozen status map in errors.ts (full map equality)", () => {
    expect(errorCodesVec.codes).toEqual(ERROR_STATUS);
  });
});

describe("request vectors replayed against the worker", () => {
  it("replays put_chunk_ok → duplicate → conflict → finish_ok in one vector flow", async () => {
    const dev = await pairDevice("default");
    expect((await createRecording(dev, VECTOR_RECORDING)).status).toBe(201);

    const okRes = await vectorPut(putChunkOkVec, PCM440, dev.token);
    expect(okRes.status).toBe(putChunkOkVec.expected_status);
    expect(await jsonObjectOf(okRes)).toEqual(putChunkOkVec.expect_fields);

    const dupRes = await vectorPut(putChunkDuplicateVec, PCM440, dev.token);
    expect(dupRes.status).toBe(putChunkDuplicateVec.expected_status);
    expect(await jsonObjectOf(dupRes)).toMatchObject(putChunkDuplicateVec.expect_fields);
    expect(await eventCount(VECTOR_RECORDING, "chunk.committed")).toBe(1);

    const conflictRes = await vectorPut(putChunkConflictVec, SILENCE, dev.token);
    expect(conflictRes.status).toBe(putChunkConflictVec.expected_status);
    expect(await errorCodeOf(conflictRes)).toBe(putChunkConflictVec.expected_error);

    // finish_ok 前置：seq 1、2（offset 16000、32000），复用 1s 正弦向量
    expect((await putChunk(dev, VECTOR_RECORDING, 1, PCM440, pcm440Vec.content_sha256, { sampleOffset: 16000 })).status).toBe(200);
    expect((await putChunk(dev, VECTOR_RECORDING, 2, PCM440, pcm440Vec.content_sha256, { sampleOffset: 32000 })).status).toBe(200);

    const finishRes = await finishRecording(dev, VECTOR_RECORDING, 3, 48000, 3000);
    expect(finishRes.status).toBe(finishOkVec.expected_status);
    expect(await jsonObjectOf(finishRes)).toMatchObject(finishOkVec.expect_fields);
    expect(await eventCount(VECTOR_RECORDING, "recording.finished")).toBe(1);

    // sync_page 向量：事件类型序列与升序 id
    const sync = await mintSyncToken("default");
    const syncRes = await SELF.fetch(`${BASE}/v1/sync?cursor=0&limit=100`, {
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${sync.token}` },
    });
    expect(syncRes.status).toBe(syncPageVec.expected_status);
    const page = await jsonObjectOf(syncRes);
    for (const key of ["events", "next_cursor", "has_more", "max_event_id", "server_protocol_version"]) {
      expect(page).toHaveProperty(key);
    }
    const events = page.events;
    if (!Array.isArray(events)) throw new Error("sync page events missing");
    const types = events.map((event) => {
      if (!isJsonObject(event) || typeof event.type !== "string") throw new Error("malformed event entry");
      return event.type;
    });
    expect(types).toEqual(syncPageVec.expected_event_types_for_finish_ok_setup);
    const ids = events.map((event) => {
      if (!isJsonObject(event) || typeof event.id !== "number") throw new Error("malformed event entry");
      return event.id;
    });
    for (let i = 1; i < ids.length; i++) {
      expect(ids[i]!).toBeGreaterThan(ids[i - 1]!);
    }
  });

  it("replays the finish_gap vector and keeps state recording", async () => {
    const dev = await pairDevice("default");
    const recordingId = crypto.randomUUID();
    expect((await createRecording(dev, recordingId)).status).toBe(201);
    expect((await putChunk(dev, recordingId, 0, PCM440, pcm440Vec.content_sha256, { sampleOffset: 0 })).status).toBe(200);
    expect((await putChunk(dev, recordingId, 2, PCM440, pcm440Vec.content_sha256, { sampleOffset: 32000 })).status).toBe(200);

    const res = await finishRecording(dev, recordingId, 3, 48000, 3000);
    expect(res.status).toBe(finishGapVec.expected_status);
    const err = await errorOf(res);
    expect(err.code).toBe(finishGapVec.expected_error);
    expect(err.details.missing_sequences).toEqual(finishGapVec.expected_details.missing_sequences);

    const detail = await jsonObjectOf(await getRecording(dev.token, recordingId));
    expect(detail.state).toBe("recording");
  });

  it("pins the frozen status/error pairs from the remaining vectors", () => {
    expect(cursorExpiredVec.expected_status).toBe(410);
    expect(cursorExpiredVec.expected_error).toBe("cursor_expired");
    expect(putChunkConflictVec.expected_status).toBe(409);
    expect(putChunkConflictVec.expected_error).toBe("conflict_checksum");
    expect(putChunkDuplicateVec.expected_status).toBe(200);
    expect(finishOkVec.expected_status).toBe(200);
    expect(syncPageVec.expected_status).toBe(200);
    expect(syncPageVec.request.path).toBe("/v1/sync?cursor=0&limit=100");
  });
});

describe("healthz", () => {
  it("responds with the contract liveness shape without auth", async () => {
    const res = await SELF.fetch(`${BASE}/healthz`);
    expect(res.status).toBe(200);
    const data = await jsonObjectOf(res);
    expect(data.status).toBe("ok");
    expect(data.version).toBe("ingest-v1");
    expect(typeof data.now).toBe("string");
  });
});
