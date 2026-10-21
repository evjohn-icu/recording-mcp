# 本地 Web 浏览器运行手册

本地 Web 服务是一个默认绑定且只允许 loopback 的浏览器界面，使用 `context-web` 的实际参数：

- `--db`：SQLite memory database；默认 `data/context-memory.db`；
- `--audio-dir`：只允许服务该目录下的 `.pcm` 文件，默认 `data/synced-audio`；
- `--addr`：监听地址，默认 `127.0.0.1:8788`；
- `--web-dir`：已构建的前端目录，默认 `web/dist`；
- `--semantic-provider`：`off`、离线确定性的 `hash`，或需要凭据的 `openai`。

## 合成演示（推荐的首次检查）

```sh
make build
make web
make web-demo                 # 127.0.0.1:8788
# make web-demo PORT=8790     # 换一个 loopback 端口
```

`scripts/web-demo.sh` 只接受一个可选端口参数。它总是在 `/tmp` 下 `mktemp -d`，并把
`context-eval` 指向这个新目录中的数据库；脚本不接受数据库或 data 目录参数，也不会
删除、重置或打开已有 `data/`。`context-eval` 本身会重建传入的 `--db` 路径，因此这
个“只允许新路径”的约束是有意的。脚本会打印：

- `SYNTHETIC DATA ONLY` 标签；
- synthetic SQLite、PCM 目录和 eval report 的绝对路径；
- synthetic fixture 中实际的 `source_attachments` provider `recording_id` 和
  `device_id` 映射；
- `context-web` 的启动日志。

PCM 是 Python 标准库生成的确定性 16 kHz、单声道、s16le 原始 PCM。脚本根据数据库中
每个录音的真实 `global_start_ms`/`global_end_ms` 留出尾部，再写入 `sync_recordings`
的 `stitched_path`；因此演示中的播放 offset 有效。它不是实际录音，也不能证明 ASR
质量。`context-eval` 已经直接完成合成摄入、修订、notes、pipeline、链接和离线 hash
索引，不需要 worker。

启动日志会打印类似下面的随机 bootstrap 地址：

```text
context-web: open http://127.0.0.1:8788/#token=<single-use-token> (single use, 10 min)
```

将完整 URL 粘贴到本机浏览器。token 只交换一次，10 分钟后失效；不要把 token 写入
文档或脚本，也不要用 `--addr 0.0.0.0` 暴露服务。停止演示只会停止该演示进程；脚本
打印的临时目录会保留，便于检查，确认不再需要后可手动删除。

## 使用已有本地库

已有库不要经过 `context-eval`，也不要使用 `make web-demo` 的 synthetic 路径。构建
前端和 Go 二进制后，显式启动 loopback 服务：

```sh
make build
make web
bin/context-web \
  --db data/context-memory.db \
  --audio-dir data/synced-audio \
  --addr 127.0.0.1:8788 \
  --web-dir web/dist \
  --semantic-provider hash
```

`context-web` 启动时使用当前程序内置的 migration runner；本次 Web 演示/文档改动不
引入新的 schema migration，正常启动不应需要人工迁移步骤。若数据库来自很旧的版本，
仍应先备份并让同一版本的程序报告其已有 migration 行为；不要用 synthetic eval 去
“升级”生产库。

worker 是另一个生命周期：真实摄入需要时，在单独终端按实际 generator 配置运行
`bin/context-worker --db data/context-memory.db ...`。启动 Web 不会隐式启动 worker，
合成演示也不需要它。

## 数据与操作边界

### 删除录音只删除音频

`recording.deleted` 的同步事件会删除本地录音分块并把 `sync_recordings.state` 设为
`deleted`。它**只影响音频可用性**：转写 turns、revision 历史、notes、证据和派生
记忆保留。Web 上对应的录音/片段会显示 `deleted`，播放接口返回 410，并提示“录音已
删除（转写与归纳保留）”。删除音频不会抹除审计证据，也不是删除会话或记忆的操作。

### ASR 编辑目前不支持

Web 的转写 tab 是 ASR 原文只读视图；不能在浏览器中直接改字、改 speaker 或覆盖
`turns`。纠错必须通过新的转录 revision 走既有 ingestion 路径，使旧 revision 能够
审计并按 stale 规则处理。Web 支持的是带证据的 note summary 修订，不等于 ASR 编辑。

### 备份：使用 SQLite backup API

不要用 `cp` 在数据库仍有 WAL 写入时复制单个 `.db` 文件。可以先停写入进程，或使用
SQLite online backup API 创建一致副本：

```sh
mkdir -p /absolute/path/backups
python3 - /absolute/path/data/context-memory.db \
  /absolute/path/backups/context-memory-$(date +%Y%m%d-%H%M%S).db <<'PY'
import sqlite3
import sys

source = sqlite3.connect(sys.argv[1])
destination = sqlite3.connect(sys.argv[2])
try:
    with destination:
        source.backup(destination)
finally:
    destination.close()
    source.close()
PY
```

备份文件应有严格的本地权限，并在恢复演练中验证副本能被程序打开；`context-web` 当前没有只读启动模式，
启动时可能运行迁移，不要将此操作当作只读检查。备份不包含云端 R2/D1 的远端生命周期操作；那些部分按
[docs/cloud-deploy.md](cloud-deploy.md) 的流程另行备份。

## 发布、回滚与证据口径

1. 发布前用 SQLite backup API 做带时间戳备份，记录将使用的 binary、`web/dist` 和
   loopback 端口。
2. 先停止旧的 Web 进程，再替换 binary/前端并启动；用 `/healthz` 和随机 bootstrap
   token 做本机检查。不要让新版本覆盖数据库文件。
3. 回滚时停止新进程，恢复旧 binary/前端并重新启动；只有在明确的灾难恢复操作中才
   从已验证备份恢复数据库。回滚不得把空的 synthetic DB 或旧副本直接覆盖当前数据。
4. 记录结果时区分真实证据：合成 eval、合成正弦 PCM 和 loopback UI 检查不能替代
   真实 ASR、真实云端、Android 真机或生产数据验证。没有相应日志/设备/凭据的项目
   必须标为 `NOT_RUN`，不能因为合成演示通过就写成 PASS；例如真实 ASR 准确率、真实
   Cloudflare 部署、真机行为和公网暴露检查均应在没有证据时保持 `NOT_RUN`。


当前未通过项及实际浏览器验证见 [local-web-verification.md](local-web-verification.md)。首次运行可用合成演示检查完整流程。
