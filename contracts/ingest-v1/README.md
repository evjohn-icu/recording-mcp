# ingest-v1 契约（冻结）

录音采集端的 HTTPS 分块上传协议与记忆消费端的事件同步协议。本目录是语义权威：

- `openapi.yaml` — OpenAPI 3.1 机读描述（路径、请求/响应 schema、错误码）。
- `vectors/*.json` — 由 `go run ./cmd/ingest-vectors` 生成的跨语言确定性测试向量；Go（`internal/cloudsync`）、TypeScript（`apps/cloud-ingest`）、Kotlin（`apps/android`）三侧测试都以它为基准做漂移校验。

修改本契约必须：更新 `openapi.yaml` 与本 README → 重新生成 `vectors/` → 同步三侧实现与测试。破坏性变更必须升版本号（`ingest-v2`），不得原地改语义。

## 1. 传输基线

- 控制面为 HTTPS + JSON；音频块体为 `application/octet-stream` 原始 PCM 字节。
- 所有 `/v1/*` 请求必须带 `X-Ingest-Version: ingest-v1`，缺失或不匹配 → `400 unsupported_protocol_version`。JSON 体内另含 `protocol_version: "ingest-v1"` 字段（双重校验）。
- 认证：`Authorization: Bearer <token>`。三类凭据：设备 token（`role=device`）、同步 token（`role=sync`，只读事件 + 音频下载 + ack）、管理 token（env `ADMIN_TOKEN`，未配置时管理路由一律 `404`）。
- 错误体固定：`{"error":{"code":"<code>","message":"<msg>","details":{...}}}`。
- 时间均为 RFC3339 UTC。未知时间一律 `null`，不得伪造。

## 2. ID 与格式（冻结）

| 字段 | 约束 |
| --- | --- |
| `device_id` | 服务端生成，`dev_<uuidv4>` |
| `recording_id` | 客户端 UUIDv4 字符串 |
| chunk 身份 | `(recording_id, seq)`，`seq ∈ [0, 9999999]` |
| `codec` | 固定 `pcm_s16le` |
| `sample_rate` / `channels` | 固定 `16000` / `1`（mono） |
| 块时长 | 1000–90000 ms |
| `content_sha256` | 64 位小写 hex（PCM 字节 SHA-256） |
| 单块上限 | ≤ 5 MiB（60 s = 1,920,000 B） |
| JSON 体上限 | ≤ 64 KiB |
| 单录音上限 | ≤ 720 块 / 6 小时 |
| owner | 服务端从已认证凭据派生，客户端不得传 |

音频本体是裸 PCM S16LE / 16 kHz / mono，无容器、无头，按 `seq` 顺序字节拼接即为完整录音。

## 3. 错误码（冻结字面量）

| code | HTTP | 语义 |
| --- | --- | --- |
| `unsupported_protocol_version` | 400 | `X-Ingest-Version` 缺失/不匹配 |
| `validation_failed` | 400 | 字段/格式/校验和不符 |
| `unauthorized` | 401 | 缺凭据或凭据无效 |
| `token_revoked` | 401 | 凭据已被撤销 |
| `forbidden_scope` | 403 | 角色无权执行该操作 |
| `pairing_code_invalid` | 400 | 配对码不存在或已使用 |
| `pairing_code_expired` | 400 | 配对码已过期 |
| `recording_not_found` | 404 | 不存在或不属于该 owner（不区分存在性） |
| `conflict_payload` | 409 | 同 ID 不同语义内容 |
| `conflict_checksum` | 409 | 同 chunk 身份不同字节 |
| `sequence_gap` | 409 | finish 时存在缺失序列 |
| `cursor_expired` | 410 | 同步游标超出事件保留窗口 |
| `payload_too_large` | 413 | 体超限 |
| `rate_limited` | 429 | 超出固定窗口限速 |
| `internal` | 500 | 服务端错误；客户端必须原样重试（重试恒安全） |

## 4. 端点语义

