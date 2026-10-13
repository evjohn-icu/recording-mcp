// 限速（D1 固定窗口，契约 §6）与请求体上限（契约 §2）。
// 注：各用例以独立桶（不同 IP 头 / 设备 / 直接调用）互不污染。
import { describe, expect, it } from "vitest";
import { SELF, env } from "cloudflare:test";
import { ApiError } from "../src/errors";
import { rateLimit } from "../src/limits";
import { BASE, VERSION_HEADERS, errorCodeOf, pairDevice } from "./helpers";

const ADMIN_IP = "10.9.9.9"; // 独立 admin 限速桶

async function mintWithIp(owner: string, ip: string): Promise<Response> {
  return SELF.fetch(`${BASE}/v1/admin/pairing-codes`, {
    method: "POST",
    headers: { ...VERSION_HEADERS, authorization: "Bearer test-admin-token", "content-type": "application/json", "cf-connecting-ip": ip },
    body: JSON.stringify({ owner }),
  });
}

async function pairWithCode(code: string): Promise<Response> {
  return SELF.fetch(`${BASE}/v1/devices/pair`, {
    method: "POST",
    headers: { ...VERSION_HEADERS, "content-type": "application/json" },
    body: JSON.stringify({ protocol_version: "ingest-v1", pairing_code: code }),
  });
}

describe("rate limits (D1 fixed window)", () => {
  it("limits pairing to 10 requests per minute per IP", async () => {
    const badCode = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"; // 未入库：前 10 次 → 400
    for (let i = 0; i < 10; i++) {
      const res = await pairWithCode(badCode);
      expect(res.status).toBe(400);
    }
    const eleventh = await pairWithCode(badCode);
    expect(eleventh.status).toBe(429);
    expect(await errorCodeOf(eleventh)).toBe("rate_limited");
  });

  it("limits admin endpoints to 60 requests per minute per IP", async () => {
    for (let i = 0; i < 60; i++) {
      const res = await mintWithIp("rate-admin", ADMIN_IP);
      expect(res.status).toBe(200);
    }
    const sixtyFirst = await mintWithIp("rate-admin", ADMIN_IP);
    expect(sixtyFirst.status).toBe(429);
    expect(await errorCodeOf(sixtyFirst)).toBe("rate_limited");
  });

  it("limits JSON endpoints to 120 requests per minute per token", async () => {
    const dev = await pairDevice();
    for (let i = 0; i < 120; i++) {
      const res = await SELF.fetch(`${BASE}/v1/recordings/${crypto.randomUUID()}`, {
        headers: { ...VERSION_HEADERS, authorization: `Bearer ${dev.token}` },
      });
      expect(res.status).toBe(404); // 未知录音：认证通过、计数后 404
    }
    const oneTwentyFirst = await SELF.fetch(`${BASE}/v1/recordings/${crypto.randomUUID()}`, {
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${dev.token}` },
    });
    expect(oneTwentyFirst.status).toBe(429);
    expect(await errorCodeOf(oneTwentyFirst)).toBe("rate_limited");
  });

  it("chunk PUT bucket allows 300/min per device and rejects the 301st", async () => {
    for (let i = 0; i < 300; i++) {
      await rateLimit(env, "chunk:unit-test", 300);
    }
    const err = await rateLimit(env, "chunk:unit-test", 300).then(
      () => null,
      (e: unknown) => e,
    );
    if (!(err instanceof ApiError)) throw new Error(`expected ApiError, got: ${String(err)}`);
    expect(err.code).toBe("rate_limited");
  });
});

describe("body size caps", () => {
  it("rejects JSON bodies over 64 KiB with 413 payload_too_large", async () => {
    const dev = await pairDevice();
    const padding = "a".repeat(70 * 1024);
    const res = await SELF.fetch(`${BASE}/v1/recordings`, {
      method: "POST",
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${dev.token}`, "content-type": "application/json" },
      body: JSON.stringify({ protocol_version: "ingest-v1", padding }),
    });
    expect(res.status).toBe(413);
    expect(await errorCodeOf(res)).toBe("payload_too_large");
  });
});
