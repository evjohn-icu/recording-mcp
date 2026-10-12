#!/usr/bin/env python3
"""lite-transcribe: verbatim transcription pipeline on doubao-seed-2.1-lite (Ark coding gateway).

Pipeline per --audio file:
  ffprobe duration -> silencedetect -> silence-aligned chunk plan (~chunk-seconds)
  -> extract 16k mono mp3 chunks (cached) -> transcribe chunks (concurrent, thinking off,
  far-field-aware verbatim prompt) -> per-chunk sanity scan + one re-ask for flagged chunks
  -> assemble with [mm:ss] markers -> <stem>.lite.txt

Resume-safe: chunk plan.json and chunk-N.mp3/.txt are cached in --work/<stem>/; re-runs only
process missing pieces. Exit 1 if any chunk failed after retries.

Usage:
  scripts/lite-transcribe.py --audio /path/to/a.mp3 [--audio b.mp3] --out /path/to/transcripts
Key: --key > $ARK_CODING_API_KEY > apiKey of provider "volces-coding-02" in ~/.omp/agent/models.yml
"""
import argparse, base64, difflib, json, os, re, subprocess, sys, time, urllib.error, urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path

DEFAULT_ENDPOINT = 'https://ark.cn-beijing.volces.com/api/coding/v3/chat/completions'
DEFAULT_MODEL = 'doubao-seed-2.1-lite'
PROMPT = ('只输出转写文本本身。请逐字转写这段音频中的所有语音内容,包括远处、小声、模糊的对话也要转写,'
          '按时间顺序输出,加标点。不要时间戳、不要编号、不要总结、不要解释;听不清的词用(听不清)标注。')
NUDGE = '上一次输出无效。请只重新逐字转写这段音频里的语音内容(含小声对话),不要输出时间戳、列表或说明。'
TS_PATTERN = re.compile(r'\d+\.\d+\s*-\s*\d+\.\d+')
SANITY_MAX_CHARS = 4000
HEAD_REPAIR_CHARS_PER_SEC = 0.3  # below this (with voice present) the chunk likely lost its opening

def log(*a):
    print(*a, flush=True)

def run(cmd):
    return subprocess.run(cmd, capture_output=True, text=True)

def dur_of(f):
    out = run(['ffprobe', '-v', 'error', '-show_entries', 'format=duration', '-of', 'csv=p=0', f]).stdout.strip()
    return float(out)

def silence_spans(f):
    p = run(['ffmpeg', '-i', f, '-af', 'silencedetect=noise=-32dB:d=0.4', '-f', 'null', '-']).stderr
    starts = [float(x) for x in re.findall(r'silence_start: ([0-9.]+)', p)]
    ends = [float(x) for x in re.findall(r'silence_end: ([0-9.]+)', p)]
    return [(s, ends[i] if i < len(ends) else s + 0.4) for i, s in enumerate(starts)]

def plan_chunks(dur, spans, target, window, min_chunk):
    mids = sorted((s + e) / 2 for s, e in spans)
    cuts, t = [0.0], target
    while t < dur - 30.0:
        cands = [m for m in mids if t - window <= m <= t + window and m > cuts[-1] + min_chunk]
        cut = min(cands, key=lambda m: abs(m - t)) if cands else t
        cuts.append(cut); t = cut + target
    cuts.append(dur)
    return [(a, b) for a, b in zip(cuts, cuts[1:]) if b - a > 1.0]

def voiced_ratio(a, b, spans):
    tot = b - a
    if tot <= 0:
        return 0.0
    sil = sum(max(0.0, min(b, e) - max(a, s)) for s, e in spans)
    return 1.0 - sil / tot

def extract_chunk(src, a, b, dst):
    r = run(['ffmpeg', '-y', '-v', 'error', '-ss', f'{a:.2f}', '-to', f'{b:.2f}', '-i', src,
             '-ac', '1', '-ar', '16000', '-b:a', '24k', str(dst)])
    if r.returncode != 0:
        raise RuntimeError(f'ffmpeg chunk extract failed: {r.stderr[:200]}')

def resolve_key(args):
    if args.key:
        return args.key
    env = os.environ.get('ARK_CODING_API_KEY')
    if env:
        return env
    yml = Path.home() / '.omp/agent/models.yml'
    if yml.exists():
        text = yml.read_text()
        m = re.search(r'volces-coding-02:.*?apiKey:\s*(\S+)', text, re.S)
        if m:
            return m.group(1)
    sys.exit('no API key: --key, $ARK_CODING_API_KEY, or ~/.omp/agent/models.yml volces-coding-02')

