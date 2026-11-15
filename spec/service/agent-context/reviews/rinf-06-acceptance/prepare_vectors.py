"""Prepare resumable supplier embeddings; no relation labels are sent."""
import concurrent.futures as futures
import hashlib
import json
import os
from pathlib import Path
import threading
import urllib.request
import time
import sys

BASE=Path(__file__).resolve().parent
ROOT=next(p for p in BASE.parents if (p/'go.mod').exists())

def main():
    import fcntl
    campaign_lock=(BASE/"vectors.lock").open("a")
    fcntl.flock(campaign_lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
    raw=(BASE/'samples.json').read_bytes();samples=json.loads(raw)['samples']
    texts={hashlib.sha256(s[k]['body'].encode()).hexdigest():s[k]['body'] for s in samples for k in ('target','candidate','distractor')}
    key=os.environ.get('GEMINI_API_KEY','')
    if not key:
        for line in (ROOT/'.env').read_text().splitlines():
            k,sep,v=line.partition('=')
            if sep and k.strip()=='GEMINI_API_KEY':key=v.strip().strip('\"\x27')
    if not key:raise RuntimeError('missing key')
    ledger=BASE/'vector-receipts.jsonl';items={};tokens=0;prior_charge=0.;intents={};settled={};attempts={}
    if (BASE/'vectors.json').exists():raise RuntimeError('vectors already complete; no new API calls')
    if ledger.exists():
        if '--resume' not in sys.argv:raise RuntimeError('partial embedding campaign; explicit --resume required')
        for line in ledger.read_text().splitlines():
            r=json.loads(line);digest=r['body_sha256'];attempts[digest]=max(attempts.get(digest,0),r['attempt'])
            if r['status']=='started':intents[r['request_id']]=r
            else:
                settled[r['request_id']]=r
                if r['status']=='completed':
                    items[digest]=r['values'];tokens+=r['input_tokens']
        for k,r in intents.items():
            row=settled.get(k);prior_charge+=(row['cost_usd'] if row and row['status']=='completed' else r['reservation_usd'])
    lock=threading.Lock();stop=threading.Event();start=time.monotonic()
    f=ledger.open('a');os.chmod(ledger,0o600)
    charged=prior_charge
    def durable(row):f.write(json.dumps(row)+'\n');f.flush();os.fsync(f.fileno())
    def embed(pair):
        nonlocal tokens,charged
        digest,text=pair
        if stop.is_set():return
        payload={'model':'models/gemini-embedding-2','content':{'parts':[{'text':'title: none | text: '+text}]},'embedContentConfig':{'outputDimensionality':768,'autoTruncate':False}}
        body=json.dumps(payload,ensure_ascii=False).encode()
        reservation=(len(body)+1024)*.2/1e6
        with lock:
            if charged+reservation>.5:stop.set();raise RuntimeError('embedding reservation exceeded')
            charged+=reservation;attempt=attempts.get(digest,0)+1;attempts[digest]=attempt
            request_id=digest+':'+str(attempt)
            durable({'request_id':request_id,'attempt':attempt,'body_sha256':digest,'status':'started','reservation_usd':reservation})
        r={'request_id':request_id,'attempt':attempt,'body_sha256':digest,'status':'failed','reservation_usd':reservation}
        try:
            req=urllib.request.Request('https://generativelanguage.googleapis.com/v1beta/models/gemini-embedding-2:embedContent',data=body,headers={'Content-Type':'application/json','x-goog-api-key':key})
            with urllib.request.urlopen(req,timeout=60) as response:data=json.load(response)
            v=data['embedding']['values'];n=data['usageMetadata']['promptTokenCount']
            if len(v)!=768 or type(n) is not int or n<0:raise ValueError('invalid embedding contract')
            r.update(status='completed',input_tokens=n,values=v,cost_usd=n*.2/1e6)
        except Exception as e:r['error_type']=type(e).__name__;stop.set()
        with lock:
            durable(r)
            if r['status']=='completed':
                items[digest]=r['values'];tokens+=r['input_tokens'];charged+=r['cost_usd']-reservation
            if len(items)%25==0:print(json.dumps({'embedded':len(items),'planned':len(texts),'cost_usd':tokens*.2/1e6,'elapsed_s':round(time.monotonic()-start)}),flush=True)
    try:
        with futures.ThreadPoolExecutor(max_workers=4) as pool:list(pool.map(embed,[(d,t) for d,t in texts.items() if d not in items]))
    finally:f.close()
    if stop.is_set() or len(items)!=len(texts):raise RuntimeError('incomplete embeddings; preserve receipts')
    result={'model':'gemini-embedding-2','dimension':768,'sample_sha256':hashlib.sha256(raw).hexdigest(),'input_tokens':tokens,'cost_usd':tokens*.2/1e6,'conservative_spend_usd':charged,'items':items}
    with (BASE/'vectors.json').open('x') as f:json.dump(result,f)
    print(json.dumps({'completed':len(items),'tokens':tokens,'cost_usd':result['cost_usd']}),flush=True)

if __name__=='__main__':main()
