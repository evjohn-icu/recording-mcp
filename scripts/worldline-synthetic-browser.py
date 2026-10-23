import asyncio
import json
import os
import traceback
from pathlib import Path
from urllib.parse import urlparse, unquote, parse_qs
from playwright.async_api import async_playwright

BASE = os.environ.get('BASE_URL', 'http://127.0.0.1:5173').rstrip('/')
OUT = Path(os.environ.get('BROWSER_ARTIFACT_DIR', '/tmp')) / 'worldline-synthetic-browser.json'
BROWSER_PATH = os.environ.get('BROWSER_PATH')

FIXTURE = {
    'version': 'projection-v1',
    'total': 1,
    'next_offset': None,
    'stats': {'source_notes': 3, 'topics': 1, 'claims': 3, 'dated_claims': 2, 'unknown_time': 1, 'conflicts': 1},
    'range': {'start': '2022-01-01T00:00:00Z', 'end': '2025-01-01T00:00:00Z'},
    'topics': [{
        'id': 'macro:work', 'title': '工作与项目', 'source_topic_ids': ['topic:atlas'],
        'has_more': True,
        'lanes': [
            {'id': 'topic:atlas:facts', 'title': '事实', 'subject': 'Atlas 项目', 'category': 'facts', 'claims': [
                {'id': 'claim:budget', 'version': 'c1', 'summary': 'Atlas 项目预算为 20 万元', 'kind': 'observation', 'status': 'conflict', 'valid_from': '2023-03-01T00:00:00Z', 'valid_until': '2024-02-01T00:00:00Z', 'mentioned_at': '2023-02-18T00:00:00Z', 'time_basis': '用户明确陈述', 'time_precision': 'interval', 'change_type': 'report', 'evidence': [{'turn_id': 'turn:budget', 'revision': 'r4', 'conversation_id': 'conv:budget', 'recording_id': 'recording:budget', 'text': 'Atlas 今年的项目预算大概是二十万元。', 'offset_ms': 12000}], 'conflict_with': ['claim:old-budget']},
                {'id': 'claim:role', 'version': 'c1', 'summary': 'Mina 负责设计审查', 'kind': 'observation', 'status': 'reviewed', 'valid_from': '2022-07-01T00:00:00Z', 'valid_until': None, 'mentioned_at': '2022-06-29T00:00:00Z', 'time_basis': '明确适用日期', 'time_precision': 'open', 'change_type': 'reality_change', 'evidence': [{'turn_id': 'turn:role', 'revision': 'r2', 'conversation_id': 'conv:role', 'recording_id': 'recording:role', 'text': '从七月开始，Mina 会负责设计审查。'}], 'conflict_with': []},
            ]},
            {'id': 'topic:atlas:plans', 'title': '计划', 'subject': 'Atlas 项目', 'category': 'plans', 'claims': [
                {'id': 'claim:trip', 'version': 'c1', 'summary': '计划在秋季启动试点', 'kind': 'intention', 'status': 'unconfirmed', 'valid_from': None, 'valid_until': None, 'mentioned_at': '2024-09-12T00:00:00Z', 'time_basis': '仅有提及时间', 'time_precision': 'unknown', 'change_type': 'report', 'evidence': [{'turn_id': 'turn:trip', 'revision': 'r1', 'conversation_id': 'conv:trip', 'recording_id': 'recording:trip', 'text': '我们计划秋季启动试点。'}], 'conflict_with': []},
            ]},
            {'id': 'topic:unknown:facts', 'title': '未确认归属', 'subject': 'unknown', 'category': 'facts', 'claims': []},
        ],
    }],
}

REVIEW_STATE = {'accepted': False, 'source_changed': False, 'accept_conflict_sent': False}

