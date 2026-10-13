// 音频分块上传：正常 / 重复 / checksum 冲突 / 413 / 校验失败 / 四种 X-Debug-Fault 注入与重试（契约 §4.4、§5、§7）。
import { describe, expect, it } from "vitest";
import { SELF, env } from "cloudflare:test";
import pcm440Vector from "../../../contracts/ingest-v1/vectors/pcm_1s_440hz.json";
import pcmSilenceVector from "../../../contracts/ingest-v1/vectors/pcm_1s_silence.json";
import {
  BASE,
  VERSION_HEADERS,
  createRecording,
  decodeBase64,
  errorCodeOf,
  eventCount,
  jsonObjectOf,
  pairDevice,
  putChunk,
} from "./helpers";

const PCM = decodeBase64(pcm440Vector.pcm_base64);
const SILENCE = decodeBase64(pcmSilenceVector.pcm_base64);
const ZERO_SHA = "0".repeat(64);

describe("PUT /v1/recordings/{id}/chunks/{seq}", () => {
  it("commits a vector chunk with durable ACK and stores the R2 object", async () => {
    const dev = await pairDevice();
    const recordingId = crypto.randomUUID();
    expect((await createRecording(dev, recordingId)).status).toBe(201);

    const res = await putChunk(dev, recordingId, 0, PCM, pcm440Vector.content_sha256);
    expect(res.status).toBe(200);
    const data = await jsonObjectOf(res);
    expect(data).toEqual({
      seq: 0,
      state: "committed",
      r2_key: `owners/default/recordings/${recordingId}/chunks/00000000.pcm`,
      checksum: pcm440Vector.content_sha256,
      durable: true,
    });

    const r2Key = data.r2_key;
    if (typeof r2Key !== "string") throw new Error("r2_key missing in commit response");
    const stored = await env.AUDIO.get(r2Key);
    expect(stored).not.toBeNull();
    expect(new Uint8Array(await stored!.arrayBuffer())).toEqual(PCM);
    expect(await eventCount(recordingId, "chunk.committed")).toBe(1);
  });

  it("returns duplicate:true for a byte-identical replay without a new event", async () => {
    const dev = await pairDevice();
    const recordingId = crypto.randomUUID();
    await createRecording(dev, recordingId);
    expect((await putChunk(dev, recordingId, 0, PCM, pcm440Vector.content_sha256)).status).toBe(200);

    const replay = await putChunk(dev, recordingId, 0, PCM, pcm440Vector.content_sha256);
    expect(replay.status).toBe(200);
    const data = await jsonObjectOf(replay);
    expect(data.duplicate).toBe(true);
    expect(data.state).toBe("committed");
    expect(data.durable).toBe(true);
    expect(await eventCount(recordingId, "chunk.committed")).toBe(1);
  });

  it("returns conflict_checksum when the same seq carries different bytes", async () => {
    const dev = await pairDevice();
    const recordingId = crypto.randomUUID();
    await createRecording(dev, recordingId);
    expect((await putChunk(dev, recordingId, 0, PCM, pcm440Vector.content_sha256)).status).toBe(200);

    const res = await putChunk(dev, recordingId, 0, SILENCE, pcmSilenceVector.content_sha256);
    expect(res.status).toBe(409);
    expect(await errorCodeOf(res)).toBe("conflict_checksum");
  });

  it("returns 413 payload_too_large for bodies over 5 MiB", async () => {
    const dev = await pairDevice();
    const recordingId = crypto.randomUUID();
    await createRecording(dev, recordingId);
    const oversized = new Uint8Array(5 * 1024 * 1024 + 1);

    // 元数据本身合法（16000 采样 / 1000ms）；超限在体读取阶段命中 → 413。
    const res = await putChunk(dev, recordingId, 0, oversized, ZERO_SHA, { sampleCount: 16000, durationMs: 1000 });
    expect(res.status).toBe(413);
    expect(await errorCodeOf(res)).toBe("payload_too_large");
  });

  it("returns validation_failed when the body does not match content_sha256", async () => {
    const dev = await pairDevice();
    const recordingId = crypto.randomUUID();
    await createRecording(dev, recordingId);

    const res = await putChunk(dev, recordingId, 0, PCM, ZERO_SHA);
    expect(res.status).toBe(400);
    expect(await errorCodeOf(res)).toBe("validation_failed");
  });

  it("returns validation_failed when body length != sample_count*2", async () => {
    const dev = await pairDevice();
    const recordingId = crypto.randomUUID();
    await createRecording(dev, recordingId);

    // 元数据声明 99 采样（198 字节），实发 200 字节。
    const res = await putChunk(dev, recordingId, 0, new Uint8Array(200), ZERO_SHA, { sampleCount: 99, durationMs: 6 });
    expect(res.status).toBe(400);
    expect(await errorCodeOf(res)).toBe("validation_failed");
  });

  it("returns validation_failed when duration_ms deviates more than 20ms", async () => {
    const dev = await pairDevice();
    const recordingId = crypto.randomUUID();
    await createRecording(dev, recordingId);

    // 16000 采样 = 1000ms；声明 1050ms 偏差 50ms。
    const res = await putChunk(dev, recordingId, 0, PCM, pcm440Vector.content_sha256, { durationMs: 1050 });
    expect(res.status).toBe(400);
    expect(await errorCodeOf(res)).toBe("validation_failed");
  });

  it("returns 404 recording_not_found for an unknown recording", async () => {
    const dev = await pairDevice();
    const res = await putChunk(dev, crypto.randomUUID(), 0, PCM, pcm440Vector.content_sha256);
    expect(res.status).toBe(404);
    expect(await errorCodeOf(res)).toBe("recording_not_found");
  });

  it("refuses chunk uploads to a deleted recording with 404", async () => {
    const dev = await pairDevice();
    const recordingId = crypto.randomUUID();
    await createRecording(dev, recordingId);
    expect((await putChunk(dev, recordingId, 0, PCM, pcm440Vector.content_sha256)).status).toBe(200);
    const deleted = await SELF.fetch(`${BASE}/v1/recordings/${recordingId}`, {
      method: "DELETE",
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${dev.token}` },
    });
    expect(deleted.status).toBe(200);

    const replay = await putChunk(dev, recordingId, 0, PCM, pcm440Vector.content_sha256);
    expect(replay.status).toBe(404);
    expect(await errorCodeOf(replay)).toBe("recording_not_found");
    const fresh = await putChunk(dev, recordingId, 1, PCM, pcm440Vector.content_sha256);
    expect(fresh.status).toBe(404);
  });

  describe("X-Debug-Fault injections (ENABLE_DEBUG_FAULTS=1)", () => {
    it("r2-after-write: R2 object written, D1 untouched; retry commits", async () => {
      const dev = await pairDevice();
      const recordingId = crypto.randomUUID();
      await createRecording(dev, recordingId);
      const key = `owners/default/recordings/${recordingId}/chunks/00000000.pcm`;

      const first = await putChunk(dev, recordingId, 0, PCM, pcm440Vector.content_sha256, { fault: "r2-after-write" });
      expect(first.status).toBe(500);
      expect(await errorCodeOf(first)).toBe("internal");
      expect(await env.AUDIO.get(key)).not.toBeNull();
      expect(await eventCount(recordingId, "chunk.committed")).toBe(0);

      const retry = await putChunk(dev, recordingId, 0, PCM, pcm440Vector.content_sha256);
      expect(retry.status).toBe(200);
      const data = await jsonObjectOf(retry);
      expect(data.durable).toBe(true);
      expect(data.duplicate).toBeUndefined();
      expect(await eventCount(recordingId, "chunk.committed")).toBe(1);
    });

    it("d1-manifest: batch rolls back entirely; retry commits", async () => {
      const dev = await pairDevice();
      const recordingId = crypto.randomUUID();
      await createRecording(dev, recordingId);
      const key = `owners/default/recordings/${recordingId}/chunks/00000000.pcm`;

      const first = await putChunk(dev, recordingId, 0, PCM, pcm440Vector.content_sha256, { fault: "d1-manifest" });
      expect(first.status).toBe(500);
      expect(await env.AUDIO.get(key)).not.toBeNull();
      expect(await eventCount(recordingId, "chunk.committed")).toBe(0);
      const manifest = await env.DB.prepare("SELECT COUNT(*) AS n FROM chunks WHERE recording_id = ?").bind(recordingId).first<{ n: number }>();
      expect(manifest?.n).toBe(0);

      const retry = await putChunk(dev, recordingId, 0, PCM, pcm440Vector.content_sha256);
      expect(retry.status).toBe(200);
      expect(await eventCount(recordingId, "chunk.committed")).toBe(1);
    });

    it("d1-event: batch rolls back entirely; retry commits", async () => {
      const dev = await pairDevice();
      const recordingId = crypto.randomUUID();
      await createRecording(dev, recordingId);

      const first = await putChunk(dev, recordingId, 0, PCM, pcm440Vector.content_sha256, { fault: "d1-event" });
      expect(first.status).toBe(500);
      expect(await eventCount(recordingId, "chunk.committed")).toBe(0);
      const manifest = await env.DB.prepare("SELECT COUNT(*) AS n FROM chunks WHERE recording_id = ?").bind(recordingId).first<{ n: number }>();
      expect(manifest?.n).toBe(0);

      const retry = await putChunk(dev, recordingId, 0, PCM, pcm440Vector.content_sha256);
      expect(retry.status).toBe(200);
      expect(await eventCount(recordingId, "chunk.committed")).toBe(1);
    });

    it("response-drop: commit persisted; retry hits duplicate:true without a new event", async () => {
      const dev = await pairDevice();
      const recordingId = crypto.randomUUID();
      await createRecording(dev, recordingId);

      const first = await putChunk(dev, recordingId, 0, PCM, pcm440Vector.content_sha256, { fault: "response-drop" });
      expect(first.status).toBe(500);
      expect(await eventCount(recordingId, "chunk.committed")).toBe(1);

      const retry = await putChunk(dev, recordingId, 0, PCM, pcm440Vector.content_sha256);
      expect(retry.status).toBe(200);
      const data = await jsonObjectOf(retry);
      expect(data.duplicate).toBe(true);
      expect(data.durable).toBe(true);
      expect(await eventCount(recordingId, "chunk.committed")).toBe(1);
    });
  });
});
