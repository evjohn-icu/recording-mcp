# asr-repair(plan-ASR 定向修复)

`scripts/asr-repair.py`:用 `doubao-seed-2.1-lite` 重转 plan-ASR 的可疑跨度,产出提案并
生成版本化修复文件。背景:plan-ASR 的 normalized turns `confidence` 全 null、`definite` 标志
只反映流式确认而非质量,唯一验证有效的质量信号是**与 lite 稿按时间分桶的分歧度**。

## 用法

```sh
# 审计:对比库内 ASR 与 lite 稿(sim<0.5 的最差跨度),出 repairs.md/.json 提案
python3 scripts/asr-repair.py --run <stem> --lite /path/to/<stem>.lite.txt --audio-root /path/to/RECORD

# 显式跨度修复(提案)
python3 scripts/asr-repair.py --run <stem> --span 28-31

# 生成版本化修复文件(仅 ASR 侧为空/听不清密集的跨度;其余跨度一律保持提案)
python3 scripts/asr-repair.py --run <stem> --span 182-185 --apply
```

## 落库语义(2026-10 收敛为版本化更正)

- **只自动修复"明确垃圾"跨度**:重叠 turns 全部为空文本或 `(听不清)` 为主。分歧 ≠ 错误——
  实测同一段里 ASR 抓到人声报数、lite 抓到重叠广播,双方都对,盲改会毁掉正确转写;
  这类跨度永远停在 repairs.md 供人工判断
- **版本化 apply,不写库**:`--apply` 为每个受影响 segment 生成
  `<run>/repairs/<segment>-repair-normalized.json`(asr-input/0.1):`source` 复制自当前
  normalized 文件,`transcript_revision` 在当前修订后追加 `-r1`(修复文件已存在则 `-r2`
  递增;已存在的修复文件不可解析或修订号不延续时报错退出,绝不覆盖),`supersedes_revision`
  指向当前修订;turns 中被修复跨度的文本替换(原文留在
  `turn.repaired.original_text`),其余原样。脚本不碰数据库、不改原始 normalized 文件
- **应用**:逐文件执行脚本打印的
  `bin/import-transcript --db <db> --input <run>/repairs/<segment>-repair-normalized.json`;
  旧修订由修订模型自动归档(active=0),原文保留为审计历史——脚本不再承担备份职责
- **边界**:import 时若该 segment 的活动 head 已移动(supersedes 目标非 active),
  `IngestTranscript` 拒绝——需基于当前 active 修订重新生成提案
- lite 结果为空/`(无有效语音)`/纯 `(听不清)` 时,turn 置空(正确的"清垃圾"语义)

## 已知边界

- lite 转写存在跑次间非确定性(低噪远场段两次结果可不同),提案文本供参考
- 审计质量取决于 lite 稿;先跑 `scripts/lite-transcribe.py`(断点缓存使审计增量为 0 请求)
- apply 不写库、不改原始 normalized 文件;无需备份惯例,旧修订由版本模型自动归档
