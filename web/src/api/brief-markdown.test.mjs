import test from 'node:test'
import assert from 'node:assert/strict'
import { exportBriefMarkdown } from './brief-markdown.ts'

const result = {
  snapshot_id: 'snap-1',
  scope: { kind: 'conversation', conversation_ids: ['conv-1'], topic_ids: [], person_ids: [] },
  sections: { background: [{ id: 'n1', text: 'A **claim** <script>alert(1)</script> &lt;svg&gt;', evidence_ids: ['ev1'], source_state: 'superseded', assertion_state: 'proposed', text_truncated: true }], previous_discussions: [], follow_up_candidates: [], changes_conflicts: [], unknowns: [{ id: 'n2', text: 'Unknown time', evidence_ids: [], source_state: 'current', assertion_state: 'unverified' }] },
  evidence: [{ id: 'ev1', turn_id: 'turn-1', conversation_id: 'conv-1', recording_id: 'rec-1', source_attachment_id: 'att-1', transcript_revision: 'rev-2', text: 'Quote | with `markup`', started_at: null, source_state: 'unavailable', provenance: '<img src=x>', topic_ids: [], person_ids: [] }],
  coverage: { eligible_turns: 5, selected_turns: 2, selected_notes: 1, omitted_turns: 3, omitted_notes: 0 },
  gaps: [{ kind: 'unknown_time_excluded', detail: '1 turn excluded' }], truncated: true, degraded: true,
}

test('compact and detailed exports are deterministic and preserve provenance caveats', () => {
  const compact = exportBriefMarkdown(result, 'compact')
  assert.equal(compact, exportBriefMarkdown(result, 'compact'))
  assert.match(compact, /Snapshot: snap\\-1/)
  assert.match(compact, /\\\*\\\*claim\\\*\\\*/)
  assert.match(compact, /正文已截断/)
  assert.match(compact, /未知时间/)
  assert.match(compact, /无法撤回/)
  assert.match(compact, /修订：rev\\-2；来源附件：att\\-1/)
  const detailed = exportBriefMarkdown(result, 'detailed')
  assert.match(detailed, /att\\-1/)
  assert.match(detailed, /rev\\-2/)
  assert.match(detailed, /ev1/)
  assert.match(detailed, /1 turn excluded/)
})

test('untrusted text cannot inject HTML or Markdown links', () => {
  const text = exportBriefMarkdown(result, 'detailed')
  assert.doesNotMatch(text, /<(?:script|img)/)
  assert.match(text, /&lt;script&gt;/)
  assert.match(text, /&amp;lt;svg&amp;gt;/)
  assert.match(text, /\\\|/)
  assert.match(text, /\\`markup\\`/)
})

test('real conversation scope payloads may omit irrelevant ID arrays', () => {
  const { topic_ids, person_ids, ...conversationScope } = result.scope
  const realShape = { ...result, scope: conversationScope }
  assert.doesNotThrow(() => exportBriefMarkdown(realShape, 'compact'))
  assert.match(exportBriefMarkdown(realShape, 'compact'), /会话 conv\\-1/)
})

test('hostile multiline transcript text stays inside its list item', () => {
  const hostile = { ...result, sections: { ...result.sections, background: [{ ...result.sections.background[0], text: 'safe\n- forged heading\n    code block\n[link](javascript:alert(1))' }] } }
  const exported = exportBriefMarkdown(hostile, 'compact')
  assert.doesNotMatch(exported, /\n- forged heading|\n    code block|\[link\]\(/)
  assert.match(exported, /\\- forged heading/)
})
