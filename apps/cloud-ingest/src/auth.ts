// Bearer 凭据认证与编码原语。token 只以 SHA-256 hex 哈希存 D1（契约 §7）。

import type { Env } from "./index";
import { ApiError } from "./errors";

export type Principal =
  | { role: "device"; owner: string; deviceId: string; tokenHash: string }
  | { role: "sync"; owner: string; tokenId: string; tokenHash: string };

export async function sha256Hex(input: string | Uint8Array): Promise<string> {
  const data = typeof input === "string" ? new TextEncoder().encode(input) : input;
  const digest = await crypto.subtle.digest("SHA-256", data);
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

export function base64UrlEncode(bytes: Uint8Array): string {
  let binary = "";
  for (let i = 0; i < bytes.length; i++) binary += String.fromCharCode(bytes[i]!);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export function base64UrlDecode(text: string): Uint8Array {
  const b64 = text.replace(/-/g, "+").replace(/_/g, "/");
  const padded = b64 + "=".repeat((4 - (b64.length % 4)) % 4);
  const binary = atob(padded);
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i);
  return out;
}

export function bearerToken(request: Request): string | null {
  const header = request.headers.get("authorization");
  if (!header) return null;
  const match = /^Bearer\s+(\S+)\s*$/i.exec(header);
  return match ? match[1]! : null;
}

export async function authenticate(request: Request, env: Env): Promise<Principal> {
  const token = bearerToken(request);
  if (!token) throw new ApiError("unauthorized", "missing bearer token");
  const tokenHash = await sha256Hex(token);

  const device = await env.DB
    .prepare("SELECT device_id, owner, revoked_at FROM devices WHERE token_hash = ?")
    .bind(tokenHash)
    .first<{ device_id: string; owner: string; revoked_at: string | null }>();
  if (device) {
    if (device.revoked_at !== null) throw new ApiError("token_revoked", "device token has been revoked");
    return { role: "device", owner: device.owner, deviceId: device.device_id, tokenHash };
  }

  const sync = await env.DB
    .prepare("SELECT token_id, owner, revoked_at FROM sync_tokens WHERE token_hash = ?")
    .bind(tokenHash)
    .first<{ token_id: string; owner: string; revoked_at: string | null }>();
  if (sync) {
    if (sync.revoked_at !== null) throw new ApiError("token_revoked", "sync token has been revoked");
    return { role: "sync", owner: sync.owner, tokenId: sync.token_id, tokenHash };
  }

  throw new ApiError("unauthorized", "invalid credentials");
}

export function requireRole(principal: Principal, role: "device"): asserts principal is Extract<Principal, { role: "device" }>;
export function requireRole(principal: Principal, role: "sync"): asserts principal is Extract<Principal, { role: "sync" }>;
export function requireRole(principal: Principal, role: "device" | "sync"): void {
  if (principal.role !== role) {
    throw new ApiError("forbidden_scope", `endpoint requires ${role} credentials`);
  }
}
