# asr-repair(plan-ASR 定向修复)

`scripts/asr-repair.py`:用 `doubao-seed-2.1-lite` 重转 plan-ASR 的可疑跨度,产出提案并
(受控)回写库。背景:plan-ASR 的 normalized turns `confidence` 全 null、`definite` 标志
只反映流式确认而非质量,唯一验证有效的质量信号是**与 lite 稿按时间分桶的分歧度**。

## 用法

```sh
# 审计:对比库内 ASR 与 lite 稿(sim<0.5 的最差跨度),出 repairs.md/.json 提案
python3 scripts/asr-repair.py --run <stem> --lite /path/to/<stem>.lite.txt --audio-root /path/to/RECORD

# 显式跨度修复(提案)
python3 scripts/asr-repair.py --run <stem> --span 28-31

# 落库(仅 ASR 侧为空/听不清密集的跨度;其余跨度一律保持提案)
python3 scripts/asr-repair.py --run <stem> --span 182-185 --apply
```

## 落库语义(2026-10 实测后收敛)

- **只自动改写"明确垃圾"跨度**:重叠 turns 全部为空文本或 `(听不清)` 为主。分歧 ≠ 错误——
  实测同一段里 ASR 抓到人声报数、lite 抓到重叠广播,双方都对,盲改会毁掉正确转写;
  这类跨度永远停在 repairs.md 供人工判断
- **原地更新**:DB `turns` 行 + `segments/segment-*-normalized.json` 同时改;turn id/时间戳/
  外键不动;原文存 `turn_json.repaired.original`
- **id 映射**:ingest 时代的 recording/turn id 是一次性随机 hash,与现派生 id 不同源;
  映射走 `(global_start_ms, global_end_ms, content)` 三元组,UPDATE 校验 rowcount=1
- **空文本 turn 从未入库**(`content NOT NULL` 被跳过),此类修复只写 normalized.json,
  日志标注 `normalized.json only`
- lite 结果为空/`(无有效语音)`/纯 `(听不清)` 时,turn 置空(正确的"清垃圾"语义)

## 已知边界

- lite 转写存在跑次间非确定性(低噪远场段两次结果可不同),提案文本供参考
- 审计质量取决于 lite 稿;先跑 `scripts/lite-transcribe.py`(断点缓存使审计增量为 0 请求)
- 测试备份惯例:动库前 `cp context-memory.db /tmp/` + `cp -r segments /tmp/`
