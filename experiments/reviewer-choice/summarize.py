import json,glob,sys,collections
for d in sorted(glob.glob('logs/*/')):
    cost=collections.Counter(); runs=0; tools=collections.Counter(); raw=[]; critic=[]
    for f in glob.glob(d+'run-*.jsonl'):
        last=None
        for l in open(f):
            if '"type":"assistant"' in l:
                e=json.loads(l)
                for b in e.get('message',{}).get('content',[]):
                    if b.get('type')=='tool_use' and b['name']!='StructuredOutput': tools[b['name']]+=1
            if '"type":"result"' in l: last=l
        if not last: continue
        e=json.loads(last); runs+=1
        for m,u in e.get('modelUsage',{}).items(): cost[m]+=u.get('costUSD',0)
        so=e.get('structured_output') or {}
        if 'valid' in so or 'verdict' in so: critic.append(so); continue
        for u in so.get('units',[so]):
            for i in u.get('issues') or []: raw.append((u.get('id','?'),i))
    print('=====',d, 'runs',runs,'cost $%.2f'%sum(cost.values()), {k:round(v,2) for k,v in cost.items()}, 'tools',dict(tools), 'wall',open(d+'wall_seconds').read().strip()+'s')
    for uid,i in raw: print('  RAW',i.get('severity'),'|',i.get('title'),'||',(i.get('detail') or '')[:300])
    for c in critic: print('  CRITIC',json.dumps(c)[:250])
