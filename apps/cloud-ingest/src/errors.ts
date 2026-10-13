// ingest-v1 契约的错误码表（冻结字面量）与请求体格式校验原语。
// 漂移基准：contracts/ingest-v1/vectors/error_codes.json（apps/cloud-ingest/test/vectors.test.ts 做全表比对）。

export type ErrorCode =
  | "unsupported_protocol_version"
  | "validation_failed"
  | "unauthorized"
  | "token_revoked"
  | "forbidden_scope"
  | "pairing_code_invalid"
  | "pairing_code_expired"
  | "recording_not_found"
  | "conflict_payload"
  | "conflict_checksum"
  | "sequence_gap"
  | "cursor_expired"
  | "payload_too_large"
  | "rate_limited"
  | "internal";

export const ERROR_STATUS: Record<ErrorCode, number> = {
  unsupported_protocol_version: 400,
  validation_failed: 400,
  unauthorized: 401,
  token_revoked: 401,
  forbidden_scope: 403,
  pairing_code_invalid: 400,
  pairing_code_expired: 400,
  recording_not_found: 404,
  conflict_payload: 409,
  conflict_checksum: 409,
  sequence_gap: 409,
  cursor_expired: 410,
  payload_too_large: 413,
  rate_limited: 429,
  internal: 500,
};

export const PROTOCOL_VERSION = "ingest-v1";

export class ApiError extends Error {
  readonly code: ErrorCode;
  readonly details: Record<string, unknown>;

  constructor(code: ErrorCode, message: string, details: Record<string, unknown> = {}) {
    super(message);
    this.name = "ApiError";
    this.code = code;
    this.details = details;
  }
}

export function json(data: unknown, status = 200): Response {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "content-type": "application/json" },
  });
}

export function errorResponse(err: unknown): Response {
  if (err instanceof ApiError) {
    return json({ error: { code: err.code, message: err.message, details: err.details } }, ERROR_STATUS[err.code]);
  }
  return json({ error: { code: "internal", message: "internal server error", details: {} } }, 500);
}

// ---- 冻结格式校验（契约 §2）----

const UUID_V4_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const RFC3339_RE = /^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(\.\d+)?([Zz]|[+-]\d{2}:\d{2})$/;
const SHA256_HEX_RE = /^[0-9a-f]{64}$/;
const BASE32_RE = /^[A-Z2-7]{32}$/;

export function isUuidV4(value: unknown): value is string {
  return typeof value === "string" && UUID_V4_RE.test(value);
}

export function isRfc3339(value: unknown): value is string {
  return typeof value === "string" && RFC3339_RE.test(value) && !Number.isNaN(Date.parse(value));
}

export function isSha256Hex(value: unknown): value is string {
  return typeof value === "string" && SHA256_HEX_RE.test(value);
}

export function isBase32Code(value: unknown): value is string {
  return typeof value === "string" && BASE32_RE.test(value);
}

export function requireProtocolVersion(body: Record<string, unknown>): void {
  if (body.protocol_version !== PROTOCOL_VERSION) {
    throw new ApiError("validation_failed", `protocol_version must be "${PROTOCOL_VERSION}"`);
  }
}

export function intField(body: Record<string, unknown>, name: string, min: number, max: number): number {
  const value = body[name];
  if (typeof value !== "number" || !Number.isInteger(value) || value < min || value > max) {
    throw new ApiError("validation_failed", `${name} must be an integer in [${min}, ${max}]`);
  }
  return value;
}
