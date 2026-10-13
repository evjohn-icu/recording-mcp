// 配对码换取设备凭据（契约 §4.2）：配对码一次性（原子 UPDATE 领取），限速 10 次/分钟/IP。

import type { Env } from "./index";
import { ApiError, PROTOCOL_VERSION, isBase32Code, json, requireProtocolVersion } from "./errors";
import { base64UrlEncode, sha256Hex } from "./auth";
import { MAX_JSON_BODY_BYTES, RATE_LIMITS, rateLimit, readJsonWithLimit } from "./limits";

const BASE32_ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";

// base32(A-Z2-7) 随机码：256 % 32 == 0，字节取模均匀无偏。
export function randomBase32(length: number): string {
  const bytes = crypto.getRandomValues(new Uint8Array(length));
  let out = "";
  for (let i = 0; i < length; i++) out += BASE32_ALPHABET[bytes[i]! % 32];
  return out;
}

// 契约 §7 冻结公式：配对码哈希 = SHA-256(pepper + ":" + code)（明文码只在创建响应出现一次）。
export function pairingCodeHash(pepper: string, code: string): Promise<string> {
  return sha256Hex(`${pepper}:${code}`);
}

export async function handlePair(request: Request, env: Env): Promise<Response> {
  const ip = request.headers.get("cf-connecting-ip") ?? "local";
  await rateLimit(env, `pair:${ip}`, RATE_LIMITS.pairPerMinute);

  const body = await readJsonWithLimit(request, MAX_JSON_BODY_BYTES);
  requireProtocolVersion(body);

  const code = body.pairing_code;
  if (!isBase32Code(code)) throw new ApiError("pairing_code_invalid", "pairing_code must be 32 chars of A-Z2-7");

  let deviceName = "";
  if (body.device_name !== undefined) {
    if (typeof body.device_name !== "string" || body.device_name.length > 128) {
      throw new ApiError("validation_failed", "device_name must be a string of at most 128 chars");
    }
    deviceName = body.device_name;
  }

  const now = new Date();
  const nowIso = now.toISOString();
  const codeHash = await pairingCodeHash(env.PAIRING_PEPPER ?? "", code);

  // 一次性领取：原子 UPDATE 是唯一闸门（used_at IS NULL 且未过期才允许）。
  const claimed = await env.DB
    .prepare("UPDATE pairing_codes SET used_at = ? WHERE code_hash = ? AND used_at IS NULL AND expires_at > ?")
    .bind(nowIso, codeHash, nowIso)
    .run();
  if ((claimed.meta.changes ?? 0) !== 1) {
    const row = await env.DB
      .prepare("SELECT expires_at, used_at FROM pairing_codes WHERE code_hash = ?")
      .bind(codeHash)
      .first<{ expires_at: string; used_at: string | null }>();
    if (row && row.used_at === null && Date.parse(row.expires_at) <= now.getTime()) {
      throw new ApiError("pairing_code_expired", "pairing code has expired");
    }
    throw new ApiError("pairing_code_invalid", "pairing code does not exist or was already used");
  }

  const ownerRow = await env.DB
    .prepare("SELECT owner FROM pairing_codes WHERE code_hash = ?")
    .bind(codeHash)
    .first<{ owner: string }>();
  if (!ownerRow) throw new ApiError("internal", "pairing code row vanished after claim");

  const deviceId = `dev_${crypto.randomUUID()}`;
  const deviceToken = base64UrlEncode(crypto.getRandomValues(new Uint8Array(32)));
  const tokenHash = await sha256Hex(deviceToken);
  await env.DB
    .prepare("INSERT INTO devices(device_id, owner, name, token_hash, created_at) VALUES(?,?,?,?,?)")
    .bind(deviceId, ownerRow.owner, deviceName, tokenHash, nowIso)
    .run();
  return json({ device_id: deviceId, owner: ownerRow.owner, device_token: deviceToken, server_protocol_version: PROTOCOL_VERSION });
}