1. `GET /healthz`（无认证）→ `200 {"status":"ok","version":"ingest-v1","now"}`。
2. `POST /v1/devices/pair`（无认证，凭配对码，限速 10 次/分钟/IP）→ 换取 `device_id` + `device_token`。配对码一次性：已用/不存在 → `pairing_code_invalid`，过期 → `pairing_code_expired`。
3. `POST /v1/recordings`（device）→ 新建 `201`；等价重放 `200`（`"duplicate":true`）；同 `recording_id` 不同字段 → `409 conflict_payload`。首次成功写 `recording.created` 事件。
4. `PUT /v1/recordings/{id}/chunks/{seq}`（device）：元数据在请求头 `X-Chunk-Meta`（base64url(JSON)，JSON 字段 `sample_offset`/`sample_count`/`duration_ms`/`content_sha256`/`recorded_at`），体为原始 PCM。服务端校验：体字节数 == `sample_count*2`；`duration_ms` 与 `sample_count/16` 偏差 ≤ 20 ms；body SHA-256 == `content_sha256`。持久化顺序：R2 put `owners/{owner}/recordings/{recording_id}/chunks/{seq:08d}.pcm` → D1 `batch()` 原子写 chunk manifest 行 + `chunk.committed` 事件行 → 返回 `200 {"seq","state":"committed","r2_key","checksum","durable":true}`。**200 仅在三者全部落定后返回（durable ACK）。** 同 seq 同 sha 重放 → `200` + `"duplicate":true`（不发新事件）；同 seq 异 sha → `409 conflict_checksum`。乱序/重放均允许。
5. `POST /v1/recordings/{id}/finish`（device）：body `{protocol_version,total_chunks,total_samples,total_duration_ms}`。校验 manifest 行数 == `total_chunks`、seq 恰为 `0..total_chunks-1`、`SUM(sample_count)==total_samples`、offset 链连续（`offset[0]==0` 且 `offset[i]==offset[i-1]+count[i-1]`）。缺口 → `409 sequence_gap` + `details.missing_sequences`；总量/偏移不符 → `409 conflict_payload` + details。成功置 `state='complete'` + `recording.finished` 事件；同 body 重放 → `200` 不重复发事件。**缺失序列永远不得 complete。**
6. `GET /v1/recordings/{id}`（device 或 sync）→ 录音详情 + chunk 清单 + `sync:{synced_at,transcription_state}`。跨 owner → `404`。
7. `GET /v1/sync?cursor&limit`（sync）：事件严格 `id > cursor` 升序；`next_cursor` = 最后返回 id（空页 = cursor）；`max_event_id` = 当前最大事件 id。事件保留期内 `cursor < MIN(id)-1` → `410 cursor_expired` + `details.min_available_cursor`。事件类型与 payload：
   - `recording.created{codec,sample_rate,channels,recorded_at,timezone}`
   - `chunk.committed{seq,sample_offset,sample_count,bytes,duration_ms,content_sha256,r2_key}`
   - `recording.finished{total_chunks,total_samples,total_duration_ms}`
   - `recording.deleted{reason}`
   - `transcript.available{segment_id,transcript_revision}`（v1 保留类型，无产出方）
8. `GET /v1/recordings/{id}/chunks/{seq}/audio`（sync）→ 原始 PCM，`Content-Type: audio/L16`，`X-Content-SHA256`。
9. `GET /v1/sync/recordings?after&limit`（sync）→ 全量快照分页（按 `created_at,recording_id` 升序），游标过期后的恢复通道。
10. `POST /v1/sync/recordings/{id}/ack`（sync）→ 回填 `synced_at` 与 `transcription_state`（`pending|ingested|failed|skipped`），设备经端点 6 轮询读取。
11. `DELETE /v1/recordings/{id}`（device）→ 置 `deleted`、尽力删 R2 分块对象、发 `recording.deleted`；幂等。删除后音频下载返回 `404`。
12. 管理面（仅 `ADMIN_TOKEN`）：`POST /v1/admin/pairing-codes`、`POST /v1/admin/sync-tokens`、`POST /v1/admin/devices/{id}/revoke`、`POST /v1/admin/sync-tokens/{id}/revoke`。