class Gateway:
    def __init__(self, endpoint, key, model):
        self.endpoint, self.key, self.model = endpoint, key, model

    def chat(self, b64, prompt, thinking=False):
        body = {'model': self.model, 'messages': [{'role': 'user', 'content': [
            {'type': 'text', 'text': prompt},
            {'type': 'input_audio', 'input_audio': {'data': b64, 'format': 'mp3'}}]}]}
        if not thinking:
            body['thinking'] = {'type': 'disabled'}
        req = urllib.request.Request(self.endpoint, data=json.dumps(body).encode(),
                                     headers={'Authorization': f'Bearer {self.key}', 'Content-Type': 'application/json'})
        with urllib.request.urlopen(req, timeout=600) as r:
            resp = json.loads(r.read())
        if 'error' in resp:
            raise RuntimeError(resp['error'].get('message', str(resp['error']))[:200])
        return resp['choices'][0]['message'].get('content') or '', resp.get('usage', {})

def sane(text):
    if not text or not text.strip():
        return 'EMPTY'
    if len(TS_PATTERN.findall(text)) >= 2:
        return 'TIMESTAMP_PATTERN'
    if len(text) > SANITY_MAX_CHARS:
        return 'LEN_OUTLIER'
    if text.count('(听不清)') > len(text) / 20:
        return 'UNINTELLIGIBLE_HEAVY'
    return None


def head_repair(gw, stem, idx, a, b, text, voiced, cp):
    """Chunk contains speech but transcript is suspiciously short -> lite drops the opening
    when the chunk starts with non-speech (attention contamination, observed on real data).
    Re-transcribe a narrow head window; splice in sentences not already present."""
    dur = b - a
    if voiced < 0.05 or len(norm_text(text)) >= HEAD_REPAIR_CHARS_PER_SEC * dur:
        return text, False
    head_b = a + min(90.0, dur / 2)
    raw, _ = gw.chat(base64.b64encode(extract_bytes(cp, a, head_b)).decode(), PROMPT)
    if sane(raw) or not raw.strip():
        return text, False
    have = set(re.findall(r'[\u4e00-\u9fff\w]{2,}', norm_text(text)))
    extra = [s for s in re.split(r'(?<=[。！？!?；;\n])', raw.strip()) if s.strip()
             and set(re.findall(r'[\u4e00-\u9fff\w]{2,}', norm_text(s))) - have]
    if not extra:
        return text, False
    log(f'  [{stem}#{idx}] head repair: +{len("".join(extra))} chars from [{a:.0f}-{head_b:.0f}s]')
    REPAIR_STATS['head_repaired'] = REPAIR_STATS.get('head_repaired', 0) + 1
    return ''.join(extra) + '\n' + text, True


REPAIR_STATS = {}


def norm_text(t):
    return re.sub(r'[^\w\u4e00-\u9fff]+', '', t or '')


def extract_bytes(cp, a, b):
    """Extract [0, b-a] from the chunk file — chunk timeline starts at 0, not at a."""
    import tempfile
    tmp = Path(tempfile.mkstemp(suffix='.mp3')[1])
    r = subprocess.run(['ffmpeg', '-y', '-v', 'error', '-i', str(cp), '-t', f'{b - a:.2f}',
                        '-ac', '1', '-ar', '16000', '-b:a', '24k', str(tmp)],
                       capture_output=True, text=True)
    if r.returncode != 0:
        raise RuntimeError(f'ffmpeg head extract failed: {r.stderr[:200]}')
    data = tmp.read_bytes()
    tmp.unlink(missing_ok=True)
    return data

def transcribe_chunk(gw, stem, idx, a, b, cp, txt_path, voiced, retries):
    if txt_path.exists() and txt_path.stat().st_size > 0:
        text = txt_path.read_text(encoding='utf-8').strip()
        if text and text != '(无有效语音)':
            text, _ = head_repair(gw, stem, idx, a, b, text, voiced, cp)
            txt_path.write_text(text, encoding='utf-8')
        return {'stem': stem, 'idx': idx, 'a': a, 'b': b, 'text': text,
                'usage': {}, 'cached': True, 'error': None}
    b64 = base64.b64encode(cp.read_bytes()).decode()
    last, usage = None, {}
    for attempt in range(1, retries + 1):
        try:
            text, usage = gw.chat(b64, PROMPT)
            flag = sane(text)
            if flag:
                log(f'  [{stem}#{idx}] sanity flag {flag}; re-asking')
                text2, _ = gw.chat(b64, NUDGE)
                if sane(text2) is None and len(text2) > len(text):
                    text = text2
                flag = sane(text)
                # a far-field/quiet chunk may legitimately be mostly (听不清): accept rather than fail
                if flag == 'UNINTELLIGIBLE_HEAVY' and text.strip():
                    flag = None
            txt_path.write_text(text, encoding='utf-8')
            if text and text.strip() and text != '(无有效语音)':
                text, _ = head_repair(gw, stem, idx, a, b, text, voiced, cp)
                txt_path.write_text(text, encoding='utf-8')
            return {'stem': stem, 'idx': idx, 'a': a, 'b': b, 'text': text.strip(), 'usage': usage,
                    'cached': False, 'error': flag}
        except Exception as e:
            last = f'HTTP/{type(e).__name__}: {str(e)[:180]}'
            log(f'  [{stem}#{idx}] attempt {attempt} failed: {last}')
            time.sleep(15 * attempt)
    return {'stem': stem, 'idx': idx, 'a': a, 'b': b, 'text': None, 'usage': usage, 'cached': False, 'error': last}

