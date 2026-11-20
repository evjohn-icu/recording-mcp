# Local-first Beta 部署指南

本文记录已验证的容器与本地部署步骤。所有命令在仓库根目录执行。

## 前置条件

- Docker 29.x（含 compose 插件）
- 2 GiB 可用内存；首次构建需要能拉取 `golang:1.27.1-bookworm`、`node:22-bookworm-slim`、`debian:bookworm-slim`
- 宿主端口 `127.0.0.1:8788` 空闲

## 配置

复制 `.env.example` 为 `.env` 并至少设置：

```sh
RECORDING_PROFILE=home
MEMORY_PROFILE_PASSWORD=<至少 12 个字符的本地密码>
WEB_PORT=8788                # 宿主发布端口（可选；容器内固定 8788）
ASR_PROVIDER=off            # off | fixture | doubao
GENERATOR_PROVIDER=off
EMBED_PROVIDER=hash
VOLC_API_KEY=               # 仅 ASR_PROVIDER=doubao 时需要
```

`.env` 不提交。密码只通过环境变量进入容器，不写入镜像。

## 启动

```sh
docker compose up -d --build
docker compose ps
curl -fsS http://127.0.0.1:8788/healthz
```

首次启动顺序：`init` 创建或校验 profile（失败则不启动 web/worker）；`web` 先启动，健康检查通过后
`worker` 才启动。浏览器打开 <http://127.0.0.1:8788>，选择 profile 并输入密码。

未配置 ASR（`ASR_PROVIDER=off`）时：

- 服务正常启动，`GET /api/capabilities` 返回 `asr.available=false`、`reason=asr_not_configured`；
- 上传接口返回 409 `asr_unavailable` 与下一步 action，不会静默排队；
- 已有录音的浏览、搜索与决策轨迹仍可用。

## 停止与重启

```sh
docker compose stop          # 保留数据卷
docker compose restart web worker
docker compose down          # 保留命名卷 recording-mcp-data
docker compose down -v       # 删除数据卷（不可恢复）
```

## 备份

备份只允许在**停写**状态进行。`context-backup` 在拷贝前后比较数据库、WAL/SHM 与受管音频的完整文件清单和
hash，任何变化都会以 `writers_active` 失败且不发布归档；归档记录 `backup-archive-manifest-v1`（profile、
相对路径、大小、hash、schema、音频引用状态）。原生备份/恢复必须显式传入 `--confirm-writers-stopped`，
它只是操作员断言，工具不会声称能阻止未参与加锁的进程。

归档前还有两项硬性前置：每个 profile 的永久遗忘清理必须已完成（存在未完成 tombstone 或待清理文件时返回
`forget_unfinished`），且受管音频引用必须完整（缺文件、越界或 chunk hash 不符会直接失败）。归档输出必须在
profiles root 之外，已有同名归档会以 `archive_exists` 拒绝。脚本对不存在的 `--out` 目录按 `01777` 创建，
因为备份容器以 UID 10001 运行，无法写入 `0700` 的宿主目录；复用已有目录时请保证该用户可写。

Docker（推荐，使用 `DATA_VOLUME`）：

```sh
python3 scripts/backup-profile-volume.py backup \
  --data-volume "${DATA_VOLUME:-recording-mcp-data}" \
  --out ./backups --compose-project recording-mcp
```

脚本先 `docker compose -p recording-mcp stop`（失败即中止），再枚举仍挂载该卷的 running 容器；
存在其它 writer 时拒绝继续，不会替用户停止它。

原生安装：

```sh
bin/context-backup backup --profiles-root /srv/recording-mcp/profiles \
  --out /srv/backups/context-engine-$(date +%Y%m%d).tgz --confirm-writers-stopped
```

## 恢复

恢复需要一份**停写且包含最新永久遗忘**的受信 live root 作为屏障来源。归档缺少其中任一屏障时返回
`backup_predates_forget`；profile 不匹配、来源缺失、来源未完成遗忘或屏障在恢复期间变化时返回
`forget_barrier_unverifiable`。没有存活可信 root 的灾难恢复不受支持——工具无法仅凭两个静态副本证明
哪一份更新，不提供绕过开关。

