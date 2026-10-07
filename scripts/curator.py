#!/usr/bin/env python3
"""Curator command for the pipeline curate stage.

One run proposes topic-to-topic links for one conversation. The worker
invokes it with CONTEXT_DB, CONTEXT_PROPOSALS, CONTEXT_CURATE_CONVERSATION and
CONTEXT_CURATE_VERSION in the environment (see docs/associations.md); this
script only reads the database and writes the proposals file — validation and
storage happen in the worker process (ValidateTopicLinks + InsertLinks), so a
bad proposal is rejected there with a reason, never silently stored.

Judgment follows docs/curator-prompt.md. Ambient-sound content (broadcasts,
device commands, lyrics, media playback) is excluded: clue chains are built
from spoken conversation only. Evidence ids the model may cite are restricted
to the real turn ids gathered here; anything else is dropped before writing.
"""
import json
import os
import sqlite3
import sys
import time
import urllib.request

DB = os.environ["CONTEXT_DB"]
OUT = os.environ["CONTEXT_PROPOSALS"]
CONV = os.environ.get("CONTEXT_CURATE_CONVERSATION", "")
VERSION = os.environ.get("CONTEXT_CURATE_VERSION", "")
URL = os.environ.get("CURATOR_MODEL_URL", "https://ark.cn-beijing.volces.com/api/plan/v3/chat/completions")
# The default is a non-reasoning model: glm-5.3-flash deliberates without a
# bound on this judgment shape and exhausts any output budget before writing
# content. CURATOR_MODEL / CURATOR_MODEL_URL / CURATOR_MAX_TOKENS can still
# point a reasoning model at the task when someone wants to pay its thinking.
MODEL = os.environ.get("CURATOR_MODEL", "doubao-seed-2.0-mini")
KEY = os.environ.get("CURATOR_API_KEY") or os.environ.get("VOLC_API_KEY", "")
PROXY = {"https": os.environ.get("HTTPS_PROXY", "http://172.22.23.201:6152")}
opener = urllib.request.build_opener(urllib.request.ProxyHandler(PROXY))

RELATIONS = ("same_topic", "follow_up", "contradiction", "update")

SYSTEM = """你是本地记忆库的记忆策展人。你的任务是发现两个会话话题之间的关联并提出建议；建议由程序校验后保存，不会自动合并话题。
只输出严格 JSON：{"proposals":[...]}，不要多余文字。

relation 取四种之一：
- same_topic：两次讨论说的是同一件事的不同表述。无序关系。
- follow_up：后一次讨论是对前一次的直接延续或追问。有向关系，from_topic_id 写较早一轮的话题，to_topic_id 写较晚一轮的话题。
- contradiction：两次讨论的说法相互矛盾。无序关系。
- update：后一次讨论更新或修正了前一次的信息。有向关系，from 写较早，to 写较晚。

每条提议的字段：kind（固定 "topic_link"）、from_topic_id、to_topic_id、relation、reason（≤500 字，说明证据中的原话支持了什么关系）、evidence_turn_ids（1 到 100 个轮次 ID）、confidence（0 到 1）。

判断标准：同一实体、项目、设备、客户或需求在两个会话被讨论，就应提议 same_topic 或 follow_up，confidence 取 0.5–0.8 即可；提议只是 status=proposed 的候选，有人工复核把关，宁可提出可复核的建议，也不要漏掉真实关联。只有内容完全无关时才不提。

示例（正确提议）：
- 会话A话题「门禁与飞书合作咨询」与会话B话题「门禁布线方案建议」围绕同一门禁项目 → {"relation":"same_topic","confidence":0.6}
- 会话A话题「工牌电池续航反馈」与会话B话题「电池结构改进」是同一产品问题的后续 → {"relation":"follow_up","from_topic_id":"较早会话的话题","to_topic_id":"较晚会话的话题","confidence":0.7}
- 会话A「工厂能耗采集需求」与会话B「报餐群使用」内容无关 → 不提议

硬性规则：
- from_topic_id 与 to_topic_id 必须不同，且都必须来自提供的真实话题 ID。
- evidence_turn_ids 只能引用提供的真实轮次 ID，严禁编造。
- 没有足够证据就不要提议；宁可少提议，不要编造。只有两个话题都来自真实讨论且内容确实相关时才提议。
"""


def first_json_object(s):
    """First complete top-level JSON object; a greedy regex would span two
    sibling objects and produce malformed JSON."""
    start = s.find("{")
    if start < 0:
        return ""
    depth, in_string, escaped = 0, False, False
    for i in range(start, len(s)):
        c = s[i]
        if escaped:
            escaped = False
        elif c == "\\" and in_string:
            escaped = True
        elif c == '"':
            in_string = not in_string
        elif in_string:
            pass
        elif c == "{":
            depth += 1
        elif c == "}":
            depth -= 1
            if depth == 0:
                return s[start:i + 1]
    return ""


