#!/usr/bin/env python3.14
"""Verify opt-in retrieval version selection in the synthetic browser profile."""
import asyncio, json, os, pathlib
from urllib.parse import urlencode, urlparse, parse_qs
from playwright.async_api import async_playwright

BASE = os.environ.get('BASE_URL', 'http://127.0.0.1:8788').rstrip('/')
PASSWORD = os.environ.get('ALPHA_PASSWORD', '')
OUT = pathlib.Path(os.environ.get('BROWSER_ARTIFACT_DIR', '/tmp'))
OUT.mkdir(parents=True, exist_ok=True)

async def main():
    cases = []
    async with async_playwright() as p:
        opts = {'headless': True, 'args': ['--no-sandbox']}
        if os.environ.get('BROWSER_PATH'):
            opts['executable_path'] = os.environ['BROWSER_PATH']
        browser = await p.chromium.launch(**opts)
        page = await browser.new_page()
        page_errors = []
        page.on('pageerror', lambda e: page_errors.append(str(e)))
        try:
            await page.goto(BASE, wait_until='domcontentloaded')
            if await page.locator('#auth-credential').count():
                if await page.locator('#auth-profile').count():
                    await page.locator('#auth-profile').select_option('alpha')
                await page.locator('#auth-credential').fill(PASSWORD)
                await page.get_by_role('button', name='进入').click()
                await page.locator('.profile-toolbar').wait_for(timeout=20000)

            query = urlencode({'q': 'synthetic retrieval probe', 'hybrid': '1'})
            async with page.expect_response(lambda r: '/api/search/hybrid' in r.url, timeout=15000) as initial_response:
                await page.goto(f'{BASE}/search?{query}', wait_until='domcontentloaded')
            initial = await initial_response.value
            assert initial.status == 200, initial.status
            assert "sufficiency" not in await initial.json()
            initial_url = initial.url
            selector = page.get_by_role('combobox', name='检索版本')
            await selector.wait_for(timeout=10000)
            default_value = await selector.input_value()
            initial_version = parse_qs(urlparse(initial_url).query).get('retrieval_version', [''])[0]
            assert default_value == 'v1' and initial_version == 'v1', (default_value, initial_url)
            cases.append({'name': 'hybrid-default-is-v1', 'status': 'PASS'})

            async with page.expect_response(lambda r: '/api/search/hybrid' in r.url and 'retrieval_version=v2' in r.url, timeout=15000) as v2_response:
                await selector.select_option('v2')
            v2 = await v2_response.value
            assert v2.status == 200, v2.status
            assert "sufficiency" in await v2.json()
            v2_url = v2.url
            assert parse_qs(urlparse(v2_url).query).get('retrieval_version') == ['v2'], v2_url
            assert 'retrieval_version=v2' in page.url, page.url
            cases.append({'name': 'v2-selection-updates-request-and-url', 'status': 'PASS'})

            await page.reload(wait_until='domcontentloaded')
            assert await page.get_by_role('combobox', name='检索版本').input_value() == 'v2'
            cases.append({'name': 'v2-selection-restores-from-url', 'status': 'PASS'})
            assert not page_errors, '; '.join(page_errors)
        except Exception as exc:
            cases.append({'name': 'retrieval-version-ui', 'status': 'FAIL', 'detail': str(exc)})
        finally:
            report = {'base_url': BASE, 'synthetic_profile': 'alpha', 'cases': cases, 'page_errors': page_errors}
            (OUT / 'retrieval-version-browser-results.json').write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n')
            print(json.dumps(report, ensure_ascii=False, indent=2))
            await browser.close()
    if any(case['status'] == 'FAIL' for case in cases):
        raise SystemExit(1)

if __name__ == '__main__':
    asyncio.run(main())
