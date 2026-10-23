#!/usr/bin/env python3
"""Validate and score the frozen synthetic Worldline relation fixture."""
from __future__ import annotations
import argparse, collections, hashlib, ipaddress, json, math, statistics, time, urllib.error, urllib.parse, urllib.request
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_FIXTURE = ROOT / 'fixtures/worldline-relations-quality.json'
LABELS = {
    'text_similarity': 'The supplied text has substantially similar content; this does not establish same speaker or same real-world fact.',
    'complementary_context': 'The text adds context without a supported contradiction.',
    'review_required': 'The supplied text may conflict; human review is required and no reality change is asserted.',
    'unrelated': 'The excerpts concern unrelated content despite the exact same source topic title.',
    'insufficient_evidence': 'The supplied evidence is clipped, missing, ambiguous, or otherwise insufficient to judge.',
}
INSTRUCTIONS = 'Compare only the supplied pair; both have the same normalized source-topic title. Identity is unconfirmed unless explicitly stated, so judge text resemblance only and never imply the same speaker or real-world fact. Use summaries and marked transcript excerpts. Choose insufficient_evidence when the evidence does not support a clear judgment. review_required is a human-review hint, not an assertion of truth or change.'

def validate_fixture(f):
    if not isinstance(f,dict) or f.get('synthetic_only') is not True or f.get('fixture_version') != 'worldline-relations-quality-v1' or not isinstance(f.get('cases'),list): raise ValueError('invalid fixture header')
    cases=f['cases']; ids=set(); groups=set(); splits=collections.Counter(); labels=collections.Counter()
    for c in cases:
        if not isinstance(c,dict) or not isinstance(c.get('id'),str) or not c['id'] or c['id'] in ids: raise ValueError('case IDs must be unique non-empty strings')
        ids.add(c['id'])
        if c.get('expected') not in LABELS: raise ValueError(f"{c['id']}: invalid label")
        if c.get('split') not in ('calibration','test'): raise ValueError(f"{c['id']}: invalid split")
        group=c.get('group')
        if not isinstance(group,str) or not group or group in groups: raise ValueError('groups must be unique across splits')
        groups.add(group); splits[c['split']]+=1; labels[(c['split'],c['expected'])]+=1
        state=c.get('state')
        if not isinstance(state,dict) or set(state)!={'left','right'}: raise ValueError(f"{c['id']}: state must contain left and right only")
        for side in ('left','right'):
            item=state[side]
            if not isinstance(item,dict) or set(item)-{'title','summary','source','excerpt'}: raise ValueError('invalid pair item')
            if not all(isinstance(item.get(k),str) and item[k].strip() for k in ('title','summary','source')): raise ValueError('title, summary and source are required')
        if state['left']['title'] != state['right']['title']: raise ValueError('titles must be normalized identical')
        if any(k.lower() in {'expected','gold','label','answer','adjudication'} for k in state): raise ValueError('label leakage')
    if len(cases)!=120 or splits != {'calibration':60,'test':60}: raise ValueError('fixture must have exactly 120 cases, 60 per split')
    if any(labels[(s,l)]!=12 for s in ('calibration','test') for l in LABELS): raise ValueError('each split must have 12 examples per label')
    return f

def load_fixture(path): return validate_fixture(json.loads(Path(path).read_text(encoding='utf-8')))

def validate_answer(answer, labels):
    if not isinstance(answer,dict) or answer.get('type')!='choice': return None,None,'answer is not a choice object'
    choice=answer.get('choice'); probs=answer.get('probabilities')
    if not isinstance(choice,str) or choice not in labels: return None,None,'choice is not a declared label'
    if not isinstance(probs,dict) or set(probs)!=labels: return None,None,'probability keys do not match labels'
    vals={}
    for k,v in probs.items():
        if isinstance(v,bool) or not isinstance(v,(int,float)) or not math.isfinite(v) or not 0<=v<=1: return None,None,f'invalid probability for {k}'
        vals[k]=float(v)
    if abs(sum(vals.values())-1)>1e-6: return None,None,'probabilities do not sum to one'
    return choice,vals,None

def score(records):
    matrix={l:{p:0 for p in LABELS} for l in LABELS}; errors=0; abstain=0; valid=0
    for r in records:
        if r.get('prediction') is None: errors+=1; continue
        valid+=1; matrix[r['expected']][r['prediction']]+=1
        abstain+=r['prediction']=='insufficient_evidence'
    metrics={}
    for l in LABELS:
        tp=matrix[l][l]; fp=sum(matrix[g][l] for g in LABELS if g!=l); fn=sum(matrix[l][p] for p in LABELS if p!=l)
        metrics[l]={'support':sum(matrix[l].values()),'precision':tp/(tp+fp) if tp+fp else None,'recall':tp/(tp+fn) if tp+fn else None}
    return {'denominator':valid,'errors':errors,'abstain':abstain,'confusion':matrix,'per_class':metrics}

