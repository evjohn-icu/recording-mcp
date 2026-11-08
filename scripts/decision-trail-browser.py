#!/usr/bin/env python3.14
"""Synthetic browser checks for the decision trail panel.

Uses `vite preview` (built assets) with every /api route mocked. No profile,
database or audio file is touched.
"""
from __future__ import annotations

import asyncio
import copy
import json
import os
import sys
from pathlib import Path
from urllib.parse import urlparse

from playwright.async_api import async_playwright

BASE = os.environ.get("BASE_URL", "http://127.0.0.1:5173").rstrip("/")
OUT = Path(os.environ.get("BROWSER_ARTIFACT_DIR", "/tmp"))
OUT.mkdir(parents=True, exist_ok=True)

THREADS = [
    {
        "id": "thread:atlas", "title": "Atlas 项目", "macro_topic_id": "macro:work",
        "macro_topic_title": "工作与项目", "source_topic_ids": ["topic:atlas"],
        "conversation_count": 3, "entry_count": 3, "dated_entry_count": 3, "unknown_entry_count": 0,
        "first_mentioned_at": "2026-01-05T00:00:00Z", "last_mentioned_at": "2026-02-02T00:00:00Z",
    }
]


def claim(claim_id: str, summary: str, kind: str, text: str, start: str | None) -> dict:
    global_index = claim_id.split(":")[-1]
    return {
        "id": claim_id, "version": "c1", "summary": summary, "kind": kind, "status": "unconfirmed",
        "valid_from": None, "valid_until": None, "mentioned_at": start, "time_basis": "仅有提及时间",
        "time_precision": "unknown", "change_type": "report", "conflict_with": [],
        "evidence": [{
            "turn_id": f"turn:{global_index}", "revision": "rev-1", "conversation_id": f"conv:{global_index}",
            "recording_id": f"recording:{global_index}", "text": text, "offset_ms": 1200,
        }],
    }


ENTRIES = [
    {"id": "entry:1", "claim_id": "claim:1", "version": "c1", "summary": "客户要求 6 月前上线",
     "relation": "first_mention", "mentioned_at": "2026-01-05T00:00:00Z", "time_known": True,
     "status": "unconfirmed", "category": "plans", "macro_topic_id": "macro:work", "lane_id": "topic:atlas:plans",
     "claim": claim("claim:1", "客户要求 6 月前上线", "observation", "客户要求 6 月前上线。", "2026-01-05T00:00:00Z"),
     "evidence": [], "conversation_ids": ["conv:1"]},
    {"id": "entry:2", "claim_id": "claim:2", "version": "c1", "summary": "建议先采用方案 A 快速上线",
     "relation": "subsequent_mention", "mentioned_at": "2026-01-19T00:00:00Z", "time_known": True,
     "status": "unconfirmed", "category": "plans", "macro_topic_id": "macro:work", "lane_id": "topic:atlas:plans",
     "claim": claim("claim:2", "建议先采用方案 A 快速上线", "suggestion", "建议先采用方案 A 快速上线。", "2026-01-19T00:00:00Z"),
     "evidence": [], "conversation_ids": ["conv:2"]},
    {"id": "entry:3", "claim_id": "claim:3", "version": "c1", "summary": "最终确定采用方案 B",
     "relation": "subsequent_mention", "mentioned_at": "2026-02-02T00:00:00Z", "time_known": True,
     "status": "unconfirmed", "category": "plans", "macro_topic_id": "macro:work", "lane_id": "topic:atlas:plans",
     "claim": claim("claim:3", "最终确定采用方案 B", "suggestion", "最终确定采用方案 B，分两阶段交付。", "2026-02-02T00:00:00Z"),
     "evidence": [], "conversation_ids": ["conv:3"]},
]
for entry in ENTRIES:
    entry["evidence"] = entry["claim"]["evidence"]