from collections import Counter


def bigrams(s):
    s = "".join((s or "").split())
    return {s[i:i + 2] for i in range(len(s) - 1)} or {s}


def pair_containment(target_texts, other_title):
    """Max over target texts of |bigrams(t) ∩ bigrams(o)| / |bigrams(t)|.

    Containment against one target text at a time; scoring an other-title
    against the concatenated blob dilutes every pair to noise.
    """
    ob = bigrams(other_title)
    best = 0.0
    for t in target_texts:
        tb = bigrams(t)
        if tb and ob:
            best = max(best, len(tb & ob) / len(tb))
    return best


def rows(conn, sql, args=()):
    cols = [d[0] for d in cur.description] if (cur := conn.execute(sql, args)) else []
    return [dict(zip(cols, r)) for r in cur.fetchall()]


def speech_turn_ids(conn):
    """Turn ids that are not classified ambient (missing class counts as speech)."""
    return {r[0] for r in conn.execute(
        "SELECT id FROM turns WHERE id NOT IN (SELECT turn_id FROM turn_classes WHERE class='ambient')")}


def main():
    if not CONV:
        print("no CONTEXT_CURATE_CONVERSATION; nothing to do")
        json.dump({"proposals": []}, open(OUT, "w", encoding="utf8"))
        return
    conn = sqlite3.connect(f"file:{DB}?mode=ro", uri=True)
    conn.execute("pragma busy_timeout=5000")
    speech = speech_turn_ids(conn)

    # Target conversation: validated candidates with real evidence, speech only.
    target_topics = {}   # topic_id -> title
    target_evidence = set()
    target_lines = []
    for r in rows(conn, "SELECT kind, summary, topic_title, evidence_turn_ids_json FROM memory_candidates WHERE conversation_id=? AND status='validated'", (CONV,)):
        ev = [e for e in json.loads(r["evidence_turn_ids_json"] or "[]") if e in speech]
        if not ev or not r["summary"]:
            continue
        target_evidence.update(ev)
        target_lines.append(f"- [{r['kind']}] {r['summary']}（主题：{r['topic_title']}）证据轮次：{','.join(ev[:4])}")
    for r in rows(conn, """SELECT DISTINCT tp.topic_id, t.title FROM turn_topics tp
        JOIN topics t ON t.id=tp.topic_id JOIN turns ON turns.id=tp.turn_id
        WHERE turns.conversation_id=?""", (CONV,)):
        if r["title"]:
            target_topics[r["topic_id"]] = r["title"]
    if not target_lines or not target_topics:
        json.dump({"proposals": []}, open(OUT, "w", encoding="utf8"))
        return

    # Other conversations: rank their topics by title overlap with the target's
    # titles and keep a bounded shortlist; the model judges only this set.
    other = {}
    for r in rows(conn, """SELECT DISTINCT tp.topic_id AS tid, t.title AS title, conv.title AS conv_title
        FROM turn_topics tp JOIN topics t ON t.id=tp.topic_id
        JOIN turns ON turns.id=tp.turn_id JOIN conversations conv ON conv.id=turns.conversation_id
        WHERE turns.conversation_id!=?""", (CONV,)):
        if not r["title"] or r["tid"] in target_topics:
            continue
        e = other.setdefault(r["tid"], {"title": r["title"], "convs": set()})
        e["convs"].add(r["conv_title"][:40])
    target_texts = list(target_topics.values()) + [ln.split("：", 1)[-1] for ln in target_lines]
    all_titles = list(target_topics.values()) + [e["title"] for e in other.values()]
    df = Counter(b for title in all_titles for b in bigrams(title))
    rare_cut = max(2, len(all_titles) // 100)
    target_blob = set()
    for t in target_texts:
        target_blob |= bigrams(t)

    def rank_score(e):
        ob = bigrams(e["title"])
        contain = pair_containment(target_texts, e["title"])
        # Rare shared bigrams (entity names like 展卓/门禁/交换机) break the
        # flat containment ties toward genuinely related topics.
        rare_shared = {b for b in ob if df[b] <= rare_cut} & target_blob
        return contain + 0.25 * len(rare_shared) / max(len(ob), 1)

    ranked = sorted(other.items(), key=lambda kv: -rank_score(kv[1]))
    shortlist = ranked[:60]

    other_evidence = set()
    other_topic_lines = []
    other_evidence_lines = []
    if shortlist:
        ids = [tid for tid, _ in shortlist]
        q = ",".join("?" * len(ids))
        for r in rows(conn, f"""SELECT c.topic_title, c.kind, c.summary, c.evidence_turn_ids_json
            FROM memory_candidates c WHERE c.status='validated'
            AND c.topic_title IN (SELECT title FROM topics WHERE id IN ({q}))""", ids):
            ev = [e for e in json.loads(r["evidence_turn_ids_json"] or "[]") if e in speech]
            if not ev:
                continue
            other_evidence.update(ev)
            other_evidence_lines.append(f"- [{r['kind']}] {r['summary']}（主题：{r['topic_title']}）证据轮次：{','.join(ev[:4])}")
        for tid, e in shortlist:
            other_topic_lines.append(f"话题ID {tid}｜{e['title']}｜出现于：{'；'.join(sorted(e['convs'])[:3])}")
        # Evidence lines are capped; the topic-id section is never truncated —
        # the model can only cite ids it has seen.
        other_evidence_lines = other_evidence_lines[:120]

    allowed_evidence = target_evidence | other_evidence
    user = (
        f"目标会话话题（真实 ID）：\n" +
        "\n".join(f"{tid}｜{title}" for tid, title in target_topics.items()) +
        f"\n\n目标会话证据（可引用的轮次 ID）：\n" + "\n".join(target_lines) +
        f"\n\n其他会话的话题（真实 ID，关联的另一端从这里选）：\n" + "\n".join(other_topic_lines) +
        f"\n\n其他会话的证据（可引用的轮次 ID）：\n" + "\n".join(other_evidence_lines) +
        f"\n\n请提出这两个会话话题之间的关联提议（没有把握就不提）。"
    )

    body = json.dumps({
        "model": MODEL,
        "messages": [{"role": "system", "content": SYSTEM}, {"role": "user", "content": user}],
        "temperature": 0, "max_tokens": int(os.environ.get("CURATOR_MAX_TOKENS", "2048")),
    }).encode()
    proposals = []
    err, ok = None, False
    for attempt in range(4):
        try:
            req = urllib.request.Request(URL, data=body, headers={
                "Authorization": f"Bearer {KEY}", "Content-Type": "application/json"})
            with opener.open(req, timeout=300) as r:
                d = json.load(r)
            raw = d["choices"][0]["message"].get("content") or ""
            if os.environ.get("CURATOR_DEBUG"):
                dbg = {"prompt_chars": len(user), "usage": d.get("usage"),
                       "finish": d["choices"][0].get("finish_reason"),
                       "reasoning": (d["choices"][0]["message"].get("reasoning_content") or "")[:3000],
                       "raw": raw[:4000]}
                json.dump(dbg, open(f"/tmp/curator-debug-{CONV[:24]}.json", "w", encoding="utf8"),
                          ensure_ascii=False, indent=1)
            m = first_json_object(raw)
            if not m:
                msg = d["choices"][0].get("message", {})
                raise ValueError(
                    f"no JSON in output: {raw[:80]!r} finish={d['choices'][0].get('finish_reason')!r} "
                    f"usage={d.get('usage')} reasoning_len={len(msg.get('reasoning_content') or '')}")
            proposals = json.loads(m).get("proposals", [])
            ok = True
            break
        except Exception as e:
            err = e
            time.sleep(3 * (attempt + 1))
    if not ok:
        # A failed model call must fail the curate job so it is retried;
        # writing an empty proposal file would fake a successful curation.
        print(f"curator: model call failed: {err}", file=sys.stderr)
        sys.exit(1)

    # Defensive filter: the model may only cite ids that exist in the provided
    # sets; the worker re-validates everything anyway.
    kept = []
    for p in proposals:
        if not isinstance(p, dict):
            continue
        if p.get("kind") != "topic_link":
            continue
        if p.get("relation") not in RELATIONS:
            continue
        if p.get("from_topic_id") not in target_topics or p.get("to_topic_id") not in set(target_topics) | set(other):
            continue
        if p.get("from_topic_id") == p.get("to_topic_id"):
            continue
        ev = [e for e in (p.get("evidence_turn_ids") or []) if e in allowed_evidence]
        if not ev:
            continue
        try:
            conf = float(p.get("confidence", 0))
        except (TypeError, ValueError):
            conf = 0
        kept.append({
            "kind": "topic_link",
            "from_topic_id": p["from_topic_id"],
            "to_topic_id": p["to_topic_id"],
            "relation": p["relation"],
            "reason": str(p.get("reason", ""))[:500],
            "evidence_turn_ids": ev[:100],
            "confidence": min(max(conf, 0.0), 1.0),
        })

    json.dump({"proposals": kept}, open(OUT, "w", encoding="utf8"), ensure_ascii=False)
    print(f"curator: {CONV[:24]} version={VERSION} proposed={len(kept)}/{len(proposals)} error={err if proposals == [] and err else ''}")


if __name__ == "__main__":
    main()
