// 开发专用故障注入（契约 §7）：仅 env.ENABLE_DEBUG_FAULTS === "1" 时生效。
// 语义：
//   r2-after-write — R2 对象已写，D1 未写，返回 500；
//   d1-manifest    — D1 batch 内 manifest 语句失败（整批回滚），500；
//   d1-event       — D1 batch 内事件语句失败（整批回滚），500；
//   response-drop  — 全部持久化成功后响应丢失，500（重试必须命中 duplicate:true）。

import type { Env } from "./index";

export type DebugFault = "r2-after-write" | "d1-manifest" | "d1-event" | "response-drop";

const KNOWN_FAULTS: readonly string[] = ["r2-after-write", "d1-manifest", "d1-event", "response-drop"];

export function debugFault(request: Request, env: Env): DebugFault | null {
  if (env.ENABLE_DEBUG_FAULTS !== "1") return null;
  const value = request.headers.get("x-debug-fault");
  if (!value) return null;
  return KNOWN_FAULTS.includes(value) ? (value as DebugFault) : null;
}

// 故障注入用语句：指向不存在的表，D1 batch 必然整体回滚。
export const FAULT_FAILING_SQL = "INSERT INTO debug_fault_no_such_table (x) VALUES (1)";
