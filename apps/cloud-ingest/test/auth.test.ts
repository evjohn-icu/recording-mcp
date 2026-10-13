// 认证与授权：Bearer → SHA-256 查表；撤销 → token_revoked；角色不符 → forbidden_scope；跨 owner → 404（契约 §7）。
import { describe, expect, it } from "vitest";
import { SELF } from "cloudflare:test";
import pcm440Vector from "../../../contracts/ingest-v1/vectors/pcm_1s_440hz.json";
import {
  ADMIN_TOKEN,
  BASE,
  VERSION_HEADERS,
  createRecording,
  decodeBase64,
  errorCodeOf,
  jsonObjectOf,
  mintSyncToken,
  pairDevice,
  putChunk,
} from "./helpers";

const PCM440_BYTES = decodeBase64(pcm440Vector.pcm_base64);
const PCM440_SHA = pcm440Vector.content_sha256;

describe("auth", () => {
  it("rejects unauthenticated requests with 401 unauthorized", async () => {
    const res = await SELF.fetch(`${BASE}/v1/recordings`, { method: "POST", headers: { ...VERSION_HEADERS }, body: JSON.stringify({}) });
    expect(res.status).toBe(401);
    expect(await errorCodeOf(res)).toBe("unauthorized");
  });

  it("rejects unknown bearer tokens with 401 unauthorized", async () => {
    const res = await SELF.fetch(`${BASE}/v1/recordings`, {
      method: "POST",
      headers: { ...VERSION_HEADERS, authorization: "Bearer not-a-real-token" },
      body: JSON.stringify({}),
    });
    expect(res.status).toBe(401);
    expect(await errorCodeOf(res)).toBe("unauthorized");
  });

  it("returns token_revoked after admin device revocation", async () => {
    const dev = await pairDevice();
    const revoke = await SELF.fetch(`${BASE}/v1/admin/devices/${dev.deviceId}/revoke`, {
      method: "POST",
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${ADMIN_TOKEN}` },
    });
    expect(revoke.status).toBe(200);

    const res = await createRecording(dev);
    expect(res.status).toBe(401);
    expect(await errorCodeOf(res)).toBe("token_revoked");
  });

  it("returns token_revoked for revoked sync tokens", async () => {
    const sync = await mintSyncToken();
    const revoke = await SELF.fetch(`${BASE}/v1/admin/sync-tokens/${sync.tokenId}/revoke`, {
      method: "POST",
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${ADMIN_TOKEN}` },
    });
    expect(revoke.status).toBe(200);

    const res = await SELF.fetch(`${BASE}/v1/sync?cursor=0`, { headers: { ...VERSION_HEADERS, authorization: `Bearer ${sync.token}` } });
    expect(res.status).toBe(401);
    expect(await errorCodeOf(res)).toBe("token_revoked");
  });

  it("enforces role scope: sync token cannot PUT chunks", async () => {
    const sync = await mintSyncToken();
    const res = await SELF.fetch(`${BASE}/v1/recordings/${crypto.randomUUID()}/chunks/0`, {
      method: "PUT",
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${sync.token}`, "x-chunk-meta": "e30" },
      body: new Uint8Array(2),
    });
    expect(res.status).toBe(403);
    expect(await errorCodeOf(res)).toBe("forbidden_scope");
  });

  it("enforces role scope: device token cannot read the event log", async () => {
    const dev = await pairDevice();
    const res = await SELF.fetch(`${BASE}/v1/sync?cursor=0`, { headers: { ...VERSION_HEADERS, authorization: `Bearer ${dev.token}` } });
    expect(res.status).toBe(403);
    expect(await errorCodeOf(res)).toBe("forbidden_scope");
  });

  it("enforces role scope: device token cannot download chunk audio", async () => {
    const dev = await pairDevice();
    const res = await SELF.fetch(`${BASE}/v1/recordings/${crypto.randomUUID()}/chunks/0/audio`, {
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${dev.token}` },
    });
    expect(res.status).toBe(403);
    expect(await errorCodeOf(res)).toBe("forbidden_scope");
  });

  it("hides other owners' recordings behind 404 recording_not_found", async () => {
    const devA = await pairDevice("default");
    const devB = await pairDevice("other");
    const created = await createRecording(devA);
    expect(created.status).toBe(201);
    const createdBody = await jsonObjectOf(created);
    const recordingId = createdBody.recording_id;
    if (typeof recordingId !== "string") throw new Error("recording_id missing in create response");

    const res = await SELF.fetch(`${BASE}/v1/recordings/${recordingId}`, {
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${devB.token}` },
    });
    expect(res.status).toBe(404);
    expect(await errorCodeOf(res)).toBe("recording_not_found");

    // 合法分块请求（校验全部通过）跨 owner 上传 → 仍 404，不区分存在性。
    const put = await putChunk(devB, recordingId, 0, PCM440_BYTES, PCM440_SHA, { sampleOffset: 0 });
    expect(put.status).toBe(404);
    expect(await errorCodeOf(put)).toBe("recording_not_found");
  });

  it("rejects a wrong admin token with 401 unauthorized", async () => {
    const res = await SELF.fetch(`${BASE}/v1/admin/pairing-codes`, {
      method: "POST",
      headers: { ...VERSION_HEADERS, authorization: "Bearer wrong-admin-token" },
      body: "{}",
    });
    expect(res.status).toBe(401);
    expect(await errorCodeOf(res)).toBe("unauthorized");
  });

  it("returns 403 forbidden_scope when a valid device token calls an admin route", async () => {
    const dev = await pairDevice();
    const res = await SELF.fetch(`${BASE}/v1/admin/pairing-codes`, {
      method: "POST",
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${dev.token}`, "content-type": "application/json" },
      body: JSON.stringify({ owner: "default" }),
    });
    expect(res.status).toBe(403);
    expect(await errorCodeOf(res)).toBe("forbidden_scope");
  });

  it("returns 401 token_revoked when a revoked device token calls an admin route", async () => {
    const dev = await pairDevice();
    const revoke = await SELF.fetch(`${BASE}/v1/admin/devices/${dev.deviceId}/revoke`, {
      method: "POST",
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${ADMIN_TOKEN}` },
    });
    expect(revoke.status).toBe(200);

    const res = await SELF.fetch(`${BASE}/v1/admin/pairing-codes`, {
      method: "POST",
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${dev.token}`, "content-type": "application/json" },
      body: JSON.stringify({ owner: "default" }),
    });
    expect(res.status).toBe(401);
    expect(await errorCodeOf(res)).toBe("token_revoked");
  });

  it("returns 403 forbidden_scope when a valid sync token calls an admin route", async () => {
    const sync = await mintSyncToken();
    const res = await SELF.fetch(`${BASE}/v1/admin/pairing-codes`, {
      method: "POST",
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${sync.token}`, "content-type": "application/json" },
      body: JSON.stringify({ owner: "default" }),
    });
    expect(res.status).toBe(403);
    expect(await errorCodeOf(res)).toBe("forbidden_scope");
  });

  it("rejects /v1/* requests missing X-Ingest-Version with 400 unsupported_protocol_version", async () => {
    const res = await SELF.fetch(`${BASE}/v1/recordings`, { method: "POST", body: JSON.stringify({}) });
    expect(res.status).toBe(400);
    expect(await errorCodeOf(res)).toBe("unsupported_protocol_version");
  });

  it("rejects a mismatched X-Ingest-Version value", async () => {
    const res = await SELF.fetch(`${BASE}/v1/recordings`, {
      method: "POST",
      headers: { "x-ingest-version": "ingest-v2" },
      body: JSON.stringify({}),
    });
    expect(res.status).toBe(400);
    expect(await errorCodeOf(res)).toBe("unsupported_protocol_version");
  });
});
