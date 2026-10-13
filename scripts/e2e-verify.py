#!/usr/bin/env python3
"""e2e-cloud verification: drive bin/context-memory over MCP stdio (NDJSON
JSON-RPC) and assert the recording synced from the cloud (context-sync +
fixture ASR → IngestTranscript → pipeline) is searchable with verbatim text,
non-empty evidence ids, a fresh evidence window, and a ready index. Exits 1
with a clear message on any failure."""

import argparse
import json
import subprocess
import sys


def fail(msg):
    sys.exit("e2e-verify: " + msg)


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--bin", default="bin/context-memory",
                    help="context-memory 二进制路径")
    ap.add_argument("--db", required=True, help="context-memory SQLite 库路径")
    ap.add_argument("--recording-id", dest="recording_id", required=True,
                    help="云端 recording_id（= 本地 conversation 的录音 ID）")
    ap.add_argument("--expect-phrase", dest="expect_phrase", required=True,
                    help="断言可检索到的原话短语")
    args = ap.parse_args()

    proc = subprocess.Popen(
        [args.bin, "--db", args.db],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL, text=True, bufsize=1,
    )
    next_id = 0

    def rpc(method, params=None):
        nonlocal next_id
        next_id += 1
        req = {"jsonrpc": "2.0", "id": next_id, "method": method}
        if params is not None:
            req["params"] = params
        proc.stdin.write(json.dumps(req) + "\n")
        proc.stdin.flush()
        while True:
            line = proc.stdout.readline()
            if not line:
                fail(f"no response for {method} (server closed stdio)")
            line = line.strip()
            if not line:
                continue
            msg = json.loads(line)
            if msg.get("id") == next_id:
                return msg

    def tool(name, tool_args):
        nonlocal next_id
        next_id += 1
        req = {"jsonrpc": "2.0", "id": next_id, "method": "tools/call",
               "params": {"name": name, "arguments": tool_args}}
        proc.stdin.write(json.dumps(req) + "\n")
        proc.stdin.flush()
        while True:
            line = proc.stdout.readline()
            if not line:
                fail(f"no response for {name} (server closed stdio)")
            line = line.strip()
            if not line:
                continue
            msg = json.loads(line)
            if msg.get("id") != next_id:
                continue
            if msg.get("error"):
                fail(f"{name} returned error: {json.dumps(msg['error'])}")
            result = msg.get("result") or {}
            if result.get("isError"):
                fail(f"{name} reported isError: {json.dumps(result.get('content'))}")
            structured = result.get("structuredContent")
            if structured is None:
                for item in result.get("content") or []:
                    if item.get("type") == "text":
                        try:
                            structured = json.loads(item.get("text") or "")
                        except ValueError:
                            structured = {}
                        break
            return structured or {}

    init = rpc("initialize", {
        "protocolVersion": "2024-11-05",
        "capabilities": {},
        "clientInfo": {"name": "recording-mcp-e2e-cloud", "version": "0"},
    })
    if "result" not in init:
        fail("initialize failed: " + json.dumps(init))
    proc.stdin.write(json.dumps({"jsonrpc": "2.0", "method": "notifications/initialized"}) + "\n")
    proc.stdin.flush()

    listing = rpc("tools/list")
    tools = (listing.get("result") or {}).get("tools") or []
    if len(tools) != 27:
        fail(f"tools/list returned {len(tools)} tools, want 27")

    # 检索：先整句短语；若检索器分词导致整句不命中，退到更短的稳定前缀，
    # 但最终断言不变：命中文本必须包含完整 expect-phrase。
    hit = None
    last_hits = []
    for query in (args.expect_phrase,
                  f"fixture transcript recording={args.recording_id}",
                  f"recording={args.recording_id}"):
        search = tool("search_memory", {"text": query})
        hits = search.get("hits") or []
        if hits:
            last_hits = hits
        verbatim = [h for h in hits if args.expect_phrase in (h.get("text") or "")]
        if verbatim:
            hit = verbatim[0]
            break
    if hit is None:
        fail("search_memory has no hit containing %r; hits=%s" % (
            args.expect_phrase, json.dumps(last_hits, ensure_ascii=False)))
    # 与 demo-verify 一致：字面命中的 turn-kind 结果不带 evidence_turn_ids，
    # 此时用该 hit 自身的 turn id 作为证据（evidence window 断言仍是硬校验）。
    evidence_id = None
    if hit.get("evidence_turn_ids"):
        evidence_id = hit["evidence_turn_ids"][0]
    else:
        for h in [hit] + last_hits:
            if h.get("kind") == "turn" and args.expect_phrase in (h.get("text") or ""):
                evidence_id = h.get("id")
                break
    if not evidence_id:
        fail("no evidence turn id in verbatim hits: " +
             json.dumps(last_hits, ensure_ascii=False))

    evidence = tool("get_evidence_context", {"turn_id": evidence_id})
    if evidence.get("stale"):
        fail("evidence turn %s reported stale" % evidence_id)
    target = next((t for t in (evidence.get("turns") or [])
                   if t.get("id") == evidence_id), None)
    if target is None or args.expect_phrase not in (target.get("text") or ""):
        fail("evidence window missing verbatim turn %s: %s" % (
            evidence_id, json.dumps(evidence, ensure_ascii=False)))

    page = tool("list_conversations", {"limit": 10})
    items = page.get("items") or []
    conv = next((c for c in items
                 if c.get("recording_id") == args.recording_id), None)
    if conv is None:
        conv = next((c for c in items
                     if c.get("title") in ("Recording %s" % args.recording_id,
                                           args.recording_id)), None)
    if conv is None and len(items) == 1:
        conv = items[0]
    if conv is None:
        fail("no conversation for recording %s; items=%s" % (
            args.recording_id, json.dumps(items, ensure_ascii=False)))
    conv_id = conv.get("conversation_id")

    status = tool("get_processing_status", {"conversation_id": conv_id})
    if status.get("indexing_status") != "ready":
        fail("indexing_status=%r, want ready" % status.get("indexing_status"))

    print("--- search_memory (verbatim hit) ---")
    print(json.dumps(hit, ensure_ascii=False, indent=2))
    print("--- get_evidence_context ---")
    print(json.dumps(evidence, ensure_ascii=False, indent=2))
    print("--- get_processing_status ---")
    print(json.dumps(status, ensure_ascii=False, indent=2))
    print("e2e-verify: PASS — recording=%s conversation=%s evidence=%s indexing=ready" % (
        args.recording_id, conv_id, evidence_id))
    proc.stdin.close()
    proc.wait(timeout=10)


if __name__ == "__main__":
    main()