```sh
docker volume create recording-mcp-restore
python3 scripts/backup-profile-volume.py restore \
  --archive ./backups/context-engine-20261113.tgz \
  --data-volume recording-mcp-restore \
  --barrier-source-volume "${DATA_VOLUME:-recording-mcp-data}" \
  --compose-project recording-mcp
python3 scripts/backup-profile-volume.py verify --data-volume recording-mcp-restore
```

原生安装的 `context-backup restore` 需要 `--archive`、`--dest`、`--barrier-source` 与
`--confirm-writers-stopped`；`--dest` 必须为空或不存在，且不能与屏障来源互相包含：

```sh
mv /srv/recording-mcp/profiles /srv/recording-mcp/profiles.stopped
bin/context-backup restore \
  --archive /srv/backups/context-engine-20261113.tgz \
  --dest /srv/recording-mcp/profiles \
  --barrier-source /srv/recording-mcp/profiles.stopped \
  --confirm-writers-stopped
bin/context-backup verify --profiles-root /srv/recording-mcp/profiles
```

恢复会重放 WAL，并对每个 profile 的 `db/context-memory.db` 执行 `integrity_check`（唯一 `ok`）与
`foreign_key_check`，同时检查所有可用音频引用：缺文件、越界、chunk hash 不符都会失败；已声明 deleted 的
录音不算缺失。事件平面（`plane/events.db`）与 profile 其余文件随归档一起恢复，但工具只对上述 profile
数据库做完整性校验。

用**新的 compose project、新端口、新 env 文件**启动恢复栈，并把 `DATA_VOLUME` 指向恢复卷：

```sh
DATA_VOLUME=recording-mcp-restore WEB_PORT=8790 docker compose -p recording-mcp-restored \
  --env-file restore.env up -d
```

之后不要再用旧 env 重启原栈，否则会写回未恢复的卷。

`restore.env` 需至少包含 `MEMORY_PROFILE_PASSWORD`（与原 profile 相同，否则 `init` 校验失败）、
`RECORDING_PROFILE`、`WEB_PORT` 与 `DATA_VOLUME`；缺少密码时 compose 会直接退出。

原生恢复：`--dest` 是新 root。SQLite 会正确恢复，但受管音频以绝对路径存于 `sync_recordings`，因此原生
恢复应把 `--dest` 设回原 profiles 绝对路径（先整体移走旧目录），否则播放会报告音频缺失；Docker 卷始终
挂载在 `/data`，不存在该问题。备份工具校验音频引用时会按当前 audio root 重锚定路径，`verify` 因此可能
通过；但应用播放端只接受 audio 目录内的原始绝对路径，`verify` 通过不等于播放可用。

## 升级

1. 先按[备份](#备份)的停写步骤做一次 `context-backup` 归档；
2. `git pull` 后 `docker compose build`；
3. 迁移在启动时自动执行：v20 worker 能力列、v21 `recordings.source_bytes` 与 v22 ASR 进度/GPU 心跳列是
   新增列；v23 新建 audio analysis 表并补挂退役 fence，不是加列；
4. `docker compose up -d`；`init` 会校验现有 profile 密码；
5. 若启动失败，保留日志 `docker compose logs web worker`，不要删除数据卷。

## 安全边界

- 容器以 UID/GID `10001` 运行，`read_only` 根文件系统 + `/tmp` tmpfs；
- `cap_drop: ALL`、`no-new-privileges`；
- 宿主只发布 `127.0.0.1:8788`；容器内监听 `0.0.0.0:8788` 需要显式 `--container-bind`；
- worker 健康端点只在容器内 `127.0.0.1:8789`，不发布；
- 数据卷 `recording-mcp-data` 保存 profile、数据库、事件平面与音频。

## 排障

| 症状 | 检查 |
|---|---|
| `init` 失败 `MEMORY_PROFILE_PASSWORD is required` | `.env` 未设置密码 |
| `init` 失败 `verification failed` | 现有 profile 密码与 `.env` 不一致 |
| web 健康检查失败 | `docker compose logs web`；确认 `/data` 可写 |
| worker 健康检查失败 | `docker compose logs worker`；心跳超过 90s 视为过期 |
| 上传 409 `asr_unavailable` | 设置 `ASR_PROVIDER` 并重启 web 与 worker |
| 上传 503 `import_disabled` | web 启动时没有导入器；按响应的 action 以 `--asr-provider` 和音频目录重启 context-web |
| 数据库 `SQLITE_BUSY` | 确认进程使用 WAL 与 `_txlock=immediate`；不要用只读卷 |
