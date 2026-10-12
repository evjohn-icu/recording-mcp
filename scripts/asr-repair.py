#!/usr/bin/env python3
"""asr-repair: targeted repair of plan-ASR text with doubao-seed-2.1-lite.

Plan-ASR run data has no usable per-turn confidence (normalized confidence is null; the
indefinite utterance flag in server-only.jsonl marks streaming revisions, not quality).
What works (validated 2026-10): diff ASR turns against a lite transcript of the same
recording and repair the spans where they disagree most.

Modes:
  --lite FILE      audit mode: compare <stem>.lite.txt (produced by scripts/lite-transcribe.py,
                   [mm:ss] markers) against ASR turns; write repairs.md/.json proposals for the
                   worst-diverging spans (top --max-windows)
  --span A-B       repair explicit time range(s) in seconds (repeatable), proposal unless --apply

--apply is conservative: a span is rewritten ONLY when every overlapped ASR turn is empty or
(听不清)-dominated (clear garbage; diff against lite may otherwise be overlapping speech, not
an ASR error — those stay proposals in repairs.md). Rewrites are in-place: DB row content +
turn_json via (start,end,text) match (turn ids differ across ingests), original text kept in
turn_json.repaired.original; empty lite result blanks the turn.

Usage:
  scripts/asr-repair.py --run <stem> --lite /path/to/<stem>.lite.txt --audio-root /path/to/RECORD
  scripts/asr-repair.py --run <stem> --span 28-31 --apply
"""
import argparse, base64, difflib, json, os, re, sqlite3, subprocess, sys, time, urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
PROMPT = ('只输出转写文本本身。请逐字转写这段音频中的所有语音内容,包括远处、小声、模糊的对话也要转写,'
          '按时间顺序输出,加标点。不要时间戳、不要编号、不要总结、不要解释;听不清的词用(听不清)标注。')
PUNCT = re.compile(r'[\s，。！？、；：“”‘’《》（）\[\]\(\)!?,\.:;\'"…—·\-–<>《》【】~]+')


def log(*a):
    print(*a, flush=True)


def norm(s):
    return PUNCT.sub('', s or '')


def resolve_key(args):
    import os
    if args.key:
        return args.key
    env = os.environ.get('ARK_CODING_API_KEY')
    if env:
        return env
    yml = Path.home() / '.omp/agent/models.yml'
    m = re.search(r'volces-coding-02:.*?apiKey:\s*(\S+)', yml.read_text(), re.S) if yml.exists() else None
    return m.group(1) if m else sys.exit('no API key: --key, $ARK_CODING_API_KEY, or models.yml')


def lite_transcribe(audio_path, a, b, key, model, endpoint):
    tmp = Path('/tmp') / f'asr-repair-{Path(audio_path).stem}-{a:.0f}-{b:.0f}.mp3'
    r = subprocess.run(['ffmpeg', '-y', '-v', 'error', '-ss', f'{a:.2f}', '-to', f'{b:.2f}', '-i',
                        str(audio_path), '-ac', '1', '-ar', '16000', '-b:a', '24k', str(tmp)],
                       capture_output=True, text=True)
    if r.returncode != 0:
        raise RuntimeError(f'ffmpeg failed: {r.stderr[:200]}')
    body = {'model': model, 'thinking': {'type': 'disabled'}, 'messages': [{'role': 'user', 'content': [
        {'type': 'text', 'text': PROMPT},
        {'type': 'input_audio', 'input_audio': {'data': base64.b64encode(tmp.read_bytes()).decode(), 'format': 'mp3'}}]}]}
    req = urllib.request.Request(endpoint, data=json.dumps(body).encode(),
                                 headers={'Authorization': f'Bearer {key}', 'Content-Type': 'application/json'})
    t0 = time.time()
    with urllib.request.urlopen(req, timeout=600) as resp:
        out = json.loads(resp.read())
    text = out['choices'][0]['message'].get('content', '').strip()
    at = out.get('usage', {}).get('prompt_tokens_details', {}).get('audio_tokens', 0)
    tmp.unlink(missing_ok=True)
    return text, time.time() - t0, at


def asr_turns(run_dir):
    turns = []
    for p in sorted(run_dir.glob('segments/segment-*-normalized.json'),
                    key=lambda p: int(re.search(r'segment-(\d+)-', p.name).group(1))):
        for t in json.loads(p.read_text()).get('turns', []):
            if t.get('global_start_ms') is not None:
                turns.append(t)
    turns.sort(key=lambda t: t['global_start_ms'])
    return turns


