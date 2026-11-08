#!/usr/bin/env python3
"""Synthetic Playwright regression for detail processing polling and retries."""
import asyncio
import json
import os
import sys

from playwright.async_api import async_playwright

BASE = os.environ.get("BASE_URL", "http://127.0.0.1:8798").rstrip("/")
CONVERSATION = "synthetic-poll-conversation"


async def main():
    # The local preview must not go through a workstation HTTP proxy.
    for key in ("NO_PROXY", "no_proxy"):
        os.environ[key] = ",".join(filter(None, [os.environ.get(key, ""), "127.0.0.1", "localhost"]))
    observations = {"processing": 0, "retry": [], "page_errors": []}
    retry_success = False
    async with async_playwright() as p:
        options = {"headless": True, "args": ["--no-sandbox", "--no-proxy-server"]}
        if os.environ.get("BROWSER_PATH"):
            options["executable_path"] = os.environ["BROWSER_PATH"]
        browser = await p.chromium.launch(**options)
        page = await browser.new_page()
        page.on("pageerror", lambda error: observations["page_errors"].append(str(error)))
        page.on("requestfailed", lambda request: observations.setdefault("request_failed", []).append({"url": request.url, "failure": request.failure}))
        page.on("console", lambda message: observations.setdefault("console", []).append(message.text) if message.type == "error" else None)

        async def route_api(route):
            nonlocal retry_success
            req = route.request
            path = req.url.split("?", 1)[0]
            if path.endswith("/api/session"):
                await route.fulfill(json={"csrf_token": "synthetic-csrf", "profile": "synthetic"})
            elif path.endswith("/api/auth-info"):
                await route.fulfill(json={"mode": "profile", "profile": "synthetic"})
            elif path.endswith(f"/api/conversations/{CONVERSATION}"):
                status = {"conversation_id": CONVERSATION, "received_segments": 0,
                          "original_turns": 0, "topic_linked_turns": 0,
                          "unclassified_turns": 0, "ambient_turns": 0,
                          "identity_unconfirmed_turns": 0,
                          "analysis_status": "pending", "indexing_status": "pending"}
                await route.fulfill(json={"conversation_id": CONVERSATION, "title": "Synthetic polling",
                    "summary": "", "annotation_version": "", "annotation_basis": "",
                    "recording_started_at": None, "recordings": [], "topics": [], "notes": [], "processing": status})
            elif path.endswith(f"/api/conversations/{CONVERSATION}/processing/retry"):
                payload = json.loads(req.post_data or "{}")
                observations["retry"].append(payload)
                if len(observations["retry"]) == 1:
                    await route.fulfill(status=503, json={"error": "synthetic retry interruption"})
                else:
                    retry_success = True
                    await route.fulfill(json={"status": "queued"})
            elif path.endswith(f"/api/conversations/{CONVERSATION}/processing"):
                observations["processing"] += 1
                # The initial pending state includes null arrays to cover old-server
                # responses. After retry succeeds, transition to terminal immediately.
                if not retry_success:
                    body = {"status": {"analysis_status": "pending", "indexing_status": "pending"},
                            "jobs": [{"id": "job-synthetic", "stage": "ingest", "status": "failed",
                                      "attempt_count": 1, "max_attempts": 2}],
                            "workers": None, "generator_enabled": False,
                            "phases": [{"name": "ingest", "state": "failed", "job_id": "job-synthetic",
                                        "attempt": 1, "error": "synthetic failure",
                                        "error_code": "asr_unavailable", "error_action": "配置 ASR 后重试"},
                                       {"name": "asr", "state": "skipped"},
                                       {"name": "extract", "state": "skipped"},
                                       {"name": "publish", "state": "skipped"},
                                       {"name": "index", "state": "skipped"}],
                            "current_stage": "ingest", "asr_provider": "off",
                            "asr_available": False, "worker_alive": False}
                else:
                    body = {"status": {"analysis_status": "ready", "indexing_status": "ready"},
                            "jobs": None,
                            "workers": None, "generator_enabled": False,
                            "phases": [{"name": "ingest", "state": "done"},
                                       {"name": "asr", "state": "done"},
                                       {"name": "extract", "state": "done"},
                                       {"name": "publish", "state": "skipped"},
                                       {"name": "index", "state": "done"}],
                            "current_stage": "completed", "asr_provider": "fixture",
                            "asr_available": True, "worker_alive": True}
                await route.fulfill(json=body)
            elif path.endswith("/annotations/history"):
                await route.fulfill(json={"revisions": []})
            elif path.endswith("/turns"):
                await route.fulfill(json={"conversation_id": CONVERSATION, "recording_id": "", "items": [], "next_offset": None})
            else:
                await route.fulfill(status=200, json={})

        await page.route("**/api/**", route_api)
        await page.goto(f"{BASE}/c/{CONVERSATION}", wait_until="networkidle")
        try:
            await page.get_by_role("heading", name="Synthetic polling").wait_for(timeout=8000)
        except Exception:
            raise AssertionError(f"synthetic detail did not load: {observations}; body={await page.locator('body').inner_text()}")
        await page.locator(".processing-progress").wait_for()
        progress_text = await page.locator(".processing-progress").inner_text()
        assert "候选生成器状态未知" in progress_text, "missing heartbeat must report unknown generator state"
        assert "语音转写：未配置" in progress_text, progress_text
        assert "配置 ASR 后重试" in progress_text, progress_text
        phase = page.locator('[data-testid="phase-ingest"]')
        assert await phase.get_attribute("data-state") == "failed", progress_text
        assert await page.locator('[data-testid="phase-asr"]').get_attribute("data-state") == "skipped", progress_text
        # The poller must continue during pending work.
        for _ in range(120):
            if observations["processing"] >= 2:
                break
            await page.wait_for_timeout(100)
        assert observations["processing"] >= 2, f"pending work did not poll again: {observations}"
        retry = page.get_by_role("button", name="重试")
        await retry.click()
        await page.get_by_role("alert").wait_for()
        await retry.click()
        for _ in range(40):
            if retry_success and observations["processing"] >= 3:
                break
            await page.wait_for_timeout(100)
        assert len(observations["retry"]) == 2, observations
        assert observations["retry"][0]["request_id"] == observations["retry"][1]["request_id"], observations["retry"]
        assert observations["processing"] >= 3, observations
        count_at_terminal = observations["processing"]
        # Idle pending work polls every ten seconds; wait beyond that interval
        # so a mistakenly continuing terminal poll is observable here.
        await page.wait_for_timeout(10500)
        assert observations["processing"] == count_at_terminal, f"terminal state kept polling: {observations}"
        assert not observations["page_errors"], observations["page_errors"]
        print(json.dumps({"status": "PASS", "synthetic": True, **observations}, indent=2))
        await browser.close()


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except Exception as exc:
        print(f"FAIL: {exc}", file=sys.stderr)
        raise
