# 真实中文 embedding 评估（SMART-03）

## 模型与运行环境

| 项 | 值 |
|---|---|
| Embedding | `Qwen/Qwen3-Embedding-0.6B` @ `97b0c614be4d77ee51c0cef4e5f07c00f9eb65b3`，`model.safetensors` SHA-256 校验通过，1024 维，CPU float32 |
| 评估器 | `chaoliangUNSW/Jev-Style-0.8B-Decision-v3` @ `b023d1f9c7858fbf01504577a3bfc349ea5c7385`，`model.safetensors` SHA-256 校验通过，cuda float16，加载 6.6 s |
| 量化 | 无（float32 CPU embedding / float16 GPU assessor） |
| 硬件 | RTX 2070 8192 MiB，driver 610.88；WSL2 |
| 二进制 | `bin/context-eval`（sha256 记录在 manifest） |
| 冻结协议 | `fixtures/eval/quality-v2-r3/`，`--max-items 10 --max-bytes 65536`，test 未触碰 |
| 服务 | `scripts/local-embed.py`（127.0.0.1:8790）、`scripts/decision-local-server.py`（127.0.0.1:8791） |

评估器运行时需要支持 `qwen3_5` 的 transformers。`.venv-asr` 固定 4.57.6（ASR 依赖），因此本轮把 `transformers 5.22.2` 及兼容的 `huggingface_hub/tokenizers/safetensors/httpx2/httpcore2/truststore/certifi` 安装到独立目录 `$TMPDIR/jev-tf`，用 `PYTHONPATH` 只影响评估器进程；未改动 `.venv-asr`。这是运行环境差异，不是模型版本差异。

命令：

```sh
.venv-asr/bin/python scripts/local-embed.py --serve --host 127.0.0.1 --port 8790 \
  --model Qwen/Qwen3-Embedding-0.6B --revision 97b0c614be4d77ee51c0cef4e5f07c00f9eb65b3 \
  --device cpu --dtype auto --preload

PYTHONPATH="$TMPDIR/jev-tf" .venv-asr/bin/python scripts/decision-local-server.py \
  --model-dir "$HOME/.cache/huggingface/hub/models--chaoliangUNSW--Jev-Style-0.8B-Decision-v3/snapshots/b023d1f9c7858fbf01504577a3bfc349ea5c7385" \
  --addr 127.0.0.1:8791 --device cuda --dtype float16

python3 scripts/run-retrieval-quality-v2.py --binary bin/context-eval \
  --out docs/evals/sprint3-real-embedding \
  --embed-endpoint http://127.0.0.1:8790/v1/embeddings \
  --embed-model Qwen/Qwen3-Embedding-0.6B --embed-dims 1024 --embed-input-type \
  --assessor-url http://127.0.0.1:8791/v1/systemone --assessor-model decision-v3-0.8b
```

`--embed-input-type` 仅对内置 Qwen 服务发送 `input_type`（query 指令）；默认不发送，避免严格 OpenAI 兼容端点因未知字段返回 400。

## 结果

| 配置（calibration） | Recall@5 | MRR@10 | p95 (ms) | 无证据误报 | 假充分 | embedding 调用 | 模型调用 | 全门禁 |
|---|---|---|---|---|---|---|---|---|
| v1 lexical | 0.0000 | 0.0000 | 3.0 | 2/20 | 0/0 | 0 | 0 | None |
| v2 lexical | 0.8750 | 0.3721 | 323.7 | 19/20 | 1/24 | 0 | 59 | False |
| v1 hash | 0.1500 | 0.1052 | 19.0 | 19/20 | 0/0 | 68 | 0 | None |
| v2 hash | 0.1625 | 0.0784 | 335.2 | 19/20 | 3/51 | 68 | 59 | False |
| v1 真实 embedding | 0.8500 | 0.2679 | 262.3 | 19/20 | 0/0 | 68 | 0 | None |
| v2 真实 embedding + 充分性 | 0.7125 | 0.2278 | 590.4 | 19/20 | 1/24 | 68 | 59 | False |

校准集共 60 个检索问题，其中 20 个无证据问题。真实 embedding 使用 CPU（float32）；v2 的每次查询调用一次评估器（59 次成功，0 次失败）；v1 配置 0 次评估器调用。embedding 调用只出现在 hash/openai 配置（68 次），lexical 配置为 0。

**没有任何 v2 候选通过全部门禁**，因此未创建 `selection-lock.json`，未运行 holdout，`quality_gate_passed=false`、`default_changed=false`、`test_touched=false`。

失败检查：

- `false_positive_rate_at_most_0_05`：所有 hybrid 配置误报 19/20 无证据问题（lexical 仅 2/20）。
- `p95_at_most_1_5x_same_provider_v1`：v2 加入充分性评估后 p95 明显高于同 provider 的 v1（v2-openai 590 ms vs v1-openai 262 ms）。
- `recall_at_5_strictly_improves` / `mrr_at_10_not_regressed`：v2-openai（0.7125 / 0.2278）低于 v1-openai（0.8500 / 0.2679）；v2-hash 的 MRR 也从 0.1052 降到 0.0784。
- v2-hash 另失败 `false_sufficient_rate_at_most_0_05`（3/51）。

泄漏与引用检查全部为 0（stale/ambient/scope/跨 split/invalid refs），失败计数为 0。

**先前一轮评测作废**：该轮 `local-embed.py` 在左填充批次里用 `attention_mask.sum()-1` 取池化位置，短文档实际池化的是第一个真实 token。修正为 `hidden[:, -1]` 后重跑，本文所有数字来自修正后的 manifest；作废轮次不引用。

CPU embedding 的 p95 失败不改变结论：v2-openai 同时失败召回与 MRR，换 GPU 也不能通过，因此未执行 `EMBED_DEVICE=cuda:0` 重跑（`NOT_RUN`）。

索引构建时间/内存未单独测量；manifest 记录每配置的 embedding/模型调用与失败计数。

## 默认值决定

默认保持 **v1 + hash**：本轮真实 embedding 未通过冻结门禁，且质量低于同 provider 的 v1。真实 embedding 只作为可选路径记录：

```sh
# 先启动本地 embedding 服务（CPU 默认），再设置 provider
EMBED_PROVIDER=local EMBED_ENDPOINT=http://127.0.0.1:8790/v1/embeddings \
EMBED_MODEL=Qwen/Qwen3-Embedding-0.6B EMBED_DIMS=1024 EMBED_KEY_ENV= \
  scripts/local-gpu-up.sh --profile <profile> --backend ark
```

或 `scripts/local-gpu-up.sh --with-embedding`。这不是质量推荐，只是可复现的启用方式；全局默认不变，`quality_gate_passed=false`。