def thread_entry(claim, macro_id='macro:work', lane_id='topic:atlas:facts', relation='subsequent_mention'):
    evidence = claim['evidence']
    result = {
        'id': 'thread-entry:' + claim['id'], 'claim_id': claim['id'], 'version': claim['version'],
        'summary': claim['summary'], 'relation': relation, 'relation_reason': '同一具体主题的来源提及',
        'mentioned_at': claim.get('mentioned_at'), 'time_known': bool(claim.get('mentioned_at')),
        'status': claim['status'], 'category': 'facts' if claim['kind'] == 'observation' else 'plans',
        'macro_topic_id': macro_id, 'lane_id': lane_id, 'claim': claim, 'evidence': evidence,
        'conversation_ids': list(dict.fromkeys(item['conversation_id'] for item in evidence)),
    }
    if claim['id'] == 'claim:budget':
        result['semantic_suggestion'] = {
            'status': 'review_required', 'relation': 'review_required', 'model': 'synthetic/mock-engine',
            'weights_version': 'synthetic/mock-weights', 'source_claim_ids': ['claim:role', 'claim:budget'],
            'source_evidence': [{'turn_id': 'turn:role', 'revision': 'r2'}, {'turn_id': 'turn:budget', 'revision': 'r4'}],
            'earlier_summary': 'Mina 负责设计审查',
            'later_summary': 'Atlas 项目预算为 20 万元',
            'earlier_evidence': [{'turn_id': 'turn:role', 'revision': 'r2', 'conversation_id': 'conv:role', 'recording_id': 'recording:role', 'text': '从七月开始，Mina 会负责设计审查。'}],
            'later_evidence': [{'turn_id': 'turn:budget', 'revision': 'r4', 'conversation_id': 'conv:budget', 'recording_id': 'recording:budget', 'text': 'Atlas 今年的项目预算大概是二十万元。', 'offset_ms': 12000}],
            'reason': 'synthetic browser fixture only', 'requires_review': not REVIEW_STATE['accepted'],
            'review_status': 'accepted' if REVIEW_STATE['accepted'] else 'review_required', 'suggestion_id': 'suggestion:atlas-budget',
        }
        if REVIEW_STATE['source_changed']:
            result['semantic_suggestion']['later_summary'] = 'Atlas 项目预算更新为 23 万元'
    elif claim['id'] == 'claim:role':
        result['semantic_suggestion'] = {
            'status': 'review_required', 'relation': 'insufficient_evidence', 'model': 'synthetic/mock-engine',
            'source_claim_ids': ['claim:budget', 'claim:role'], 'source_evidence': [],
            'earlier_summary': 'Atlas 项目预算为 20 万元', 'later_summary': 'Mina 负责设计审查',
            'earlier_evidence': [], 'later_evidence': [], 'requires_review': True,
        }
    return result

