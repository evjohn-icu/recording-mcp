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

首次启动顺序：`init` 创建或校验 profile（失败则不启动 web/worker），然后 `web` 与 `worker` 并行启动。浏览器打开 <http://127.0.0.1:8788>，选择 profile 并输入密码。

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

```sh
docker run --rm -v recording-mcp-data:/data:ro -v "$PWD":/backup \
  debian:bookworm-slim tar czf /backup/recording-mcp-backup.tgz -C /data .
```

恢复到一个新卷：

```sh
docker volume create recording-mcp-restore
docker run --rm -v recording-mcp-restore:/data -v "$PWD":/backup \
  debian:bookworm-slim tar xzf /backup/recording-mcp-backup.tgz -C /data
```

校验：对备份前后卷内文件做 `find ... -exec sha256sum` 汇总，哈希应一致（见 `deployment-verification.md`）。

## 升级

1. `git pull` 后 `docker compose build`；
2. 迁移在启动时自动执行（v20 worker 能力列、v21 `recordings.source_bytes`），均为新增列；
3. `docker compose up -d`；`init` 会校验现有 profile 密码；
4. 若启动失败，保留日志 `docker compose logs web worker`，不要删除数据卷。

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
| 上传 503 `import_disabled` | 启用 profile 模式后重启；非容器模式传 `--asr-provider` |
| 数据库 `SQLITE_BUSY` | 确认进程使用 WAL 与 `_txlock=immediate`；不要用只读卷 |
