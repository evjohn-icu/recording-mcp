#!/usr/bin/env python3
"""Private, segmented full-file SAUC transcription runner. API key is read via getpass."""
from __future__ import annotations
import asyncio, base64, getpass, gzip, json, os, shutil, struct, sys, time, uuid, hashlib
from datetime import datetime, timezone
from pathlib import Path
import websockets

ROOT=Path(__file__).resolve().parents[1]
AUDIO: Path | None = None
OUT=Path(os.environ.get('JEV_ASR_OUT') or (ROOT/'data/full-transcription')); TMP=OUT/'segments'; URL='wss://openspeech.bytedance.com/api/v3/plan/sauc/bigmodel_async'; RESOURCE='volc.seedasr.sauc.duration'
CHUNK=6400; SEG_MS=300000; OVERLAP_MS=2000; CONCURRENCY=3
FFPROBE=os.environ.get('JEV_FFPROBE') or shutil.which('ffprobe') or '/opt/homebrew/bin/ffprobe'
FFMPEG=os.environ.get('JEV_FFMPEG') or shutil.which('ffmpeg') or '/opt/homebrew/bin/ffmpeg'

def hdr(t,f): return bytes((0x11,(t<<4)|f,0x11,0))
def full(seq,obj):
 b=gzip.compress(json.dumps(obj,ensure_ascii=False,separators=(',',':')).encode()); return hdr(1,1)+struct.pack('>iI',seq,len(b))+b
def audio(seq,b,final=False): return hdr(2,3 if final else 1)+struct.pack('>iI',-seq if final else seq,len(gzip.compress(b)))+gzip.compress(b)
def parse(b):
 hs=(b[0]&15)*4; typ,fl=b[1]>>4,b[1]&15; ser,comp=b[2]>>4,b[2]&15; o=hs; z={'message_type':typ,'flags':fl,'is_last_package':bool(fl&2),'code':0}
 if fl&1: z['sequence']=struct.unpack_from('>i',b,o)[0]; o+=4
 if typ==9: z['payload_size']=struct.unpack_from('>I',b,o)[0]; o+=4
 elif typ==15: z['code']=struct.unpack_from('>i',b,o)[0]; o+=4; z['payload_size']=struct.unpack_from('>I',b,o)[0]; o+=4
 body=b[o:]
 if comp==1 and body: body=gzip.decompress(body)
 if ser==1 and body: z['payload']=json.loads(body.decode())
 elif body: z['payload_base64']=base64.b64encode(body).decode()
 return z

def mode(path): os.chmod(path,0o600)
def now(): return datetime.now(timezone.utc).isoformat()
def prep():
 OUT.mkdir(parents=True,exist_ok=True); TMP.mkdir(exist_ok=True); os.chmod(OUT,0o700); os.chmod(TMP,0o700)
 # duration via ffprobe, then fixed 5m windows stepped by 298s
 import subprocess
 dur=float(subprocess.check_output([FFPROBE,'-v','error','-show_entries','format=duration','-of','default=noprint_wrappers=1:nokey=1',str(AUDIO)],text=True).strip())
 step=SEG_MS-OVERLAP_MS; segments=[]; start=0; n=0
 while start<round(dur*1000):
  end=min(start+SEG_MS,round(dur*1000)); n+=1; pcm=TMP/f'segment-{n:02d}.pcm'
  subprocess.run([FFMPEG,'-nostdin','-v','error','-ss',f'{start/1000:.3f}','-i',str(AUDIO),'-t',f'{(end-start)/1000:.3f}','-f','s16le','-acodec','pcm_s16le','-ar','16000','-ac','1','-y',str(pcm)],check=True)
  mode(pcm); segments.append({'index':n,'start_ms':start,'end_ms':end,'pcm':pcm.name,'status':'prepared','attempts':[]})
  if end>=round(dur*1000): break
  start+=step
 return dur,segments

