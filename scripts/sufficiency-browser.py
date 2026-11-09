#!/usr/bin/env python3
"""Synthetic browser check for the evidence-sufficiency panel.

`vite preview` serves the built UI; every /api route is mocked. No profile,
database or audio file is touched. Proves the three statuses, the cited
evidence text, gap messages and evidence links are visible in the existing
Search page.
"""
from __future__ import annotations

import asyncio
import json
import os
from pathlib import Path
from urllib.parse import parse_qs, urlparse

from playwright.async_api import async_playwright

BASE = os.environ.get("BASE_URL", "http://127.0.0.1:4173").rstrip("/")
OUT = Path(os.environ.get("BROWSER_ARTIFACT_DIR", "/tmp"))
OUT.mkdir(parents=True, exist_ok=True)


def evidence(turn_id: str, text: str, start_ms: int, item_id: str) -> dict:
    return {
        "turn_id": turn_id,
        "text": text,
        "speaker_scoped_id": None,
        "global_start_ms": start_ms,
        "global_end_ms": start_ms + 4000,
        "used_by_item_ids": [item_id],
    }


def turn_item(item_id: str, turn_id: str, text: str, conversation_id: str, start_ms: int) -> dict:
    return {
        "kind": "turn",
        "id": item_id,
        "turn_id": turn_id,
        "conversation_id": conversation_id,
        "content": text,
        "summary": "",
        "title": "",
        "evidence_turn_ids": [turn_id],
        "global_start_ms": start_ms,
        "speaker_person_id": None,
        "stale": False,
        "ambient": False,
        "derived_from": "",
    }


def package(status: str, item: dict, ev: dict, gaps: list[dict], evidence_refs: list[str]) -> dict:
    return {
        "items": [item],
        "evidence": [ev],
        "coverage": {
            "conversations": [item["conversation_id"]],
            "topic_ids": [],
            "time_range": {"started_after": None, "started_before": None, "unknown_time_included": False},
            "branches": {"lexical": True, "links": False, "evidence": True, "semantic": False},
            "scanned": {"notes": 0, "turns": 1},
        },
        "gaps": gaps,
        "truncated": False,
        "degraded": bool(gaps),
        "degrade_reason": "; ".join(gap["detail"] for gap in gaps),
        "trace": {
            "lexical": {"query": "预算", "hit_ids": [item["id"]]},
            "semantic": {"query": "预算", "backend": "hash", "indexed": 0, "hit_ids": [], "rejected_stale": 0, "rejected_scope": 0},
            "links_expanded": [],
        },
        "ranking": [{"kind": "turn", "item_id": item["id"], "retrieval_sources": ["lexical"], "raw_scores": {"lexical": 1.0}, "retrieval_score": 1.0}],
        "conversation_titles": {item["conversation_id"]: "合成会话"},
        "sufficiency": {"status": status, "evidence_refs": evidence_refs},
    }


SUFFICIENT_ITEM = turn_item("turn:1", "turn:1", "季度预算已批准，金额八十万元。", "conv:1", 65000)
INSUFFICIENT_ITEM = turn_item("turn:2", "turn:2", "季度预算已批准。", "conv:2", 12000)
UNKNOWN_ITEM = turn_item("turn:3", "turn:3", "预算金额是五十万元。", "conv:3", 3000)

PACKAGES = {
    "sufficient-case": package(
        "sufficient", SUFFICIENT_ITEM,
        evidence("turn:1", "季度预算已批准，金额八十万元。", 65000, "turn:1"), [], ["turn:1"]),
    "insufficient-case": package(
        "insufficient", INSUFFICIENT_ITEM,
        evidence("turn:2", "季度预算已批准。", 12000, "turn:2"),
        [{"kind": "budget_exhausted", "detail": "证据预算已用尽"}], ["turn:2"]),
    "unknown-case": package(
        "unknown", UNKNOWN_ITEM,
        evidence("turn:3", "预算金额是五十万元。", 3000, "turn:3"),
        [{"kind": "evidence_assessment_unavailable", "detail": "v2 evidence sufficiency was not assessed"}], []),
}