def is_garbage(text):
    """ASR side carries no usable content: empty, or (听不清)-dominated fragments."""
    t = (text or '').strip()
    if not t:
        return True
    n_hedge = t.count('(听不清)')
    return n_hedge > 0 and len(norm(t)) <= 6 * n_hedge


def audit_spans(lite_path, turns, max_windows, min_sim=0.5, min_chars=40):
    """Compare lite marker-partitions against ASR turns in the same span; worst first."""
    raw = lite_path.read_text(encoding='utf-8')
    parts = [(int(m.group(1)) * 60 + int(m.group(2)), m.start())
             for m in re.finditer(r'^\[(\d{1,3}):(\d{2})\]\s*$', raw, flags=re.M)]
    dur_s = max((t.get('global_end_ms') or 0) for t in turns) / 1000 + 1
    parts.append((int(dur_s), len(raw)))
    spans = [(parts[i][0], parts[i + 1][0], raw[parts[i][1]:parts[i + 1][1]]) for i in range(len(parts) - 1)]
    scored = []
    for a, b, chunk in spans:
        lite_n = norm(re.sub(r'^\[.*$', '', chunk, flags=re.M))
        asr_n = norm(''.join(t['text'] for t in turns if t['global_start_ms'] < b * 1000 and
                             (t.get('global_end_ms') or 0) > a * 1000))
        if len(asr_n) + len(lite_n) < min_chars:
            continue
        sim = difflib.SequenceMatcher(None, asr_n, lite_n, autojunk=False).ratio()
        scored.append((sim, a, b, asr_n, lite_n))
    scored.sort(key=lambda s: s[0])
    return [(a, b, asr_n, lite_n) for sim, a, b, asr_n, lite_n in scored if sim < min_sim][:max_windows]


def turns_in_span(turns, a_ms, b_ms):
    return [t for t in turns if t['global_start_ms'] < b_ms
            and (t.get('global_end_ms') or t['global_start_ms'] + 1) > a_ms]




