// 限速（D1 固定窗口，契约 §6）与请求体上限（契约 §2）。
// 各桶限流阈值用"种子计数 + 两次真实请求"验证：count=limit-1 时放行、count=limit 后再请求即 429，
// 精确证明阈值等于契约字面量，且不受分钟窗口翻转的时间抖动影响。
import { describe, expect, it } from "vitest";
import { SELF, env } from "cloudflare:test";
import { ApiError } from "../src/errors";
import { rateLimit } from "../src/limits";
import { sha256Hex } from "../src/auth";
import { BASE, VERSION_HEADERS, errorCodeOf, pairDevice } from "./helpers";

const ADMIN_TOKEN = "test-admin-token";
const ADMIN_IP = "10.9.9.9"; // 独立 admin 限速桶

// 与 src/limits.ts 相同的窗口算法：UTC epoch 分钟。
function currentWindow(): number {
  return Math.floor(Date.now() / 60_000);
}

// 把 (bucket, 当前窗口) 的计数种子为 limit-1，使下一次请求恰好用尽限额。
async function seedCounter(bucket: string, count: number): Promise<void> {
  await env.DB.prepare(
    "INSERT INTO rate_counters(bucket, window_start, count) VALUES(?, ?, ?) " +
      "ON CONFLICT(bucket, window_start) DO UPDATE SET count = excluded.count",
  )
    .bind(bucket, currentWindow(), count)
    .run();
}

async function mintWithIp(owner: string, ip: string): Promise<Response> {
  return SELF.fetch(`${BASE}/v1/admin/pairing-codes`, {
    method: "POST",
    headers: { ...VERSION_HEADERS, authorization: `Bearer ${ADMIN_TOKEN}`, "content-type": "application/json", "cf-connecting-ip": ip },
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
  it("limits pairing to exactly 10 requests per minute per IP", async () => {
    await seedCounter("pair:local", 9);
    const badCode = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"; // 未入库：放行时 → 400
    const tenth = await pairWithCode(badCode);
    expect(tenth.status).toBe(400); // count=10 ≤ limit：放行
    const eleventh = await pairWithCode(badCode);
    expect(eleventh.status).toBe(429);
    expect(await errorCodeOf(eleventh)).toBe("rate_limited");
  });

  it("limits admin endpoints to exactly 60 requests per minute per IP", async () => {
    await seedCounter(`admin:${ADMIN_IP}`, 59);
    const sixtieth = await mintWithIp("rate-admin", ADMIN_IP);
    expect(sixtieth.status).toBe(200); // count=60 ≤ limit：放行
    const sixtyFirst = await mintWithIp("rate-admin", ADMIN_IP);
    expect(sixtyFirst.status).toBe(429);
    expect(await errorCodeOf(sixtyFirst)).toBe("rate_limited");
  });

  it("limits JSON endpoints to exactly 120 requests per minute per token", async () => {
    const dev = await pairDevice();
    await seedCounter(`json:${await sha256Hex(dev.token)}`, 119);
    const oneHundredTwentieth = await SELF.fetch(`${BASE}/v1/recordings/${crypto.randomUUID()}`, {
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${dev.token}` },
    });
    expect(oneHundredTwentieth.status).toBe(404); // count=120 ≤ limit：放行（未知录音 404）
    const oneTwentyFirst = await SELF.fetch(`${BASE}/v1/recordings/${crypto.randomUUID()}`, {
      headers: { ...VERSION_HEADERS, authorization: `Bearer ${dev.token}` },
    });
    expect(oneTwentyFirst.status).toBe(429);
    expect(await errorCodeOf(oneTwentyFirst)).toBe("rate_limited");
  });

  it("chunk PUT bucket allows 300/min per device and rejects the 301st", async () => {
    await seedCounter("chunk:unit-test", 299);
    await rateLimit(env, "chunk:unit-test", 300); // count=300 ≤ limit：放行
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
