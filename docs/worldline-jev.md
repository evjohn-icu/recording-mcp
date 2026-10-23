# 本地 JEV 风格决策接入

这里运行的是本机缓存的 `Jev-Style-0.8B-Decision-v3` 专用决策模型，不是 `cmd/context-decision` 的合成服务，也不是 TypeSafe 托管 JEV。现有 8082 Jev-Style v2 服务没有被修改。0.8B 的模型加载、verdict logits 读出和概率校准由模型包自带 `jev_style_decision.py` 完成，适配器只映射本项目 `system_one` 协议，不使用聊天生成 JSON 代替判断。

## 实际运行环境与启动

本轮使用已缓存权重，无外部录音上传；隔离 Python 3.12 环境在 `/tmp/jev-decision-venv`，通过本机 uv 缓存离线安装了 torch 2.14.1+cu130、transformers 5.18.0、tokenizers 0.23.2、numpy 2.5.3 和 safetensors 0.8.0。硬件为 RTX 2070 8GB。临时环境重启后如被清理，需要重新建立匹配环境；不依赖系统默认 Python。

```sh
/tmp/jev-decision-venv/bin/python scripts/decision-local-server.py --verify --addr 127.0.0.1:8799
```

默认模型位于当前用户 Hugging Face 缓存：`chaoliangUNSW/Jev-Style-0.8B-Decision-v3`，快照 `b023d1f9c7858fbf01504577a3bfc349ea5c7385`。启动强制校验 manifest；模型版本来源于实际权重校验，而不是客户端默认名字。换目录可传 `--model-dir`，响应必须标识实际文件版本。

另一个终端启动 Web：

```sh
go build -o bin/context-web ./cmd/context-web
./bin/context-web --profiles-root data/worldline-local/profiles \
  --addr 127.0.0.1:8790 --web-dir web/dist \
  --worldline-decision-url http://127.0.0.1:8799/v1/systemone \
  --worldline-decision-model decision-v3-0.8b
```

打开世界线并点击“整理世界线”会运行真实语义评估。普通 GET 不调用模型。未配置服务、失败或证据不足都有独立状态，不能把基础投影成功解释为 JEV 调用成功。

## 处理边界

候选来自当前、非 ambient、有真实来源的跨会话陈述。逐对独立传递原话摘录、修订、提及时间及身份状态，避免不同问题的来源串扰。当前仅评估已有同名 Topic 中的观察陈述；计划不自动成为事实，尚未进行异名 Topic 的自动语义归并。

每次最多 128 对，推理总预算 45 秒；超出范围会明确返回未覆盖计数。模型只能建议文本相似、补充信息、可能矛盾、无关或信息不足。未知说话人仍未知；所有建议都需人工核对，不能确认现实事实，不显示真实性百分比。

建议写入现有 plane 事件，绑定两侧来源版本和稳定来源快照。调用后及写入后都会重查来源；遇到修订、身份变更或遗忘则拒绝/清理结果。模型服务仅允许 loopback、拒绝带 Origin 的浏览器请求、不记录输入正文；Go 客户端禁用代理和重定向，不能通过此配置向远端发私人记忆。

## 验证

- **PASS**：真实 0.8B 权重加载和 manifest 校验；专用 Choice/Noul/Score 三类分布。
- **PASS**：固定中文合成样例 4/4（同义、可能矛盾、无关、无证据），使用应用相同 prompt。这是小样本冒烟检查，不是通用准确率或校准证明。
- **PASS**：16 项前端合成浏览器检查；语义卡片能查看两侧来源且只读、不自动确认。
- **NOT_RUN**：2B 和 8082 v2 的同条件质量/性能对比；TypeSafe 托管模型调用。

真实运行明细只保存在被忽略的 `data/worldline-local/`，不提交原文、密钥或截图。停用 `--worldline-decision-url` 即可关闭新推理；已有建议仍受来源版本过滤，原始数据库和录音无需回退。

## 本次真实本地结果

2026-10-23，本地副本共 79 对符合条件的跨会话观察陈述；已使用上述真实 0.8B 模型全部评估，耗时 13.953 秒，未触发截断。30 对为文本相似、1 对为补充信息、4 对为需要人工核对的可能矛盾、44 对为信息不足。79 条建议对应的原始陈述状态仍为 `unconfirmed`。同一 request_id 重试返回同一个完成任务，不重复调用模型。

列表优先显示含待核对关系的主题，标注“4 处关系待核对”；逐条可看到较早/较新原话并打开录音。这里的 4 对不是已确认事实冲突，44 对信息不足也不是无关结论。上述数量是模型输出统计，不是人工标注正确率；尚未对私人录音做人工语义准确率验收。

最终验收：**PASS** 全量 `go test -race ./...`；最终来源快照/排序修改后 `go test -race ./internal/app ./internal/web ./cmd/context-web`；前端 typecheck/build 和 17 项合成浏览器检查。**PASS** 真实 Chromium 显示模型名和两侧来源卡片，音频播放时钟前进且无媒体或页面错误；首批条目可见约 2.433 秒。**PASS** 原始数据库 SHA-256 未变，25,113 个源文件大小/mtime 未变。截图、模型结果及运行报告只留在本机忽略目录。
