# asr-health(ASR 服务健康判别)

`scripts/asr-health.sh`:asr-ingest 批量失败时,30 秒定位是哪一层的问题,避免盲等/盲试。
2026-10 实测背景:一次 8h+ 的 `init ack timeout` 全拒,判别脚本证明 DNS/TCP/TLS 全通、
是服务端配额拒绝——与历史上真正的 DNS 超时故障(解析器 i/o timeout)是两回事。

## 三层判别

| 层 | 方法 | 结论 |
|---|---|---|
| DNS | `getent hosts openspeech.bytedance.com` | 失败 → `DNS_FAIL`(WSL NAT 解析器问题,换 DNS/重试) |
| TCP/TLS | `curl https://openspeech.bytedance.com` | 失败 → `NETWORK_FAIL`(代理/防火墙) |
| WS 会话 | 2s 正弦波走 `bin/asr-ingest`(无 `--db`,一次性 out 目录) | 链路通但会话不成 → `SERVICE_REJECT`(配额/限流,查火山控制台) |

## 用法

```sh
bash scripts/asr-health.sh --key "$VOLC_API_KEY"          # 单次判别,exit 0=OK 1=SERVICE_REJECT 2=网络层
bash scripts/asr-health.sh --key "$VOLC_API_KEY" --wait   # 每 5 分钟探针,恢复即 exit 0(默认上限 6h)
bash scripts/asr-health.sh --key "$KEY" --wait --wait-hours 12 && ./bin/asr-ingest ...  # 恢复后自动补跑
```

Key 来源:`--key` 或环境变量 `VOLC_API_KEY`(与 asr-ingest 相同变量,勿落盘)。
