// Cloudflare Worker 入口：手写路径路由（14 个端点，不引入框架）。
// 语义权威：contracts/ingest-v1/README.md；错误码与状态映射见 src/errors.ts。

import { ApiError, PROTOCOL_VERSION, errorResponse, json } from "./errors";
import { handlePair } from "./pairing";
import {
  handleCreateRecording,
  handleDeleteRecording,
  handleFinish,
  handleGetRecording,
} from "./recordings";
import { handlePutChunk } from "./chunks";
import { handleAck, handleChunkAudio, handleListRecordings, handleSyncEvents } from "./sync";
import {
  handleAdminPairingCode,
  handleAdminRevokeDevice,
  handleAdminRevokeSyncToken,
  handleAdminSyncToken,
} from "./admin";
import { runReconcile } from "./reconcile";

export interface Env {
  DB: D1Database;
  AUDIO: R2Bucket;
  ADMIN_TOKEN?: string;
  PAIRING_PEPPER?: string;
  EVENT_RETENTION_DAYS?: string;
  ENABLE_DEBUG_FAULTS?: string;
}

function notFound(): Response {
  return new Response("not found", { status: 404 });
}

async function route(request: Request, env: Env): Promise<Response> {
  const url = new URL(request.url);
  const path = url.pathname;
  const method = request.method;

  if (path === "/healthz" && method === "GET") {
    return json({ status: "ok", version: PROTOCOL_VERSION, now: new Date().toISOString() });
  }

  if (!path.startsWith("/v1/")) return notFound();

  // 契约 §1：所有 /v1/* 请求必须带 X-Ingest-Version: ingest-v1。
  if (request.headers.get("x-ingest-version") !== PROTOCOL_VERSION) {
    throw new ApiError("unsupported_protocol_version", `X-Ingest-Version header must be "${PROTOCOL_VERSION}"`);
  }

  let match: RegExpExecArray | null;

  if (path === "/v1/devices/pair" && method === "POST") return handlePair(request, env);
  if (path === "/v1/recordings" && method === "POST") return handleCreateRecording(request, env);

  if ((match = /^\/v1\/recordings\/([^/]+)$/.exec(path))) {
    if (method === "GET") return handleGetRecording(request, env, match[1]!);
    if (method === "DELETE") return handleDeleteRecording(request, env, match[1]!);
    return notFound();
  }
  if ((match = /^\/v1\/recordings\/([^/]+)\/chunks\/(\d+)$/.exec(path)) && method === "PUT") {
    return handlePutChunk(request, env, match[1]!, match[2]!);
  }
  if ((match = /^\/v1\/recordings\/([^/]+)\/chunks\/(\d+)\/audio$/.exec(path)) && method === "GET") {
    return handleChunkAudio(request, env, match[1]!, match[2]!);
  }
  if ((match = /^\/v1\/recordings\/([^/]+)\/finish$/.exec(path)) && method === "POST") {
    return handleFinish(request, env, match[1]!);
  }

  if (path === "/v1/sync" && method === "GET") return handleSyncEvents(request, env);
  if (path === "/v1/sync/recordings" && method === "GET") return handleListRecordings(request, env);
  if ((match = /^\/v1\/sync\/recordings\/([^/]+)\/ack$/.exec(path)) && method === "POST") {
    return handleAck(request, env, match[1]!);
  }

  if (path === "/v1/admin/pairing-codes" && method === "POST") return handleAdminPairingCode(request, env);
  if (path === "/v1/admin/sync-tokens" && method === "POST") return handleAdminSyncToken(request, env);
  if ((match = /^\/v1\/admin\/devices\/([^/]+)\/revoke$/.exec(path)) && method === "POST") {
    return handleAdminRevokeDevice(request, env, match[1]!);
  }
  if ((match = /^\/v1\/admin\/sync-tokens\/([^/]+)\/revoke$/.exec(path)) && method === "POST") {
    return handleAdminRevokeSyncToken(request, env, match[1]!);
  }

  return notFound();
}

export default {
  async fetch(request: Request, env: Env, _ctx: ExecutionContext): Promise<Response> {
    try {
      return await route(request, env);
    } catch (err) {
      return errorResponse(err);
    }
  },
  async scheduled(_controller: ScheduledController, env: Env, _ctx: ExecutionContext): Promise<void> {
    await runReconcile(env);
  },
} satisfies ExportedHandler<Env>;