def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument('--run', required=True)
    ap.add_argument('--audio-root', default=os.environ.get('ASR_AUDIO_ROOT'),
                    help='directory containing <run>.mp3 (e.g. a RECORD directory)')
    ap.add_argument('--db', default=str(REPO / 'data/asr/library/context-memory.db'))
    ap.add_argument('--lite', help='audit against this <stem>.lite.txt')
    ap.add_argument('--span', action='append', default=[], help='A-B seconds, repeatable')
    ap.add_argument('--max-windows', type=int, default=5)
    ap.add_argument('--apply', action='store_true')
    ap.add_argument('--model', default='doubao-seed-2.1-lite')
    ap.add_argument('--endpoint', default='https://ark.cn-beijing.volces.com/api/coding/v3/chat/completions')
    ap.add_argument('--key', default=None)
    args = ap.parse_args()

    lib = REPO / 'data/asr/library'
    run_dir = lib / args.run
    root = args.audio_root or os.environ.get('ASR_AUDIO_ROOT')
    if not root:
        sys.exit('audio location unknown: pass --audio-root or set $ASR_AUDIO_ROOT (RECORD directory)')
    audio = next((p for p in Path(root).rglob(f'{args.run}.mp3')), None)
    if not audio:
        sys.exit(f'audio for {args.run} not found under {root}')
    key = resolve_key(args)
    turns = asr_turns(run_dir)
    log(f'{args.run}: {len(turns)} turns')

    spans = []
    if args.lite:
        cand = audit_spans(Path(args.lite), turns, args.max_windows)
        log(f'audit: {len(cand)} divergent span(s) below sim=0.5')
        spans += [(a * 1000, b * 1000) for a, b, _, _ in cand]
    for s in args.span:
        a, b = map(float, s.split('-'))
        spans.append((a * 1000, b * 1000))

    proposals, tok = [], 0
    for a_ms, b_ms in spans:
        sel = turns_in_span(turns, a_ms, b_ms)
        orig = ' / '.join(t['text'].strip() for t in sel)
        try:
            text, dt, at = lite_transcribe(audio, a_ms / 1000, b_ms / 1000, key, args.model, args.endpoint)
            tok += at
        except Exception as e:
            log(f'  [{a_ms/1000:.0f}-{b_ms/1000:.0f}s] lite FAILED: {e}')
            continue
        log(f'  [{a_ms/1000:.0f}-{b_ms/1000:.0f}s] turns={len(sel)} orig={len(norm(orig))}ch lite={len(norm(text))}ch {dt:.0f}s')
        log(f'    ASR : {orig[:90]}')
        log(f'    lite: {text[:90]}')
        proposals.append({'span_s': [round(a_ms / 1000, 2), round(b_ms / 1000, 2)],
                          'orig': orig, 'lite': text, 'turn_ids': [t['id'] for t in sel]})

    md = [f'# repair proposals: {args.run}', '', f'audio_tokens: {tok}', '']
    for p in proposals:
        md.append(f"## [{p['span_s'][0]:.0f}s - {p['span_s'][1]:.0f}s] ({len(p['turn_ids'])} turns)")
        md.append(f"- ASR: {p['orig'] or '(空)'}")
        md.append(f"- lite: {p['lite']}")
        md.append('')
    (run_dir / 'repairs.md').write_text('\n'.join(md), encoding='utf-8')
    (run_dir / 'repairs.json').write_text(json.dumps(proposals, ensure_ascii=False, indent=1), encoding='utf-8')
    log(f'wrote {run_dir}/repairs.md + repairs.json')

    if not args.apply:
        log('(proposal mode; add --apply to rewrite turns)')
        return
    db = sqlite3.connect(args.db)
    db.row_factory = sqlite3.Row
    # resolve the recording's DB id by duration (ingest-time ids are random hashes)
    dur_ms = max((t.get('global_end_ms') or 0) for t in turns)
    rid = db.execute('SELECT id FROM recordings WHERE duration_ms BETWEEN ? AND ?',
                     (dur_ms - 2000, dur_ms + 2000)).fetchall()
    if len(rid) != 1:
        sys.exit(f'cannot resolve recording id: {len(rid)} candidates by duration {dur_ms}ms')
    rid = rid[0]['id']
    db_rows = db.execute('SELECT id, global_start_ms, global_end_ms, content FROM turns '
                         'WHERE recording_id=?', (rid,)).fetchall()
    applied = matched = 0
    for p in proposals:
        a_ms, b_ms = p['span_s'][0] * 1000, p['span_s'][1] * 1000
        sel = turns_in_span(turns, a_ms, b_ms)
        if not all(is_garbage(t['text']) for t in sel):
            log(f"  hold [{p['span_s']}]: ASR side has real content -> proposal only "
                f"(diff may be overlapping speech, not error)")
            continue
        # map normalized turns to DB rows by (start,end,content) — ids differ across ingests;
        # empty-text turns were never ingested (content NOT NULL) -> normalized-only repair
        updates, normalized_only = [], []
        for t in sel:
            row = next((r for r in db_rows if r['global_start_ms'] == t['global_start_ms']
                        and r['global_end_ms'] == t['global_end_ms']
                        and (r['content'] or '') == (t['text'] or '')), None)
            if row is None:
                if not (t['text'] or '').strip():
                    normalized_only.append(t)
                else:
                    log(f"  warn: no DB row for non-empty turn at {t['global_start_ms']}ms")
                continue
            updates.append((t, row))
        lite_n = norm(p['lite'])
        new = lite_n if lite_n and lite_n not in ('无有效语音', '听不清') else ''
        if normalized_only:
            log(f'  info: {len(normalized_only)} empty turn(s) -> normalized.json only (never ingested)')
        for t, row in updates:
            tj_raw = db.execute('SELECT turn_json FROM turns WHERE id=?', (row['id'],)).fetchone()[0]
            tj = json.loads(tj_raw)
            tj['text'] = new
            tj['repaired'] = {'tool': 'asr-repair', 'span_ms': [int(a_ms), int(b_ms)],
                              'original_text': row['content']}
            cur = db.execute('UPDATE turns SET content=?, turn_json=? WHERE id=?',
                             (new, json.dumps(tj, ensure_ascii=False), row['id']))
            if cur.rowcount != 1:
                sys.exit(f'UPDATE matched {cur.rowcount} rows for {row["id"]} — aborting')
            matched += 1
        # normalized.json in place (DB-matched turns + empty-text turns)
        for p2 in run_dir.glob('segments/segment-*-normalized.json'):
            data = json.loads(p2.read_text())
            touched = False
            for t in data['turns']:
                if any(t['id'] == u[0]['id'] for u in updates) or \
                   any(t['id'] == e['id'] for e in normalized_only):
                    new = lite_n if lite_n and lite_n not in ('无有效语音', '听不清') else ''
                    t['repaired'] = {'tool': 'asr-repair', 'span_ms': [int(a_ms), int(b_ms)],
                                     'original_text': t['text']}
                    t['text'] = new
                    touched = True
            if touched:
                p2.write_text(json.dumps(data, ensure_ascii=False, indent=1), encoding='utf-8')
        applied += len(sel)
    db.commit()
    db.close()
    log(f'applied: {matched} DB row(s) rewritten across {applied} turn(s); originals in turn_json.repaired')


if __name__ == '__main__':
    main()
