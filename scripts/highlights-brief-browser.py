#!/usr/bin/env python3.14
"""Exercise Memory Highlights and Brief in the disposable synthetic profile."""
import asyncio, json, os, pathlib, sys
from urllib.parse import unquote
import re
from playwright.async_api import async_playwright

BASE = os.environ.get('BASE_URL', 'http://127.0.0.1:8798').rstrip('/')
PASSWORD = os.environ.get('ALPHA_PASSWORD', 'synthetic-pass-alpha')
OUT = pathlib.Path(os.environ.get('BROWSER_ARTIFACT_DIR', '/tmp'))
OUT.mkdir(parents=True, exist_ok=True)
cases = []

def record(name, ok, detail=''):
    cases.append({'name': name, 'status': 'PASS' if ok else 'FAIL', 'detail': detail})

async def main():
  async with async_playwright() as p:
    opts = {'headless': True, 'args': ['--no-sandbox', '--autoplay-policy=no-user-gesture-required']}
    if os.environ.get('BROWSER_PATH'): opts['executable_path'] = os.environ['BROWSER_PATH']
    browser = await p.chromium.launch(**opts)
    context = await browser.new_context(viewport={'width': 1280, 'height': 1000})
    await context.grant_permissions(['clipboard-read', 'clipboard-write'], origin=BASE)
    page = await context.new_page()
    page_errors = []
    page.on('pageerror', lambda e: page_errors.append(str(e)))
    try:
      await page.goto(BASE, wait_until='networkidle')
      if await page.locator('#auth-credential').count():
        if await page.locator('#auth-profile').count(): await page.locator('#auth-profile').select_option('alpha')
        await page.locator('#auth-credential').fill(PASSWORD)
        await page.get_by_role('button', name='进入').click()
        await page.locator('.profile-toolbar').wait_for(timeout=20000)
      await page.goto(BASE + '/recordings', wait_until='networkidle')
      await page.locator('.timeline-row').first.wait_for(timeout=20000)
      await page.locator('.timeline-row').first.click()
      await page.locator('.turn-row').first.wait_for(timeout=20000)
      conversation_id = unquote(page.url.rsplit('/', 1)[-1])
      recording_id = await page.locator('.recording-group').first.get_attribute('aria-label')
      recording_id = recording_id.removeprefix('录音 ')
      turn_id = await page.locator('.turn-row').first.get_attribute('data-turn-id')
      create = page.get_by_role('button', name=re.compile('^从此轮次标记')).first
      await create.click()
      panel = page.locator('.highlight-panel')
      await panel.get_by_text('来源有效').wait_for(timeout=10000)
      record('highlight-create-from-turn', await panel.locator('.note-card').count() > 0)
      playable_turn = None
      for i in range(await page.locator('.turn-row').count()):
        row = page.locator('.turn-row').nth(i)
        button = row.get_by_role('button', name='播放')
        if await button.count() and await button.is_enabled():
          playable_turn = row
          break
      if playable_turn is None: raise AssertionError('synthetic recording has no playable turn')
      await playable_turn.get_by_role('button', name='播放').click()
      await page.locator('.audio-bar audio').wait_for(timeout=10000)
      for _ in range(100):
        if await page.locator('.audio-bar audio').evaluate('(a)=>a.currentTime') > 0: break
        await page.wait_for_timeout(100)
      else:
        diagnostic = await page.locator('.audio-bar').inner_text()
        media = await page.locator('.audio-bar audio').evaluate('(a)=>({currentTime:a.currentTime,duration:a.duration,readyState:a.readyState,error:a.error?.message,src:a.currentSrc})')
        raise AssertionError(f'turn audio currentTime did not advance: {diagnostic}; {media}')
      await panel.get_by_role('button', name='标记当前播放位置').click()
      for _ in range(100):
        if await panel.locator('.note-card').count() >= 2: break
        await page.wait_for_timeout(100)
      record('highlight-create-at-current-playhead', await panel.locator('.note-card').count() >= 2)
      card = panel.locator('.note-card').first
      note = card.get_by_label('备注')
      await note.fill('synthetic retained note draft')
      await card.get_by_label('标签').select_option('follow_up')
      highlight_id = await page.evaluate("""async recordingId => {
        const r=await fetch(`/api/recordings/${encodeURIComponent(recordingId)}/highlights`);
        const j=await r.json(); return j.highlights?.[0]?.id;
      }""", recording_id)
      staged = await page.evaluate("""async id => {
        const session=await fetch('/api/session').then(r=>r.json());
        const h=await fetch(`/api/highlights/${encodeURIComponent(id)}`).then(r=>r.json());
        const response=await fetch(`/api/highlights/${encodeURIComponent(id)}`, {method:'PATCH',headers:{'Content-Type':'application/json','X-CSRF-Token':session.csrf_token},body:JSON.stringify({expected_version:h.version,tag:'question'})});
        return {status:response.status};
      }""", highlight_id)
      await card.get_by_role('button', name='保存').click()
      await panel.get_by_role('alert').wait_for(timeout=10000)
      retained = await note.input_value() == 'synthetic retained note draft'
      record('highlight-edit-409-retains-draft', staged['status'] == 200 and retained, f"race={staged['status']}, retained={retained}")
      await panel.get_by_role('button', name='重新加载版本').click()
      await card.get_by_role('button', name='保存').click()
      await page.wait_for_timeout(500)
      saved = await page.evaluate("""async id => { const h=await fetch(`/api/highlights/${encodeURIComponent(id)}`).then(r=>r.json()); return {tag:h.tag,note:h.note_text,version:h.version}; }""", highlight_id)
      record('highlight-edit-tag-note', saved['tag'] == 'follow_up' and saved['note'] == 'synthetic retained note draft', json.dumps(saved))
      audio = card.locator('audio')
      await audio.evaluate('(a)=>a.load()')
      try:
        await audio.evaluate("a => new Promise((resolve,reject)=>{ if(a.readyState>=2) resolve(); a.oncanplay=resolve; a.onerror=()=>reject(Error('highlight audio unavailable')); setTimeout(()=>reject(Error('audio timeout')),5000) })")
        record('highlight-audio-controlled-route', True)
      except Exception as e: record('highlight-audio-controlled-route', False, str(e))
      await page.goto(BASE + '/brief', wait_until='networkidle')
      await page.get_by_role('heading', name='记忆简报').wait_for()
      await page.get_by_label('范围类型').select_option('conversation')
      # Brief selectors are populated from real conversation/topic APIs.
      selector = page.locator('fieldset').filter(has=page.get_by_text('会话', exact=True)).first
      matching_index = await page.evaluate("""async conversationId => {
        const list=await fetch('/api/conversations?limit=100').then(r=>r.json());
        return (list.items||[]).findIndex(c=>c.conversation_id===conversationId);
      }""", conversation_id)
      check = selector.locator('input[type=checkbox]').nth(matching_index)
      if matching_index >= 0 and await check.count(): await check.check()
      await page.get_by_role('button', name='生成简报预览').click()
      try: await page.locator('.brief-result').wait_for(timeout=20000)
      except Exception as e:
        alert = await page.locator('[role="alert"]').all_text_contents()
        selected = await selector.locator('input[type=checkbox]:checked').count()
        raise AssertionError(f'brief preview not rendered: {e}; conversation_id={conversation_id}; matching_index={matching_index}; selectors={await page.locator("fieldset").count()}; inputs={await selector.locator("input[type=checkbox]").count()}; selected={selected}; alerts={alert}')
      await page.get_by_label('范围类型').select_option('project')
      async with page.expect_response(lambda response: response.url.endswith('/api/briefs/preview') and response.request.method == 'POST') as project_response:
        await page.get_by_role('button', name='生成简报预览').click()
      project_json = await (await project_response.value).json()
      await page.locator('.brief-result').wait_for(timeout=20000)
      record('brief-project-topic-conversation-union', project_json.get('scope', {}).get('kind') == 'project', json.dumps(project_json.get('scope', {})))
      await page.get_by_label('范围类型').select_option('person')
      people_response = await page.evaluate("""async () => (await fetch('/api/briefs/people')).json()""")
      people_group = page.locator('fieldset').filter(has=page.get_by_text('当前已确认关联的人物', exact=True)).first
      visible_people = [text.strip() for text in await people_group.locator('label.toggle-label').all_inner_texts()]
      expected_people = [person['label'] for person in people_response.get('people', [])]
      no_unconfirmed = visible_people == expected_people
      record('brief-person-picker-only-current-confirmed-identities', no_unconfirmed, f'API={expected_people}; UI={visible_people}')
      await page.get_by_label('范围类型').select_option('conversation')
      selector = page.locator('fieldset').filter(has=page.get_by_text('会话', exact=True)).first
      if await selector.locator('input[type=checkbox]:checked').count() == 0:
        await selector.locator('input[type=checkbox]').nth(matching_index).check()
      async with page.expect_response(lambda response: response.url.endswith('/api/briefs/preview') and response.request.method == 'POST') as conversation_response:
        await page.get_by_role('button', name='生成简报预览').click()
      await conversation_response.value
      await page.locator('.brief-result').wait_for(timeout=20000)
      evidence_links = page.get_by_role('button', name='查看证据')
      evidence_section = page.get_by_role('heading', name=re.compile('^证据')).locator('xpath=..')
      evidence_count = await evidence_section.locator('.note-card').count()
      record('brief-preview', await page.locator('.brief-result').count() == 1 and evidence_count > 0, f'evidence groups={await evidence_links.count()}; evidence={evidence_count}')
      if evidence_count == 0: raise AssertionError('synthetic brief fixture must contain evidence')
      old_quote = (await evidence_section.locator('.note-card').first.inner_text()).splitlines()[0]
      if await evidence_links.count():
        await evidence_links.first.click()
        play_evidence = page.get_by_role('button', name='播放证据').first
        if await play_evidence.count():
          await play_evidence.click()
          evidence_audio = page.locator('.audio-bar audio')
          await evidence_audio.wait_for(timeout=5000)
          played = False
          for _ in range(100):
            if await evidence_audio.evaluate('(a)=>a.currentTime') > 0: played = True; break
            await page.wait_for_timeout(100)
          record('brief-evidence-expand-and-play', played, 'evidence audio currentTime advanced' if played else 'playback did not advance')
        else: record('brief-evidence-expand-and-play', False, 'evidence playback control missing')
      else: record('brief-evidence-expand-and-play', False, 'entry has no linked evidence')
      await page.get_by_role('button', name='复制 Markdown').click()
      copied = await page.evaluate('() => navigator.clipboard.readText()')
      copy_ok = 'Snapshot:' in copied and '来源状态' in copied and '来源说明' in copied and old_quote in copied
      record('brief-copy-markdown-content', copy_ok, 'snapshot, evidence text, and caveats copied' if copy_ok else copied[:250])
      await page.screenshot(path=str(OUT/'highlights-brief-browser.png'), full_page=True)
      # Make source revision stale through the authenticated correction API, then
      # verify the saved highlight retains its note and offers explicit relinking.
      source = await page.evaluate("""async turnId => {
        const session=await fetch('/api/session').then(r=>r.json());
        const src=await fetch(`/api/turns/${encodeURIComponent(turnId)}/transcript-source`).then(r=>r.json());
        const texts=Object.fromEntries(src.result.turns.map((t,i)=>[t.id,`Synthetic revised turn ${i}`]));
        const r=await fetch(`/api/conversations/${encodeURIComponent(src.conversation_id)}/recordings/${encodeURIComponent(src.recording_id)}/segments/${encodeURIComponent(src.segment_id)}/correction`,{method:'POST',headers:{'Content-Type':'application/json','X-CSRF-Token':session.csrf_token},body:JSON.stringify({expected_revision:src.result.source.transcript_revision,new_revision:`brief-e2e-${crypto.randomUUID()}`,texts})});
        return {status:r.status};
      }""", turn_id)
      await page.goto(BASE + f'/c/{conversation_id}', wait_until='networkidle')
      await page.locator('.highlight-panel').get_by_text('需要重新关联').first.wait_for(timeout=15000)
      relink = page.locator('.highlight-panel .note-card').first.get_by_label('重新关联到当前轮次')
      await relink.select_option(index=1)
      await page.wait_for_timeout(500)
      relinked = await page.evaluate("""async id => (await fetch(`/api/highlights/${encodeURIComponent(id)}`).then(r=>r.json())).state""", highlight_id)
      record('highlight-source-revision-relink', source['status'] == 200 and relinked == 'current', f"revision={source['status']}, state={relinked}")
      await page.goto(BASE + '/brief', wait_until='networkidle')
      await page.get_by_label('范围类型').select_option('conversation')
      selector = page.locator('fieldset').filter(has=page.get_by_text('会话', exact=True)).first
      matching_index = await page.evaluate("""async conversationId => { const list=await fetch('/api/conversations?limit=100').then(r=>r.json()); return (list.items||[]).findIndex(c=>c.conversation_id===conversationId); }""", conversation_id)
      await selector.locator('input[type=checkbox]').nth(matching_index).check()
      await page.get_by_role('button', name='生成简报预览').click()
      await page.locator('.brief-result').wait_for(timeout=20000)
      revised_evidence = page.get_by_role('heading', name=re.compile('^证据')).locator('xpath=..').locator('.note-card')
      revised_text = await revised_evidence.all_inner_texts()
      record('brief-refresh-after-transcript-revision', all(old_quote not in text for text in revised_text), f'evidence={len(revised_text)}; old evidence text absent={all(old_quote not in text for text in revised_text)}')
      await page.goto(BASE + f'/c/{conversation_id}', wait_until='networkidle')
      await page.locator('.recording-group').first.wait_for(timeout=15000)
      # Forget is intentionally last: this suite runs against a fresh disposable
      # profile and verifies retention behavior after all highlight checks.
      forget = page.get_by_role('button', name='永久遗忘此录音').first
      if await forget.count():
        await forget.click()
        dialog = page.get_by_role('dialog')
        broaden = dialog.get_by_label('我明确确认：时间偏移未知，允许删除整段音频文件')
        if await broaden.count(): await broaden.check()
        await dialog.get_by_role('button', name='确认永久遗忘').click(timeout=10000)
        await page.wait_for_timeout(1000)
        state = await page.evaluate("""async recordingId => { const r=await fetch(`/api/recordings/${encodeURIComponent(recordingId)}/highlights`); const body=r.ok?await r.json():null; return {status:r.status,count:body?.highlights?.length??0}; }""", recording_id)
        record('highlight-forget-erases-source-linked-data', state['status'] in (404, 410) or (state['status'] == 200 and state['count'] == 0), json.dumps(state))
        remaining = await page.evaluate("""async conversationId => {
          const session=await fetch('/api/session').then(r=>r.json());
          const response=await fetch('/api/briefs/preview',{method:'POST',headers:{'Content-Type':'application/json','X-CSRF-Token':session.csrf_token},body:JSON.stringify({scope:{kind:'conversation',person_ids:[],topic_ids:[],conversation_ids:[conversationId]}})});
          if(!response.ok) return {status:response.status};
          const result=await response.json();
          const entries=Object.values(result.sections).flat().length;
          return {status:response.status,evidence:result.evidence.length,entries};
        }""", conversation_id)
        record('forget-removes-brief-evidence-and-notes', remaining.get('status') == 200 and remaining.get('evidence') == 0 and remaining.get('entries') == 0, json.dumps(remaining))
      else: record('highlight-forget-erases-source-linked-data', False, 'recording forget control missing')
      record('uncaught-browser-errors', not page_errors, '; '.join(page_errors))
      print(json.dumps({'base_url': BASE, 'synthetic_fixture': True, 'cases': cases, 'page_errors': page_errors}, ensure_ascii=False, indent=2))
      if any(c['status'] == 'FAIL' for c in cases): raise AssertionError('one or more Memory Highlights / Brief cases failed')
    finally:
      (OUT/'highlights-brief-browser-results.json').write_text(json.dumps({'base_url': BASE, 'synthetic_fixture': True, 'cases': cases, 'page_errors': page_errors}, ensure_ascii=False, indent=2)+'\n')
      await browser.close()

if __name__ == '__main__': asyncio.run(main())