def base_records() -> list[dict]:
    def record(record_id: str, state: str, text: str, **extra: object) -> dict:
        data = {
            "id": record_id, "candidate_id": "decision-candidate-1" if state != "observation" else "",
            "topic_id": "macro:work", "topic_title": "工作与项目", "state": state, "text": text,
            "source_kind": "review" if "review" in record_id else "suggestion",
            "valid_from": None, "valid_until": None, "mentioned_at": "2026-02-02T00:00:00Z",
            "recorded_at": "2026-02-03T00:00:00Z", "confirmed_at": None, "evidence": [{
                "turn_id": "turn:3", "revision": "rev-1", "conversation_id": "conv:3",
                "recording_id": "recording:3", "text": "最终确定采用方案 B，分两阶段交付。",
                "offset_ms": 1200, "source_status": "current",
            }],
        }
        data.update(extra)
        return data

    return [
        record("decision-candidate-0", "observation", "客户要求 6 月前上线"),
        record("decision-candidate-1", "decision_candidate", "最终确定采用方案 B", candidate_source="rule-v1"),
        record("review-old", "superseded", "最终确定采用方案 A", superseeded_by="review-new"),
        record("review-new", "confirmed", "最终确定采用方案 B", reviewer="alice"),
        record("review-stale", "unresolved", "方案 B 来源已失效", unresolved_reasons=["source_invalidated"]),
    ]


TRAIL = {
    "schema_version": "decision-trail-v1",
    "version": "wl-synthetic-1",
    "topics": [{
        "id": "macro:work", "title": "工作与项目", "current_state": "confirmed",
        "current": None, "history": base_records(), "unresolved": [],
        "unresolved_reasons": [], "counts": {},
    }],
    "truncated": False,
}
TRAIL["topics"][0]["current"] = TRAIL["topics"][0]["history"][3]


def apply_review(payload: dict) -> None:
    topic = TRAIL["topics"][0]
    action = payload.get("action")
    candidate = payload.get("candidate_id")
    if action == "confirm":
        topic["current_state"] = "confirmed"
        record = {
            "id": f"review-{len(topic['history'])}", "candidate_id": candidate, "topic_id": "macro:work",
            "topic_title": "工作与项目", "state": "confirmed", "review_status": "accepted",
            "text": "最终确定采用方案 B", "source_kind": "review", "valid_from": None, "valid_until": None,
            "mentioned_at": None, "recorded_at": "2026-02-04T00:00:00Z", "confirmed_at": "2026-02-04T00:00:00Z",
            "reviewer": payload.get("reviewer") or "local-user", "evidence": [{
                "turn_id": "turn:3", "revision": "rev-1", "conversation_id": "conv:3",
                "recording_id": "recording:3", "text": "最终确定采用方案 B", "offset_ms": 1200,
                "source_status": "current",
            }],
        }
        topic["history"].append(record)
        topic["current"] = record
    elif action == "reject":
        topic["current_state"] = "unresolved"
        topic["current"] = None
        topic["unresolved_reasons"] = ["rejected"]
        topic["history"].append({
            "id": f"review-{len(topic['history'])}", "candidate_id": candidate, "topic_id": "macro:work",
            "topic_title": "工作与项目", "state": "unresolved", "review_status": "rejected",
            "text": "最终确定采用方案 B", "source_kind": "review", "valid_from": None, "valid_until": None,
            "mentioned_at": None, "recorded_at": "2026-02-04T00:00:00Z", "confirmed_at": "2026-02-04T00:00:00Z",
            "reviewer": payload.get("reviewer") or "local-user", "evidence": [],
        })
    elif action == "reopen":
        topic["current_state"] = "unresolved"
        topic["current"] = None
        topic["unresolved_reasons"] = ["reopened"]
        topic["history"].append({
            "id": f"review-{len(topic['history'])}", "candidate_id": candidate, "topic_id": "macro:work",
            "topic_title": "工作与项目", "state": "unresolved", "review_status": "reopened",
            "text": "", "source_kind": "review", "valid_from": None, "valid_until": None,
            "mentioned_at": None, "recorded_at": "2026-02-04T00:00:00Z", "confirmed_at": "2026-02-04T00:00:00Z",
            "reviewer": payload.get("reviewer") or "local-user", "evidence": [],
        })


