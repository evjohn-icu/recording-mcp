// 请求体大小上限与 D1 固定窗口限速（契约 §6：超限 → 429 rate_limited）。

import type { Env } from "./index";
import { ApiError } from "./errors";

export const MAX_CHUNK_BODY_BYTES = 5 * 1024 * 1024; // 契约 §2：单块 ≤ 5 MiB
export const MAX_JSON_BODY_BYTES = 64 * 1024; // 契约 §2：JSON 体 ≤ 64 KiB

export const RATE_LIMITS = {
  chunkPutPerMinute: 300, // 每 设备/分钟
  jsonPerMinute: 120, // 每 token/分钟（其余 JSON 端点）
  pairPerMinute: 10, // 每 IP/分钟
  adminPerMinute: 60, // 每 IP/分钟
} as const;

export async function readBodyWithLimit(request: Request, maxBytes: number): Promise<Uint8Array> {
  const contentLength = request.headers.get("content-length");
  if (contentLength !== null && Number(contentLength) > maxBytes) {
    throw new ApiError("payload_too_large", `body exceeds ${maxBytes} bytes`);
  }
  const buffer = await request.arrayBuffer();
  if (buffer.byteLength > maxBytes) {
    throw new ApiError("payload_too_large", `body exceeds ${maxBytes} bytes`);
  }
  return new Uint8Array(buffer);
}

export async function readJsonWithLimit(request: Request, maxBytes: number): Promise<Record<string, unknown>> {
  const bytes = await readBodyWithLimit(request, maxBytes);
  let parsed: unknown;
  try {
    parsed = JSON.parse(new TextDecoder().decode(bytes));
  } catch {
    throw new ApiError("validation_failed", "request body must be valid JSON");
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    throw new ApiError("validation_failed", "request body must be a JSON object");
  }
  return parsed as Record<string, unknown>;
}

// window_start 以"分钟"为单位（UTC epoch 分钟数）；过期窗口由 cron reconcile 清理。
export async function rateLimit(env: Env, bucket: string, limitPerMinute: number): Promise<void> {
  const windowStart = Math.floor(Date.now() / 60_000);
  const row = await env.DB.prepare(
    "INSERT INTO rate_counters(bucket, window_start, count) VALUES(?, ?, 1) " +
      "ON CONFLICT(bucket, window_start) DO UPDATE SET count = count + 1 " +
      "RETURNING count"
  )
    .bind(bucket, windowStart)
    .first<{ count: number }>();
  if ((row?.count ?? 0) > limitPerMinute) {
    throw new ApiError("rate_limited", "rate limit exceeded, retry later");
  }
}
