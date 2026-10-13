// 管理面（契约 §4.12）：仅 ADMIN_TOKEN；未配置 ADMIN_TOKEN 时这些路由一律 404。

import type { Env } from "./index";
import { ApiError, json } from "./errors";
import { bearerToken, sha256Hex } from "./auth";
import { MAX_JSON_BODY_BYTES, RATE_LIMITS, rateLimit, readJsonWithLimit } from "./limits";
import { pairingCodeHash, randomBase32 } from "./pairing";

// 返回非 null 时直接作为响应（admin 关闭 → 404）。
function adminAuth(request: Request, env: Env): Response | null {
  if (!env.ADMIN_TOKEN) return new Response("not found", { status: 404 });
  const token = bearerToken(request);
  if (!token || token !== env.ADMIN_TOKEN) throw new ApiError("unauthorized", "admin token required");
  return null;
}

async function limitAdmin(request: Request, env: Env): Promise<void> {
  const ip = request.headers.get("cf-connecting-ip") ?? "local";
  await rateLimit(env, `admin:${ip}`, RATE_LIMITS.adminPerMinute);
}

async function upsertOwner(env: Env, owner: string, nowIso: string): Promise<void> {
  await env.DB.prepare("INSERT OR IGNORE INTO owners(owner, created_at) VALUES(?, ?)").bind(owner, nowIso).run();
}

export async function handleAdminPairingCode(request: Request, env: Env): Promise<Response> {
  const gate = adminAuth(request, env);
  if (gate) return gate;
  await limitAdmin(request, env);

  const body = await readJsonWithLimit(request, MAX_JSON_BODY_BYTES);
  const owner = body.owner === undefined ? "default" : body.owner;
  if (typeof owner !== "string" || owner.length === 0 || owner.length > 128) {
    throw new ApiError("validation_failed", "owner must be a string of 1..128 chars");
  }
  const ttlSeconds = body.ttl_seconds === undefined ? 900 : body.ttl_seconds;
  if (typeof ttlSeconds !== "number" || !Number.isInteger(ttlSeconds) || ttlSeconds < 60 || ttlSeconds > 3600) {
    throw new ApiError("validation_failed", "ttl_seconds must be an integer in [60, 3600]");
  }

  const now = new Date();
  const nowIso = now.toISOString();
  await upsertOwner(env, owner, nowIso);
  const code = randomBase32(32);
  const expiresAt = new Date(now.getTime() + ttlSeconds * 1000).toISOString();
  const codeHash = await pairingCodeHash(env.PAIRING_PEPPER ?? "", code);
  await env.DB
    .prepare("INSERT INTO pairing_codes(code_hash, owner, expires_at, created_at) VALUES(?,?,?,?)")
    .bind(codeHash, owner, expiresAt, nowIso)
    .run();
  return json({ pairing_code: code, expires_at: expiresAt });
}

export async function handleAdminSyncToken(request: Request, env: Env): Promise<Response> {
  const gate = adminAuth(request, env);
  if (gate) return gate;
  await limitAdmin(request, env);

  const body = await readJsonWithLimit(request, MAX_JSON_BODY_BYTES);
  const owner = body.owner === undefined ? "default" : body.owner;
  if (typeof owner !== "string" || owner.length === 0 || owner.length > 128) {
    throw new ApiError("validation_failed", "owner must be a string of 1..128 chars");
  }
  let label = "";
  if (body.label !== undefined) {
    if (typeof body.label !== "string" || body.label.length > 128) {
      throw new ApiError("validation_failed", "label must be a string of at most 128 chars");
    }
    label = body.label;
  }

  const nowIso = new Date().toISOString();
  await upsertOwner(env, owner, nowIso);
  const tokenId = `tok_${crypto.randomUUID()}`;
  // 明文 sync token 只在此响应出现一次；D1 只存 SHA-256 哈希（契约 §7）。
  const syncToken = btoa(String.fromCharCode(...crypto.getRandomValues(new Uint8Array(32))))
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");
  const tokenHash = await sha256Hex(syncToken);
  await env.DB
    .prepare("INSERT INTO sync_tokens(token_id, owner, label, token_hash, created_at) VALUES(?,?,?,?,?)")
    .bind(tokenId, owner, label, tokenHash, nowIso)
    .run();
  return json({ token_id: tokenId, sync_token: syncToken });
}

export async function handleAdminRevokeDevice(request: Request, env: Env, deviceId: string): Promise<Response> {
  const gate = adminAuth(request, env);
  if (gate) return gate;
  await limitAdmin(request, env);

  const nowIso = new Date().toISOString();
  const result = await env.DB
    .prepare("UPDATE devices SET revoked_at = ? WHERE device_id = ? AND revoked_at IS NULL")
    .bind(nowIso, deviceId)
    .run();
  if ((result.meta.changes ?? 0) === 0) {
    const row = await env.DB.prepare("SELECT revoked_at FROM devices WHERE device_id = ?").bind(deviceId).first<{ revoked_at: string | null }>();
    if (!row) throw new ApiError("recording_not_found", "device not found");
    return json({ revoked_at: row.revoked_at ?? nowIso }); // 幂等：已撤销 → 返回既有时间戳
  }
  return json({ revoked_at: nowIso });
}

export async function handleAdminRevokeSyncToken(request: Request, env: Env, tokenId: string): Promise<Response> {
  const gate = adminAuth(request, env);
  if (gate) return gate;
  await limitAdmin(request, env);

  const nowIso = new Date().toISOString();
  const result = await env.DB
    .prepare("UPDATE sync_tokens SET revoked_at = ? WHERE token_id = ? AND revoked_at IS NULL")
    .bind(nowIso, tokenId)
    .run();
  if ((result.meta.changes ?? 0) === 0) {
    const row = await env.DB.prepare("SELECT revoked_at FROM sync_tokens WHERE token_id = ?").bind(tokenId).first<{ revoked_at: string | null }>();
    if (!row) throw new ApiError("recording_not_found", "sync token not found");
    return json({ revoked_at: row.revoked_at ?? nowIso }); // 幂等：已撤销 → 返回既有时间戳
  }
  return json({ revoked_at: nowIso });
}