## 5. 幂等矩阵（验收基准）

| 场景 | 结果 |
| --- | --- |
| 同 ID + 同数据（任意端点重放） | 成功，不产生重复记录/事件 |
| 同 ID + 不同数据 | 冲突：chunk → `conflict_checksum`；recording/finish → `conflict_payload` |
| 缺失序列 | 永远不得 complete |
| 重复/乱序上传 | 允许且幂等 |
| durable ACK | 仅当 R2 对象 + manifest 行 + 事件行三者全部持久化后才返回 200 |
| R2 写成功 + D1 失败 | 客户端重试：同 R2 key 覆盖写幂等，随后补 D1 → committed |
| D1 提交成功 + 响应丢失 | 客户端重试 → `duplicate:true`，无新事件 |
| 删除后旧事件重放 | 事件唯一索引阻止重复插入；音频读侧 `404` |
| 已删除 recording 的后续操作 | `PUT chunk` / `finish` / 音频下载 → `404 recording_not_found`；`POST /v1/recordings` 同元数据重放 → `200 duplicate:true` 且 `state=deleted`（不复活、不产生新事件）；详情返回 tombstone（`state=deleted`） |

持久化证据只认 R2 对象 + D1 行；内存、WebSocket、队列状态一律不算（本版本不使用 Queues/Durable Objects）。

## 6. 限速与配额（固定窗口，D1 计数）

| 桶 | 限制 |
| --- | --- |
| chunk PUT | 300 次/分钟/设备 |
| 其余 JSON 端点 | 120 次/分钟/token |
| pair | 10 次/分钟/IP |
| admin | 60 次/分钟 |

超限 → `429 rate_limited`。

## 7. 安全语义

- token 仅以 SHA-256 哈希存 D1；配对码以 SHA-256(pepper + ":" + code) 存 D1，明文只在创建响应中出现一次。
- 设备凭据只能触达自身 owner 的 recording；sync 凭据只读 + ack；跨 owner 一律 `404 recording_not_found`。
- R2 桶私有，音频仅经认证 Worker 读取；日志不记录 token、配对码与音频内容。
- 开发专用故障注入：请求头 `X-Debug-Fault: r2-after-write|d1-manifest|d1-event|response-drop`，仅 env `ENABLE_DEBUG_FAULTS="1"` 时生效，在指定步骤后返回 `500 internal`（生产必须为 0）。语义：
  - `r2-after-write` — R2 对象已写，D1 未写，500；
  - `d1-manifest` — D1 batch 内 manifest 语句失败（整批回滚），500；
  - `d1-event` — D1 batch 内事件语句失败（整批回滚），500；
  - `response-drop` — 全部持久化成功后响应丢失，500；重试必须命中 `duplicate:true`。

## 8. 测试向量

`vectors/` 全部由 `cmd/ingest-vectors` 确定性生成（固定 UUID、无运行时时间戳）。关键文件：

- `pcm_1s_440hz.json` / `pcm_1s_silence.json` — 16000 采样（1 s）PCM，含 base64、sha256、首尾 8 字节 hex。
- `chunk_meta_ok.json` — seq 0 的 `X-Chunk-Meta` base64url 字面量。
- `put_chunk_ok.json` / `put_chunk_duplicate.json` / `put_chunk_conflict.json` — chunk 上传三种分支。
- `finish_gap.json` / `finish_ok.json` — finish 缺口与成功。
- `sync_page.json` / `cursor_expired.json` — 同步分页与游标过期。
- `error_codes.json` — 错误码 → HTTP 状态全表 + 错误体形状。

漂移校验：`internal/cloudsync/vectors_test.go`（Go，重新生成比对仓库文件）与 `apps/cloud-ingest/test/vectors.test.ts`（TS，解码 base64 重算 sha256 + 错误码表比对）。
