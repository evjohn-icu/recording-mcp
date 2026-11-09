#!/usr/bin/env python3.14
"""Product workflow browser checks against a real local profile + worker.

The runner (scripts/run-product-browser.py) starts context-web and
context-worker with the fixture ASR provider, creates a 6 s WAV and drives the
built SPA. No network and no private audio are involved.
"""
from __future__ import annotations

import asyncio
import json
import os
import pathlib
import tempfile
import wave

from playwright.async_api import async_playwright

BASE = os.environ.get("BASE_URL", "http://127.0.0.1:8788").rstrip("/")
ALPHA = os.environ.get("ALPHA_PASSWORD", "")
BETA = os.environ.get("BETA_PASSWORD", "")
OUT = pathlib.Path(os.environ.get("BROWSER_ARTIFACT_DIR", "/tmp"))
OUT.mkdir(parents=True, exist_ok=True)

CONVERSATION_TITLE = "ui-fixture.wav"


def make_wav(path: pathlib.Path, seconds: int = 6) -> None:
    with wave.open(str(path), "wb") as handle:
        handle.setnchannels(1)
        handle.setsampwidth(2)
        handle.setframerate(16000)
        handle.writeframes(b"\x00\x00" * (16000 * seconds))


async def login(page, profile: str, password: str) -> None:
    await page.goto(BASE, wait_until="domcontentloaded")
    if await page.locator("#auth-credential").count():
        if await page.locator("#auth-profile").count():
            await page.locator("#auth-profile").select_option(profile)
        await page.locator("#auth-credential").fill(password)
        await page.get_by_role("button", name="进入").click()
        await page.locator(".profile-toolbar").wait_for(timeout=20000)


async def test_product_workflow(page, base: str, wav: pathlib.Path, record) -> str:
    """Steps 2-8: onboarding, import, phases, transcript, search, evidence, audio."""
    await page.goto(f"{base}/recordings", wait_until="domcontentloaded")
    record("web-profile-login", True)
    await page.get_by_test_id("empty-onboarding").wait_for(timeout=15000)
    record("web-empty-onboarding", await page.locator(".timeline-row").count() == 0
           and await page.get_by_test_id("import-panel").is_visible())
    await page.get_by_test_id("import-file-input").set_input_files(str(wav))
    await page.get_by_test_id("import-file-name").wait_for()
    record("web-select-file", (await page.get_by_test_id("import-file-name").inner_text()) == CONVERSATION_TITLE,
           await page.get_by_test_id("import-file-name").inner_text())
    await page.get_by_test_id("import-submit").click()
    await page.wait_for_url("**/c/**", timeout=30000)
    conversation_url = page.url
    await page.get_by_test_id("processing-phases").wait_for(timeout=20000)
    terminal = False
    for _ in range(150):
        states = await page.locator('[data-testid^="phase-"]').evaluate_all(
            "nodes => nodes.map(n => n.getAttribute('data-state'))")
        if len(states) == 5 and all(state in ("done", "skipped") for state in states):
            terminal = True
            break
        if any(state == "failed" for state in states):
            break
        await page.wait_for_timeout(500)
    detail_text = await page.locator(".processing-progress").inner_text()
    record("web-processing-phases", terminal, detail_text)
    coverage = page.get_by_test_id("asr-coverage")
    try:
        await coverage.wait_for(state="visible", timeout=15000)
        coverage_visible = True
    except Exception:
        coverage_visible = False
    coverage_text = await coverage.inner_text() if coverage_visible else ""
    record("web-asr-coverage", coverage_visible and "窗口" in coverage_text,
           f"visible={coverage_visible} text={coverage_text[:120]}")
    await page.locator(".turn-row").first.wait_for(timeout=20000)
    transcript = await page.locator(".turn-row").first.inner_text()
    record("web-transcript-visible", "fixture transcript" in transcript, transcript[:160])
    await page.goto(f"{base}/search?q=fixture", wait_until="domcontentloaded")
    await page.locator(".search-result-row").first.wait_for(timeout=20000)
    record("web-search-result", True)
    await page.locator(".search-result-row").first.click()
    await page.get_by_role("dialog", name="证据窗口").wait_for(timeout=20000)
    evidence_text = await page.get_by_role("dialog", name="证据窗口").inner_text()
    record("web-evidence", "fixture transcript" in evidence_text, evidence_text[:160])
    await page.get_by_role("dialog", name="证据窗口").get_by_role("button", name="播放").first.click()
    await page.locator("audio").first.wait_for(timeout=10000)
    played = 0.0
    for _ in range(50):
        played = await page.locator("audio").first.evaluate("el => el.currentTime")
        if played > 0:
            break
        await page.wait_for_timeout(200)
    record("web-audio-playback", played > 0, f"currentTime={played}")
    return conversation_url