THREADS = [
    {'id': 'thread:atlas', 'title': 'Atlas 项目', 'macro_topic_id': 'macro:work', 'macro_topic_title': '工作与项目', 'source_topic_ids': ['topic:atlas'], 'conversation_count': 3, 'entry_count': 3, 'dated_entry_count': 3, 'unknown_entry_count': 1, 'semantic_review_count': 2, 'first_mentioned_at': '2022-06-29T00:00:00Z', 'last_mentioned_at': '2024-09-12T00:00:00Z'},
    {'id': 'thread:home', 'title': '生活与居住安排', 'macro_topic_id': 'macro:life', 'macro_topic_title': '生活与居住', 'source_topic_ids': ['topic:home'], 'conversation_count': 2, 'entry_count': 1, 'dated_entry_count': 1, 'unknown_entry_count': 0, 'first_mentioned_at': '2023-04-01T00:00:00Z', 'last_mentioned_at': '2023-04-01T00:00:00Z'},
]
THREADS.extend({'id': f'thread:extra-{i}', 'title': f'跨会话主题 {i}', 'macro_topic_id': 'macro:work', 'macro_topic_title': '工作与项目', 'source_topic_ids': [f'topic:extra-{i}'], 'conversation_count': 1, 'entry_count': 1, 'dated_entry_count': 1, 'unknown_entry_count': 0, 'first_mentioned_at': '2023-01-01T00:00:00Z', 'last_mentioned_at': '2023-01-01T00:00:00Z'} for i in range(3, 10))
UNKNOWN_MENTION = {
    'id': 'thread-entry:undated', 'claim_id': 'claim:undated', 'version': 'c1', 'summary': '提及日期不明的项目安排',
    'relation': 'review_required', 'relation_reason': '来源中没有明确录音日期', 'mentioned_at': None, 'time_known': False,
    'status': 'unconfirmed', 'category': 'plans', 'macro_topic_id': 'macro:work', 'lane_id': 'topic:atlas:plans',
    'claim': {'id': 'claim:undated', 'version': 'c1', 'summary': '提及日期不明的项目安排', 'kind': 'intention', 'status': 'unconfirmed', 'valid_from': None, 'valid_until': None, 'mentioned_at': None, 'time_basis': '未记录提及日期', 'time_precision': 'unknown', 'change_type': 'report', 'evidence': [{'turn_id': 'turn:undated', 'revision': 'r1', 'conversation_id': 'conv:undated', 'recording_id': 'recording:undated', 'text': '项目安排还要再确认。'}], 'conflict_with': []},
    'evidence': [{'turn_id': 'turn:undated', 'revision': 'r1', 'conversation_id': 'conv:undated', 'recording_id': 'recording:undated', 'text': '项目安排还要再确认。'}],
    'conversation_ids': ['conv:undated'],
}

