#!/usr/bin/env python3
"""Explicit live-model smoke test. Sends only fresh synthetic fixture content.
Requires built binaries/frontend and Python Playwright. Never accepts an existing DB.
"""
import argparse
import asyncio
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import tempfile
from playwright.async_api import async_playwright

ROOT = Path(__file__).resolve().parents[1]

async def run(args):
    artifacts = Path(tempfile.mkdtemp(prefix='recording-brief-live-'))
    os.chmod(artifacts, 0o700)
    db = artifacts / 'synthetic.db'
    audio = artifacts / 'audio'
    audio.mkdir()
    profiles = artifacts / 'profiles'
    log_path = artifacts / 'synthetic-services.log'
    env = {k: v for k, v in os.environ.items() if not k.startswith(('GENERATOR_', 'BRIEF_POLISH_', 'WORLDLINE_'))}
    with log_path.open('w') as log:
        subprocess.run([str(ROOT / 'bin/context-eval'), '--db', str(db), '--out', str(artifacts / 'eval.json')], cwd=ROOT, env=env, stdout=log, stderr=log, check=True)
        fixture = subprocess.run(['python3', str(ROOT / 'scripts/synthetic-profile-fixture.py'), '--profiles-root', str(profiles), '--db', str(db), '--audio-dir', str(audio)], cwd=ROOT, env=env, capture_output=True, text=True, check=True)
        password = json.loads(fixture.stdout.splitlines()[-1])['alpha_password']
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0)); port = sock.getsockname()[1]
        env.update(MEMORY_PROFILE_PASSWORD=password, BRIEF_POLISH_PI_PROVIDER=args.pi_provider, BRIEF_POLISH_MODEL=args.model)
        if args.pi_models_file: env['BRIEF_POLISH_PI_MODELS_FILE'] = args.pi_models_file
        proc = subprocess.Popen(['bash', str(ROOT / 'scripts/local-start.sh'), 'alpha', str(port), str(profiles)], cwd=ROOT, env=env, stdout=log, stderr=log, start_new_session=True)
        base = f'http://127.0.0.1:{port}'
        checks = {}
        try:
            async with async_playwright() as pw:
                browser = await pw.chromium.launch(headless=True, executable_path=args.browser, args=['--no-sandbox'])
                try:
                    page = await browser.new_page()
                    page_errors = []
                    page.on('pageerror', lambda e: page_errors.append(str(e)))
                    for _ in range(100):
                        if proc.poll() is not None: raise RuntimeError('synthetic service exited; inspect local synthetic log')
                        try:
                            await page.goto(base, wait_until='networkidle', timeout=2000)
                            break
                        except Exception: await asyncio.sleep(.2)
                    await page.locator('#auth-credential').fill(password)
                    await page.get_by_role('button', name='进入', exact=True).click()
                    await page.locator('.profile-toolbar').wait_for(timeout=20000)
                    await page.goto(base + '/brief', wait_until='networkidle')
                    await page.locator('fieldset input[type=checkbox]').first.check()
                    async with page.expect_response(lambda r: r.url.endswith('/api/briefs/preview') and r.request.method == 'POST') as preview:
                        await page.get_by_role('button', name='生成简报预览').click()
                    original = await (await preview.value).json()
                    checks['deterministic_preview'] = bool(original.get('snapshot_id') and original.get('evidence'))
                    button = page.get_by_role('button', name='生成独立候选')
                    checks['explicit_consent_required'] = not await button.is_enabled()
                    await page.get_by_label('我同意将此范围的简报内容发送给服务端配置的模型进行润色。').check()
                    async with page.expect_response(lambda r: r.url.endswith('/api/briefs/polish') and r.request.method == 'POST', timeout=45000) as polished:
                        await button.click()
                    response = await polished.value
                    result = await response.json()
                    candidates = result.get('candidates', [])
                    checks['live_model_candidates'] = response.status == 200 and not result.get('degraded', True) and bool(candidates)
                    entries = {entry['id']: entry for section in original['sections'].values() for entry in section}
                    checks['closed_current_evidence_proposed'] = bool(candidates) and all(c['assertion_state'] == 'proposed' and c['entry_id'] in entries and sorted(c['evidence_ids']) == sorted(entries[c['entry_id']]['evidence_ids']) for c in candidates)
                    checks['snapshot_preserved'] = result.get('snapshot_id') == original['snapshot_id']
                    if candidates:
                        panel = page.get_by_label('AI 润色候选', exact=True)
                        await panel.get_by_role('button', name='查看关联证据').first.click()
                        checks['candidate_evidence_expands'] = await panel.locator('blockquote').count() > 0
                    checks['no_page_errors'] = not page_errors
                    await page.screenshot(path=str(artifacts / 'live-brief.png'), full_page=True)
                finally:
                    await browser.close()
        finally:
            if proc.poll() is None: os.killpg(proc.pid, signal.SIGTERM)
            try: proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(proc.pid, signal.SIGKILL); proc.wait()
    report = {'synthetic_only': True, 'provider': args.pi_provider, 'model': args.model, 'checks': checks, 'passed': bool(checks) and all(checks.values())}
    (artifacts / 'live-brief-results.json').write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n')
    print(json.dumps(report, ensure_ascii=False))
    print('Artifacts:', artifacts)
    return 0 if report['passed'] else 1

if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--pi-provider', required=True)
    parser.add_argument('--model', required=True)
    parser.add_argument('--pi-models-file')
    parser.add_argument('--browser', default='/usr/bin/google-chrome')
    raise SystemExit(asyncio.run(run(parser.parse_args())))