async def test_decision_trail_ui(page, open_panel, observations: dict, check) -> None:
    await open_panel()
    check("decision-topic-selected", True)
    history = await page.get_by_test_id("decision-history").inner_text()
    check("decision-history-three-sessions", history.count("最终确定") + history.count("客户要求") >= 3, history[:200])
    check("decision-states-visible", "候选决定" in history and "已确认" in history and "已取代" in history)
    current = await page.get_by_test_id("decision-current").inner_text()
    check("decision-current-projection", "已确认" in current, current)
    await page.get_by_role("button", name="查看证据").first.click()
    evidence = page.get_by_test_id("decision-evidence").first
    await evidence.wait_for()
    check("decision-evidence-visible", "最终确定采用方案 B" in await evidence.inner_text())
    await evidence.get_by_role("button", name="播放").first.click()
    await page.locator(".audio-bar").wait_for(timeout=10000)
    check("decision-evidence-audio", True)
    before = len(observations["reviews"])
    await page.get_by_test_id("decision-review-confirm").first.click()
    await page.wait_for_timeout(500)
    check("decision-review-confirm", len(observations["reviews"]) == before + 1
          and observations["reviews"][-1]["action"] == "confirm"
          and observations["reviews"][-1]["expected_version"] == "wl-synthetic-1")
    await page.get_by_test_id("decision-review-reject").first.click()
    await page.wait_for_timeout(400)
    check("decision-review-reject", observations["reviews"][-1]["action"] == "reject")
    await page.get_by_test_id("decision-review-reopen").first.click()
    await page.wait_for_timeout(400)
    check("decision-review-reopen", observations["reviews"][-1]["action"] == "reopen")
    screenshot = OUT / "decision-trail.png"
    await page.screenshot(path=str(screenshot), full_page=True)
    observations["screenshots"].append(str(screenshot))


async def test_unresolved_state_is_visible(page, open_panel, check) -> None:
    topic = TRAIL["topics"][0]
    topic["current"] = None
    topic["current_state"] = "unresolved"
    topic["unresolved_reasons"] = ["multiple_candidates"]
    topic["history"].append({
        "id": "review-conflict", "candidate_id": "decision-candidate-9", "topic_id": "macro:work",
        "topic_title": "工作与项目", "state": "unresolved", "review_status": "rejected",
        "text": "方案 A 与方案 B 冲突", "source_kind": "review", "valid_from": None, "valid_until": None,
        "mentioned_at": None, "recorded_at": "2026-02-04T00:00:00Z", "confirmed_at": None,
        "reviewer": "alice", "evidence": [],
    })
    await page.reload(wait_until="domcontentloaded")
    await open_panel()
    unresolved = await page.get_by_test_id("decision-current").inner_text()
    check("decision-unresolved-visible", "待解决" in unresolved and "已确认" not in unresolved, unresolved)
    check("decision-unresolved-reason", "存在多个候选" in unresolved, unresolved)


async def test_confidence_is_not_rendered_as_truth(page, open_panel, check) -> None:
    TRAIL["topics"][0]["history"][0]["confidence"] = 0.99
    await page.reload(wait_until="domcontentloaded")
    await open_panel()
    body = await page.locator("body").inner_text()
    check("decision-no-confidence", "99%" not in body and "置信度" not in body)


async def test_review_persists_after_refresh(page, open_panel, check) -> None:
    topic = TRAIL["topics"][0]
    topic["current_state"] = "confirmed"
    topic["current"] = topic["history"][3]
    await page.reload(wait_until="domcontentloaded")
    await open_panel()
    check("decision-review-persists-after-refresh", "已确认" in await page.get_by_test_id("decision-current").inner_text())


