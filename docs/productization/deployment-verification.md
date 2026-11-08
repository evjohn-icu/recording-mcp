# 容器部署验证（2026-11-09）

环境：Docker 29.9.2（Docker Desktop / WSL2），本机 `go1.27.3`、`ffmpeg 9.0.2`。镜像 `recording-mcp:beta`。

## 命令与结果

```sh
docker compose --env-file ci.env config            # exit 0
RUN_DOCKER_DEPLOYMENT_TEST=1 python3 -m unittest discover -s scripts -p 'test_productization_deploy.py' -v
# Ran 6 tests in 58.020s — OK
```

6 项检查全部通过：

| 测试 | 结果 | 说明 |
|---|---|---|
| `test_compose_config_is_valid` | PASS | 服务、卷、`no-new-privileges`、发布端口与目标端口存在 |
| `test_clean_start_and_independent_health` | PASS | web `/healthz` 200；worker 容器内 `127.0.0.1:8789/healthz` 200 |
| `test_container_runs_as_non_root` | PASS | web 与 worker `id -u` 均为 `10001` |
| `test_three_restarts_keep_imported_data` | PASS | 通过真实 `POST /api/imports` 导入 1 秒 WAV，连续重启 3 次后会话仍在 |
| `test_startup_without_external_api_reports_unavailable` | PASS | `ASR_PROVIDER=off` 时服务启动且健康 |
| `test_backup_restore_preserves_sources` | PASS | 停止写入后备份卷、恢复到新卷，文件哈希一致 |

构建耗时：首次约 104 秒（含拉取基础镜像），后续增量约 60 秒。首次 `up` 到登录可用约 6 秒（init 完成 + web 健康）。

## 关键实现细节

- web/worker 分离进程，共享命名卷；卷名由 `DATA_VOLUME` 控制，测试与 CI 使用一次性名称，不接触 `recording-mcp-data`。
- 宿主端口由 `WEB_PORT` 控制（默认 8788）。本机 Docker Desktop 对 `127.0.0.1:8788` 的旧 forward 曾返回 `500 /forwards/expose`；测试改用动态端口后稳定通过。用户环境若遇到同样错误，重启 Docker Desktop 或换 `WEB_PORT`。
- 非 root UID 10001；根文件系统只读，`/tmp` 为 256 MB tmpfs；`cap_drop: ALL`。
- 无外部 API 启动不会崩溃：能力接口报告 ASR 不可用。

## 未执行项（NOT_RUN）

- 真实 doubao ASR 的容器内调用：无凭据，未授权付费调用。
- 真实 embedding/reranker：同上。
- 长时间（小时级）容器运行与磁盘压力：未做 soak 测试。