async def test_sufficiency_panel(page, check) -> None:
    async def open_case(name: str, version: str = "v2") -> None:
        await page.goto(f"{BASE}/search?q={name}&hybrid=1&retrieval_version={version}", wait_until="domcontentloaded")
        await page.get_by_test_id("sufficiency-panel").wait_for(timeout=15000)

    await open_case("sufficient-case")
    panel = page.get_by_test_id("sufficiency-panel")
    check("sufficiency-sufficient-label", "证据充分" in await panel.inner_text())
    evidence_list = page.get_by_test_id("sufficiency-evidence")
    text = await evidence_list.inner_text()
    check("sufficiency-evidence-text", "季度预算已批准，金额八十万元。" in text, text)
    check("sufficiency-evidence-time", "1:05" in text, text)
    href = await evidence_list.locator("a").first.get_attribute("href")
    check("sufficiency-evidence-link", href is not None and "/c/conv%3A1" in href and "item=turn%3A1" in href, str(href))

    await open_case("insufficient-case")
    panel = page.get_by_test_id("sufficiency-panel")
    check("sufficiency-insufficient-label", "证据不足" in await panel.inner_text())
    check("sufficiency-budget-gap", "budget_exhausted" in await page.get_by_test_id("sufficiency-gaps").inner_text())

    await open_case("unknown-case")
    panel = page.get_by_test_id("sufficiency-panel")
    panel_text = await panel.inner_text()
    check("sufficiency-unknown-label", "无法判定" in panel_text, panel_text)
    check("sufficiency-no-assessor-message", "未配置充分性评估器" in panel_text, panel_text)
    check("sufficiency-assessment-gap", "evidence_assessment_unavailable" in await page.get_by_test_id("sufficiency-gaps").inner_text())

    await page.goto(f"{BASE}/search?q=sufficient-case&hybrid=1&retrieval_version=v1", wait_until="domcontentloaded")
    await page.locator(".search-results").wait_for(timeout=15000)
    check("sufficiency-v1-hidden", await page.get_by_test_id("sufficiency-panel").count() == 0)

    screenshot = OUT / "sufficiency-panel.png"
    await page.goto(f"{BASE}/search?q=sufficient-case&hybrid=1&retrieval_version=v2", wait_until="domcontentloaded")
    await page.get_by_test_id("sufficiency-panel").wait_for(timeout=15000)
    await page.screenshot(path=str(screenshot), full_page=True)
    return str(screenshot)


async def main() -> dict:
    observations: dict = {"checks": [], "page_errors": [], "screenshots": []}

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
            parsed = urlparse(route.request.url)
            if parsed.path == "/api/session":
                await route.fulfill(json={"authenticated": True, "csrf_token": "synthetic-csrf", "profile": "fixture"})
            elif parsed.path == "/api/search/hybrid":
                query = (parse_qs(parsed.query).get("q") or [""])[0]
                await route.fulfill(json=PACKAGES.get(query, PACKAGES["unknown-case"]))
            elif parsed.path == "/api/topics":
                await route.fulfill(json={"experimental": True, "topics": []})
            elif parsed.path == "/healthz":
                await route.fulfill(json={"status": "ok"})
            else:
                await route.fulfill(json={})

        await page.route("**/api/**", route_api)
        try:
            observations["screenshots"].append(await test_sufficiency_panel(page, check))
        except Exception as exc:  # noqa: BLE001
            check("sufficiency-ui", False, repr(exc))
        finally:
            check("sufficiency-no-page-errors", not observations["page_errors"], "; ".join(observations["page_errors"]))
            await browser.close()

    report = {"base_url": BASE, "synthetic_only": True, **observations,
              "passed": sum(1 for c in observations["checks"] if c["status"] == "PASS"),
              "total": len(observations["checks"])}
    (OUT / "sufficiency-browser.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return report


if __name__ == "__main__":
    report = asyncio.run(main())
    failed = [c["name"] for c in report["checks"] if c["status"] == "FAIL"]
    if failed:
        raise SystemExit(f"sufficiency browser failures: {failed}")
