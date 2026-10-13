// cron 回收（wrangler.toml [triggers] 17 3 * * *）：
//   1) 删除超过 EVENT_RETENTION_DAYS 的事件行；
//   2) 删除上传超过 24h 但没有 manifest 行的 R2 孤儿对象（r2-after-write 故障残留）；
//   3) 清理 1h 前的 rate_counters 固定窗口。

import type { Env } from "./index";

const ORPHAN_GRACE_MS = 24 * 60 * 60 * 1000;
const RATE_WINDOW_MINUTES = 60;

export async function runReconcile(env: Env): Promise<void> {
  const now = new Date();

  const parsed = Number.parseInt(env.EVENT_RETENTION_DAYS ?? "30", 10);
  const retentionDays = Number.isFinite(parsed) && parsed > 0 ? parsed : 30;
  const eventCutoff = new Date(now.getTime() - retentionDays * 24 * 60 * 60 * 1000).toISOString();
  await env.DB.prepare("DELETE FROM events WHERE created_at < ?").bind(eventCutoff).run();

  // R2 孤儿：对象存在但 chunks.r2_key 无对应行，且上传时间超过宽限期。
  let cursor: string | undefined = undefined;
  for (;;) {
    const page = await env.AUDIO.list(cursor === undefined ? {} : { cursor });
    const orphans: string[] = [];
    for (const object of page.objects) {
      if (now.getTime() - object.uploaded.getTime() <= ORPHAN_GRACE_MS) continue;
      const row = await env.DB.prepare("SELECT seq FROM chunks WHERE r2_key = ?").bind(object.key).first<{ seq: number }>();
      if (!row) orphans.push(object.key);
    }
    if (orphans.length > 0) await env.AUDIO.delete(orphans);
    if (!page.truncated) break;
    cursor = page.cursor;
  }

  const currentWindow = Math.floor(now.getTime() / 60_000);
  await env.DB.prepare("DELETE FROM rate_counters WHERE window_start < ?").bind(currentWindow - RATE_WINDOW_MINUTES).run();
}