async def main():
    result = {'synthetic_only': True, 'checks': []}
    feedback_posts = []
    generation_posts = []
    topic_detail_requests = []
    thread_mutations = []
    semantic_review_posts = []
    async with async_playwright() as p:
        browser_options = {'headless': True, 'args': ['--no-sandbox', '--no-proxy-server']}
        if BROWSER_PATH:
            browser_options['executable_path'] = BROWSER_PATH
        browser = await p.chromium.launch(**browser_options)
        page = await browser.new_page(viewport={'width': 1280, 'height': 900})
        page.on('console', lambda msg: print('console', msg.type, msg.text) if msg.type == 'error' else None)
        page.on('pageerror', lambda err: print('pageerror', repr(err)))
        page.on('requestfailed', lambda req: print('requestfailed', req.url, req.failure))
        async def route_api(route):
            request = route.request
            path = urlparse(request.url).path
            if path == '/api/session' and request.method == 'GET':
                await route.fulfill(json={'authenticated': True, 'csrf_token': 'synthetic-csrf', 'profile': 'worldline-fixture'})
                return
            if path == '/api/forget':
                await route.fulfill(json={'operations': []})
                return
            if path == '/api/suppressions':
                await route.fulfill(json={'operations': []})
                return
            if path == '/api/worldline/threads/group' and request.method == 'POST':
                payload = request.post_data_json
                thread_mutations.append(payload)
                if payload.get('action') == 'group':
                    for thread in THREADS:
                        if thread['id'] in payload.get('thread_ids', []): thread['manual_group'] = True
                elif payload.get('action') == 'revoke':
                    for thread in THREADS:
                        if thread['id'] == payload.get('group_id'): thread.pop('manual_group', None)
                await route.fulfill(json={'version': 'projection-v2'})
                return
            if path.startswith('/api/worldline/threads/') and path.endswith('/rename') and request.method == 'POST':
                thread_mutations.append(request.post_data_json | {'path': path})
                await route.fulfill(json={'version': 'projection-v2'})
                return
            if path == '/api/worldline/semantic-review' and request.method == 'POST':
                payload = request.post_data_json
                semantic_review_posts.append(payload)
                if payload.get('decision') == 'accept' and not REVIEW_STATE['accept_conflict_sent']:
                    REVIEW_STATE['accept_conflict_sent'] = True
                    REVIEW_STATE['source_changed'] = True
                    await route.fulfill(status=409, json={'error': {'code': 'conflict', 'message': 'stale projection'}})
                else:
                    REVIEW_STATE['accepted'] = payload.get('decision') == 'accept'
                    await route.fulfill(json={'version': 'projection-v2'})
                return
            if path == '/api/worldline/feedback':
                payload = request.post_data_json
                feedback_posts.append(payload)
                if len(feedback_posts) == 1:
                    await route.fulfill(status=409, json={'error': {'code': 'conflict', 'message': 'stale projection'}})
                else:
                    await route.fulfill(status=200, json={'version': 'projection-v2'})
                return
            if path == '/api/worldline/analysis':
                generation_posts.append(request.post_data_json)
                await route.fulfill(status=202, json={'job_id': 'synthetic-worldline-job', 'request_id': generation_posts[-1]['request_id'], 'status': 'queued', 'attempt': 1})
                return
            if path == '/api/worldline/analysis/synthetic-worldline-job':
                if len(generation_posts) == 1:
                    # A transient GET transport/API failure leaves the same
                    # idempotency key available for the explicit user retry.
                    await route.fulfill(status=503, json={'error': {'code': 'temporary', 'message': 'synthetic retry'}})
                elif len(generation_posts) == 2:
                    await route.fulfill(json={'job_id': 'synthetic-worldline-job', 'request_id': generation_posts[-1]['request_id'], 'status': 'complete', 'attempt': 2, 'version': 'projection-v1', 'semantic_status': 'evaluated', 'semantic_model': 'synthetic/mock-engine', 'semantic_candidate_pair_count': 1, 'semantic_pair_count': 1, 'semantic_suggestion_count': 1})
                else:
                    await route.fulfill(json={'job_id': 'synthetic-worldline-job', 'request_id': generation_posts[-1]['request_id'], 'status': 'complete', 'attempt': 1, 'version': 'projection-v1', 'semantic_status': 'not_configured', 'semantic_pair_count': 0, 'semantic_suggestion_count': 0})
                return
            if path == '/api/worldline/semantic-review':
                payload = request.post_data_json
                feedback_posts.append(payload)
                await route.fulfill(status=409 if payload.get('decision') == 'accept' and len(feedback_posts) == 1 else 200,
                                    json={'error': {'code': 'conflict', 'message': 'stale projection'}} if payload.get('decision') == 'accept' and len(feedback_posts) == 1 else {'version': 'projection-v2'})
                return
            if path == '/api/worldline/topics/macro%3Awork' or path == '/api/worldline/topics/macro:work':
                topic_detail_requests.append({key: values[0] for key, values in parse_qs(urlparse(request.url).query).items()})
                await route.fulfill(json={
                    'version': 'projection-v1', 'total': 4, 'next_offset': None,
                    'stats': FIXTURE['stats'],
                    'topic': {'id': 'macro:work', 'title': '工作与项目', 'source_topic_ids': ['topic:atlas', 'topic:more'], 'source_topic_count': 2, 'has_more': False, 'lanes': [
                        {'id': 'topic:more:facts', 'title': '后续来源主题', 'subject': 'Atlas 项目', 'category': 'facts', 'source_topic_ids': ['topic:more'], 'claims': [
                            {'id': 'claim:later', 'version': 'c1', 'summary': '追加分页中的第四条主张', 'kind': 'observation', 'status': 'reviewed', 'valid_from': '2024-05-01T00:00:00Z', 'mentioned_at': None, 'time_basis': '明确适用日期', 'time_precision': 'instant', 'change_type': 'report', 'evidence': [], 'conflict_with': []},
                        ]},
                    ]},
                })
                return
            if path == '/api/worldline/threads':
                if 'missing' in parse_qs(urlparse(request.url).query).get('q', [''])[0]:
                    await route.fulfill(json={'version': 'projection-v1', 'total': 0, 'threads': [], 'range': {'start': '2022-01-01T00:00:00Z', 'end': '2025-01-01T00:00:00Z', 'mentioned_start': '2022-06-29T00:00:00Z', 'mentioned_end': '2024-09-12T00:00:00Z'}})
                else:
                    query = parse_qs(urlparse(request.url).query)
                    offset = int(query.get('offset', ['0'])[0])
                    end = min(offset + 8, len(THREADS))
                    payload = {'version': 'projection-v1', 'total': len(THREADS), 'cross_conversation_threads': 1, 'threads': THREADS[offset:end], 'range': {'start': '2022-01-01T00:00:00Z', 'end': '2025-01-01T00:00:00Z', 'mentioned_start': '2022-06-29T00:00:00Z', 'mentioned_end': '2024-09-12T00:00:00Z'}}
                    if end < len(THREADS): payload['next_offset'] = end
                    await route.fulfill(json=payload)
                return
            if path.startswith('/api/worldline/threads/'):
                thread_id = unquote(path.rsplit('/', 1)[-1])
                if thread_id == 'thread:atlas':
                    entries = [
                        thread_entry(FIXTURE['topics'][0]['lanes'][0]['claims'][0]),
                        thread_entry({**FIXTURE['topics'][0]['lanes'][0]['claims'][1], 'summary': 'Atlas 项目预算更新为 23 万元'} if REVIEW_STATE['source_changed'] else FIXTURE['topics'][0]['lanes'][0]['claims'][1]),
                        thread_entry(FIXTURE['topics'][0]['lanes'][1]['claims'][0], lane_id='topic:atlas:plans', relation='plan'),
                    ]
                    unknown_entries = [UNKNOWN_MENTION]
                    thread = THREADS[0]
                else:
                    entries = [thread_entry(FIXTURE['topics'][0]['lanes'][0]['claims'][1], 'macro:life', 'topic:home:facts', 'first_mention')]
                    unknown_entries = []
                    thread = THREADS[1]
                query = parse_qs(urlparse(request.url).query)
                entries_offset = int(query.get('entries_offset', ['0'])[0])
                unknown_offset = int(query.get('unknown_offset', ['0'])[0])
                await route.fulfill(json={
                    'version': 'projection-v1', 'thread': thread,
                    'entries_total': len(entries), 'unknown_entries_total': len(unknown_entries),
                    'entries': entries[entries_offset:entries_offset + 25], 'unknown_entries': unknown_entries[unknown_offset:unknown_offset + 25],
                })
                return
            if path == '/api/worldline':
                if 'q=missing' in urlparse(request.url).query:
                    await route.fulfill(json={**FIXTURE, 'topics': [], 'total': 0, 'stats': {**FIXTURE['stats'], 'topics': 0, 'claims': 0, 'unknown_time': 0, 'conflicts': 0}})
                else:
                    await route.fulfill(json=FIXTURE)
                return
            if path.startswith('/api/items/'):
                await route.fulfill(json={'experimental': False, 'item': {'audio_status': 'available'}, 'evidence': []})
                return
            if path == '/api/conversations':
                await route.fulfill(json={'items': [], 'next_cursor': None})
                return
            if path == '/api/topics':
                await route.fulfill(json={'topics': []})
                return
            await route.continue_()
        await page.route('**/api/**', route_api)
        try:
            await page.goto(BASE, wait_until='networkidle')
            await page.get_by_role('heading', name='主题世界线').wait_for()
            await page.get_by_role('heading', name='适用时间未知').wait_for()
            await page.screenshot(path=str(OUT.with_suffix('.png')), full_page=True)
            await page.get_by_text('当时这样说过，不代表事实生效').first.wait_for()
            axis_box = await page.locator('.wl-thread-board .wl-axis-track').first.bounding_box()
            lane_box = await page.locator('.wl-thread-row .wl-thread-track').first.bounding_box()
            assert abs(axis_box['x'] - lane_box['x']) < 1
            result['checks'].append({'name': 'mention-axis-and-thread-lanes-align', 'status': 'PASS'})
            assert await page.get_by_role('button', name='Atlas 项目').count() >= 1
            atlas_row = page.locator('.wl-thread-row').filter(has_text='Atlas 项目').first
            await atlas_row.get_by_text('2 处关系待核对').wait_for()
            assert await atlas_row.get_by_text('关系冲突').count() == 0
            result['checks'].append({'name': 'semantic-review-count-is-not-presented-as-fact-conflict', 'status': 'PASS'})
            assert await page.locator('.wl-thread-marker').count() >= 2
            await page.locator('.wl-thread-entries li').first.wait_for()
            assert await page.locator('.wl-thread-entries li').count() == 3
            suggestion = page.locator('.wl-semantic-suggestion').first
            await suggestion.get_by_text('语义建议 · 待核对').wait_for()
            assert await suggestion.get_by_text('synthetic/mock-engine').count() == 1
            await page.locator('.wl-semantic-suggestion').nth(1).get_by_text('无法判断，来源信息不足').wait_for()
            assert await suggestion.get_by_role('button', name='查看这条整理记录').count() == 2
            assert await suggestion.get_by_role('link', name='打开来源对话').count() == 2
            before_suggestion_feedback = len(feedback_posts)
            await suggestion.get_by_role('button', name='查看这条整理记录').first.click()
            await page.get_by_role('complementary', name='主张详情').get_by_text('Mina 负责设计审查').wait_for()
            assert len(feedback_posts) == before_suggestion_feedback
            result['checks'].append({'name': 'semantic-suggestion-is-review-only-and-opens-source-side', 'status': 'PASS', 'model': 'synthetic/mock-engine'})
            assert await page.locator('.wl-thread-unknown').get_by_text('提及日期不明的项目安排').count() == 1
            await page.locator('.wl-thread-entries li > button').first.click()
            thread_inspector = page.get_by_role('complementary', name='主张详情')
            await thread_inspector.get_by_text('当时这样说过，不代表事实生效').wait_for()
            assert await thread_inspector.get_by_text('同主题不代表同一说话人').count() == 1
            result['checks'].append({'name': 'default-topic-thread-mentions-stay-distinct-from-valid-time', 'status': 'PASS'})
            # Exercise semantic review's conflict/rebase path, retaining the
            # chosen decision and rendering current source text before retry.
            review_suggestion = page.locator('.wl-semantic-suggestion').first
            await review_suggestion.get_by_role('button', name='接受关系建议').click()
            await review_suggestion.get_by_role('alert').wait_for()
            await review_suggestion.get_by_text('当前：Atlas 项目预算更新为 23 万元').wait_for()
            retry_review = review_suggestion.get_by_role('button', name='重试已接受决定')
            await retry_review.wait_for()
            await retry_review.click()
            await page.locator('.wl-semantic-suggestion').first.get_by_role('button', name='重新打开核对').wait_for()
            assert len(semantic_review_posts) == 2
            assert semantic_review_posts[0]['request_id'] == semantic_review_posts[1]['request_id']
            result['checks'].append({'name': 'semantic-review-409-preserves-choice-and-compares-updated-sources', 'status': 'PASS'})
            await page.locator('.wl-semantic-suggestion').first.get_by_role('button', name='重新打开核对').click()
            await page.locator('.wl-semantic-suggestion').first.get_by_role('button', name='接受关系建议').wait_for()
            assert semantic_review_posts[-1]['decision'] == 'reopen'
            result['checks'].append({'name': 'semantic-review-reopen', 'status': 'PASS'})
            # Rename, group two synthetic topics, then revoke that group.
            atlas_row = page.locator('.wl-thread-row').filter(has_text='Atlas 项目').first
            async def accept_prompt(dialog): await dialog.accept('合成主题别名')
            page.once('dialog', accept_prompt)
            await atlas_row.get_by_role('button', name='重命名').click()
            await page.wait_for_timeout(100)
            assert thread_mutations[-1]['title'] == '合成主题别名'
            result['checks'].append({'name': 'topic-rename-ui-request', 'status': 'PASS'})
            await page.get_by_label('选择主题 Atlas 项目 用于分组').check()
            await page.get_by_label('选择主题 生活与居住安排 用于分组').check()
            async def accept_group_prompt(dialog): await dialog.accept('合成分组')
            page.once('dialog', accept_group_prompt)
            await page.get_by_role('button', name='手动分组').click()
            await page.wait_for_timeout(150)
            group_request = next(post for post in thread_mutations if post.get('action') == 'group')
            assert len(group_request['thread_ids']) == 2 and group_request['title'] == '合成分组'
            result['checks'].append({'name': 'topic-group-ui-request', 'status': 'PASS'})
            atlas_row = page.locator('.wl-thread-row').filter(has_text='Atlas 项目').first
            await atlas_row.get_by_role('button', name='撤销分组').click()
            await page.wait_for_timeout(150)
            assert any(post.get('action') == 'revoke' and post.get('group_id') == 'thread:atlas' for post in thread_mutations)
            result['checks'].append({'name': 'topic-group-revoke-ui-request', 'status': 'PASS'})
            await page.get_by_role('button', name='整理世界线').click()
            await page.get_by_role('alert').filter(has_text='synthetic retry').wait_for()
            await page.get_by_role('button', name='重试整理').click()
            await page.get_by_role('status').filter(has_text='整理完成 · synthetic/mock-engine').wait_for()
            assert generation_posts[0]['request_id'] == generation_posts[1]['request_id']
            await page.get_by_role('button', name='整理世界线').click()
            await page.get_by_role('status').filter(has_text='语义引擎未配置').wait_for()
            result['checks'].append({'name': 'generation-error-retry-keeps-request-id', 'status': 'PASS'})
            result['checks'].append({'name': 'unconfigured-engine-is-explicit', 'status': 'PASS'})
            await page.get_by_role('button', name='下一页').click()
            await page.locator('.wl-thread-toggle').filter(has_text='跨会话主题 9').wait_for()
            assert await page.locator('.wl-thread-row').count() == 1
            await page.get_by_role('button', name='上一页').click()
            await page.locator('.wl-thread-toggle').filter(has_text='Atlas 项目').wait_for()
            result['checks'].append({'name': 'all-concrete-topics-are-searchable-and-paged', 'status': 'PASS'})
            await page.get_by_role('button', name='事实适用时间').click()
            await page.get_by_role('heading', name='适用时间未知').wait_for()
            await page.get_by_label('搜索主题与主张').fill('Atlas')
            await page.get_by_role('button', name='搜索').click()
            await page.get_by_role('button', name='Atlas 项目预算为 20 万元', exact=True).wait_for()
            await page.get_by_role('button', name='Atlas 项目预算为 20 万元', exact=True).click()
            await page.get_by_role('complementary', name='主张详情').wait_for()
            inspector = page.get_by_role('complementary', name='主张详情')
            assert await inspector.get_by_text('原文提及时间').count() == 1
            assert await inspector.get_by_text('Atlas 今年的项目预算大概是二十万元。').count() == 1
            result['checks'].append({'name': 'dated-claim-inspector-source', 'status': 'PASS'})
            await page.locator('.wl-lane-facts .wl-lane-actions').first.get_by_role('button', name='合并').click()
            merge_dialog = page.get_by_role('dialog', name='合并主题')
            assert await merge_dialog.locator('select option').count() == 1
            await merge_dialog.get_by_role('button', name='取消').click()
            result['checks'].append({'name': 'merge-destinations-match-subject-and-category', 'status': 'PASS'})
            await inspector.get_by_role('button', name='修正记录').click()
            dialog = page.get_by_role('dialog', name='修正记录')
            summary = dialog.locator('textarea').first
            await summary.fill('修正后的 Atlas 项目预算是 21 万元')
            await dialog.get_by_role('button', name='保存').click()
            await dialog.get_by_role('alert').wait_for()
            assert await summary.input_value() == '修正后的 Atlas 项目预算是 21 万元'
            await dialog.get_by_role('button', name='保存').click()
            await dialog.wait_for(state='detached')
            assert feedback_posts[-1]['expected_version'] == 'projection-v1'
            assert feedback_posts[-1]['action'] == 'correct'
            assert feedback_posts[-1]['target_claim_id'] == 'claim:budget'
            result['checks'].append({'name': '409-draft-retained-and-retry-contract', 'status': 'PASS'})
            await page.locator('.wl-unknown-list button').click()
            await page.get_by_role('complementary', name='主张详情').get_by_text('未知 / 开放').wait_for()
            result['checks'].append({'name': 'undated-claim-remains-in-separate-panel', 'status': 'PASS'})
            await page.get_by_role('button', name='加载更多主张').click()
            new_lane = page.locator('.wl-lane').filter(has_text='后续来源主题')
            await new_lane.wait_for()
            assert await page.get_by_text('后续来源主题').count() == 1
            assert '追加分页中的第四条主张' in (await new_lane.locator('.wl-claim-bar').get_attribute('title') or '')
            result['checks'].append({'name': 'topic-detail-pagination-adds-new-source-lane', 'status': 'PASS'})
            assert topic_detail_requests and topic_detail_requests[-1].get('q') == 'Atlas' and topic_detail_requests[-1].get('status') == 'all'
            result['checks'].append({'name': 'validity-detail-pagination-preserves-search-and-status', 'status': 'PASS'})
            await page.get_by_label('搜索主题与主张').fill('')
            await page.get_by_role('button', name='搜索').click()
            await page.locator('.wl-unknown-list button').wait_for()
            await page.get_by_role('button', name='周', exact=True).click()
            await page.get_by_role('button', name='向前浏览').click()
            result['checks'].append({'name': 'zoom-and-pan-controls', 'status': 'PASS'})
            await page.get_by_role('link', name='录音', exact=True).click()
            await page.get_by_role('heading', name='时间线').wait_for()
            await page.goto(BASE, wait_until='networkidle')
            await page.get_by_label('搜索主题与主张').fill('missing')
            await page.get_by_role('button', name='搜索').click()
            await page.get_by_text('没有匹配的具体主题').wait_for()
            result['checks'].append({'name': 'empty-state', 'status': 'PASS'})
            await page.set_viewport_size({'width': 390, 'height': 844})
            await page.goto(BASE, wait_until='networkidle')
            await page.get_by_role('heading', name='主题世界线').wait_for()
            await page.locator('.wl-thread-row .wl-thread-track').first.wait_for()
            overflow = await page.evaluate('document.documentElement.scrollWidth > window.innerWidth')
            result['checks'].append({'name': 'responsive-390-no-page-overflow', 'status': 'FAIL' if overflow else 'PASS'})
            mobile_axis = await page.locator('.wl-thread-board .wl-axis-track').first.bounding_box()
            mobile_lane = await page.locator('.wl-thread-row .wl-thread-track').first.bounding_box()
            assert abs(mobile_axis['x'] - mobile_lane['x']) < 1
            result['checks'].append({'name': 'mobile-mention-axis-and-thread-lanes-align', 'status': 'PASS'})
        except Exception as e:
            result['error'] = repr(e)
            result['checks'].append({'name': 'browser-run', 'status': 'FAIL', 'detail': repr(e), 'traceback': traceback.format_exc()})
            await page.screenshot(path=str(OUT.with_suffix('.png')), full_page=True)
        finally:
            await browser.close()
    failed = [check for check in result['checks'] if check.get('status') != 'PASS']
    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text(json.dumps(result, ensure_ascii=False, indent=2) + '\n')
    print(json.dumps(result, ensure_ascii=False, indent=2))
    if result.get('error') or failed:
        raise SystemExit(1)

asyncio.run(main())
