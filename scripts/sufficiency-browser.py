#!/usr/bin/env python3
"""Synthetic browser check for the evidence-sufficiency panel.

`vite preview` serves the built UI; every /api route is mocked. No profile,
database or audio file is touched. Proves the three statuses, the cited
evidence text, gap messages and evidence links are visible in the existing
Search page.

Also pins the explicit retrieval_version=v3 contract on the Search page: the
hybrid request keeps v3 while capabilities load slowly, return a legacy `{}`
body or report v3 unavailable, the backend's unknown/degraded answer (empty
evidence plus admission_unavailable) is displayed, and the dropdown only
offers v3 to users who did not request it while the backend advertises it.
"""
from __future__ import annotations

import asyncio
import json
import os
import time
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


def capabilities_body(v3_available: bool) -> dict:
    return {
        "version": "beta",
        "asr": {
            "provider": "qwen", "configured": True, "worker_available": True,
            "available": True, "gpu_name": "Synthetic GPU", "vram_total_mb": 8192,
            "vram_free_mb": 7000, "device": "cuda:0", "dtype": "float16",
        },
        "generator": {"enabled": True},
        "retrieval": {"default_version": "v1", "v2_available": True, "v3_available": v3_available},
        "worker": {"alive": True, "count": 1},
        "limits": {"upload_bytes": 2147483648},
    }