async def test_refresh_restores_state(page, base: str, conversation_url: str, record) -> None:
    await page.goto(conversation_url, wait_until="domcontentloaded")
    await page.locator(".turn-row").first.wait_for(timeout=20000)
    record("web-refresh-detail", CONVERSATION_TITLE in await page.locator(".detail-header").inner_text()
           and "fixture transcript" in await page.locator(".turn-row").first.inner_text()
           and await page.get_by_test_id("processing-phases").is_visible())
    await page.goto(f"{base}/recordings", wait_until="domcontentloaded")
    await page.locator(".timeline-row").first.wait_for(timeout=20000)
    record("web-refresh-list", CONVERSATION_TITLE in await page.locator(".timeline-row").first.inner_text())


async def test_mobile_and_desktop_layout(page, base: str, conversation_url: str, record, out: pathlib.Path, screenshots: list) -> None:
    await page.set_viewport_size({"width": 390, "height": 844})
    await page.goto(conversation_url, wait_until="domcontentloaded")
    await page.get_by_test_id("processing-phases").wait_for(timeout=20000)
    overflow = await page.evaluate("() => document.documentElement.scrollWidth - window.innerWidth")
    mobile_ok = (overflow <= 1 and await page.get_by_test_id("import-panel").count() == 0
                 and await page.get_by_test_id("asr-coverage").is_visible())
    mobile_shot = out / "product-workflow-mobile.png"
    await page.screenshot(path=str(mobile_shot), full_page=True)
    screenshots.append(str(mobile_shot))
    await page.set_viewport_size({"width": 1440, "height": 900})
    await page.goto(f"{base}/recordings", wait_until="domcontentloaded")
    await page.get_by_test_id("import-submit").wait_for(timeout=20000)
    overflow_desktop = await page.evaluate("() => document.documentElement.scrollWidth - window.innerWidth")
    record("web-responsive-layout", mobile_ok and overflow_desktop <= 1,
           f"mobile-overflow={overflow} desktop-overflow={overflow_desktop}")


async def test_profile_isolation(page, base: str, beta_password: str, record) -> None:
    await page.get_by_role("button", name="退出登录").click()
    await page.locator("#auth-credential").wait_for(timeout=20000)
    await login(page, "beta", beta_password)
    await page.goto(f"{base}/recordings", wait_until="domcontentloaded")
    await page.locator(".timeline-list, .empty-onboarding").first.wait_for(timeout=20000)
    beta_rows = await page.locator(".timeline-row").count()
    record("web-profile-isolation", beta_rows == 0 and CONVERSATION_TITLE not in await page.locator("body").inner_text(),
           f"rows={beta_rows}")


def test_no_blocking_page_errors(observations: dict, record) -> None:
    record("web-no-page-errors", not observations["page_errors"], "; ".join(observations["page_errors"]))


async def run() -> dict:
    observations: dict = {"cases": [], "page_errors": [], "screenshots": []}
    wav = pathlib.Path(tempfile.mkdtemp(prefix="recording-product-audio-")) / CONVERSATION_TITLE
    make_wav(wav)

    def record(name: str, ok: bool, detail: str = "") -> None:
        observations["cases"].append({"name": name, "status": "PASS" if ok else "FAIL", "detail": detail})

    async with async_playwright() as p:
        options = {"headless": True, "args": ["--no-sandbox", "--autoplay-policy=no-user-gesture-required"]}
        if os.environ.get("BROWSER_PATH"):
            options["executable_path"] = os.environ["BROWSER_PATH"]
        browser = await p.chromium.launch(**options)
        context = await browser.new_context(viewport={"width": 1280, "height": 900})
        page = await context.new_page()
        page.on("pageerror", lambda error: observations["page_errors"].append(str(error)))
        try:
            await login(page, "alpha", ALPHA)
            conversation_url = await test_product_workflow(page, BASE, wav, record)
            screenshot = OUT / "product-workflow-desktop.png"
            await page.screenshot(path=str(screenshot), full_page=True)
            observations["screenshots"].append(str(screenshot))
            await test_refresh_restores_state(page, BASE, conversation_url, record)
            await test_mobile_and_desktop_layout(page, BASE, conversation_url, record, OUT, observations["screenshots"])
            await test_profile_isolation(page, BASE, BETA, record)
        except Exception as exc:  # noqa: BLE001 - reported as a failed case
            record("web-workflow", False, repr(exc))
        finally:
            test_no_blocking_page_errors(observations, record)
            await browser.close()

    report = {"base_url": BASE, "conversation_title": CONVERSATION_TITLE, **observations,
              "passed": sum(1 for c in observations["cases"] if c["status"] == "PASS"),
              "total": len(observations["cases"])}
    (OUT / "product-workflow-results.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return report


if __name__ == "__main__":
    report = asyncio.run(run())
    required = {
        "web-profile-login", "web-empty-onboarding", "web-select-file", "web-processing-phases",
        "web-transcript-visible", "web-search-result", "web-evidence", "web-audio-playback",
        "web-refresh-detail", "web-refresh-list", "web-responsive-layout", "web-profile-isolation",
        "web-no-page-errors",
    }
    failed = [case["name"] for case in report["cases"] if case["status"] == "FAIL"]
    missing = required - {case["name"] for case in report["cases"]}
    if failed or missing:
        raise SystemExit(f"product browser failures: {failed}; missing: {sorted(missing)}")
