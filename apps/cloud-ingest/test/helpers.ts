// 测试公共设施：走 SELF（真实 worker 入口 + 真实 D1/R2 绑定）驱动全部场景。
import { SELF, env } from "cloudflare:test";
import { base64UrlEncode } from "../src/auth";

export const ADMIN_TOKEN = "test-admin-token";
export const BASE = "https://ingest.test";
export const VERSION_HEADERS: Record<string, string> = { "x-ingest-version": "ingest-v1" };

export interface Device {
  deviceId: string;
  token: string;
  owner: string;
}

export interface SyncCreds {
  tokenId: string;
  token: string;
}

// ---- 运行时校验后再读取响应体（不做未校验断言）----

let ipCounter = 0;

// 每次调用分配唯一源 IP：pair/admin 的固定窗口限速桶按 IP 计，
// 同文件内多个用例若共用桶会互相挤占（10/min、60/min）。
function uniqueTestIp(): string {
  ipCounter += 1;
  return `10.${(ipCounter >>> 8) & 255}.${ipCounter & 255}.1`;
}

export function isJsonObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

export async function jsonObjectOf(res: Response): Promise<Record<string, unknown>> {
  const body: unknown = await res.json();
  if (!isJsonObject(body)) throw new Error(`expected JSON object body, got: ${JSON.stringify(body)}`);
  return body;
}

export interface ApiErrorBody {
  code: string;
  details: Record<string, unknown>;
}

// 单次读取错误体（Response body 只能消费一次），同时取出 code 与 details。
export async function errorOf(res: Response): Promise<ApiErrorBody> {
  const body = await jsonObjectOf(res);
  const err = body.error;
  if (!isJsonObject(err) || typeof err.code !== "string" || !isJsonObject(err.details)) {
    throw new Error(`expected {error:{code:string,details:object}}, got: ${JSON.stringify(body)}`);
  }
  return { code: err.code, details: err.details };
}

export async function errorCodeOf(res: Response): Promise<string> {
  return (await errorOf(res)).code;
}

export async function mintPairingCode(owner = "default", ttlSeconds = 900): Promise<string> {
  const res = await SELF.fetch(`${BASE}/v1/admin/pairing-codes`, {
    method: "POST",
    headers: { ...VERSION_HEADERS, authorization: `Bearer ${ADMIN_TOKEN}`, "content-type": "application/json", "cf-connecting-ip": uniqueTestIp() },
    body: JSON.stringify({ owner, ttl_seconds: ttlSeconds }),
  });
  if (res.status !== 200) throw new Error(`mintPairingCode(${owner}) failed: ${res.status} ${await res.text()}`);
  const data = await jsonObjectOf(res);
  const code = data.pairing_code;
  if (typeof code !== "string") throw new Error("mintPairingCode: pairing_code missing");
  return code;
}

export async function pairDevice(owner = "default"): Promise<Device> {
  const code = await mintPairingCode(owner);
  const res = await SELF.fetch(`${BASE}/v1/devices/pair`, {
    method: "POST",
    headers: { ...VERSION_HEADERS, "content-type": "application/json", "cf-connecting-ip": uniqueTestIp() },
    body: JSON.stringify({ protocol_version: "ingest-v1", pairing_code: code, device_name: "test-device" }),
  });
  if (res.status !== 200) throw new Error(`pairDevice(${owner}) failed: ${res.status} ${await res.text()}`);
  const data = await jsonObjectOf(res);
  const deviceId = data.device_id;
  const token = data.device_token;
  const ownerOut = data.owner;
  if (typeof deviceId !== "string" || typeof token !== "string" || typeof ownerOut !== "string") {
    throw new Error(`pairDevice: unexpected response shape: ${JSON.stringify(data)}`);
  }
  return { deviceId, token, owner: ownerOut };
}