def mmss(sec):
    return f'{int(sec) // 60:02d}:{int(sec) % 60:02d}'

def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument('--audio', action='append', required=True)
    ap.add_argument('--out', required=True, help='output dir for <stem>.lite.txt')
    ap.add_argument('--work', default='/tmp/lite-transcribe-work')
    ap.add_argument('--chunk-seconds', type=float, default=240.0)
    ap.add_argument('--window', type=float, default=30.0)
    ap.add_argument('--min-chunk', type=float, default=30.0)
    ap.add_argument('--concurrency', type=int, default=3)
    ap.add_argument('--retries', type=int, default=3)
    ap.add_argument('--model', default=DEFAULT_MODEL)
    ap.add_argument('--endpoint', default=DEFAULT_ENDPOINT)
    ap.add_argument('--key', default=None)
    args = ap.parse_args()

    gw = Gateway(args.endpoint, resolve_key(args), args.model)
    work, out = Path(args.work), Path(args.out)
    work.mkdir(parents=True, exist_ok=True)
    out.mkdir(parents=True, exist_ok=True)

    jobs, plans, spans_by_stem, voiced_map = [], {}, {}, {}
    for src in args.audio:
        stem = Path(src).stem
        d = work / stem
        d.mkdir(parents=True, exist_ok=True)
        plan_p = d / 'plan.json'
        if plan_p.exists():
            chunks = [tuple(c) for c in json.loads(plan_p.read_text())]
        else:
            dur = dur_of(src)
            chunks = plan_chunks(dur, silence_spans(src), args.chunk_seconds, args.window, args.min_chunk)
            plan_p.write_text(json.dumps(chunks))
        plans[stem] = chunks
        spans_by_stem[stem] = silence_spans(src)
        log(f'{stem}: dur={dur_of(src):.0f}s chunks={len(chunks)}')
        for i, (a, b) in enumerate(chunks, 1):
            cp = d / f'chunk-{i:02d}.mp3'
            if not (cp.exists() and cp.stat().st_size > 1024):
                extract_chunk(src, a, b, cp)
            if voiced_ratio(a, b, spans_by_stem[stem]) < 0.02 and not (d / f'chunk-{i:02d}.txt').exists():
                (d / f'chunk-{i:02d}.txt').write_text('(无有效语音)', encoding='utf-8')
                log(f'  [{stem}#{i}] skipped: voiced ratio < 2%')
            jobs.append((stem, i, a, b, cp, d / f'chunk-{i:02d}.txt'))
            voiced_map[(stem, i)] = voiced_ratio(a, b, spans_by_stem[stem])

    log(f'total chunks: {len(jobs)}; concurrency={args.concurrency}')
    results, usage_tot = {}, {'audio_tokens': 0, 'completion_tokens': 0, 'requests': 0, 'failed': 0, 'cached': 0}
    with ThreadPoolExecutor(max_workers=args.concurrency) as ex:
        futs = {ex.submit(transcribe_chunk, gw, *j, voiced_map[(j[0], j[1])], args.retries): j for j in jobs}
        for fu in as_completed(futs):
            r = fu.result()
            results[(r['stem'], r['idx'])] = r
            u = r['usage'] or {}
            usage_tot['audio_tokens'] += (u.get('prompt_tokens_details') or {}).get('audio_tokens', 0) or 0
            usage_tot['completion_tokens'] += u.get('completion_tokens', 0) or 0
            usage_tot['requests'] += 0 if r['cached'] else 1
            usage_tot['cached'] += 1 if r['cached'] else 0
            if r['error']:
                usage_tot['failed'] += 1
                log(f"{r['stem']}#{r['idx']} FAILED: {r['error']}")
            else:
                log(f"{r['stem']}#{r['idx']} ok {'(cached)' if r['cached'] else ''} "
                    f"audio_tok={(u.get('prompt_tokens_details') or {}).get('audio_tokens', 0)} :: {(r['text'] or '')[:40]!r}")

    for src in args.audio:
        stem = Path(src).stem
        parts = []
        for i, (a, b) in enumerate(plans[stem], 1):
            r = results[(stem, i)]
            if r['text'] is None:
                parts.append(f'[{mmss(a)}] (本段转写失败: {r["error"]})')
            else:
                body = r['text'].strip() if r['text'] and r['text'].strip() else '(无有效语音)'
                parts.append(f'[{mmss(a)}]\n{body}')
        dest = out / f'{stem}.lite.txt'
        dest.write_text('\n\n'.join(parts) + '\n', encoding='utf-8')
        log(f'wrote {dest} ({len(parts)} parts)')

    log('USAGE', json.dumps({**usage_tot, 'head_repaired': REPAIR_STATS.get('head_repaired', 0)}))
    sys.exit(1 if usage_tot['failed'] else 0)

if __name__ == '__main__':
    main()
