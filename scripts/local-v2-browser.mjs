#!/usr/bin/env node
// Local-only profile-auth browser smoke test using the existing Python Playwright install.
// Usage: BASE_URL=http://127.0.0.1:PORT PROFILE_PASSWORD=... node scripts/local-v2-browser.mjs
// Optional profile switching: SECOND_PROFILE=... SECOND_PROFILE_PASSWORD=...
import { spawnSync } from 'node:child_process'

if (!process.env.BASE_URL) throw new Error('Set BASE_URL to the already-running local browser fixture/server')
if (!process.env.PROFILE_PASSWORD && !process.env.PASSWORD) throw new Error('Set PROFILE_PASSWORD (or PASSWORD) for the fixture profile')
const python = process.env.PYTHON || 'python3.14'
const code = String.raw`
import json, os, sys, shutil
from playwright.sync_api import sync_playwright
base = os.environ['BASE_URL']
with sync_playwright() as p:
    browser = p.chromium.launch(headless=True, executable_path=os.environ.get('BROWSER_PATH') or shutil.which('google-chrome') or None)
    page = browser.new_page()
    errors = []
    page.on('pageerror', lambda error: errors.append(str(error)))
    response = page.request.get(base.rstrip('/') + '/api/auth-info')
    assert response.status == 200, 'auth-info should be available'
    info = response.json()
    assert info.get('mode') in ('profiles', 'profile'), 'expected profile auth, got ' + str(info.get('mode'))
    primary = (os.environ.get('PROFILE') or ((info.get('profiles') or [None])[0])) if info['mode'] == 'profiles' else info.get('profile')
    assert primary, 'No fixture profile is available'
    page.goto(base)
    if info['mode'] == 'profiles': page.get_by_label('配置').select_option(primary)
    page.get_by_label('密码').fill(os.environ.get('PROFILE_PASSWORD') or os.environ['PASSWORD'])
    page.get_by_role('button', name='进入').click()
    page.get_by_text('配置：' + primary, exact=True).wait_for()
    session = page.request.get(base.rstrip('/') + '/api/session').json()
    assert session.get('profile') == primary, 'session profile mismatch'
    second = os.environ.get('SECOND_PROFILE')
    if second:
        assert info['mode'] == 'profiles', 'switch requires profiles dispatcher'
        assert second in info.get('profiles', []), 'SECOND_PROFILE is not listed by auth-info'
        assert os.environ.get('SECOND_PROFILE_PASSWORD'), 'Set SECOND_PROFILE_PASSWORD when switching'
        page.get_by_role('button', name='退出登录').click()
        page.get_by_label('配置').select_option(second)
        page.get_by_label('密码').fill(os.environ['SECOND_PROFILE_PASSWORD'])
        page.get_by_role('button', name='进入').click()
        page.get_by_text('配置：' + second, exact=True).wait_for()
        assert page.request.get(base.rstrip('/') + '/api/session').json().get('profile') == second
    assert not errors, 'browser page errors: ' + '; '.join(errors)
    print(json.dumps({'baseURL': base, 'mode': info['mode'], 'profile': primary, 'switchedTo': second, 'pageErrors': errors}, ensure_ascii=False, indent=2))
    browser.close()
`
const result = spawnSync(python, ['-c', code], { encoding: 'utf8', env: process.env })
if (result.stdout) process.stdout.write(result.stdout)
if (result.stderr) process.stderr.write(result.stderr)
if (result.error) throw result.error
process.exitCode = result.status ?? 1