export async function mintSyncToken(owner = "default"): Promise<SyncCreds> {
  const res = await SELF.fetch(`${BASE}/v1/admin/sync-tokens`, {
    method: "POST",
    headers: { ...VERSION_HEADERS, authorization: `Bearer ${ADMIN_TOKEN}`, "content-type": "application/json", "cf-connecting-ip": uniqueTestIp() },
    body: JSON.stringify({ owner }),
  });
  if (res.status !== 200) throw new Error(`mintSyncToken(${owner}) failed: ${res.status} ${await res.text()}`);
  const data = await jsonObjectOf(res);
  const tokenId = data.token_id;
  const token = data.sync_token;
  if (typeof tokenId !== "string" || typeof token !== "string") {
    throw new Error(`mintSyncToken: unexpected response shape: ${JSON.stringify(data)}`);
  }
  return { tokenId, token };
}

export async function createRecording(dev: Device, recordingId: string = crypto.randomUUID(), extra: Record<string, unknown> = {}): Promise<Response> {
  return SELF.fetch(`${BASE}/v1/recordings`, {
    method: "POST",
    headers: { ...VERSION_HEADERS, authorization: `Bearer ${dev.token}`, "content-type": "application/json" },
    body: JSON.stringify({ protocol_version: "ingest-v1", recording_id: recordingId, codec: "pcm_s16le", sample_rate: 16000, channels: 1, ...extra }),
  });
}

// 契约 §4.4：X-Chunk-Meta = base64url(JSON)。
export function chunkMetaHeader(sampleOffset: number, sampleCount: number, durationMs: number, contentSha256: string, recordedAt: string | null = null): string {
  const payload = { sample_offset: sampleOffset, sample_count: sampleCount, duration_ms: durationMs, content_sha256: contentSha256, recorded_at: recordedAt };
  return base64UrlEncode(new TextEncoder().encode(JSON.stringify(payload)));
}

export interface PutChunkOptions {
  sampleOffset?: number;
  sampleCount?: number; // 覆盖元数据声明值，用于制造长度不符
  durationMs?: number;
  recordedAt?: string | null;
  fault?: string; // X-Debug-Fault 值
}

export async function putChunk(dev: Device, recordingId: string, seq: number, body: Uint8Array, contentSha256: string, opts: PutChunkOptions = {}): Promise<Response> {
  const headers: Record<string, string> = {
    ...VERSION_HEADERS,
    authorization: `Bearer ${dev.token}`,
    "content-type": "application/octet-stream",
    "x-chunk-meta": chunkMetaHeader(
      opts.sampleOffset ?? 0,
      opts.sampleCount ?? body.byteLength / 2,
      opts.durationMs ?? Math.round(body.byteLength / 32),
      contentSha256,
      opts.recordedAt ?? null,
    ),
  };
  if (opts.fault !== undefined) headers["x-debug-fault"] = opts.fault;
  return SELF.fetch(`${BASE}/v1/recordings/${recordingId}/chunks/${seq}`, { method: "PUT", headers, body });
}

export async function finishRecording(dev: Device, recordingId: string, totalChunks: number, totalSamples: number, totalDurationMs: number): Promise<Response> {
  return SELF.fetch(`${BASE}/v1/recordings/${recordingId}/finish`, {
    method: "POST",
    headers: { ...VERSION_HEADERS, authorization: `Bearer ${dev.token}`, "content-type": "application/json" },
    body: JSON.stringify({ protocol_version: "ingest-v1", total_chunks: totalChunks, total_samples: totalSamples, total_duration_ms: totalDurationMs }),
  });
}

export async function getRecording(token: string, recordingId: string): Promise<Response> {
  return SELF.fetch(`${BASE}/v1/recordings/${recordingId}`, { headers: { ...VERSION_HEADERS, authorization: `Bearer ${token}` } });
}

export async function eventCount(recordingId: string, type: string): Promise<number> {
  const row = await env.DB.prepare("SELECT COUNT(*) AS n FROM events WHERE recording_id = ? AND type = ?").bind(recordingId, type).first<{ n: number }>();
  return row?.n ?? 0;
}

export function decodeBase64(text: string): Uint8Array {
  const binary = atob(text);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return bytes;
}

export function toHex(bytes: Uint8Array): string {
  return [...bytes].map((b) => b.toString(16).padStart(2, "0")).join("");
}
