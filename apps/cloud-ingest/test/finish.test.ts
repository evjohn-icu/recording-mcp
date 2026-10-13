// finish 语义：缺口 → sequence_gap 且 state 保持 recording；总量/offset 链不符 → conflict_payload；
// 成功 → complete + 一次性事件；同 body 重放 → 200 无新事件（契约 §4.5）。
import { describe, expect, it } from "vitest";
import pcm440Vector from "../../../contracts/ingest-v1/vectors/pcm_1s_440hz.json";
import {
  createRecording,
  decodeBase64,
  errorCodeOf,
  errorOf,
  eventCount,
  finishRecording,
  getRecording,
  jsonObjectOf,
  pairDevice,
  putChunk,
  type Device,
} from "./helpers";

const PCM = decodeBase64(pcm440Vector.pcm_base64);
const SHA = pcm440Vector.content_sha256;

async function uploadSequence(dev: Device, recordingId: string, seqs: number[], offsets: number[]): Promise<void> {
  for (let i = 0; i < seqs.length; i++) {
    const res = await putChunk(dev, recordingId, seqs[i]!, PCM, SHA, { sampleOffset: offsets[i]! });
    expect(res.status).toBe(200);
  }
}

describe("POST /v1/recordings/{id}/finish", () => {
  it("refuses with sequence_gap and keeps state recording while chunks are missing", async () => {
    const dev = await pairDevice();
    const recordingId = crypto.randomUUID();
    await createRecording(dev, recordingId);
    await uploadSequence(dev, recordingId, [0, 2], [0, 32000]); // 缺 seq 1

    const res = await finishRecording(dev, recordingId, 3, 48000, 3000);
    expect(res.status).toBe(409);
    const err = await errorOf(res);
    expect(err.code).toBe("sequence_gap");
    expect(err.details.missing_sequences).toEqual([1]);

    const detail = await jsonObjectOf(await getRecording(dev.token, recordingId));
    expect(detail.state).toBe("recording");
    expect(await eventCount(recordingId, "recording.finished")).toBe(0);
  });

  it("refuses with conflict_payload when total_samples does not match the manifest", async () => {
    const dev = await pairDevice();
    const recordingId = crypto.randomUUID();
    await createRecording(dev, recordingId);
    await uploadSequence(dev, recordingId, [0, 1, 2], [0, 16000, 32000]);

    const res = await finishRecording(dev, recordingId, 3, 47999, 3000);
    expect(res.status).toBe(409);
    const err = await errorOf(res);
    expect(err.code).toBe("conflict_payload");
    expect(err.details.sum_samples).toBe(48000);
    expect(err.details.total_samples).toBe(47999);
  });

  it("refuses with conflict_payload when a non-last offset breaks the chain", async () => {
    const dev = await pairDevice();
    const recordingId = crypto.randomUUID();
    await createRecording(dev, recordingId);
    // 3 块：seq1 为非末块且 offset 断链；seq2 为末块（offset 不参与链校验）。
    await uploadSequence(dev, recordingId, [0, 1, 2], [0, 17000, 999999]);

    const res = await finishRecording(dev, recordingId, 3, 48000, 3000);
    expect(res.status).toBe(409);
    const err = await errorOf(res);
    expect(err.code).toBe("conflict_payload");
    expect(err.details.bad_offset_sequences).toEqual([1]);
  });

  it("excludes the last chunk from the offset-chain check (frozen semantics)", async () => {
    const dev = await pairDevice();
    const recordingId = crypto.randomUUID();
    await createRecording(dev, recordingId);
    await uploadSequence(dev, recordingId, [0, 1], [0, 999999]); // 末块 offset 不校验

    const res = await finishRecording(dev, recordingId, 2, 32000, 2000);
    expect(res.status).toBe(200);
  });

  it("refuses with conflict_payload when the first offset is not zero", async () => {
    const dev = await pairDevice();
    const recordingId = crypto.randomUUID();
    await createRecording(dev, recordingId);
    await uploadSequence(dev, recordingId, [0], [16000]);

    const res = await finishRecording(dev, recordingId, 1, 16000, 1000);
    expect(res.status).toBe(409);
    const err = await errorOf(res);
    expect(err.code).toBe("conflict_payload");
    expect(err.details.bad_offset_sequences).toEqual([0]);
  });

  it("completes once, emits one event, and replays idempotently", async () => {
    const dev = await pairDevice();
    const recordingId = crypto.randomUUID();
    await createRecording(dev, recordingId);
    await uploadSequence(dev, recordingId, [0, 1, 2], [0, 16000, 32000]);

    const first = await finishRecording(dev, recordingId, 3, 48000, 3000);
    expect(first.status).toBe(200);
    expect(await first.json()).toEqual({ state: "complete", total_chunks: 3, total_samples: 48000, durable: true });
    expect(await eventCount(recordingId, "recording.finished")).toBe(1);

    const detail = await jsonObjectOf(await getRecording(dev.token, recordingId));
    expect(detail.state).toBe("complete");
    expect(detail.finished_at).not.toBeNull();
    expect(detail.total_chunks).toBe(3);

    const replay = await finishRecording(dev, recordingId, 3, 48000, 3000);
    expect(replay.status).toBe(200);
    expect(await eventCount(recordingId, "recording.finished")).toBe(1);
  });

  it("returns conflict_payload when replaying finish with different totals after completion", async () => {
    const dev = await pairDevice();
    const recordingId = crypto.randomUUID();
    await createRecording(dev, recordingId);
    await uploadSequence(dev, recordingId, [0, 1, 2], [0, 16000, 32000]);
    expect((await finishRecording(dev, recordingId, 3, 48000, 3000)).status).toBe(200);

    const res = await finishRecording(dev, recordingId, 3, 47999, 3000);
    expect(res.status).toBe(409);
    expect(await errorCodeOf(res)).toBe("conflict_payload");
  });

  it("validates body fields and rejects unknown recordings", async () => {
    const dev = await pairDevice();
    const recordingId = crypto.randomUUID();
    await createRecording(dev, recordingId);

    const zeroChunks = await finishRecording(dev, recordingId, 0, 16000, 1000);
    expect(zeroChunks.status).toBe(400);
    expect(await errorOf(zeroChunks).then((e) => e.code)).toBe("validation_failed");

    const unknown = await finishRecording(dev, crypto.randomUUID(), 1, 16000, 1000);
    expect(unknown.status).toBe(404);
    expect(await errorOf(unknown).then((e) => e.code)).toBe("recording_not_found");
  });
});
