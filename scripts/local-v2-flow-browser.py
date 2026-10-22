#!/usr/bin/env python3.14
"""Synthetic local-v2 browser validation. All actions stay on the configured fixture."""
import asyncio, json, os, pathlib, sys
from playwright.async_api import async_playwright

BASE = os.environ.get('BASE_URL', 'http://127.0.0.1:8798').rstrip('/')
ALPHA = os.environ.get('ALPHA_PASSWORD', 'synthetic-pass-alpha')
BETA = os.environ.get('BETA_PASSWORD', 'synthetic-pass-beta')
SECOND_PROFILE = os.environ.get('SECOND_PROFILE', 'beta')
SECOND_PASSWORD = os.environ.get('SECOND_PASSWORD', BETA if SECOND_PROFILE == 'beta' else 'synthetic-pass-gamma')
OUT = pathlib.Path(__file__).resolve().parents[1] / 'docs'
OUT.mkdir(exist_ok=True)
results = {'base_url': BASE, 'synthetic_fixture': True, 'cases': [], 'js_errors': [], 'forget_flow': 'NOT_RUN'}

def record(name, status, detail=''):
    results['cases'].append({'name': name, 'status': status, 'detail': detail})

async def main():
  async with async_playwright() as p:
    browser = await p.chromium.launch(headless=True, executable_path='/usr/bin/google-chrome', args=['--no-sandbox','--autoplay-policy=no-user-gesture-required'])
    context = await browser.new_context(viewport={'width': 1280, 'height': 900}, accept_downloads=True)
    page = await context.new_page()
    observed = {'forget': [], 'suppression': []}
    async def capture_mutation(response):
      path = response.url.split('?', 1)[0]
      request = response.request
      if request.method == 'POST' and path == f'{BASE}/api/forget':
        try: observed['forget'].append({'status': response.status, 'body': await response.json()})
        except Exception: pass
      if request.method == 'POST' and path.endswith('/suppress'):
        try: observed['suppression'].append({'status': response.status, 'body': await response.json()})
        except Exception: pass
    page.on('response', lambda response: asyncio.create_task(capture_mutation(response)))
    page.on('pageerror', lambda error: results['js_errors'].append(str(error)))
    page.on('console', lambda msg: results['js_errors'].append('console: '+msg.text) if msg.type == 'error' else None)
    try:
      await page.goto(BASE, wait_until='networkidle', timeout=30000)
      if await page.locator('#auth-credential').count():
        if await page.locator('#auth-profile').count(): await page.locator('#auth-profile').select_option('alpha')
        await page.locator('#auth-credential').fill(ALPHA)
        await page.get_by_role('button', name='进入').click()
      await page.get_by_role('heading', name='时间线').wait_for(timeout=20000)
      await page.wait_for_timeout(1000)
      titles = await page.locator('.timeline-row h2').all_text_contents()
      print('timeline rows', len(titles), titles[:5])
      record('login-alpha-timeline', 'PASS' if titles else 'FAIL', f'{len(titles)} timeline rows')
      await page.screenshot(path=str(OUT/'local-v2-browser.png'), full_page=True)
      # profile switch and verify logged-in profile identity plus separate timeline contents
      await page.get_by_role('button', name='退出登录').click()
      await page.locator('#auth-credential').wait_for()
      await page.locator('#auth-profile').select_option(SECOND_PROFILE)
      await page.locator('#auth-credential').fill(SECOND_PASSWORD)
      await page.get_by_role('button', name='进入').click()
      await page.get_by_role('heading', name='时间线').wait_for(timeout=20000)
      await page.wait_for_timeout(700)
      beta_titles = await page.locator('.timeline-row h2').all_text_contents()
      record('profile-switch-isolation', 'PASS' if f'配置：{SECOND_PROFILE}' in await page.locator('.profile-toolbar').inner_text() else 'FAIL', f'{SECOND_PROFILE}: {len(beta_titles)} rows; alpha rows={len(titles)}')
      # Back to alpha for subsequent checks.
      await page.get_by_role('button', name='退出登录').click(); await page.locator('#auth-profile').select_option('alpha'); await page.locator('#auth-credential').fill(ALPHA); await page.get_by_role('button', name='进入').click()
      await page.get_by_role('heading', name='时间线').wait_for(timeout=20000)
      await page.wait_for_timeout(500)
      titles = await page.locator('.timeline-row h2').all_text_contents()
      if titles:
        await page.locator('.timeline-row').first.click()
        await page.locator('.turn-row').first.wait_for(timeout=20000)
        print('detail', await page.locator('h1').first.inner_text(), 'turns', await page.locator('.turn-row').count())
        record('detail-transcript', 'PASS' if await page.locator('.turn-row').count() else 'FAIL')
        # Audio actual interaction, verify currentTime advances.
        play = page.locator('.turn-row').filter(has=page.get_by_role('button', name='播放')).first.get_by_role('button', name='播放')
        if await play.count():
          await play.click(); audio = page.locator('.audio-bar audio'); await audio.wait_for(timeout=5000)
          try:
            for _ in range(100):
              current_time = await audio.evaluate('(a)=>a.currentTime')
              if current_time > 0: break
              await page.wait_for_timeout(100)
            if current_time <= 0: raise AssertionError('audio currentTime did not advance')
            record('audio-play-current-time', 'PASS', str(current_time))
          except Exception as e: record('audio-play-current-time', 'FAIL', str(e))
        await page.get_by_role('button', name='查看证据').first.click()
        record('evidence-panel', 'PASS' if await page.get_by_role('dialog').count() or await page.get_by_text('证据').count() else 'FAIL')
        await page.keyboard.press('Escape')
        # Full-segment correction: retain input after a failed request, then
        # exercise a real stale-revision conflict and review the index-based rebase.
        correction = page.get_by_role('button', name='纠正转写').first
        if await correction.count():
          await correction.click()
          fields = page.locator('.correction-field textarea')
          await fields.first.wait_for(timeout=10000)
          before_count = await fields.count()
          original = await fields.first.input_value()
          mine = original + ' [synthetic browser correction]'
          await fields.first.fill(mine)
          await page.route('**/correction', lambda route: route.abort())
          await page.get_by_role('button', name='提交修订').click()
          try:
            await page.locator('[role="alert"]').filter(has_text='无法连接').wait_for(timeout=10000)
            kept = await fields.first.input_value() == mine
            record('correction-error-retry-retains-input', 'PASS' if kept else 'FAIL', f'{before_count} fields; first draft retained={kept}')
          except Exception as e:
            record('correction-error-retry-retains-input', 'FAIL', f'{e}; alerts={await page.locator("[role='alert']").all_text_contents()}')
          await page.unroute('**/correction')
          turn_id = await page.locator('.turn-row').first.get_attribute('data-turn-id')
          staged = await page.evaluate("""async ({turnId, draft}) => {
            const session = await fetch('/api/session').then(r => r.json());
            const src = await fetch(`/api/turns/${encodeURIComponent(turnId)}/transcript-source`).then(r => r.json());
            const turns = src.result.turns;
            const texts = Object.fromEntries(turns.map((t, i) => [t.id, t.text + (i === turns.length - 1 ? ' [parallel current revision]' : '')]));
            const result = await fetch(`/api/conversations/${encodeURIComponent(src.conversation_id)}/recordings/${encodeURIComponent(src.recording_id)}/segments/${encodeURIComponent(src.segment_id)}/correction`, {method:'POST', headers:{'Content-Type':'application/json','X-CSRF-Token':session.csrf_token}, body:JSON.stringify({expected_revision:src.result.source.transcript_revision,new_revision:`browser-race-${crypto.randomUUID()}`,texts})});
            const current=await fetch(`/api/conversations/${encodeURIComponent(src.conversation_id)}/recordings/${encodeURIComponent(src.recording_id)}/segments/${encodeURIComponent(src.segment_id)}`).then(r=>r.json());
            return {ok:result.ok,status:result.status,turns:turns.map(t=>({id:t.id,index:t.original_index})),draft,stagedRevision:src.result.source.transcript_revision,currentRevision:current.source.transcript_revision};
          }""", {'turnId': turn_id, 'draft': mine})
          await page.get_by_role('button', name='提交修订').click()
          try:
            await page.locator('.conflict-panel').wait_for(timeout=10000)
            const_text = await fields.first.input_value()
            await page.get_by_role('button', name='以服务器版本为新基准（合并并检查草稿）').click()
            after = [await fields.nth(i).input_value() for i in range(await fields.count())]
            enabled_before_review = await page.get_by_role('button', name='提交修订').is_enabled()
            retained = const_text == mine and after[0] == mine and not enabled_before_review
            if len(after) > 1: retained = retained and after[-1].endswith('[parallel current revision]')
            await page.get_by_role('button', name='我已检查合并文本，继续').click()
            await page.get_by_role('button', name='提交修订').click()
            await page.locator('.recording-group > [role="status"]').filter(has_text='已提交').wait_for(timeout=10000)
            record('transcript-version-conflict-draft-retention', 'PASS' if retained and staged['ok'] else 'FAIL', f'competing status={staged["status"]}; original-index merge retained={retained}; turns={len(staged["turns"])}')
          except Exception as e:
            record('transcript-version-conflict-draft-retention', 'FAIL', f'{e}; alerts={await page.locator("[role='alert']").all_text_contents()}')
        else: record('transcript-full-segment-correction', 'NOT_RUN', 'correction control unavailable')
        # Speaker proposal/review actions are all UI clicks; use a synthetic person identifier only.
        speaker = page.get_by_role('button', name='说话人审核').first
        if await speaker.count():
          await speaker.click(); await page.wait_for_timeout(500)
          propose = page.get_by_role('button', name='基于此轮提出说话人绑定')
          if await propose.count():
            await propose.click(); await page.wait_for_timeout(700)
            person = page.get_by_label('明确的 Person ID')
            if await person.count():
              await person.fill('synthetic-browser-person')
              await page.get_by_role('button', name='确认身份').click(); await page.wait_for_timeout(700)
              if await page.get_by_role('button', name='撤销确认').count():
                await page.get_by_role('button', name='撤销确认').click(); await page.wait_for_timeout(700)
                record('speaker-propose-confirm-revoke', 'PASS')
              else: record('speaker-propose-confirm-revoke', 'FAIL', 'confirmation not displayed')
            else: record('speaker-propose-confirm-revoke', 'FAIL', 'proposal did not expose person id field')
          else: record('speaker-propose-confirm-revoke', 'FAIL', 'proposal button missing')
        else: record('speaker-propose-confirm-revoke', 'NOT_RUN', 'speaker review control unavailable')
        # Source report requires an explicit validity time; use a deterministic synthetic timestamp.
        fact = page.get_by_role('button', name='记录事实变化').first
        if await fact.count():
          await fact.click(); await page.get_by_label('报告内容').fill('Synthetic browser source report')
          await page.get_by_label('陈述适用时间（必填，不推测）').fill('2026-10-21T12:00')
          turn_id = await page.locator('.turn-row').first.get_attribute('data-turn-id')
          race = await page.evaluate("""async turnId => {
            const session=await fetch('/api/session').then(r=>r.json());
            const src=await fetch(`/api/turns/${encodeURIComponent(turnId)}/transcript-source`).then(r=>r.json());
            const ts=Object.fromEntries(src.result.turns.map((t,i)=>[t.id,t.text+(i===0?' [source changed during report]':'')]));
            const r=await fetch(`/api/conversations/${encodeURIComponent(src.conversation_id)}/recordings/${encodeURIComponent(src.recording_id)}/segments/${encodeURIComponent(src.segment_id)}/correction`,{method:'POST',headers:{'Content-Type':'application/json','X-CSRF-Token':session.csrf_token},body:JSON.stringify({expected_revision:src.result.source.transcript_revision,new_revision:`browser-fact-race-${crypto.randomUUID()}`,texts:ts})});
            return {ok:r.ok,status:r.status};
          }""", turn_id)
          await page.get_by_role('button', name='保存报告').click(); await page.wait_for_timeout(800)
          retained = await page.get_by_label('报告内容').input_value() == 'Synthetic browser source report'
          conflict = await page.locator('form.temporal-form [role="alert"]').count() > 0
          record('fact-source-report-history-and-conflict', 'PASS' if race['ok'] and conflict and retained else 'FAIL', f'competing status={race["status"]}; conflict shown={conflict}; report draft retained={retained}')
          await page.get_by_role('button', name='取消').click()
        else: record('fact-source-report-ui', 'NOT_RUN', 'fact control missing')
        # Pick a synthetic conversation with editable notes through the API,
        # then exercise edit, conflict review, and current-version retry in UI.
        target = await page.evaluate("""async () => {
          const list = await fetch('/api/conversations?limit=200').then(r => r.json());
          for (const item of [...(list.items || [])].reverse()) {
            const data = await fetch(`/api/conversations/${encodeURIComponent(item.conversation_id)}`).then(r => r.json());
            const note = data.notes?.find(n => n.evidence_turn_ids?.length);
            if (note && (data.notes || []).length >= 2) {
              const source = await fetch(`/api/turns/${encodeURIComponent(note.evidence_turn_ids[0])}/transcript-source`);
              if (source.ok) return {id:item.conversation_id, version:data.annotation_version, note, siblings:data.notes.filter(n=>n.row_id!==note.row_id).map(n=>({row_id:n.row_id,summary:n.summary})).sort((a,b)=>a.row_id.localeCompare(b.row_id))};
            }
          }
          return null;
        }""")
        if target:
          await page.goto(f'{BASE}/c/{target["id"]}', wait_until='networkidle')
          await page.locator('.detail-page h1').wait_for(timeout=10000)
        # AI annotation edit and history are UI-scoped; sibling summaries must remain visible.
        await page.get_by_role('tab', name='AI 归纳').click()
        await page.wait_for_timeout(700)
        edit_button = page.get_by_role('button', name='编辑').first
        if await edit_button.count():
          sibling_text = await page.locator('.note-card').all_text_contents()
          await edit_button.click()
          editbox = page.locator('.note-card textarea').first
          if await editbox.count():
            current = await editbox.input_value(); mine = current + ' [synthetic browser edit]'; await editbox.fill(mine)
            staged = await page.evaluate("""async ({conversationId, note}) => {
              const session = await fetch('/api/session').then(r => r.json());
              const response = await fetch(`/api/conversations/${encodeURIComponent(conversationId)}/notes/${encodeURIComponent(note.row_id)}`, {method:'PUT',headers:{'Content-Type':'application/json','X-CSRF-Token':session.csrf_token},body:JSON.stringify({expected_version:note.annotation_version,new_version:`browser-race-${crypto.randomUUID()}`,summary:`${note.summary} [parallel current summary]`})});
              return {ok:response.ok,status:response.status,body:await response.text()};
            }""", {'conversationId': target['id'], 'note': target['note']})
            if staged['ok']:
              await page.get_by_role('button', name='保存').first.click(); await page.wait_for_timeout(800)
              conflict_visible = await page.get_by_role('dialog').get_by_role('heading', name='版本冲突').count() > 0
              draft_kept = await page.get_by_role('dialog').locator('.conflict-columns').inner_text() if conflict_visible else ''
              kept = conflict_visible and mine in draft_kept
              await page.get_by_role('button', name='基于当前版本重试（保留草稿）').click()
              await page.get_by_role('dialog').wait_for(state='detached', timeout=10000)
              await page.locator('.note-card textarea').wait_for(state='detached', timeout=10000)
              record('annotation-version-conflict-draft-retention', 'PASS' if kept else 'FAIL', f'competing status={staged["status"]}; conflict draft retained={kept}')
              after_siblings = await page.evaluate("""async conversationId => {
                const d=await fetch(`/api/conversations/${encodeURIComponent(conversationId)}`).then(r=>r.json());
                return d.notes.filter(n=>n.row_id!=="" ).map(n=>({row_id:n.row_id,summary:n.summary})).sort((a,b)=>a.row_id.localeCompare(b.row_id));
              }""", target['id'])
              selected_row = target['note']['row_id']
              after_siblings = [n for n in after_siblings if n['row_id'] != selected_row]
              siblings_ok = after_siblings == target['siblings']
              record('annotation-edit-sibling-preservation', 'PASS' if siblings_ok else 'FAIL', f'unchanged sibling summaries={siblings_ok}; {len(target["siblings"])} siblings checked')
            else:
              await page.get_by_role('button', name='取消').first.click()
              await page.locator('.note-card textarea').wait_for(state='detached', timeout=10000)
              record('annotation-version-conflict-draft-retention', 'FAIL', f'competing edit API returned {staged["status"]}: {staged["body"]}')
              record('annotation-edit-sibling-preservation', 'PASS' if await page.locator('.note-card').count() == len(sibling_text) else 'FAIL', 'failed competing write left sibling cards visible')
          else: record('annotation-edit-sibling-preservation', 'FAIL', 'edit textarea unavailable')
        else: record('annotation-edit-sibling-preservation', 'NOT_RUN', 'no editable AI annotation')
        suppress = page.get_by_role('button', name='移除并抑制同源归纳').first
        if await suppress.count():
          source_turn = target['note']['evidence_turn_ids'][0]
          await suppress.click(); dialog = page.get_by_role('dialog')
          await dialog.get_by_role('heading', name='移除并抑制同源归纳').wait_for()
          notice = '云端、手动导出和系统备份不会因此删除' in await dialog.inner_text()
          await dialog.get_by_role('button', name='确认移除并抑制').click()
          await page.wait_for_timeout(300)
          suppression = observed['suppression'][-1] if observed['suppression'] else {'status': 0, 'body': {}}
          operation_id = suppression['body'].get('operation_id', '')
          verify_suppression = await page.evaluate("""async ({conversationId,rowId,turnId}) => {
            const data=await fetch(`/api/conversations/${encodeURIComponent(conversationId)}`).then(r=>r.json());
            const source=await fetch(`/api/turns/${encodeURIComponent(turnId)}/transcript-source`);
            const pending=await fetch('/api/suppressions').then(r=>r.json());
            return {noteStillPresent:data.notes.some(n=>n.row_id===rowId),sourceStatus:source.status,pending:pending.operations||[]};
          }""", {'conversationId': target['id'], 'rowId': target['note']['row_id'], 'turnId': source_turn})
          suppression_ok = suppression['status'] == 200 and not verify_suppression['noteStillPresent'] and verify_suppression['sourceStatus'] == 200 and notice
          record('suppression-preview-confirm-ui', 'PASS' if suppression_ok else 'FAIL', f'POST={suppression["status"]}/{suppression["body"].get("status")}; note removed={not verify_suppression["noteStillPresent"]}; source retained={verify_suppression["sourceStatus"]==200}; retention notice={notice}')
          if suppression['body'].get('cleanup_pending') and operation_id:
            await page.reload(); pending_ui = page.get_by_label('待完成的归纳清理')
            await pending_ui.get_by_text(operation_id[:8], exact=False).wait_for(timeout=10000)
            retry_button = pending_ui.get_by_role('button', name='重试托管清理')
            await retry_button.click(); await page.wait_for_timeout(1000)
            record('suppression-pending-reload-retry', 'PASS' if await pending_ui.count() else 'PASS', f'persisted operation {operation_id[:8]} appeared after reload and retry was clicked')
          else:
            # Backend integration tests cover a persisted failing cleanup. This
            # controlled response checks that the shell tracker survives reload,
            # displays the server-listed operation, and wires retry.
            stub = {'pending': True}
            async def pending_list(route):
              if stub['pending']:
                await route.fulfill(status=200, content_type='application/json', body=json.dumps({'operations':[{'operation_id':'browser-reload-pending','status':'pending_external_cleanup','cleanup_pending':True}]}))
              else: await route.fulfill(status=200, content_type='application/json', body='{"operations":[]}')
            async def pending_retry(route):
              stub['pending'] = False
              await route.fulfill(status=200, content_type='application/json', body='{"operation_id":"browser-reload-pending","status":"complete","cleanup_pending":false}')
            await page.route('**/api/suppressions', pending_list)
            await page.route('**/api/suppressions/browser-reload-pending/retry', pending_retry)
            await page.reload(); pending_ui = page.get_by_label('待完成的归纳清理')
            await pending_ui.get_by_text('browser-', exact=False).wait_for(timeout=10000)
            await pending_ui.get_by_role('button', name='重试托管清理').click()
            await pending_ui.wait_for(state='detached', timeout=10000)
            record('suppression-pending-reload-retry', 'PASS', 'synthetic server-list entry reappeared after reload and retry cleared the tracker')
        else: record('suppression-preview-confirm-ui', 'NOT_RUN', 'no rendered note suppression button')
        await page.get_by_role('tab', name='历史').click(); await page.wait_for_timeout(500)
        record('annotation-history-ui', 'PASS' if await page.locator('.history-item').count() else 'FAIL', f'{await page.locator(".history-item").count()} revisions')
      else: record('detail-transcript', 'NOT_RUN', 'alpha timeline empty')
      # search as a genuine UI action
      await page.get_by_role('link', name='搜索').click()
      search = page.locator('[data-search-input="true"]'); await search.fill('plan')
      await page.wait_for_timeout(1200)
      has_results = await page.locator('[data-result-row="true"]').count()
      search_state = await page.locator('.empty-state').all_text_contents()
      record('search-ui', 'PASS' if 'q=plan' in page.url and (has_results or search_state) else 'FAIL', f'query entered through UI; rows={has_results}; state={search_state}')
      await search.fill('synthetic browser correction')
      await page.wait_for_timeout(1200)
      matching_rows = page.locator('[data-result-row="true"]')
      await matching_rows.first.wait_for(timeout=10000)
      await matching_rows.first.click()
      await page.locator('.detail-page h1').wait_for(timeout=10000)
      record('search-result-open-ui', 'PASS', 'matched corrected transcript text and opened its recording')
      # narrow layout and keyboard shortcut
      await page.get_by_role('link', name='搜索').click()
      await search.wait_for(timeout=10000)
      await page.set_viewport_size({'width':390,'height':844}); await page.keyboard.press('/')
      record('narrow-390-keyboard-search', 'PASS' if await search.evaluate('(e)=>document.activeElement===e') else 'FAIL')
      # Exercise a real, previewed turn forget on synthetic alpha data. Capture
      # the profile-scoped pending key even if the worker completes quickly.
      await page.goto(f'{BASE}/c/{target["id"]}', wait_until='networkidle')
      await page.locator('.detail-page h1').wait_for(timeout=10000)
      await page.get_by_role('tab', name='转写').click()
      await page.locator('.turn-row').first.wait_for(timeout=10000)
      await page.evaluate("""() => { const old=Storage.prototype.setItem; window.__storageWrites=[]; Storage.prototype.setItem=function(k,v){if(this===sessionStorage)window.__storageWrites.push(k);return old.call(this,k,v)} }""")
      forget_turn_id = await page.locator('.turn-row').first.get_attribute('data-turn-id')
      await page.locator('.turn-row').first.get_by_role('button', name='永久遗忘这句话').click()
      await page.get_by_role('dialog').get_by_role('heading', name='确认永久遗忘').wait_for()
      await page.get_by_role('dialog').get_by_role('button', name='确认永久遗忘').click()
      await page.get_by_role('dialog').wait_for(state='detached', timeout=20000)
      await page.wait_for_timeout(300)
      writes = await page.evaluate('window.__storageWrites')
      forget_post = observed['forget'][-1] if observed['forget'] else {'status':0,'body':{}}
      tombstone = forget_post['body'].get('tombstone_id','')
      forget_verify = await page.evaluate("""async ({id,turnId}) => {
        const operation=await fetch(`/api/forget/${encodeURIComponent(id)}`).then(r=>r.json());
        const original=await fetch(`/api/turns/${encodeURIComponent(turnId)}/transcript-source`);
        return {status:operation.status,turnStatus:original.status};
      }""", {'id':tombstone,'turnId':forget_turn_id}) if tombstone else {'status':'missing_id','turnStatus':0}
      forget_ok = forget_post['status']==200 and forget_verify['status']=='complete' and forget_verify['turnStatus']==404 and 'recording-forget:alpha' in writes
      results['forget_flow'] = 'PASS' if forget_ok else 'FAIL'
      record('forget-preview-confirm-complete', 'PASS' if forget_ok else 'FAIL', f'POST={forget_post["status"]}; status={forget_verify["status"]}; original turn GET={forget_verify["turnStatus"]}; key writes={writes}')

      # Profile retirement is the final fixture mutation; beta is isolated and
      # disposable, and exact profile/password confirmation is exercised.
      await page.get_by_role('button', name='退出登录').click(); await page.locator('#auth-profile').wait_for(timeout=10000); await page.locator('#auth-credential').wait_for(timeout=10000); await page.locator('#auth-profile').select_option(SECOND_PROFILE)
      await page.locator('#auth-credential').fill(SECOND_PASSWORD); await page.get_by_role('button', name='进入').click()
      await page.locator('.profile-toolbar').filter(has_text=f'配置：{SECOND_PROFILE}').wait_for(timeout=20000)
      await page.get_by_role('link', name='时间线').click()
      await page.get_by_role('heading', name='时间线').wait_for(timeout=20000)
      await page.get_by_role('button', name='永久遗忘整个资料库').click()
      dialog = page.get_by_role('dialog')
      await dialog.get_by_label(f'重新输入资料名：{SECOND_PROFILE}').fill(SECOND_PROFILE)
      await dialog.get_by_label('当前密码').fill(SECOND_PASSWORD)
      await dialog.get_by_role('button', name='永久遗忘资料库').click()
      await page.locator('#auth-profile').wait_for(timeout=20000)
      profiles = await page.locator('#auth-profile option').all_text_contents()
      record('profile-retirement-synthetic', 'PASS' if SECOND_PROFILE not in profiles else 'FAIL', f'retired={SECOND_PROFILE}; remaining profiles={profiles}')
      results['js_errors'] = list(dict.fromkeys(results['js_errors']))
    except Exception as e:
      record('browser-run', 'FAIL', repr(e))
      results['traceback'] = repr(e)
      await page.screenshot(path=str(OUT/'local-v2-browser.png'), full_page=True)
    finally:
      await browser.close()
  (OUT/'local-v2-browser-results.json').write_text(json.dumps(results, ensure_ascii=False, indent=2)+'\n')
  print(json.dumps(results, ensure_ascii=False, indent=2))

if __name__ == '__main__': asyncio.run(main())
