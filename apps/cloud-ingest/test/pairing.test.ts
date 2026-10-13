// 配对码：一次性、过期与无效码的区分、响应形状（契约 §4.2、§7）。
import { describe, expect, it } from "vitest";
import { SELF, env } from "cloudflare:test";
import {
  BASE,
  VERSION_HEADERS,
  errorCodeOf,
  jsonObjectOf,
  mintPairingCode,
} from "./helpers";

async function pairWithCode(code: string): Promise<Response> {
  return SELF.fetch(`${BASE}/v1/devices/pair`, {
    method: "POST",
    headers: { ...VERSION_HEADERS, "content-type": "application/json" },
    body: JSON.stringify({ protocol_version: "ingest-v1", pairing_code: code, device_name: "pairing-test" }),
  });
}

describe("POST /v1/devices/pair", () => {
  it("exchanges a valid code for device credentials with the contract shape", async () => {
    const code = await mintPairingCode("default");
    const res = await pairWithCode(code);
    expect(res.status).toBe(200);

    const data = await jsonObjectOf(res);
    expect(data.server_protocol_version).toBe("ingest-v1");
    expect(data.owner).toBe("default");
    const deviceId = data.device_id;
    const token = data.device_token;
    if (typeof deviceId !== "string" || typeof token !== "string") throw new Error("missing device credentials");
    expect(deviceId).toMatch(/^dev_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
    expect(token).toMatch(/^[A-Za-z0-9_-]{43}$/);
  });

  it("burns the code after first use (one-time semantics)", async () => {
    const code = await mintPairingCode("default");
    expect((await pairWithCode(code)).status).toBe(200);

    const replay = await pairWithCode(code);
    expect(replay.status).toBe(400);
    expect(await errorCodeOf(replay)).toBe("pairing_code_invalid");
  });

  it("distinguishes expired codes from invalid ones", async () => {
    const code = await mintPairingCode("default", 60);
    await env.DB.prepare("UPDATE pairing_codes SET expires_at = ?").bind("2000-01-01T00:00:00.000Z").run();

    const res = await pairWithCode(code);
    expect(res.status).toBe(400);
    expect(await errorCodeOf(res)).toBe("pairing_code_expired");
  });

  it("rejects unknown well-formed codes with pairing_code_invalid", async () => {
    const fake = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"; // 32 × A，未在库中
    const res = await pairWithCode(fake);
    expect(res.status).toBe(400);
    expect(await errorCodeOf(res)).toBe("pairing_code_invalid");
  });

  it("rejects malformed codes with pairing_code_invalid", async () => {
    const res = await pairWithCode("short-code");
    expect(res.status).toBe(400);
    expect(await errorCodeOf(res)).toBe("pairing_code_invalid");
  });

  it("rejects a wrong protocol_version in the body", async () => {
    const code = await mintPairingCode("default");
    const res = await SELF.fetch(`${BASE}/v1/devices/pair`, {
      method: "POST",
      headers: { ...VERSION_HEADERS, "content-type": "application/json" },
      body: JSON.stringify({ protocol_version: "ingest-v9", pairing_code: code }),
    });
    expect(res.status).toBe(400);
    expect(await errorCodeOf(res)).toBe("validation_failed");
  });

  it("scopes the new device to the code's owner", async () => {
    const code = await mintPairingCode("tenant-x");
    const res = await pairWithCode(code);
    expect(res.status).toBe(200);
    const data = await jsonObjectOf(res);
    expect(data.owner).toBe("tenant-x");
  });
});