def admission_unavailable_package() -> dict:
    """Mirrors the backend's v3 admissionBlocked answer: no items, no evidence,
    degraded with an admission_unavailable gap and unknown sufficiency."""
    detail = "admission policy does not match the configured embedding provider"
    return {
        "items": [],
        "evidence": [],
        "coverage": {
            "conversations": [],
            "topic_ids": [],
            "time_range": {"started_after": None, "started_before": None, "unknown_time_included": False},
            "branches": {"lexical": True, "links": False, "evidence": False, "semantic": False},
            "scanned": {"notes": 0, "turns": 0},
        },
        "gaps": [{"kind": "admission_unavailable", "detail": detail}],
        "truncated": False,
        "degraded": True,
        "degrade_reason": detail,
        "trace": {
            "lexical": {"query": "预算", "hit_ids": []},
            "semantic": {"query": "预算", "backend": "hash", "indexed": 0, "hit_ids": [], "rejected_stale": 0, "rejected_scope": 0},
            "links_expanded": [],
            "admission": {"candidates": 0, "admitted": 0},
        },
        "ranking": [],
        "conversation_titles": {},
        "sufficiency": {"status": "unknown", "evidence_refs": []},
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

PACKAGES["v3-delayed-case"] = package(
    "sufficient", SUFFICIENT_ITEM,
    evidence("turn:1", "季度预算已批准，金额八十万元。", 65000, "turn:1"), [], ["turn:1"])
PACKAGES["v3-unavailable-case"] = admission_unavailable_package()


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

    await page.set_viewport_size({"width": 390, "height": 844})
    await open_case("sufficient-case")
    overflow = await page.evaluate("() => document.documentElement.scrollWidth - window.innerWidth")
    mobile_panel = page.get_by_test_id("sufficiency-panel")
    keyword_height = await page.locator(".search-input-label").evaluate("e => e.getBoundingClientRect().height")
    check("sufficiency-mobile-layout", overflow <= 1 and await mobile_panel.is_visible() and keyword_height <= 80,
          f"overflow={overflow} keyword-label-height={keyword_height}")
    mobile_shot = OUT / "sufficiency-panel-mobile.png"
    await page.screenshot(path=str(mobile_shot), full_page=True)
    await page.set_viewport_size({"width": 1280, "height": 900})

    screenshot = OUT / "sufficiency-panel.png"
    await page.goto(f"{BASE}/search?q=sufficient-case&hybrid=1&retrieval_version=v2", wait_until="domcontentloaded")
    await page.get_by_test_id("sufficiency-panel").wait_for(timeout=15000)
    await page.screenshot(path=str(screenshot), full_page=True)
    return str(screenshot)


V3_SELECT = 'select[aria-label="检索版本"]'


async def test_explicit_v3_capabilities(page, check, state, hybrid_log) -> None:
    """Explicit retrieval_version=v3 URLs keep requesting v3 no matter what the
    capabilities endpoint answers (slow, legacy `{}` or unavailable); the
    backend's unknown/degraded v3 answer is displayed; the dropdown offers v3
    only while the backend advertises it but keeps it selected for a user who
    opened a v3 URL."""

    async def wait_for_hybrid_requests(mark: int, timeout_s: float = 10.0) -> list:
        deadline = time.monotonic() + timeout_s
        while time.monotonic() < deadline:
            if len(hybrid_log) > mark:
                return hybrid_log[mark:]
            await page.wait_for_timeout(100)
        return hybrid_log[mark:]

    async def sent_versions_settled(mark: int, settle_ms: int = 400) -> list:
        # Wait for the first request, then give any re-request triggered by the
        # capabilities answer time to appear before reading the log.
        await wait_for_hybrid_requests(mark)
        await page.wait_for_timeout(settle_ms)
        entries = hybrid_log[mark:]
        return [entry["retrieval_version"] for entry in entries]

    async def select_state() -> tuple:
        select = page.locator(V3_SELECT)
        value = await select.input_value()
        options = await select.locator('option[value="v3"]').count()
        return value, options

    # Legacy `{}` capabilities body (an old backend response) with an explicit
    # v3 URL: the page must not crash, must still send v3 and keep v3 selected.
    state["capabilities"] = "legacy"
    mark = len(hybrid_log)
    await page.goto(f"{BASE}/search?q=v3-delayed-case&hybrid=1&retrieval_version=v3", wait_until="domcontentloaded")
    sent = await sent_versions_settled(mark)
    check("v3-legacy-request-versions", bool(sent) and all(v == "v3" for v in sent), str(sent))
    await page.get_by_test_id("sufficiency-panel").wait_for(timeout=15000)
    panel = page.get_by_test_id("sufficiency-panel")
    check("v3-legacy-panel-sufficient", "证据充分" in await panel.inner_text())
    value, options = await select_state()
    check("v3-legacy-dropdown-keeps-v3", value == "v3" and options == 1, f"value={value} v3-options={options}")

    # Capabilities answer late and report v3 unavailable: the hybrid request
    # already went out with v3 before the capabilities response, and no v1
    # request replaces it afterwards.
    state["capabilities"] = "delayed-unavailable"
    state["capabilities_ts"] = None
    mark = len(hybrid_log)
    await page.goto(f"{BASE}/search?q=v3-unavailable-case&hybrid=1&retrieval_version=v3", wait_until="domcontentloaded")
    await wait_for_hybrid_requests(mark)
    for _ in range(50):
        if state["capabilities_ts"] is not None:
            break
        await page.wait_for_timeout(100)
    entries = hybrid_log[mark:]
    sent = [entry["retrieval_version"] for entry in entries]
    check("v3-delayed-request-versions", bool(sent) and all(v == "v3" for v in sent), str(sent))
    check("v3-delayed-request-before-capabilities",
          bool(entries) and state["capabilities_ts"] is not None and entries[0]["ts"] < state["capabilities_ts"],
          f"hybrid_ts={entries[0]['ts'] if entries else None} capabilities_ts={state['capabilities_ts']}")
    await page.get_by_test_id("sufficiency-panel").wait_for(timeout=15000)
    status = await page.get_by_test_id("sufficiency-status").inner_text()
    check("v3-delayed-unknown-label", status == "无法判定", status)
    check("v3-delayed-empty-evidence", await page.get_by_test_id("sufficiency-evidence").count() == 0)
    value, options = await select_state()
    check("v3-delayed-dropdown-keeps-v3", value == "v3" and options == 1, f"value={value} v3-options={options}")

    # Capabilities report v3 unavailable and the backend answers the explicit
    # v3 search itself: empty evidence, admission_unavailable degradation and
    # unknown sufficiency are displayed instead of a silent v1 downgrade.
    state["capabilities"] = "unavailable"
    mark = len(hybrid_log)
    await page.goto(f"{BASE}/search?q=v3-unavailable-case&hybrid=1&retrieval_version=v3", wait_until="domcontentloaded")
    sent = await sent_versions_settled(mark)
    check("v3-unavailable-request-versions", bool(sent) and all(v == "v3" for v in sent), str(sent))
    await page.get_by_test_id("sufficiency-panel").wait_for(timeout=15000)
    status = await page.get_by_test_id("sufficiency-status").inner_text()
    check("v3-unavailable-unknown-label", status == "无法判定", status)
    check("v3-unavailable-empty-evidence", await page.get_by_test_id("sufficiency-evidence").count() == 0)
    degraded = await page.locator(".sufficiency-degraded").inner_text()
    reason = PACKAGES["v3-unavailable-case"]["degrade_reason"]
    check("v3-unavailable-degraded-message", reason in degraded, degraded)
    await page.locator("details.diagnostics summary").click()
    diagnostics = await page.locator(".diagnostics-gaps").inner_text()
    check("v3-unavailable-diagnostics-gap", "admission_unavailable" in diagnostics, diagnostics)
    value, options = await select_state()
    check("v3-unavailable-dropdown-keeps-v3", value == "v3" and options == 1, f"value={value} v3-options={options}")

    # Fresh user (no retrieval_version in the URL) with an available backend:
    # the default request stays v1 but the dropdown offers v3.
    state["capabilities"] = "available"
    mark = len(hybrid_log)
    await page.goto(f"{BASE}/search?q=v3-delayed-case&hybrid=1", wait_until="domcontentloaded")
    await page.locator(".search-results").wait_for(timeout=15000)
    sent = await sent_versions_settled(mark)
    check("v3-available-default-request-versions", bool(sent) and all(v == "v1" for v in sent), str(sent))
    value, options = await select_state()
    check("v3-available-dropdown-offers-v3", value == "v1" and options == 1, f"value={value} v3-options={options}")

    # Fresh user with an unavailable backend: no v3 option at all.
    state["capabilities"] = "unavailable"
    mark = len(hybrid_log)
    await page.goto(f"{BASE}/search?q=v3-delayed-case&hybrid=1", wait_until="domcontentloaded")
    await page.locator(".search-results").wait_for(timeout=15000)
    value, options = await select_state()
    check("v3-unavailable-dropdown-hides-v3", value == "v1" and options == 0, f"value={value} v3-options={options}")

    # Explicit v3 URL while the backend advertises v3: still one v3 request.
    state["capabilities"] = "available"
    mark = len(hybrid_log)
    await page.goto(f"{BASE}/search?q=v3-delayed-case&hybrid=1&retrieval_version=v3", wait_until="domcontentloaded")
    sent = await sent_versions_settled(mark)
    check("v3-available-request-versions", bool(sent) and all(v == "v3" for v in sent), str(sent))
    await page.get_by_test_id("sufficiency-panel").wait_for(timeout=15000)
    panel = page.get_by_test_id("sufficiency-panel")
    check("v3-available-panel-sufficient", "证据充分" in await panel.inner_text())
    value, options = await select_state()
    check("v3-available-dropdown-keeps-v3", value == "v3" and options == 1, f"value={value} v3-options={options}")


async def main() -> dict:
    observations: dict = {"checks": [], "page_errors": [], "screenshots": []}

    def check(name: str, ok: bool, detail: str = "") -> None:
        observations["checks"].append({"name": name, "status": "PASS" if ok else "FAIL", "detail": detail})

    # capabilities selects what /api/capabilities answers for the next
    # navigation; hybrid_log records every /api/search/hybrid request the page
    # sends. "legacy" keeps the historical regression: an old backend answers
    # the capabilities probe with an empty `{}` body.
    state = {"capabilities": "legacy", "capabilities_ts": None}
    hybrid_log: list = []

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
            elif parsed.path == "/api/capabilities":
                mode = state["capabilities"]
                if mode == "legacy":
                    await route.fulfill(json={})
                elif mode == "delayed-unavailable":
                    await asyncio.sleep(0.35)
                    await route.fulfill(json=capabilities_body(False))
                elif mode == "unavailable":
                    await route.fulfill(json=capabilities_body(False))
                else:
                    await route.fulfill(json=capabilities_body(True))
                state["capabilities_ts"] = time.monotonic()
            elif parsed.path == "/api/search/hybrid":
                params = parse_qs(parsed.query)
                hybrid_log.append({
                    "ts": time.monotonic(),
                    "q": (params.get("q") or [""])[0],
                    "retrieval_version": (params.get("retrieval_version") or [None])[0],
                })
                await route.fulfill(json=PACKAGES.get(params.get("q", [""])[0], PACKAGES["unknown-case"]))
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
        try:
            await test_explicit_v3_capabilities(page, check, state, hybrid_log)
        except Exception as exc:  # noqa: BLE001
            check("v3-capabilities-ui", False, repr(exc))
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