async def main() -> dict:
    observations = {"checks": [], "page_errors": [], "reviews": [], "screenshots": []}

    def check(name: str, ok: bool, detail: str = "") -> None:
        observations["checks"].append({"name": name, "status": "PASS" if ok else "FAIL", "detail": detail})

    async with async_playwright() as p:
        options = {"headless": True, "args": ["--no-sandbox", "--no-proxy-server"]}
        if os.environ.get("BROWSER_PATH"):
            options["executable_path"] = os.environ["BROWSER_PATH"]
        browser = await p.chromium.launch(**options)
        page = await browser.new_page(viewport={"width": 1280, "height": 900})
        page.on("pageerror", lambda error: observations["page_errors"].append(str(error)))

        async def route_api(route):
            request = route.request
            path = urlparse(request.url).path
            if path == "/api/session":
                await route.fulfill(json={"authenticated": True, "csrf_token": "synthetic-csrf", "profile": "fixture"})
            elif path == "/api/forget":
                await route.fulfill(json={"operations": []})
            elif path == "/api/suppressions":
                await route.fulfill(json={"operations": []})
            elif path == "/api/worldline/threads":
                await route.fulfill(json={"version": "wl-synthetic-1", "total": 1, "threads": THREADS,
                                          "range": {"start": "2026-01-05T00:00:00Z", "end": "2026-02-02T00:00:00Z"}})
            elif path == "/api/worldline/threads/thread:atlas":
                await route.fulfill(json={"version": "wl-synthetic-1", "thread": THREADS[0],
                                          "entries_total": 3, "unknown_entries_total": 0,
                                          "entries": ENTRIES, "unknown_entries": []})
            elif path.startswith("/api/worldline/threads/"):
                await route.fulfill(json={"version": "wl-synthetic-1", "thread": THREADS[0],
                                          "entries_total": 3, "unknown_entries_total": 0,
                                          "entries": ENTRIES, "unknown_entries": []})
            elif path == "/api/decisions":
                await route.fulfill(json=copy.deepcopy(TRAIL))
            elif path == "/api/decisions/review":
                payload = request.post_data_json
                observations["reviews"].append(payload)
                apply_review(payload)
                await route.fulfill(json=copy.deepcopy(TRAIL))
            elif path == "/api/turns/turn:3/audio":
                await route.fulfill(status=200, headers={"X-Audio-Start-MS": "1200", "X-Audio-End-MS": "3000",
                                                         "Content-Type": "audio/wav"}, body=b"")
            else:
                await route.fulfill(json={})

        await page.route("**/api/**", route_api)

        async def open_panel() -> None:
            await page.locator(".wl-thread-entries button").first.click(timeout=20000)
            await page.get_by_test_id("decision-panel").wait_for(timeout=15000)

        await page.goto(BASE, wait_until="domcontentloaded")
        try:
            await test_decision_trail_ui(page, open_panel, observations, check)
            await test_unresolved_state_is_visible(page, open_panel, check)
            await test_confidence_is_not_rendered_as_truth(page, open_panel, check)
            await test_review_persists_after_refresh(page, open_panel, check)
        except Exception as exc:  # noqa: BLE001
            check("decision-trail-ui", False, repr(exc))
        finally:
            check("decision-no-page-errors", not observations["page_errors"], "; ".join(observations["page_errors"]))
            await browser.close()

    report = {"base_url": BASE, "synthetic_only": True, **observations,
              "passed": sum(1 for c in observations["checks"] if c["status"] == "PASS"),
              "total": len(observations["checks"])}
    (OUT / "decision-trail-browser.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return report


if __name__ == "__main__":
    report = asyncio.run(main())
    failed = [c["name"] for c in report["checks"] if c["status"] == "FAIL"]
    if failed:
        raise SystemExit(f"decision browser failures: {failed}")
