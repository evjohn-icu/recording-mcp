"""M1 demo verification: drive bin/context-memory over MCP stdio (NDJSON
JSON-RPC) and assert the demo conversation is searchable with verbatim
evidence and a ready index. Exits 1 with a clear message on any failure."""

import json
import os
import subprocess
import sys

DB = os.environ["DB"]
DEMO_DIR = os.environ["DEMO_DIR"]
CONV_ID = os.environ["CONV_ID"]


def fail(msg):
    sys.exit("demo: " + msg)


def call_tool(proc, next_id, name, args):
    req = {"jsonrpc": "2.0", "id": next_id, "method": "tools/call",
           "params": {"name": name, "arguments": args}}
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
        if msg.get("id") == next_id:
            if msg.get("error"):
                fail(f"{name} returned error: {json.dumps(msg['error'])}")
            result = msg.get("result") or {}
            if result.get("isError"):
                fail(f"{name} reported isError: {json.dumps(result.get('content'))}")
            return result.get("structuredContent") or {}


def main():
    with open(os.path.join(DEMO_DIR, "normalized.json"), encoding="utf-8") as f:
        normalized = json.load(f)
    turns = normalized.get("turns") or []
    if not turns:
        fail("normalized.json has no turns; fixture unusable")
    first_text = (turns[0].get("text") or "").strip()
    query = "".join(first_text.split())[:8]
    if not query:
        fail("first turn text is empty; fixture unusable")

    proc = subprocess.Popen(
        ["bin/context-memory", "--db", DB],
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

    init = rpc("initialize", {
        "protocolVersion": "2024-11-05",
        "capabilities": {},
        "clientInfo": {"name": "recording-mcp-demo", "version": "0"},
    })
    if "result" not in init:
        fail("initialize failed: " + json.dumps(init))
    proc.stdin.write(json.dumps({"jsonrpc": "2.0", "method": "notifications/initialized"}) + "\n")
    proc.stdin.flush()

    def tool(name, args):
        nonlocal next_id
        next_id += 1
        return call_tool(proc, next_id, name, args)

    status = tool("get_processing_status", {"conversation_id": CONV_ID})
    if status.get("indexing_status") != "ready":
        fail("indexing_status=%r, want ready" % status.get("indexing_status"))

    search = tool("search_memory", {"text": query})
    hits = search.get("hits") or []
    verbatim = [h for h in hits if query in (h.get("text") or "")]
    if not verbatim:
        fail("search_memory(%r) has no verbatim hit; hits=%s" % (
            query, json.dumps(hits, ensure_ascii=False)))
    hit = verbatim[0]
    print("--- search_memory (verbatim hit) ---")
    print(json.dumps(hit, ensure_ascii=False, indent=2))

    evidence_id = None
    if hit.get("evidence_turn_ids"):
        evidence_id = hit["evidence_turn_ids"][0]
    else:
        for h in hits:
            if h.get("kind") == "turn":
                evidence_id = h.get("id")
                break
    if not evidence_id:
        fail("no evidence turn id in hits: " + json.dumps(hits, ensure_ascii=False))

    evidence = tool("get_evidence_context", {"turn_id": evidence_id})
    if evidence.get("stale"):
        fail("evidence turn %s reported stale" % evidence_id)
    target = next((t for t in (evidence.get("turns") or []) if t.get("id") == evidence_id), None)
    if target is None or query not in (target.get("text") or ""):
        fail("evidence window missing verbatim turn %s: %s" % (
            evidence_id, json.dumps(evidence, ensure_ascii=False)))
    print("--- get_evidence_context ---")
    print(json.dumps(evidence, ensure_ascii=False, indent=2))

    print("--- get_processing_status ---")
    print(json.dumps(status, ensure_ascii=False, indent=2))
    print("demo: PASS — verbatim hit + ready index + fresh evidence")
    proc.stdin.close()
    proc.wait(timeout=10)


if __name__ == "__main__":
    main()