def endpoint_check(url):
    p=urllib.parse.urlsplit(url)
    try: ip=ipaddress.ip_address(p.hostname or '')
    except ValueError as e: raise ValueError('endpoint requires literal loopback IP') from e
    if p.scheme!='http' or not ip.is_loopback or p.username or p.password or p.query or p.fragment: raise ValueError('endpoint must be plain HTTP loopback without credentials/query/fragment')
    return url

class NoRedirectHandler(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def request(endpoint,case,model,timeout):
    item={'state':json.dumps(case['state'],ensure_ascii=False,separators=(',',':')),'questions':{case['id']:{'type':'choice','instructions':INSTRUCTIONS,'criteria':LABELS}}}
    if model:item['model']=model
    req=urllib.request.Request(endpoint,json.dumps(item,ensure_ascii=False).encode(),{'Content-Type':'application/json'},method='POST')
    opener=urllib.request.build_opener(urllib.request.ProxyHandler({}),NoRedirectHandler())
    start=time.monotonic()
    with opener.open(req,timeout=timeout) as res: data=json.loads(res.read())
    return data,(time.monotonic()-start)*1000

def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--endpoint',default='http://127.0.0.1:8799/v1/systemone'); p.add_argument('--model'); p.add_argument('--split',choices=('calibration','test'),default='test'); p.add_argument('--fixture',type=Path,default=DEFAULT_FIXTURE); p.add_argument('--output',type=Path); p.add_argument('--timeout',type=float,default=45); p.add_argument('--limit',type=int); p.add_argument('--validate-only',action='store_true')
    a=p.parse_args()
    try:
        endpoint_check(a.endpoint); fixture=load_fixture(a.fixture)
        if a.validate_only:
            print(json.dumps({'valid':True,'synthetic_only':True,'cases':len(fixture['cases'])},ensure_ascii=False)); return 0
        raw=a.fixture.read_bytes(); cases=[c for c in fixture['cases'] if c['split']==a.split]
        if a.limit is not None:
            if a.limit < 1: raise ValueError('--limit must be positive')
            cases=cases[:a.limit]
        records=[]; latencies=[]; identities=set(); versions=set()
        for c in cases:
            row={'id':c['id'],'expected':c['expected']}
            try:
                response,elapsed=request(a.endpoint,c,a.model,a.timeout); latencies.append(elapsed)
                if not isinstance(response,dict): raise ValueError('response is not an object')
                model_name=response.get('model'); weights=response.get('weights_version')
                if not isinstance(model_name,str) or not model_name.strip(): raise ValueError('engine model identity missing')
                if not isinstance(weights,str) or not weights.strip(): raise ValueError('engine weights identity missing')
                identities.add(model_name); versions.add(weights)
                answers=response.get('answers')
                if not isinstance(answers,dict) or set(answers)!={c['id']}: raise ValueError('answer question-ID mapping does not exactly match request')
                choice,dist,error=validate_answer(answers[c['id']],set(LABELS))
                if error: raise ValueError(error)
                row.update(prediction=choice,distribution=dist)
            except (urllib.error.URLError,TimeoutError,json.JSONDecodeError,OSError,ValueError) as e:
                row.update(prediction=None,error=f'{type(e).__name__}:{e}')
            records.append(row)
        identity_unstable=len(identities)!=1 or len(versions)!=1
        if identity_unstable:
            for row in records:
                if row.get('prediction') is not None:
                    row.update(prediction=None,distribution=None,error='model or weights identity missing/inconsistent across run')
        summary=score(records); ordered=sorted(latencies)
        incorrect=sum(r.get('prediction') is not None and r['prediction']!=r['expected'] for r in records)
        failed=summary['errors']>0 or incorrect>0 or identity_unstable
        result={'experiment':'worldline-relations-quality-v1','synthetic_only':True,'status':'FAIL' if failed else 'PASS','fixture_sha256':hashlib.sha256(raw).hexdigest(),'runner_sha256':hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),'split':a.split,'endpoint':a.endpoint,'requested_model':a.model,'actual_models':sorted(identities),'weights_versions':sorted(versions),'started_at':datetime.now(timezone.utc).isoformat(),'total':len(cases),'incorrect':incorrect,'latency_ms':{'mean':statistics.mean(latencies) if latencies else None,'p50':ordered[len(ordered)//2] if ordered else None,'p95':ordered[min(len(ordered)-1,int(.95*len(ordered)))] if ordered else None},**score(records),'cases':records}
        output=json.dumps(result,ensure_ascii=False,indent=2)+'\n'
        if a.output:
            a.output.parent.mkdir(parents=True,exist_ok=True)
            a.output.write_text(output,encoding='utf8')
        print(output)
        return 2 if failed else 0
    except (ValueError,OSError,json.JSONDecodeError) as e:
        p.error(str(e))

if __name__=='__main__': raise SystemExit(main())
