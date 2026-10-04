#!/bin/sh
# run-curator.sh runs one headless curation pass for the curate stage.
#
# Contract (see docs/processing.md): context-worker sets CONTEXT_DB and
# CONTEXT_PROPOSALS. This script configures a project-local MCP server pointing
# at CONTEXT_DB, runs the curator agent with docs/curator-prompt.md, and exits
# after the agent writes its proposals to CONTEXT_PROPOSALS. Validation and
# writing of the accepted proposals happen back in the worker, not here.
set -eu

: "${CONTEXT_DB:?CONTEXT_DB must be set by context-worker}"
: "${CONTEXT_PROPOSALS:?CONTEXT_PROPOSALS must be set by context-worker}"

MODEL="${CURATOR_MODEL:-volces-agent-01/glm-5.3-flash}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO"

# The project-local config keeps the worker's database and the MCP server in
# sync; the file is gitignored.
pi mcp add context-memory -l -- "$REPO/bin/context-memory" --db "$CONTEXT_DB" >/dev/null

exec pi -p --approve --mode text --model "$MODEL" \
  --exclude-tools "bash,edit,subagent,fusion_investigate,fusion_research,fusion_reason,fusion_validate,record_advisor_outcome" \
  --append-system-prompt docs/curator-prompt.md \
  "对当前记忆库执行一轮策展。只用 context-memory 的只读工具调查（经 codemode 调用），预算最多 30 次工具调用；把提议 JSON 写入 $CONTEXT_PROPOSALS，完成后只回复该路径与条数。不要运行命令、不要查找数据库、不要调用写入类工具。"