async def transcribe(seg,key,attempt):
 idx=seg['index']; rid=uuid.uuid4().hex; cid=uuid.uuid4().hex; stem=f"segment-{idx:02d}-attempt-{attempt}"
 rawpath=TMP/(stem+'.bin'); jsonpath=TMP/(stem+'.jsonl'); pcm=TMP/seg['pcm']
 req={'user':{'uid':'recording-mcp-full-validation'},'audio':{'format':'pcm','codec':'raw','rate':16000,'bits':16,'channel':1},'request':{'model_name':'bigmodel','enable_nonstream':True,'enable_speaker_info':True,'show_utterances':True,'result_type':'full','enable_itn':True,'enable_punc':True}}
 headers={'X-Api-Key':key,'X-Api-Resource-Id':RESOURCE,'X-Api-Request-Id':rid,'X-Api-Connect-Id':cid,'X-Api-Sequence':'-1'}
 ack=asyncio.Event(); final=asyncio.Event(); errors=[]; frames=0; logid=None
 async with websockets.connect(URL,additional_headers=headers,open_timeout=30,close_timeout=10,max_size=16*1024*1024,ping_interval=20) as ws:
  headers.pop('X-Api-Key',None); logid=ws.response.headers.get('X-Tt-Logid')
  with rawpath.open('wb') as raw,jsonpath.open('w',encoding='utf8') as out:
   async def receiver():
    nonlocal frames
    while not final.is_set():
     try: fr=await asyncio.wait_for(ws.recv(),timeout=2)
     except asyncio.TimeoutError: continue
     if not isinstance(fr,bytes): errors.append({'error':'unexpected_text_frame'}); continue
     raw.write(struct.pack('>I',len(fr))+fr); obj=parse(fr); out.write(json.dumps(obj,ensure_ascii=False)+'\n'); out.flush(); raw.flush(); frames+=1
     if obj['message_type']==15 or obj.get('code'):
      errors.append({'code':obj.get('code'),'payload':obj.get('payload')}); final.set(); return
     if obj.get('is_last_package'): final.set()
     if obj.get('message_type')==9: ack.set()
   rt=asyncio.create_task(receiver())
   await ws.send(full(1,req))
   try: await asyncio.wait_for(ack.wait(),20)
   except asyncio.TimeoutError: raise RuntimeError('init_ack_timeout')
   if errors: raise RuntimeError('server_error:'+json.dumps(errors[-1],ensure_ascii=False))
   t0=time.monotonic(); n=0
   with pcm.open('rb') as f:
    while block:=f.read(CHUNK):
     n+=1; last=f.tell()>=pcm.stat().st_size
     await ws.send(audio(n+1,block,final=last))
     if errors: raise RuntimeError('server_error:'+json.dumps(errors[-1],ensure_ascii=False))
     if not last:
      due=t0+n*0.2; await asyncio.sleep(max(0,due-time.monotonic()))
   try: await asyncio.wait_for(final.wait(),max(60, pcm.stat().st_size/32000*0.3))
   except asyncio.TimeoutError: pass
   if not final.is_set(): rt.cancel(); raise RuntimeError('final_package_timeout')
   await rt
   if errors: raise RuntimeError('server_error:'+json.dumps(errors[-1],ensure_ascii=False))
 mode(rawpath); mode(jsonpath)
 if not final.is_set(): raise RuntimeError('final_package_missing')
 return {'request_id':rid,'connect_id':cid,'server_log_id':logid,'frames':frames,'raw':rawpath.name,'parsed':jsonpath.name,'final_package_received':True,'audio_seconds':round(time.monotonic()-t0,2)}

async def main():
 if not AUDIO.is_file(): raise FileNotFoundError(AUDIO)
 dur,segs=prep(); key=getpass.getpass('Volcengine API key (hidden input): ')
 metadata={'audio_file_name':AUDIO.name,'audio_sha256':hashlib.sha256(AUDIO.read_bytes()).hexdigest(),'duration_seconds':dur,'endpoint':URL,'resource_id':RESOURCE,'chunk_seconds':300,'overlap_seconds':2,'packet_ms':200,'speaker_scope':'per segment/request only','started_at':now(),'segments':segs,'concurrency':CONCURRENCY}
 meta=OUT/'run.json'; meta.write_text(json.dumps(metadata,ensure_ascii=False,indent=2)+'\n'); mode(meta)
 sem=asyncio.Semaphore(CONCURRENCY); completed=0
 async def job(s):
  nonlocal completed
  async with sem:
   for attempt in range(1,4):
    started=time.monotonic()
    try:
     result=await transcribe(s,key,attempt); result.update({'attempt':attempt,'duration_seconds':round(time.monotonic()-started,2)}); s['status']='complete'; s['attempts'].append(result); break
    except Exception as e:
     err=f'{type(e).__name__}: {e}'; s['attempts'].append({'attempt':attempt,'error':err}); s['status']='retrying' if attempt<3 else 'failed'; meta.write_text(json.dumps(metadata,ensure_ascii=False,indent=2)+'\n'); mode(meta)
     if attempt==3: break
     await asyncio.sleep(attempt*5)
   completed+=1; metadata['updated_at']=now(); meta.write_text(json.dumps(metadata,ensure_ascii=False,indent=2)+'\n'); mode(meta)
   print(f"SEGMENT {s['index']}/{len(segs)} {s['status']} ({completed}/{len(segs)})",flush=True)
 await asyncio.gather(*(job(s) for s in segs)); key=''
 metadata['completed_at']=now(); metadata['coverage']={'start_ms':0,'end_ms':round(dur*1000),'duration_seconds':dur}; metadata['all_segments_complete']=all(s['status']=='complete' for s in segs); meta.write_text(json.dumps(metadata,ensure_ascii=False,indent=2)+'\n'); mode(meta)
 if not metadata['all_segments_complete']: raise RuntimeError('one or more segments failed; inspect run.json')
 assemble(metadata)

