# Cloudflare 采集服务部署（apps/cloud-ingest）

ingest-v1 服务：Cloudflare Workers + D1（元数据/事件日志）+ R2（PCM 音频）。契约见 [contracts/ingest-v1/README.md](../contracts/ingest-v1/README.md)；本文件只讲部署与运维。

## 本地开发（wrangler miniflare，无需 Cloudflare 账号）

```sh
cd apps/cloud-ingest
npm ci
npx wrangler d1 migrations apply recording-mcp-ingest --local   # 0001_init.sql
cp .dev.vars.example .dev.vars    # ADMIN_TOKEN / PAIRING_PEPPER 本地值
npx wrangler dev --port 8787      # 本地模式；--persist-to <dir> 可隔离状态
curl -s localhost:8787/healthz    # {"status":"ok","version":"ingest-v1",...}
```

本地测试：`npm test`（vitest + @cloudflare/vitest-pool-workers，69 个用例，跑在 workerd 内直连本地 D1/R2）。

## 生产部署

```sh
npx wrangler d1 create recording-mcp-ingest     # 把返回的 database_id 填入 wrangler.toml
npx wrangler r2 bucket create recording-mcp-audio
npx wrangler d1 migrations apply recording-mcp-ingest --remote
npx wrangler secret put ADMIN_TOKEN             # 管理面 Bearer（未设置时管理路由一律 404）
npx wrangler secret put PAIRING_PEPPER          # 配对码哈希盐：sha256(pepper + ":" + code)
npx wrangler deploy                             # cron "17 3 * * *" 随部署生效
```

首次使用：`POST /v1/admin/sync-tokens` 签发 sync token（明文仅显示一次），`POST /v1/admin/pairing-codes` 签发设备配对码（TTL ≤ 3600 s，一次性）。撤销：`POST /v1/admin/devices/{id}/revoke` / `POST /v1/admin/sync-tokens/{id}/revoke`，撤销后请求返回 `401 token_revoked`。

## 运行参数

| 项 | 默认 | 调整方式 |
| --- | --- | --- |
| 事件保留 | 30 天 | `[vars] EVENT_RETENTION_DAYS`；cron 每日清理 |
| chunk 上传限速 | 300 次/分/设备 | 代码常量（D1 固定窗口 rate_counters） |
| JSON 端点限速 | 120 次/分/token | 同上 |
| pair / admin | 10 / 60 次每分 | 同上 |
| 单块上限 | 5 MiB；JSON 64 KiB | `413 payload_too_large` |
| 故障注入 | 关 | `[vars] ENABLE_DEBUG_FAULTS`，**生产必须为 "0"** |

## 备份与恢复

- D1：`npx wrangler d1 export recording-mcp-ingest --remote --output backup.sql`（建议每日，配合 cron）。
- R2：对象键 `owners/{owner}/recordings/{recording_id}/chunks/{seq:08d}.pcm`，可用 R2 提供的生命周期/复制规则做异地副本。
- 事件日志过期后消费端游标失效：`GET /v1/sync` 返回 `410 cursor_expired + min_available_cursor`，消费端走 `GET /v1/sync/recordings` 全量快照重建（`context-sync` 已内置该恢复路径）。
- 孤儿对象（R2 已写、D1 未落）由 cron 清扫（上传 > 24 h 且无 manifest 行）。

## 删除与隐私边界

- `DELETE /v1/recordings/{id}`：云端 R2 分块对象尽力删除 + tombstone 事件 + 下载 `404`；幂等。
- 消费端（`context-sync`）收到 `recording.deleted` 后删除本地音频副本与分块记录。
- **已派生的记忆数据（turns/笔记/嵌入）不随录音删除回溯清除**——审计边界，v1 明确不做级联遗忘。
- 日志不记录 token、配对码与音频内容。

## 状态一致性说明

chunk 提交顺序：R2 put → `db.batch()` 原子写 manifest + 事件 → 200 durable ACK。200 只在三者全部落定后返回；任何中断客户端原样重试：同 sha 命中 `duplicate:true`，异 sha `409 conflict_checksum`。finish 校验 `seq 0..total_chunks-1` 无缺口 + `SUM(sample_count)` + offset 链连续（末块豁免 offset 校验），缺口永不 complete。