def assemble(meta):
 turns=[]
 for s in meta['segments']:
  result=s['attempts'][-1]; parsed=TMP/result['parsed']; final_obj=None
  for line in parsed.read_text(encoding='utf8').splitlines():
   x=json.loads(line)
   if x.get('is_last_package'): final_obj=x
  if final_obj is None: raise RuntimeError(f"segment {s['index']} missing final response")
  payload=final_obj.get('payload') or {}; res=payload.get('result') or {}; utterances=res.get('utterances') or []
  for j,u in enumerate(utterances):
   add=u.get('additions') or {}; speaker=add.get('speaker_id',u.get('speaker_id')); st=u.get('start_time'); en=u.get('end_time')
   # Provider timestamps are milliseconds in current API result; retain null when absent/unparseable.
   try: st=int(st) if st is not None else None
   except (ValueError,TypeError): st=None
   try: en=int(en) if en is not None else None
   except (ValueError,TypeError): en=None
   turns.append({'id':f"seg{s['index']:02d}-turn{j+1:04d}",'segment_index':s['index'],'segment_start_ms':s['start_ms'],'segment_end_ms':s['end_ms'],'local_start_ms':st,'local_end_ms':en,'global_start_ms':s['start_ms']+st if st is not None else None,'global_end_ms':s['start_ms']+en if en is not None else None,'speaker_local_id':str(speaker) if speaker is not None else None,'speaker_scoped_id':f"segment-{s['index']:02d}:speaker-{speaker}" if speaker is not None else None,'text':u.get('text',''),'definite':u.get('definite'),'overlap_duplicate':False,'duplicate_of':None})
 # Mark exact text/time duplicates from the two-second adjacent-window overlap; do not join speakers.
 seen=[]
 for t in turns:
  if not t['text'] or t['global_start_ms'] is None or t['global_end_ms'] is None: continue
  for prev in seen:
   if prev['segment_index']!=t['segment_index'] and prev['text'].strip()==t['text'].strip() and abs(prev['global_start_ms']-t['global_start_ms'])<=2000 and abs(prev['global_end_ms']-t['global_end_ms'])<=2000:
    t['overlap_duplicate']=True; t['duplicate_of']=prev['id']; break
  seen.append(t)
 merged={'audio_file_name':meta['audio_file_name'],'duration_seconds':meta['duration_seconds'],'coverage':meta['coverage'],'turns':turns}
 p=OUT/'transcript.json'; p.write_text(json.dumps(merged,ensure_ascii=False,indent=2)+'\n'); mode(p)
 lines=[f"# 完整录音转写\n\n文件：{meta['audio_file_name']}（{meta['duration_seconds']:.3f} 秒）\n覆盖：00:00.000–{meta['duration_seconds']//60:02.0f}:{meta['duration_seconds']%60:06.3f}\n\n说话人编号仅在分段内有效；分段重叠中检测到的重复轮次会标记并在本阅读版中省略。\n"]
 for t in turns:
  if t['overlap_duplicate']: continue
  stamp='时间未知' if t['global_start_ms'] is None else f"{t['global_start_ms']//60000:02d}:{(t['global_start_ms']%60000)/1000:06.3f}"
  sp=t['speaker_scoped_id'] or '说话人未知'; lines.append(f"[{stamp}] {sp}：{t['text']}")
 txt=OUT/'transcript.md'; txt.write_text('\n'.join(lines)+'\n',encoding='utf8'); mode(txt)
 report={'completed_at':now(),'duration_seconds':meta['duration_seconds'],'coverage_start_ms':0,'coverage_end_ms':meta['coverage']['end_ms'],'segments_total':len(meta['segments']),'segments_final_received':sum(1 for s in meta['segments'] if s['status']=='complete' and s['attempts'][-1].get('final_package_received')),'turns_total':len(turns),'overlap_duplicates_marked':sum(t['overlap_duplicate'] for t in turns),'turns_without_global_start':sum(t['global_start_ms'] is None for t in turns),'speaker_scope':'IDs are local to one segment/request; cross-segment identity is not inferred','raw_segment_responses':'segments/*.bin and *.jsonl','transcript':'transcript.md','merged_json':'transcript.json','api_key_saved':False}
 rp=OUT/'report.json'; rp.write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n'); mode(rp)
 print(f"ASSEMBLY_DONE turns={len(turns)} final_segments={report['segments_final_received']}/{len(meta['segments'])}",flush=True)

if __name__=='__main__':
 import argparse
 parser = argparse.ArgumentParser(description='Optional upstream ASR experiment; not part of the context-memory server')
 parser.add_argument('audio', type=Path, help='Local audio file to transcribe')
 AUDIO = parser.parse_args().audio
 try: asyncio.run(main())
 except Exception as e: print(f"ASR_FULL_FAILED {type(e).__name__}: {e}",file=sys.stderr,flush=True); raise
